"""
Minimal self-hosted Telegram gateway for SpotiFLAC — the FlacIt pipeline.

Drives @deezload2bot over MTProto (Telethon) to fetch lossless FLAC sourced from
Deezer, and downloads the resulting document with 16 parallel exported senders
against the file's own DC — the same technique as FlacIt's
`ultra_parallel_download`, at 3.5–4.6 MB/s instead of the ~0.3 MB/s a single
MTProto stream is throttled to.

SpotiFLAC's Go downloader talks to this over a small job API:

    POST   /fetch            {"url": "<spotify or deezer track url>",
                               "title": "<expected track title>",     # optional
                               "artist": "<expected artist>"}         # optional
                              -> {job_id, state}
    GET    /fetch/<job_id>    -> {state, filename, size, downloaded, speed_mbps, error}
    GET    /fetch/<job_id>/file  -> streams the finished FLAC
    DELETE /fetch/<job_id>    -> drops the temp file

A job API rather than one blocking request because bot delivery can take tens of
seconds and the parallel download completes out of order — nothing here is safe
to serve synchronously from a single HTTP request/response.

Fetches are processed **one at a time**, in the order received. The bot chat is a
single stateful conversation — a reply is matched by "a new inbound message
carrying a FLAC, after the id recorded before sending" — so two concurrent fetches
would race for the same reply. SpotiFLAC's own download worker is already serial,
so this costs nothing in practice.

Login is a one-time browser step at http://<this-host>:8082/login — but this is
meant to be bootstrapped by copying an already-authenticated Telethon session into
place (see the SpotiFLAC deploy notes), because a fresh session also needs a human
to open Telegram, start @deezload2bot, and join its channel once — /login alone
does not do that.

Personal use only.
"""
import asyncio
import math
import os
import re
import threading
import time
from datetime import datetime, timedelta, timezone

from flask import Flask, jsonify, request, send_file
from telethon import TelegramClient, errors, events, functions, types
from telethon.tl.types import DocumentAttributeFilename, ReplyInlineMarkup

from matching import doc_text, matches_expected

API_ID = int(os.environ.get("TG_API_ID", "2040"))
API_HASH = os.environ.get("TG_API_HASH", "b18441a1ff607e10a989891a5462e627")
BOT = os.environ.get("TG_BOT", "deezload2bot")

SESSION_FILE = os.environ.get("SESSION_FILE", "/config/telegram-session.session")
_SESSION_NAME = SESSION_FILE[:-len(".session")] if SESSION_FILE.endswith(".session") else SESSION_FILE
CONFIG_DIR = os.path.dirname(SESSION_FILE) or "/config"
JOBS_DIR = os.path.join(CONFIG_DIR, "jobs")
FLAC_QUALITY_FLAG = os.path.join(CONFIG_DIR, ".flac_quality_set")

# Both of these were tuned for a bot that answered in 5-13s. Measured again on
# 2026-07-31 from this service's own request log, bracketing POST /fetch to
# GET /fetch/<job>/file: 6s, 97s, 118s, 178s. The old values were badly wrong
# against that range, in two compounding ways.
#
#   - At 35s the resend fired on essentially *every* download, so the Telegram
#     log filled with the same track requested twice. That is wasted work
#     against the very service whose slowness is the whole cost, and it opens a
#     correctness hole: the duplicate reply arrives after this job is finished,
#     is newer than the *next* job's `check_after`, and can be picked up as that
#     job's document — delivering the wrong track under the right name.
#   - At 120s a 178s reply is scored a failure, so SpotiFLAC's queue retried the
#     whole fetch (up to queueMaxAttempts=3), asking the bot yet again.
#
# 90s is comfortably past the observed slow case, so a resend now means the
# request was genuinely dropped. 270s stays under backend/flacit.go's
# flacItJobDeadline of 5 minutes, which must remain the outer bound.
FLAC_TIMEOUT = 270          # total seconds to wait for the bot's FLAC document
FLAC_RETRY_AFTER = 90       # resend the link once if nothing has arrived by then
JOB_REAP_AFTER = 600        # drop finished jobs (and their temp files) after this
DOWNLOAD_CONNECTIONS = 16
PART_SIZE = 512 * 1024
# 16 senders hammering GetFile will eventually trip Telegram's rate limiter on a
# long track — it asks for a wait rather than failing, and honouring it costs a
# second. Not honouring it failed the whole job, which the queue then retried
# from scratch three times.
FLOOD_MAX_RETRIES = 5
FLOOD_WAIT_CEILING = 60  # a longer demand than this is not worth blocking a job on

