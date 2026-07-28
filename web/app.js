'use strict';

// ---------- RPC + events ----------
async function rpc(method, ...args) {
  const res = await fetch('/api/rpc/' + method, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(args),
  });
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = text; } }
  if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
  return data;
}

const state = {
  server: { download_dir: '/downloads' },
  settings: {},
  tracks: [],        // currently fetched tracks
  collection: null,  // {title, cover, playlistName}
  downloading: false,
  filesCwd: null,
};

// ---------- tabs ----------
document.querySelectorAll('#tabs button').forEach(b => {
  b.onclick = () => {
    document.querySelectorAll('#tabs button').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    b.classList.add('active');
    document.getElementById('tab-' + b.dataset.tab).classList.add('active');
    if (b.dataset.tab === 'files') loadFiles(state.filesCwd || state.server.download_dir);
    if (b.dataset.tab === 'history') loadHistory();
    if (b.dataset.tab === 'queue') refreshQueue();
  };
});

const $ = id => document.getElementById(id);
const esc = s => (s == null ? '' : String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c])));
function firstImage(s) { if (!s) return ''; const p = String(s).split(/\s+/).filter(Boolean); return p[0] || ''; }

// ---------- init ----------
(async function init() {
  try {
    state.server = await (await fetch('/api/server-info')).json();
  } catch {}
  try {
    state.settings = (await rpc('LoadSettings')) || {};
  } catch {}
  applySettingsToUI();
  connectEvents();
  checkApiStatus();
  $('filesPath').textContent = state.server.download_dir;
})();

// ---------- SSE ----------
function connectEvents() {
  const es = new EventSource('/api/events');
  es.onopen = () => { $('dot').className = 'dot ok'; $('connText').textContent = 'connected'; };
  es.onerror = () => { $('dot').className = 'dot bad'; $('connText').textContent = 'reconnecting…'; };
  es.onmessage = e => {
    let ev; try { ev = JSON.parse(e.data); } catch { return; }
    handleEvent(ev.name, ev.data);
  };
}

function handleEvent(name, data) {
  switch (name) {
    case 'verification-required':
      showVerify(data && data.challenge_url);
      break;
    case 'verification-complete':
      $('verifyStatus').textContent = (data && data.ok) ? 'Verified! Continuing…' : 'Verification failed — try again.';
      if (data && data.ok) setTimeout(hideVerify, 1200);
      break;
    case 'metadata-stream':
      // incremental playlist metadata; ignored for simplicity (full result returned on fetch)
      break;
  }
}

// ---------- verification modal ----------
let pendingChallenge = null;
function showVerify(url) {
  pendingChallenge = url;
  $('verifyStatus').textContent = 'Waiting for you to complete verification…';
  $('verifyModal').classList.add('show');
}
function hideVerify() { $('verifyModal').classList.remove('show'); pendingChallenge = null; }
$('verifyOpen').onclick = () => { if (pendingChallenge) window.open(pendingChallenge, '_blank'); };

// ---------- fetch metadata ----------
$('mode').onchange = () => {
  $('query').placeholder = $('mode').value === 'search'
    ? 'Search tracks, e.g. "daft punk instant crush"'
    : 'https://open.spotify.com/track/…  (track, album, or playlist)';
};
$('query').addEventListener('keydown', e => { if (e.key === 'Enter') $('fetchBtn').click(); });

$('fetchBtn').onclick = async () => {
  const q = $('query').value.trim();
  if (!q) return;
  const msg = $('fetchMsg'); msg.className = 'msg'; msg.textContent = 'Fetching…';
  $('results').innerHTML = '';
  try {
    if ($('mode').value === 'search') {
      const resp = await rpc('SearchSpotify', { query: q, limit: 20 });
      renderSearch(resp);
      msg.textContent = '';
    } else {
      const raw = await rpc('GetSpotifyMetadata', { url: q, batch: false });
      const data = typeof raw === 'string' ? JSON.parse(raw) : raw;
      renderMetadata(data);
      msg.textContent = '';
    }
  } catch (e) {
    msg.className = 'msg err'; msg.textContent = 'Error: ' + e.message;
  }
};

