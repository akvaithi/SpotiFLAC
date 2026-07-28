# SpotiFLAC → Self-Hosted Web App (Docker) — Implementation Plan

Goal: turn the SpotiFLAC **Wails desktop app** into a **headless, containerized web app**
that runs on a ZimaOS server, is used from any browser on the LAN, and downloads FLACs
to a mounted volume. Target: **full feature parity**, **no auth** (trusted LAN).

Personal/private use only — same disclaimer as upstream.

---

## 1. Why this is a rewrite of the *shell*, not the *engine*

- The entire download engine lives in `backend/` (~50 Go files) and is **headless-clean**.
- Only **3 backend files** import the Wails runtime, and only for **file/folder picker
  dialogs**: `file_dialog.go`, `folder.go`, `lyrics_reader.go`.
- The desktop layer is just two files: `main.go` (Wails bootstrap) and `app.go`
  (~2400 lines of thin bindings that mostly forward JSON into `backend/`).
- Core types are **already JSON-tagged** (`DownloadRequest`, `DownloadResponse`,
  `ProgressInfo`, `DownloadQueueInfo`, `SearchResponse`, …) → they map 1:1 onto HTTP.

**Strategy:** delete the Wails layer, keep `backend/` almost untouched, add a new HTTP
server + web UI, and stub the ~5 dialog/desktop functions.

---

## 2. Target architecture

```
Browser (phone/laptop on LAN)
  static web UI  ──HTTP :8080──►  cmd/server (NEW)
                                   ├─ REST + SSE handlers
                                   ├─ verification bridge (Cloudflare)  ← see §5
                                   └─ backend/  (REUSED, ~unchanged)
                                        tidal / qobuz / amazon resolvers
                                        FLAC download, tagging, ffmpeg, lyrics, cover
Volumes:  /downloads (output)   /config (session + history + settings DB)
Image bundles: ffmpeg, libtag (cgo)
```

---

## 3. File-level change map

### Reused unchanged
- All of `backend/*.go` **except** the 3 dialog files below.
- Go module deps in `go.mod` (drop the Wails require after removing the desktop layer).

### Removed
- `main.go` (Wails bootstrap), `app.go` (Wails bindings) — logic re-expressed as HTTP handlers.
- `frontend/` (Wails/React desktop UI) — replaced by a new server-served UI (§7).
  (We can port UI logic/styling, but it no longer talks over Wails IPC.)

### Rewritten to remove Wails
- `backend/file_dialog.go` → delete (server has no file picker; output dir is fixed).
- `backend/folder.go` → `OpenFolderInExplorer` becomes a no-op / removed; dialogs removed.
- `backend/lyrics_reader.go` → keep the FLAC/MP3/ffprobe **reading** logic; drop
  `SelectLyricsFiles` (Wails dialog). Reading is the valuable part; the picker is not.

### New
```
cmd/server/main.go            HTTP server bootstrap, routing, config from env
internal/api/*.go             one handler file per feature group (mirrors app.go groups)
internal/api/verify.go        Cloudflare verification bridge (§5)
internal/api/sse.go           progress event stream (§6)
backend/community_web.go      small exported shims for web-mode verification (§5)
web/                          static SPA (HTML/CSS/JS or ported React build)
Dockerfile                    multi-stage cgo build + ffmpeg/libtag runtime (§9)
docker-compose.yml            ZimaOS-friendly compose (§10)
```

---

## 4. HTTP API surface (maps app.go bindings → endpoints)

All JSON. Base path `/api`.

