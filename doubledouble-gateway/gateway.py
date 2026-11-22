"""DoubleDouble source adapter, running in the existing server Chromium container.

The site's own page renders verification. No CAPTCHA is solved or bypassed here.
Only same-origin direct downloads are accepted; external uploads are always disabled.
"""
import json
import os
from pathlib import Path, PurePosixPath
import queue
import shutil
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ORIGIN = 'https://us.doubledouble.top'
HOME = Path(os.environ.get('DD_STATE', '/config/doubledouble'))
CDP = 'http://127.0.0.1:9223'
LIMIT = 1024**3
JOBS = {}
LOCK = threading.RLock()
PENDING = queue.Queue()

# Installed before the page's scripts. Keep the native verification flow intact.
# Observe responses rather than inventing or replaying a CAPTCHA answer.
BOOTSTRAP = r'''
(() => {
 const originalFetch = window.fetch.bind(window);
 window.fetch = async (input, options) => {
   const url = new URL(typeof input === 'string' ? input : input.url, location.href);
   if (url.origin === location.origin && url.pathname === '/dl') {
     url.searchParams.delete('external');
     url.searchParams.delete('support');
     url.searchParams.set('donotshare', 'true');
     input = url.href;
   }
   const response = await originalFetch(input, options);
   if (url.origin === location.origin && (url.pathname === '/dl' || url.pathname.startsWith('/dl/'))) {
     response.clone().json().then(data => {
       if (url.pathname === '/dl') window.__ddStart = data;
       else window.__ddStatus = data;
     }).catch(() => {});
   }
   return response;
 };
 window.open = url => { window.__ddDownloadURL = String(url); return null; };
 function directOnly() {
   const field = document.getElementById('external');
   if (field) { field.checked = false; if (!field.disabled) field.disabled = true; }
   const privacy = document.getElementById('private');
   if (privacy) privacy.checked = true;
 }
 document.addEventListener('DOMContentLoaded', () => {
   directOnly();
   new MutationObserver(directOnly).observe(document.body, {subtree:true, childList:true, attributes:true, attributeFilter:['disabled']});
 });
})();
'''


def direct_url(value):
    result = urllib.parse.urljoin(ORIGIN + '/', value)
    parsed = urllib.parse.urlsplit(result)
    if parsed.scheme != 'https' or parsed.netloc != urllib.parse.urlsplit(ORIGIN).netloc or parsed.username or parsed.password:
        raise ValueError('Refusing an external download host (Pixeldrain is disabled)')
    return result


class DirectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        direct_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def extract_flac(archive_path, output):
    """Accept exactly one real FLAC, never guess which track in an album was requested."""
    with zipfile.ZipFile(archive_path) as archive:
        entries = archive.infolist()
        if len(entries) > 1000 or sum(e.file_size for e in entries) > LIMIT:
            raise ValueError('Music ZIP exceeds the single-track size limit')
        for entry in entries:
            p = PurePosixPath(entry.filename)
            mode = (entry.external_attr >> 16) & 0o170000
            if p.is_absolute() or '..' in p.parts or '\\' in entry.filename or mode not in (0, 0o100000, 0o040000):
                raise ValueError('Unsafe ZIP entry')
        tracks = [e for e in entries if not e.is_dir() and PurePosixPath(e.filename).suffix.lower() == '.flac' and not any(p.startswith('.') or p == '__MACOSX' for p in PurePosixPath(e.filename).parts)]
        if len(tracks) != 1:
            raise ValueError('Expected exactly one FLAC track in the download')
        if archive.testzip() is not None:
            raise ValueError('Corrupt music ZIP')
        with archive.open(tracks[0]) as source, open(output, 'wb') as target:
            if source.read(4) != b'fLaC':
                raise ValueError('Download is not a lossless FLAC')
            target.write(b'fLaC')
            shutil.copyfileobj(source, target, length=1024*1024)
        return tracks[0].filename