os.makedirs(JOBS_DIR, exist_ok=True)

app = Flask(__name__)

# ---------------------------------------------------------------- event loop

_loop = asyncio.new_event_loop()


def _run_loop():
    asyncio.set_event_loop(_loop)
    _loop.run_forever()


threading.Thread(target=_run_loop, daemon=True).start()


def _await(coro, timeout=None):
    """Bridge a coroutine from Flask's (sync) request thread onto the client's
    dedicated event-loop thread, and block for the result."""
    return asyncio.run_coroutine_threadsafe(coro, _loop).result(timeout)


client = None


client = None
_fetch_queue = None
# Set by the update handler whenever @deezload2bot sends anything, so the wait
# in _process_fetch wakes the moment a reply lands instead of on a poll tick.
_bot_activity = None


async def _create_client():
    # Both must be constructed on the loop thread, not the main thread. On
    # Python < 3.10, asyncio.Queue() binds to whatever loop get_event_loop()
    # returns *at construction time* — building it on the main thread ties it
    # to a loop that's never running, so `.get()` blocks forever with no error.
    # TelegramClient has the same trap for a different reason: constructing it
    # on the main thread (even while passing `loop=_loop` to steer it) leaves
    # its internals split across two event-loop contexts, and `connect()` then
    # hangs forever, also silently.
    global client, _fetch_queue, _bot_activity
    client = TelegramClient(_SESSION_NAME, API_ID, API_HASH)
    _fetch_queue = asyncio.Queue()
    # Same loop-affinity trap as the Queue above: asyncio.Event binds to the
    # loop that is current when it is constructed.
    _bot_activity = asyncio.Event()


_await(_create_client())

# ---------------------------------------------------------------- job state

jobs = {}
jobs_lock = threading.Lock()
_active_job_id = None

# Job ids whose caller has given up on them (deleted while still in flight).
# The fetches are processed one at a time, so a fetch stuck waiting on a bad
# link would otherwise block every job behind it for up to FLAC_TIMEOUT with
# no way to skip it — this is checked at each wait/poll point so an abandoned
# job gets dropped instead. Plain set(), no lock: add/discard/contains on a
# set of hashable items is atomic under the GIL for our purposes here.
_cancelled_jobs = set()


class _JobCancelled(Exception):
    pass


def _check_not_cancelled(job_id):
    if job_id in _cancelled_jobs:
        raise _JobCancelled(job_id)


def _new_job(url, expect=None):
    job_id = os.urandom(6).hex()
    with jobs_lock:
        jobs[job_id] = {
            "id": job_id,
            "url": url,
            "expect": expect or {},
            "state": "queued",
            "filename": None,
            "size": 0,
            "downloaded": 0,
            "speed_mbps": 0.0,
            "mismatched": 0,
            "error": None,
            "path": None,
            "created": time.time(),
        }
    return job_id


def _job_view(job):
    return {k: v for k, v in job.items() if k not in ("path", "url", "created", "expect")}


def _is_flac(msg):
    """Mime and filename check — the bot silently falls back to MP3 320kbps if
    quality wasn't set, and this is what catches that instead of filing a lossy
    file under a .flac name."""
    if msg.audio:
        return True
    if msg.document:
        mime = getattr(msg.document, "mime_type", "") or ""
        if "flac" in mime:
            return True
        for attr in getattr(msg.document, "attributes", []):
            fn = getattr(attr, "file_name", "") or ""
            if fn.lower().endswith(".flac"):
                return True
    return False