| Endpoint | Method | Backs onto (app.go) |
|---|---|---|
| `/api/search` | POST | `SearchSpotify` / `SearchSpotifyByType` |
| `/api/metadata` | POST | `GetSpotifyMetadata` (SSE variant for playlist streaming, §6) |
| `/api/streaming-urls` | POST | `GetStreamingURLs` |
| `/api/track-availability` | POST | `CheckTrackAvailability` |
| `/api/download` | POST | `DownloadTrack` (the core) |
| `/api/download/cancel` | POST | `ForceStopDownloads` / `CancelAllQueuedItems` |
| `/api/queue` | GET | `GetDownloadQueue` |
| `/api/queue/add` | POST | `AddToDownloadQueue` |
| `/api/queue/clear` | POST | `ClearCompletedDownloads` / `ClearAllDownloads` |
| `/api/progress` | GET/SSE | `GetDownloadProgress` (+ SSE stream) |
| `/api/history` … | GET/POST/DELETE | download + fetch history funcs |
| `/api/recent-fetches` | GET/POST | `GetRecentFetches` / `SaveRecentFetches` |
| `/api/lyrics/download` | POST | `DownloadLyrics` |
| `/api/lyrics/extract` | POST | `ExtractLyricsToLRC` |
| `/api/cover/download` | POST | `DownloadCover` / `DownloadHeader` / `DownloadGalleryImage` / `DownloadAvatar` |
| `/api/convert` | POST | `ConvertAudio` |
| `/api/resample` | POST | `ResampleAudio` |
| `/api/flac-info` | POST | `GetFlacInfoBatch` |
| `/api/spectrum` | POST | `SaveSpectrumImage` (server-side render, §11) |
| `/api/files` | GET | `ListDirectoryFiles` (browse `/downloads`) |
| `/api/files/download` | GET | stream a finished file to the browser |
| `/api/status/*` | GET | `CheckAPIStatus*`, `GetCommunityBreakStatuses`, `GetCurrentIPInfo` |
| `/api/ffmpeg/*` | GET/POST | `IsFFmpegInstalled` etc. (image ships ffmpeg → mostly reports "installed") |
| `/api/config` | GET/POST | `GetDefaults` + settings (persist to `/config`) |
| `/verify/callback` | GET | Cloudflare grant callback (§5) |

Desktop-only bindings that change meaning: `SelectFolder`, `SelectFile`,
`SelectAudioFiles`, `OpenFolder`, `OpenConfigFolder`, `Quit`, brew-ffmpeg installers →
removed or replaced by server-appropriate behavior (§11).

---

## 5. Cloudflare / community verification bridge  ⭐ (the crux)

### How upstream works today (traced in code)
1. Every community API request → `setCommunityRequestHeaders` (`community_apikey.go`)
   → `ensureCommunitySession()` (`community_session.go`).
2. If a **valid cached session** exists (`community_session.json`, has
   `session_id` + `session_secret` + future `expires_at`), the request is **HMAC-signed**
   with the secret and sent. **No browser, no Cloudflare cookies involved.**
3. If no valid session, `runCommunityVerification()` runs the interactive flow:
   - starts a **loopback callback** on `127.0.0.1:<rand>/session-grant`,
   - calls the community `/bootstrap` endpoint → gets a Cloudflare `challenge_url`,
   - appends `?cb=<loopback callback>` and **opens the system browser**,
   - user solves the Cloudflare challenge → challenge page redirects to the callback
     with a `grant` → `exchangeCommunityGrant()` swaps it for the session → cached to disk.
4. On `401`/`428` mid-use, `doCommunityRequest` clears the session and re-verifies once.

**Critical implication:** the Cloudflare check is a **one-time gate to mint an HMAC
session**. After that, downloads are fully headless until the session expires. So a
web app only needs to move the *browser step* from the local machine to the user's
own browser tab.

### The problem in a container
- No local browser; the loopback `127.0.0.1:<port>` callback is unreachable from the
  user's phone/laptop (different host).
- `ensureCommunitySession()` **blocks up to 5 min** waiting for a grant — unacceptable
  inside a download request.

### The web design
Add `backend/community_web.go` (small, non-invasive) exposing:
```go
func CommunitySessionValid() bool                 // cached, unexpired session present?
func StartCommunityChallenge(cb string) (challengeURL string, err error) // does /bootstrap with our cb
func CompleteCommunityChallenge(grant string) error  // exchange + persist (wraps existing)
```
and make `ensureCommunitySession()` **non-interactive in server mode**: if no valid
session, return a sentinel `ErrCommunityVerificationRequired` instead of opening a
browser and blocking. (Inject a flag/strategy so desktop behavior is preserved if ever reused.)