function renderSearch(resp) {
  const items = (resp && resp.tracks) || [];
  state.tracks = []; state.collection = null;
  const el = $('results');
  if (!items.length) { el.innerHTML = '<p class="muted">No results.</p>'; return; }
  el.innerHTML = '<div class="card"><h3>Results</h3><div id="searchTracks"></div></div>';
  const box = $('searchTracks');
  items.forEach(r => {
    const t = { spotify_id: r.id || r.spotify_id, name: r.name || r.title, artists: r.artists, album_name: r.album_name, images: r.cover || r.images };
    const div = document.createElement('div');
    div.className = 'track';
    div.innerHTML = `<img src="${esc(firstImage(t.images))}" onerror="this.style.visibility='hidden'">
      <div class="meta"><div class="t">${esc(t.name)}</div><div class="a">${esc(t.artists)} · ${esc(t.album_name || '')}</div></div>
      <button class="small primary">Download</button>`;
    div.querySelector('button').onclick = () => downloadTracks([t]);
    box.appendChild(div);
  });
}

function renderMetadata(data) {
  const el = $('results');
  // track
  if (data.track) {
    const t = data.track;
    state.tracks = [t]; state.collection = null;
    el.innerHTML = `<div class="card">${trackRowHTML(t, false)}
      <div class="row" style="margin-top:12px"><button class="primary" id="dlOne">Download</button></div></div>`;
    $('dlOne').onclick = () => downloadTracks([t]);
    return;
  }
  // album or playlist
  let info, list, name, cover, playlistName = '';
  if (data.album_info) { info = data.album_info; list = data.track_list; name = info.name; cover = firstImage(info.images); }
  else if (data.playlist_info) { info = data.playlist_info; list = data.track_list; name = 'Playlist'; cover = firstImage(info.cover); playlistName = (info.owner && info.owner.display_name) ? name : name; }
  else if (data.track_list) { list = data.track_list; name = 'Tracks'; }
  else { el.innerHTML = '<p class="muted">Unrecognized metadata.</p>'; return; }

  state.tracks = list || [];
  state.collection = { title: name, cover, playlistName: data.playlist_info ? (info && info.name) || 'Playlist' : '' };

  let html = `<div class="card">
    <div class="collection-head">
      ${cover ? `<img src="${esc(cover)}" onerror="this.style.visibility='hidden'">` : ''}
      <div><div class="ttl">${esc(name)}</div><div class="muted">${(list || []).length} tracks</div></div>
    </div>
    <div class="row between" style="margin-bottom:10px">
      <label class="checkbox"><input type="checkbox" id="selAll" checked> Select all</label>
      <button class="primary" id="dlSel">Download selected</button>
    </div>
    <div id="trackList"></div></div>`;
  el.innerHTML = html;
  const tl = $('trackList');
  (list || []).forEach((t, i) => {
    const div = document.createElement('div');
    div.className = 'track';
    div.innerHTML = `<input type="checkbox" class="sel" data-i="${i}" checked>
      <div class="meta"><div class="t">${esc(t.name)}</div><div class="a">${esc(t.artists)}</div></div>
      <div class="st" id="st-${i}"></div>`;
    tl.appendChild(div);
  });
  $('selAll').onchange = e => document.querySelectorAll('.sel').forEach(c => c.checked = e.target.checked);
  $('dlSel').onclick = () => {
    const chosen = [...document.querySelectorAll('.sel:checked')].map(c => state.tracks[+c.dataset.i]);
    if (chosen.length) downloadTracks(chosen);
  };
}

function trackRowHTML(t, withCheckbox) {
  return `<div class="track">
    <img src="${esc(firstImage(t.images))}" onerror="this.style.visibility='hidden'">
    <div class="meta"><div class="t">${esc(t.name)}</div><div class="a">${esc(t.artists)} · ${esc(t.album_name || '')}</div></div>
  </div>`;
}

// ---------- download ----------
function buildRequest(t) {
  const s = state.settings || {};
  return {
    service: $('service').value,
    spotify_id: t.spotify_id || t.id || '',
    track_name: t.name || '',
    artist_name: t.artists || '',
    album_name: t.album_name || '',
    cover_url: firstImage(t.images),
    output_dir: (s.downloadPath || state.server.download_dir),
    audio_format: $('format').value,
    filename_format: $('filenameFormat').value,
    playlist_name: (state.collection && state.collection.playlistName) || '',
    embed_lyrics: !!s.embedLyrics,
    save_cover: !!s.saveCover,
    allow_fallback: s.allowFallback !== false,
  };
}

async function downloadTracks(tracks) {
  if (state.downloading) { alert('A download is already running. Check the Queue tab.'); return; }
  state.downloading = true;
  switchTab('queue');
  const poll = setInterval(refreshQueue, 700);
  for (let i = 0; i < tracks.length; i++) {
    const req = buildRequest(tracks[i]);
    setStatus(i, 'downloading');
    try {
      const resp = await rpc('DownloadTrack', req);
      setStatus(i, resp && resp.success ? 'completed' : (resp && resp.already_exists ? 'skipped' : 'failed'));
    } catch (e) {
      setStatus(i, 'failed');
      console.error('download failed', e);
    }
  }
  clearInterval(poll);
  refreshQueue();
  state.downloading = false;
}