def _safe_filename(name):
    return re.sub(r"[^A-Za-z0-9._-]+", "_", name)[:200] or "track.flac"


# ---------------------------------------------------------------- FLAC quality

async def _ensure_flac_quality():
    """Navigate /settings to lock @deezload2bot's output to FLAC.

    Ported from FlacIt's set_flac_quality.py / newsong_dl.py. Two traps recorded
    there and preserved here: clicking "Audio Quality" *edits* the settings
    message rather than sending a new one (re-fetch by id, don't wait for a new
    message), and message filtering must be id-based, not date-based — Telegram
    timestamps are whole seconds and can spuriously match datetime.now().
    """
    if os.path.exists(FLAC_QUALITY_FLAG):
        return
    try:
        last_id = 0
        async for msg in client.iter_messages(BOT, limit=1):
            last_id = msg.id
        await client.send_message(BOT, "/settings")

        settings_msg = None
        deadline = time.time() + 15
        while time.time() < deadline:
            async for msg in client.iter_messages(BOT, limit=5):
                if msg.id <= last_id:
                    break
                if msg.out:
                    continue
                if msg.reply_markup and isinstance(msg.reply_markup, ReplyInlineMarkup):
                    settings_msg = msg
                    break
            if settings_msg:
                break
            await asyncio.sleep(1)

        if not settings_msg:
            print("flacit-gateway: could not reach @deezload2bot settings — "
                  "set quality to FLAC manually", flush=True)
            return

        clicked = False
        for row in settings_msg.reply_markup.rows:
            for btn in row.buttons:
                if "quality" in (btn.text or "").lower() or "audio" in (btn.text or "").lower():
                    await settings_msg.click(text=btn.text)
                    clicked = True
                    break
            if clicked:
                break
        if not clicked:
            print("flacit-gateway: no Audio Quality button in @deezload2bot settings", flush=True)
            return

        await asyncio.sleep(2)
        updated = await client.get_messages(BOT, ids=[settings_msg.id])
        updated_msg = updated[0] if updated else None
        if updated_msg and updated_msg.reply_markup:
            for row in updated_msg.reply_markup.rows:
                for btn in row.buttons:
                    if "flac" in (btn.text or "").lower():
                        await updated_msg.click(text=btn.text)
                        open(FLAC_QUALITY_FLAG, "w").close()
                        print("flacit-gateway: @deezload2bot quality set to FLAC", flush=True)
                        return
        print("flacit-gateway: no FLAC option in the Audio Quality submenu", flush=True)
    except Exception as e:  # noqa
        print(f"flacit-gateway: ensure_flac_quality failed: {e}", flush=True)


# ---------------------------------------------------------------- download