Flow in the web app:
1. **Before** starting a community download, server checks `CommunitySessionValid()`.
2. If invalid → server calls `StartCommunityChallenge("http://<server-host>:8080/verify/callback?state=<rand>")`
   and returns `409/428 {verification_required:true, challenge_url}` to the UI.
3. UI shows a **"Verify (Cloudflare)" modal** and opens `challenge_url` in a **new tab**
   on the user's device (answers your "a new page is opened before use" — it becomes a
   new browser tab the user controls).
4. User solves the Cloudflare challenge; the challenge page redirects to the app's own
   `/verify/callback?state=…&grant=…` (reachable because it's the app's own address).
5. `/verify/callback` validates `state`, calls `CompleteCommunityChallenge(grant)`,
   persists the session to `/config`, and returns the existing pretty **"Verified"** page
   (reuse the HTML already in `community_session.go`), which auto-closes the tab.
6. UI detects verification done (poll `/api/status/session` or SSE) and **resumes the
   pending download**.
7. Mid-session expiry (401/428): the download surfaces the same
   `verification_required` state; UI re-runs steps 3–6. Sessions are long-lived, so this
   is occasional, not per-download.

**Host address handling:** the callback URL must be reachable from the user's browser.
Derive it from the incoming request's `Host` header (or a `PUBLIC_URL` env var for
reverse-proxy setups), not `127.0.0.1`.

**Open risk:** `/bootstrap` currently sends `platform=desktop`. If the verify service
validates platform/UA, we keep sending `desktop` (works today) or coordinate a value.
Flag as a live-service dependency (see §12).

---

## 6. Progress & streaming (SSE)
- `GetDownloadProgress()` / `GetDownloadQueue()` are already pollable structs.
- Add `/api/progress` as **Server-Sent Events**: a goroutine ticks (250–500 ms) and emits
  the current `ProgressInfo` + queue snapshot. Replaces Wails `EventsEmit`.
- `metadata-stream` (playlist metadata arriving incrementally) → its own SSE endpoint or
  chunked JSON, replacing the Wails event of the same name.
- Cooldown/rate-limit fields already exist in `ProgressInfo` → surface in the UI banner.

---

## 7. Web UI (full parity)
Port the desktop feature set to a browser SPA (either hand-written HTML/CSS/JS served by
Go, or the existing React UI rebuilt to call REST/SSE instead of Wails IPC). Feature areas:
- Search (track/album/playlist/artist) + paste Spotify/Tidal/Qobuz/Amazon URL.
- Metadata preview with incremental playlist loading (SSE).
- Download controls: service, quality/format, filename format, metadata tag toggles,
  fallback options, auto-convert, auto-resample, embed lyrics/cover, save cover.
- Live queue + progress + speed + cooldown/rate-limit + Cloudflare-verify modal (§5).
- History (downloads + fetches), recent fetches, export failed.
- Lyrics: download `.lrc`, extract embedded → `.lrc`.
- Cover/header/gallery/avatar download.
- Convert & resample tools (batch), FLAC info inspector.
- Spectrum view (server-rendered, §11).
- File browser over `/downloads` with per-file play/download-to-browser.
- Settings page (persisted to `/config`), API-status/IP indicators.

No auth screen (per decision). Add a note in README that it should stay on a trusted LAN.

---

## 8. Persistence & volumes
- `/downloads` — output library (bind-mounted to a ZimaOS share).
- `/config` — app data dir (`EnsureAppDir`): `community_session.json`, history DB
  (bbolt), settings, recent fetches. Must persist across container restarts so users
  don't re-verify every boot.
- Point `EnsureAppDir()` at `/config` via env (`XDG_CONFIG_HOME` or a new `CONFIG_DIR`).

---

## 9. Dockerfile (multi-stage, cgo)
Constraints found in code:
- `go.senan.xyz/taglib` is **cgo** → needs `libtag` + a C toolchain at build.
- Downloads shell out to **ffmpeg/ffprobe** → must be in the runtime image.
- Some formats use `mp4ff` decrypt (pure Go) — fine.