function setStatus(i, s) { const el = $('st-' + i); if (el) { el.textContent = s; el.className = 'st ' + s; } }
function switchTab(name) { document.querySelector(`#tabs button[data-tab="${name}"]`).click(); }

// ---------- queue ----------
async function refreshQueue() {
  let q, prog;
  try { q = await rpc('GetDownloadQueue'); } catch { return; }
  try { prog = await rpc('GetDownloadProgress'); } catch {}
  const items = (q && q.queue) || [];
  $('queueCount').textContent = items.filter(x => x.status === 'queued' || x.status === 'downloading').length;
  const sum = $('progressSummary');
  if (prog && prog.is_downloading) {
    let s = `Downloading — ${prog.mb_downloaded.toFixed(1)} MB @ ${prog.speed_mbps.toFixed(2)} MB/s`;
    if (prog.rate_limited) s += ` · rate-limited ${prog.rate_limit_secs}s`;
    if (prog.cooldown) s += ` · cooldown ${prog.cooldown_secs}s`;
    sum.textContent = s;
  } else {
    sum.textContent = `${q ? (q.completed_count || 0) : 0} done · ${q ? (q.failed_count || 0) : 0} failed`;
  }
  const list = $('queueList');
  list.innerHTML = '';
  items.slice().reverse().forEach(it => {
    const pct = it.total_size > 0 ? Math.min(100, (it.progress / it.total_size) * 100) : (it.status === 'completed' ? 100 : 0);
    const div = document.createElement('div');
    div.className = 'track';
    div.innerHTML = `<div class="meta"><div class="t">${esc(it.track_name)}</div>
      <div class="a">${esc(it.artist_name)}${it.error_message ? ' — ' + esc(it.error_message) : ''}</div>
      <div class="bar"><span style="width:${pct}%"></span></div></div>
      <div class="st ${esc(it.status)}">${esc(it.status)}</div>`;
    list.appendChild(div);
  });
}
$('stopBtn').onclick = () => rpc('ForceStopDownloads').then(refreshQueue);
$('clearDoneBtn').onclick = () => rpc('ClearCompletedDownloads').then(refreshQueue);
$('clearAllBtn').onclick = () => rpc('ClearAllDownloads').then(refreshQueue);

// ---------- files ----------
async function loadFiles(dir) {
  state.filesCwd = dir;
  $('filesPath').textContent = dir;
  const list = $('filesList'); list.innerHTML = '<p class="muted">Loading…</p>';
  let files;
  try { files = await rpc('ListDirectoryFiles', dir); } catch (e) { list.innerHTML = '<p class="msg err">' + esc(e.message) + '</p>'; return; }
  files = files || [];
  list.innerHTML = '<div class="card" style="padding:0"></div>';
  const card = list.firstChild;
  const dlRoot = state.server.download_dir;
  if (dir !== dlRoot) {
    const up = document.createElement('div');
    up.className = 'filerow';
    up.innerHTML = `<div class="nm dir">⬆ ..</div>`;
    up.querySelector('.nm').onclick = () => loadFiles(dir.replace(/\/[^/]+\/?$/, '') || dlRoot);
    card.appendChild(up);
  }
  files.sort((a, b) => (b.is_dir - a.is_dir) || a.name.localeCompare(b.name));
  files.forEach(f => {
    const row = document.createElement('div');
    row.className = 'filerow';
    if (f.is_dir) {
      row.innerHTML = `<div class="nm dir">📁 ${esc(f.name)}</div>`;
      row.querySelector('.nm').onclick = () => loadFiles(f.path);
    } else {
      row.innerHTML = `<div class="nm">🎵 ${esc(f.name)}</div>
        <div class="sz">${fmtSize(f.size)}</div>
        <a class="small" href="/api/file?path=${encodeURIComponent(f.path)}">download</a>
        <button class="small">tools</button>`;
      row.querySelector('button').onclick = () => { $('toolPath').value = f.path; switchTab('tools'); };
    }
    card.appendChild(row);
  });
  if (!files.length && dir === dlRoot) card.innerHTML = '<p class="muted" style="padding:16px">No files yet — downloads will appear here.</p>';
}
$('refreshFiles').onclick = () => loadFiles(state.filesCwd || state.server.download_dir);
function fmtSize(n) { if (!n) return ''; const u = ['B', 'KB', 'MB', 'GB']; let i = 0; while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; } return n.toFixed(i ? 1 : 0) + ' ' + u[i]; }