async def _parallel_download(document, save_path, job_id, connection_count=DOWNLOAD_CONNECTIONS):
    """16 exported senders on the document's own DC, each pulling 512KB parts
    off a shared queue — FlacIt's `ultra_parallel_download`, with one change:
    parts are written to disk at their offset as they arrive instead of held in
    an in-memory buffer, since a job here is served straight off disk."""
    dc_id = document.dc_id
    senders = [await client._borrow_exported_sender(dc_id) for _ in range(connection_count)]
    try:
        location = types.InputDocumentFileLocation(
            id=document.id,
            access_hash=document.access_hash,
            file_reference=document.file_reference,
            thumb_size="",
        )
        total_size = document.size
        part_count = math.ceil(total_size / PART_SIZE)

        work_queue = asyncio.Queue()
        for i in range(part_count):
            await work_queue.put((i * PART_SIZE, PART_SIZE))

        downloaded = 0
        t0 = time.time()
        last_update = 0.0

        with open(save_path, "wb") as f:
            f.truncate(total_size)

        # A single file handle, seeked per part: safe without a lock because
        # this all runs on one asyncio event loop — no two tasks execute Python
        # bytecode concurrently, only interleaved at `await` points.
        f = open(save_path, "r+b")
        try:
            async def worker(sender):
                nonlocal downloaded, last_update
                while True:
                    _check_not_cancelled(job_id)
                    try:
                        offset, limit = work_queue.get_nowait()
                    except asyncio.QueueEmpty:
                        return
                    req = functions.upload.GetFileRequest(
                        location=location, offset=offset, limit=limit,
                        precise=True, cdn_supported=False,
                    )

                    res = None
                    for _ in range(FLOOD_MAX_RETRIES):
                        try:
                            res = await sender.send(req)
                            break
                        except errors.FloodWaitError as fw:
                            if fw.seconds > FLOOD_WAIT_CEILING:
                                raise
                            _check_not_cancelled(job_id)
                            # +1: Telegram's own wait is a floor, and coming back
                            # a hair early just earns a longer one.
                            await asyncio.sleep(fw.seconds + 1)
                    if res is None:
                        raise RuntimeError(
                            "Telegram kept rate limiting this part after "
                            f"{FLOOD_MAX_RETRIES} attempts"
                        )
                    data = res.bytes
                    f.seek(offset)
                    f.write(data)
                    downloaded += len(data)
                    work_queue.task_done()

                    now = time.time()
                    if now - last_update >= 0.5 or downloaded >= total_size:
                        elapsed = now - t0
                        speed = (downloaded / 1024 / 1024) / elapsed if elapsed > 0 else 0
                        with jobs_lock:
                            if job_id in jobs:
                                jobs[job_id]["downloaded"] = round(downloaded / 1024 / 1024, 3)
                                jobs[job_id]["speed_mbps"] = round(speed, 3)
                        last_update = now

            await asyncio.gather(*(worker(s) for s in senders))
        finally:
            f.close()
    finally:
        for s in senders:
            await client._return_exported_sender(s)


async def _process_fetch(job_id):
    with jobs_lock:
        job = jobs[job_id]
        url = job["url"]
        expect = job.get("expect") or {}
        job["state"] = "resolving"

    sent_at = datetime.now(timezone.utc)
    try:
        await client.send_message(BOT, url)
    except errors.FloodWaitError as fw:
        await asyncio.sleep(fw.seconds)
        await client.send_message(BOT, url)

    flac_msg = None
    retried = False
    rejected_ids = set()
    start = time.time()
    check_after = sent_at - timedelta(seconds=5)

    while time.time() - start < FLAC_TIMEOUT:
        _check_not_cancelled(job_id)
        # Cleared *before* the scan, not after: a reply arriving while we are
        # mid-scan then leaves the flag set, and the wait below returns at once
        # instead of sleeping through a reply that is already sitting there.
        _bot_activity.clear()

        async for msg in client.iter_messages(BOT, limit=10):
            if msg.date < check_after:
                break
            if not _is_flac(msg):
                continue
            # Keep scanning past a FLAC that isn't ours rather than breaking on
            # the first one: the stale reply sits *above* the real one, so
            # stopping here would hand back the previous job's track.
            if not matches_expected(msg, expect):
                # Logged once per message, not once per scan — the stale reply
                # is re-seen every cycle until the real one lands above it.
                if msg.id not in rejected_ids:
                    rejected_ids.add(msg.id)
                    print(
                        f"[{job_id}] ignoring a FLAC that is not the requested track: "
                        f"got {doc_text(msg)!r}, wanted {expect.get('title')!r}",
                        flush=True,
                    )
                continue
            flac_msg = msg
            break
        if flac_msg:
            break

        if time.time() - start > FLAC_RETRY_AFTER and not retried:
            try:
                await client.send_message(BOT, url)
            except Exception:  # noqa
                pass
            retried = True

        # The bot's reply is what we're waiting for, so wake on it rather than
        # on a timer — this was a flat `sleep(2)`, which meant every fetch paid
        # ~1s of dead time on average. The 2s ceiling is kept as a backstop so a
        # missed or dropped update degrades to the old polling behaviour rather
        # than hanging until FLAC_TIMEOUT.
        try:
            await asyncio.wait_for(_bot_activity.wait(), timeout=2)
        except asyncio.TimeoutError:
            pass

    if flac_msg is None or flac_msg.document is None:
        with jobs_lock:
            job["state"] = "failed"
            job["mismatched"] = len(rejected_ids)
            if rejected_ids:
                # Distinguishable on purpose: "the bot answered, with the wrong
                # track" is a different problem from "the bot never answered",
                # and before this check the first was indistinguishable from
                # success.
                job["error"] = (
                    f"@deezload2bot delivered {len(rejected_ids)} FLAC(s), none matching "
                    f"{expect.get('title')!r}"
                )
            else:
                job["error"] = "timed out waiting for @deezload2bot to deliver a FLAC"
        return

    filename = "track.flac"
    for attr in flac_msg.document.attributes:
        if isinstance(attr, DocumentAttributeFilename) and attr.file_name:
            filename = attr.file_name
            break

    document = flac_msg.document
    with jobs_lock:
        job["filename"] = filename
        job["size"] = document.size
        job["state"] = "downloading"

    temp_path = os.path.join(JOBS_DIR, f"{job_id}.part")
    try:
        await _parallel_download(document, temp_path, job_id)
    except Exception as e:  # noqa
        with jobs_lock:
            job["state"] = "failed"
            job["error"] = f"download failed: {e}"
        if os.path.exists(temp_path):
            os.remove(temp_path)
        return

    final_path = os.path.join(JOBS_DIR, f"{job_id}-{_safe_filename(filename)}")
    os.replace(temp_path, final_path)
    with jobs_lock:
        job["path"] = final_path
        job["state"] = "ready"