```
# build stage
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y libtag1-dev pkg-config
ENV CGO_ENABLED=1
COPY . /src ; WORKDIR /src
RUN go build -o /out/spotiflac ./cmd/server
# (web assets built in a node stage or committed prebuilt)

# runtime stage
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ffmpeg libtag1v5 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/spotiflac /usr/local/bin/
COPY --from=build /src/web /web
ENV CONFIG_DIR=/config
VOLUME ["/downloads","/config"]
EXPOSE 8080
ENTRYPOINT ["spotiflac","--addr",":8080","--downloads","/downloads"]
```
(Alpine is possible but musl + cgo + taglib is fiddlier; Debian slim is the safe path.)

---

## 10. ZimaOS deployment
ZimaOS (CasaOS lineage) runs Docker/compose apps. Provide:
```yaml
services:
  spotiflac:
    image: spotiflac-web:latest      # or build: .
    ports: ["8080:8080"]
    volumes:
      - /DATA/Media/Music:/downloads
      - /DATA/AppData/spotiflac:/config
    restart: unless-stopped
```
Install via ZimaOS "Custom App / compose import", or publish an image to a registry and
point CasaOS at it. Access at `http://<zimaos-ip>:8080`.

---

## 11. Desktop-only features → server behavior
- File/folder pickers → removed; output is `/downloads`, inputs are browser uploads or
  paths under `/downloads`.
- `OpenFolderInExplorer`, `OpenConfigFolder`, `Quit`, window foreground → removed/no-op.
- FFmpeg install helpers (download binary, Homebrew) → **image already has ffmpeg**;
  endpoints just report "installed".
- Spectrum image: desktop rendered client-side then `SaveSpectrumImage`. Server option:
  render spectrum with ffmpeg (`showspectrumpic`) into `/downloads` or return PNG bytes.
- `GetCurrentIPInfo` / API-status checks → keep (server-side HTTP), display in UI.

---

## 12. Risks & unknowns (call out before coding)
1. **Live external dependency:** community verify + download endpoints are services the
   upstream author operates. If they change auth/platform checks or go down, the app
   breaks. Mitigation: also support user-supplied `TidalAPIURL`/`QobuzAPIURL` (already in
   `DownloadRequest`) to bypass community servers.
2. **`platform=desktop` in verification** may be validated server-side (§5). Needs a live
   test; may require keeping the desktop value or a UA tweak.
3. **cgo/taglib build** adds image size/complexity vs. a pure-Go binary.
4. **Cloudflare challenge UX**: some challenges are IP/UA-bound; solving on phone then
   using session from server IP works because the session is HMAC (not cookie) — but
   worth an early end-to-end test to confirm the grant→session exchange isn't IP-pinned.
5. **Concurrency**: desktop assumes one user; several backend globals hold single-flight
   download/progress state. For LAN multi-user, either accept single-active-download
   semantics (simplest, matches upstream) or refactor to per-session queues (larger).
   Recommend: keep single active queue for v1.
6. **Legal/ToS**: personal use only; document clearly.

---

## 13. Milestones
1. **Skeleton**: strip Wails, `cmd/server` boots, serves static page, `/api/config`.
2. **Core download**: `/api/search`, `/api/metadata`, `/api/download`, `/api/progress`
   (SSE), file browser — single track + album + playlist.
3. **Verification bridge**: `community_web.go` + `/verify/callback` + UI modal (§5) —
   end-to-end Cloudflare test.
4. **Parity features**: history, lyrics, cover, convert, resample, FLAC info, spectrum,
   status indicators, full settings.
5. **Packaging**: Dockerfile, compose, ZimaOS install docs, README.
6. **Hardening**: persistence checks, session-expiry re-verify, cooldown/rate-limit UX.

Estimated effort: core (M1–M2) ~1 focused day; verification bridge ~0.5 day; full parity
+ packaging ~2–3 additional days depending on how faithfully the UI is reproduced.