class Browser:
    def __init__(self):
        from websockets.sync.client import connect
        HOME.mkdir(parents=True, exist_ok=True)
        try:
            pages = json.load(urllib.request.urlopen(CDP + '/json/list', timeout=2))
        except (OSError, ValueError):
            env = dict(os.environ, DISPLAY=os.environ.get('DD_DISPLAY', ':0'))
            self.process = subprocess.Popen([
                '/usr/lib/chromium/chromium', '--no-sandbox', '--disable-dev-shm-usage',
                '--remote-debugging-port=9223', '--user-data-dir=' + str(HOME / 'profile'),
                '--no-first-run', '--no-default-browser-check', '--new-window', 'about:blank'
            ], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for _ in range(60):
                time.sleep(0.5)
                try:
                    pages = json.load(urllib.request.urlopen(CDP + '/json/list', timeout=2))
                    break
                except (OSError, ValueError):
                    if self.process.poll() is not None:
                        raise RuntimeError('Verification browser could not start')
            else:
                raise RuntimeError('Verification browser did not become ready')
        page = next(p for p in pages if p.get('type') == 'page')
        self.socket = connect(page['webSocketDebuggerUrl'], max_size=4*1024*1024)
        self.sequence = 0
        self.command('Page.enable')
        self.script = self.command('Page.addScriptToEvaluateOnNewDocument', {'source': BOOTSTRAP})['identifier']

    def command(self, method, params=None):
        self.sequence += 1
        self.socket.send(json.dumps({'id': self.sequence, 'method': method, 'params': params or {}}))
        while True:
            response = json.loads(self.socket.recv(timeout=30))
            if response.get('id') == self.sequence:
                if 'error' in response:
                    raise RuntimeError('Verification browser command failed')
                return response.get('result', {})

    def evaluate(self, expression):
        result = self.command('Runtime.evaluate', {'expression': expression, 'returnByValue': True})
        if result.get('exceptionDetails'):
            raise RuntimeError('Verification page script failed')
        return result.get('result', {}).get('value')

    def close(self):
        try:
            self.command('Page.removeScriptToEvaluateOnNewDocument', {'identifier': self.script})
            self.command('Page.navigate', {'url': 'about:blank'})
        finally:
            self.socket.close()


def update(job, **fields):
    with LOCK:
        job.update(fields)


def process(job):
    browser = None
    try:
        update(job, state='resolving')
        browser = Browser()
        browser.command('Page.navigate', {'url': ORIGIN + '/?url=' + urllib.parse.quote(job['url'], safe='')})
        for _ in range(90):
            if browser.evaluate("document.readyState === 'complete' && !!document.getElementById('dl-button') && typeof dl === 'function'"):
                break
            time.sleep(1)
        else:
            raise RuntimeError('DoubleDouble page did not load')
        browser.evaluate("document.getElementById('external').checked=false; document.getElementById('dl-button').click(); true")
        deadline = time.monotonic() + 25*60
        while time.monotonic() < deadline:
            if job['cancel'].is_set():
                raise RuntimeError('Download cancelled')
            state = browser.evaluate("({start:window.__ddStart,status:window.__ddStatus,verification:document.getElementById('prompt-captcha')?.style.display !== 'none',error:document.getElementById('error')?.style.display !== 'none' ? document.getElementById('error')?.innerText : ''})") or {}
            start = state.get('start') or {}
            status = state.get('status') or {}
            if status.get('status') == 'done':
                url = direct_url(status.get('url', ''))
                if not status.get('url'):
                    raise RuntimeError('DoubleDouble supplied no download URL')
                update(job, state='downloading')
                fetch_result(job, url)
                return
            if status.get('status') == 'error':
                raise RuntimeError(status.get('message') or 'DoubleDouble download failed')
            if state.get('error') and not state.get('verification'):
                raise RuntimeError(state['error'][:400])
            if start.get('success'):
                update(job, state='resolving', message=status.get('friendlyStatus', 'Preparing FLAC'))
            elif state.get('verification'):
                update(job, state='waiting_verification', message='Complete the CAPTCHA in server Chromium; Pixeldrain is disabled')
            time.sleep(1)
        raise RuntimeError('Verification or download timed out after 25 minutes; retry when ready')
    except Exception as error:
        update(job, state='failed', error=str(error))
    finally:
        if browser:
            try:
                browser.close()
            except Exception:
                pass


def fetch_result(job, url):
    folder = HOME / 'jobs' / job['id']
    folder.mkdir(parents=True, exist_ok=True)
    raw = folder / 'download.part'
    output = folder / 'track.flac'
    opener = urllib.request.build_opener(DirectRedirect())
    try:
        with opener.open(direct_url(url), timeout=120) as source, raw.open('wb') as target:
            size = int(source.headers.get('Content-Length', 0))
            if size > LIMIT:
                raise ValueError('Download exceeds single-track size limit')
            update(job, size=size)
            received = 0
            while chunk := source.read(1024*1024):
                if job['cancel'].is_set():
                    raise RuntimeError('Download cancelled')
                received += len(chunk)
                if received > LIMIT:
                    raise ValueError('Download exceeds single-track size limit')
                target.write(chunk)
                update(job, downloaded=received/(1024*1024))
        with raw.open('rb') as stream:
            magic = stream.read(4)
        if magic == b'fLaC':
            raw.rename(output)
        elif zipfile.is_zipfile(raw):
            extract_flac(raw, output)
        else:
            raise ValueError('Direct download did not contain FLAC audio')
        update(job, state='ready', filename='track.flac', size=output.stat().st_size, file=str(output))
    finally:
        raw.unlink(missing_ok=True)
        if job['state'] != 'ready':
            output.unlink(missing_ok=True)


def worker():
    while True:
        job = PENDING.get()
        try:
            if not job['cancel'].is_set():
                process(job)
        finally:
            if job['cancel'].is_set():
                shutil.rmtree(HOME / 'jobs' / job['id'], ignore_errors=True)
                with LOCK:
                    JOBS.pop(job['id'], None)
            PENDING.task_done()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass  # Source URLs and verification tokens do not belong in access logs.

    def respond(self, data, status=200):
        encoded = json.dumps(data).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        parts = urllib.parse.urlsplit(self.path).path.strip('/').split('/')
        if parts == ['']:
            with LOCK:
                waiting = any(j['state'] == 'waiting_verification' for j in JOBS.values())
            return self.respond({'ok': True, 'source': 'DoubleDouble', 'logged_in': True, 'waiting_verification': waiting, 'external_uploads': False})
        with LOCK:
            job = JOBS.get(parts[1]) if len(parts) >= 2 and parts[0] == 'fetch' else None
        if not job:
            return self.respond({'error': 'Unknown job'}, 404)
        if len(parts) == 3 and parts[2] == 'file':
            if job['state'] != 'ready':
                return self.respond({'error': 'File is not ready'}, 409)
            with open(job['file'], 'rb') as source:
                self.send_response(200)
                self.send_header('Content-Type', 'audio/flac')
                self.send_header('Content-Length', str(job['size']))
                self.end_headers()
                shutil.copyfileobj(source, self.wfile)
            return
        return self.respond({k:v for k,v in job.items() if k not in ('cancel', 'url', 'file')})

    def do_POST(self):
        if self.path != '/fetch':
            return self.respond({'error': 'Not found'}, 404)
        try:
            length = int(self.headers.get('Content-Length', 0))
            if not 0 < length <= 8192:
                raise ValueError('Invalid request size')
            data = json.loads(self.rfile.read(length))
            url = urllib.parse.urlsplit(data['url'])
            # Link-only; the facade supplies a known recording, never a fuzzy match.
            if url.scheme != 'https' or url.netloc not in ('www.deezer.com', 'deezer.com') or not url.path.startswith('/track/') or not url.path.removeprefix('/track/').isdigit():
                raise ValueError('Expected an exact Deezer track URL')
            ident = uuid.uuid4().hex
            job = {'id': ident, 'state': 'queued', 'url': data['url'], 'cancel': threading.Event(), 'size':0, 'downloaded':0, 'speed_mbps':0}
            with LOCK:
                if len(JOBS) >= 100:
                    return self.respond({'error': 'Source queue is full'}, 429)
                JOBS[ident] = job
            PENDING.put(job)
            return self.respond({'job_id': ident})
        except (ValueError, KeyError, TypeError):
            return self.respond({'error': 'Invalid track request'}, 400)

    def do_DELETE(self):
        parts = self.path.strip('/').split('/')
        with LOCK:
            job = JOBS.get(parts[1]) if len(parts) == 2 and parts[0] == 'fetch' else None
        if not job:
            return self.respond({'error': 'Unknown job'}, 404)
        job['cancel'].set()
        # The worker owns the files while active; remove only completed job artifacts.
        if job['state'] in ('ready', 'failed', 'queued'):
            shutil.rmtree(HOME / 'jobs' / job['id'], ignore_errors=True)
            with LOCK:
                JOBS.pop(job['id'], None)
        return self.respond({'ok': True})


if __name__ == '__main__':
    threading.Thread(target=worker, daemon=True).start()
    ThreadingHTTPServer(('0.0.0.0', 8082), Handler).serve_forever()