async def _fetch_worker():
    global _active_job_id
    while True:
        job_id = await _fetch_queue.get()
        _active_job_id = job_id
        try:
            await _process_fetch(job_id)
        except _JobCancelled:
            pass  # caller already deleted the job; nothing left to report
        except Exception as e:  # noqa
            with jobs_lock:
                if job_id in jobs:
                    jobs[job_id]["state"] = "failed"
                    jobs[job_id]["error"] = str(e)
        finally:
            _active_job_id = None
            _cancelled_jobs.discard(job_id)
            _fetch_queue.task_done()


def _reap_loop():
    while True:
        time.sleep(60)
        cutoff = time.time() - JOB_REAP_AFTER
        with jobs_lock:
            stale = [jid for jid, j in jobs.items()
                     if j["created"] < cutoff and j["state"] in ("ready", "failed")]
            removed = [jobs.pop(jid) for jid in stale]
        for job in removed:
            path = job.get("path")
            if path and os.path.exists(path):
                try:
                    os.remove(path)
                except OSError:
                    pass


# ---------------------------------------------------------------- login

_login_state = {}


@app.route("/login", methods=["GET", "POST"])
def login():
    if request.method == "GET":
        if _await(client.is_user_authorized(), timeout=15):
            return _login_page(None, already=True)
        return _login_page(None, step="phone")

    data = request.get_json(silent=True) or request.form
    step = (data.get("step") or "phone").strip()
    try:
        if step == "phone":
            phone = (data.get("phone") or "").strip()
            if not phone:
                return _login_page("Phone number required.", step="phone"), 400
            sent = _await(client.send_code_request(phone))
            _login_state["phone"] = phone
            _login_state["phone_code_hash"] = sent.phone_code_hash
            return _login_page(None, step="code")

        if step == "code":
            phone = _login_state.get("phone")
            if not phone:
                return _login_page("Session expired — start over.", step="phone"), 400
            code = (data.get("code") or "").strip()
            try:
                _await(client.sign_in(phone=phone, code=code,
                                       phone_code_hash=_login_state["phone_code_hash"]))
            except errors.SessionPasswordNeededError:
                return _login_page(None, step="password")
            _login_state.clear()
            _await(_ensure_flac_quality())
            return _login_page(None, done=True)

        if step == "password":
            password = data.get("password") or ""
            _await(client.sign_in(password=password))
            _login_state.clear()
            _await(_ensure_flac_quality())
            return _login_page(None, done=True)

        return _login_page("Unknown step.", step="phone"), 400
    except Exception as e:  # noqa
        return _login_page(f"Login failed: {e}", step=step), 400