// ---------- history ----------
async function loadHistory() {
  const list = $('historyList'); list.innerHTML = '<p class="muted">Loading…</p>';
  let h;
  try { h = await rpc('GetDownloadHistory'); } catch (e) { list.innerHTML = '<p class="msg err">' + esc(e.message) + '</p>'; return; }
  h = h || [];
  if (!h.length) { list.innerHTML = '<p class="muted">No history yet.</p>'; return; }
  list.innerHTML = '';
  h.slice().reverse().forEach(it => {
    const div = document.createElement('div');
    div.className = 'track';
    div.innerHTML = `<div class="meta"><div class="t">${esc(it.track_name || it.name || '')}</div>
      <div class="a">${esc(it.artist_name || it.artists || '')}</div></div>`;
    list.appendChild(div);
  });
}
$('clearHistory').onclick = () => rpc('ClearDownloadHistory').then(loadHistory);

// ---------- tools ----------
const CONV_MAP = {
  'mp3': { output_format: 'mp3' },
  'm4a-aac': { output_format: 'm4a', codec: 'aac' },
  'm4a-alac': { output_format: 'm4a', codec: 'alac' },
  'wav': { output_format: 'wav' },
  'opus': { output_format: 'opus' },
};
$('convertBtn').onclick = async () => {
  const p = $('toolPath').value.trim(); if (!p) return;
  const msg = $('toolsMsg'); msg.className = 'msg'; msg.textContent = 'Converting…';
  const map = CONV_MAP[$('convFormat').value] || { output_format: 'mp3' };
  try {
    await rpc('ConvertAudio', { input_files: [p], output_format: map.output_format, codec: map.codec || '', bitrate: $('convBitrate').value });
    msg.className = 'msg ok'; msg.textContent = 'Done.';
    if (state.filesCwd) loadFiles(state.filesCwd);
  } catch (e) { msg.className = 'msg err'; msg.textContent = e.message; }
};
$('resampleBtn').onclick = async () => {
  const p = $('toolPath').value.trim(); if (!p) return;
  const msg = $('toolsMsg'); msg.className = 'msg'; msg.textContent = 'Resampling…';
  try {
    await rpc('ResampleAudio', { input_files: [p], sample_rate: $('resRate').value, bit_depth: $('resDepth').value });
    msg.className = 'msg ok'; msg.textContent = 'Done.';
    if (state.filesCwd) loadFiles(state.filesCwd);
  } catch (e) { msg.className = 'msg err'; msg.textContent = e.message; }
};

// ---------- settings ----------
function applySettingsToUI() {
  const s = state.settings || {};
  $('setDownloadPath').value = s.downloadPath || state.server.download_dir;
  $('setService').value = s.service || 'tidal';
  $('setSeparator').value = s.separator || 'comma';
  $('setEmbedLyrics').checked = !!s.embedLyrics;
  $('setSaveCover').checked = !!s.saveCover;
  $('setFallback').checked = s.allowFallback !== false;
  if (s.service) $('service').value = s.service;
}
$('saveSettings').onclick = async () => {
  const s = Object.assign({}, state.settings, {
    downloadPath: $('setDownloadPath').value.trim(),
    service: $('setService').value,
    separator: $('setSeparator').value,
    embedLyrics: $('setEmbedLyrics').checked,
    saveCover: $('setSaveCover').checked,
    allowFallback: $('setFallback').checked,
  });
  const msg = $('settingsMsg'); msg.className = 'msg';
  try { await rpc('SaveSettings', s); state.settings = s; applySettingsToUI(); msg.className = 'msg ok'; msg.textContent = 'Saved.'; }
  catch (e) { msg.className = 'msg err'; msg.textContent = e.message; }
};

// ---------- api status ----------
async function checkApiStatus() {
  const el = $('apiStatus');
  try {
    const r = await rpc('GetCommunityBreakStatuses');
    const parts = Object.entries(r || {}).map(([k, v]) =>
      `${k}: ${v.available ? (v.is_break ? 'on break ' + v.remaining_minutes + 'm' : 'available') : 'unavailable'}`);
    el.textContent = parts.join(' · ') || 'unknown';
  } catch (e) { el.textContent = 'status check failed: ' + e.message; }
}
$('checkStatus').onclick = checkApiStatus;