def _login_page(err, step="phone", done=False, already=False):
    if done:
        body = (
            "<h2>✅ Logged in to Telegram.</h2>"
            "<p>One more one-time step if this is a fresh account: open Telegram, "
            "start a chat with <b>@deezload2bot</b>, press Start, and join its "
            "channel when it asks. Without that every download times out. "
            "Then you can close this tab.</p>"
        )
    elif already:
        body = "<h2>Already logged in.</h2>"
    else:
        e = f"<p style='color:#e5484d'>{err}</p>" if err else ""
        if step == "code":
            body = (
                "<h2>Telegram login — enter the code</h2>" + e +
                "<form method='post'><input type='hidden' name='step' value='code'>"
                "<input name='code' style='width:100%;padding:8px' placeholder='12345' autofocus>"
                "<br><br><button type='submit' style='padding:8px 16px'>Continue</button></form>"
            )
        elif step == "password":
            body = (
                "<h2>Telegram login — two-factor password</h2>" + e +
                "<form method='post'><input type='hidden' name='step' value='password'>"
                "<input type='password' name='password' style='width:100%;padding:8px' "
                "placeholder='password' autofocus>"
                "<br><br><button type='submit' style='padding:8px 16px'>Continue</button></form>"
            )
        else:
            body = (
                "<h2>Telegram login</h2>"
                "<p>Enter the phone number for the account that has already started "
                "@deezload2bot.</p>" + e +
                "<form method='post'><input type='hidden' name='step' value='phone'>"
                "<input name='phone' style='width:100%;padding:8px' placeholder='+15551234567' autofocus>"
                "<br><br><button type='submit' style='padding:8px 16px'>Send code</button></form>"
            )
    return (
        "<!doctype html><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>FlacIt gateway login</title>"
        "<body style='font:15px/1.6 system-ui,sans-serif;max-width:640px;margin:40px auto;padding:0 16px;color:#111'>"
        + body + "</body>"
    )


# ---------------------------------------------------------------- fetch API

@app.route("/fetch", methods=["POST"])
def fetch():
    body = request.get_json(silent=True) or {}
    url = (body.get("url") or "").strip()
    if not url:
        return jsonify({"error": "missing url"}), 400
    # A Deezer link that isn't a track — an artist or album page — is something
    # the bot cannot act on, and accepting it costs FLAC_TIMEOUT of silence
    # followed by the caller's retries. Fail it in a second instead.
    if "deezer.com" in url.lower() and "/track/" not in url:
        return jsonify({"error": f"not a Deezer track link: {url}"}), 400
    if not _await(client.is_user_authorized(), timeout=15):
        return jsonify({"error": "not logged in — open /login"}), 401

    # Optional, and old callers that omit it keep the previous behaviour: with
    # no expectation every FLAC is accepted, exactly as before.
    expect = {
        "title": (body.get("title") or "").strip(),
        "artist": (body.get("artist") or "").strip(),
    }
    job_id = _new_job(url, expect)
    _await(_fetch_queue.put(job_id))
    return jsonify({"job_id": job_id, "state": "queued"})


@app.route("/fetch/<job_id>")
def fetch_status(job_id):
    with jobs_lock:
        job = jobs.get(job_id)
    if not job:
        return jsonify({"error": "no such job"}), 404
    return jsonify(_job_view(job))


@app.route("/fetch/<job_id>/file")
def fetch_file(job_id):
    with jobs_lock:
        job = jobs.get(job_id)
    if not job:
        return jsonify({"error": "no such job"}), 404
    if job["state"] != "ready" or not job["path"]:
        return jsonify({"error": f"job not ready (state={job['state']})"}), 409
    return send_file(job["path"], as_attachment=True, download_name=job["filename"] or "track.flac")


@app.route("/fetch/<job_id>", methods=["DELETE"])
def fetch_delete(job_id):
    _cancelled_jobs.add(job_id)
    with jobs_lock:
        job = jobs.pop(job_id, None)
    if job and job.get("path") and os.path.exists(job["path"]):
        try:
            os.remove(job["path"])
        except OSError:
            pass
    return jsonify({"ok": True})


# The bot's own menu, which is the only documentation it has. Reading it is how
# you answer "what links does it actually accept?" without guessing — the
# question that came up when a resolver sent an artist page and the fetch simply
# timed out.
#
# An allowlist rather than a free-text send: this shares one stateful
# conversation with the fetch worker, and a general "message the bot" primitive
# would be both a footgun and a way to trigger downloads outside the job API.
# Every command here is read-only in effect.
BOT_COMMANDS = ("/help", "/info", "/follow", "/privacy", "/settings")


async def _ask_bot(command, timeout=25):
    sent_at = datetime.now(timezone.utc)
    await client.send_message(BOT, command)

    deadline = time.time() + timeout
    while time.time() < deadline:
        _bot_activity.clear()
        replies = []
        async for msg in client.iter_messages(BOT, limit=10):
            if msg.date < sent_at - timedelta(seconds=2):
                break
            if getattr(msg, "out", False):
                continue  # our own command echoing back
            if msg.message:
                replies.append(msg.message)
        if replies:
            return "\n\n".join(reversed(replies))
        try:
            await asyncio.wait_for(_bot_activity.wait(), timeout=2)
        except asyncio.TimeoutError:
            pass
    return ""


@app.route("/bot/command", methods=["POST"])
def bot_command():
    body = request.get_json(silent=True) or {}
    command = (body.get("command") or "").strip().lower()
    if command not in BOT_COMMANDS:
        return jsonify({"error": f"command not allowed: {command!r}", "allowed": list(BOT_COMMANDS)}), 400
    if not _await(client.is_user_authorized(), timeout=15):
        return jsonify({"error": "not logged in — open /login"}), 401
    # The reply matcher keys off "a new inbound message after the send", so a
    # command mid-fetch would put a text message where a document is expected.
    if _active_job_id is not None:
        return jsonify({"error": "a fetch is in progress; try again when idle"}), 409

    try:
        reply = _await(_ask_bot(command), timeout=40)
    except Exception as e:  # noqa
        return jsonify({"error": f"asking the bot failed: {e}"}), 502
    return jsonify({"command": command, "reply": reply})


@app.route("/")
def health():
    try:
        authorized = _await(client.is_user_authorized(), timeout=15)
    except Exception:  # noqa
        authorized = False
    me = None
    if authorized:
        try:
            u = _await(client.get_me(), timeout=15)
            me = getattr(u, "username", None) or getattr(u, "phone", None)
        except Exception:  # noqa
            pass
    return jsonify({
        "ok": True,
        "logged_in": authorized,
        "me": me,
        "flac_quality_set": os.path.exists(FLAC_QUALITY_FLAG),
        "active_job": _active_job_id,
    })


# ---------------------------------------------------------------- startup

async def _register_bot_listener():
    """Wake _process_fetch as soon as the bot sends anything.

    Deliberately a dumb flag rather than the thing that picks the message: the
    document still has to be found by the existing scan, which checks mime and
    filename and ignores anything older than the link we sent. This only removes
    the delay between the reply landing and that scan running.
    """
    @client.on(events.NewMessage(chats=BOT))
    async def _on_bot_message(event):  # noqa
        _bot_activity.set()


def _startup():
    asyncio.run_coroutine_threadsafe(_fetch_worker(), _loop)

    _await(client.connect())
    _await(_register_bot_listener())
    if _await(client.is_user_authorized(), timeout=15):
        print("flacit-gateway: Telegram session loaded.", flush=True)
        _await(_ensure_flac_quality())
    else:
        print("\n===============  TELEGRAM LOGIN REQUIRED  ===============", flush=True)
        print("  Open in a browser:  http://<your-server-ip>:8082/login", flush=True)
        print("  (one-time; also requires starting @deezload2bot manually)", flush=True)
        print("============================================================\n", flush=True)

    threading.Thread(target=_reap_loop, daemon=True).start()


if __name__ == "__main__":
    _startup()
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8082")), threaded=True)
