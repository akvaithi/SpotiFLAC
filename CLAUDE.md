# CLAUDE.md — working in this repo

This is a **headless web/Docker rebuild of SpotiFLAC** (originally a Wails desktop
app). The desktop GUI was replaced with an HTTP server + embedded web UI; the
download engine in `backend/` is reused mostly unchanged. Personal/self-hosted use.

## What it does
Paste a Spotify track/album/playlist URL (or search) → resolves to Tidal/Qobuz/
Amazon → downloads lossless FLAC to a mounted volume. Runs on the user's ZimaOS
box in Docker. A companion **Tidal gateway** (`tidal-gateway/`) lets downloads use
the user's own Tidal subscription instead of the shared community servers.

## Architecture (files added for the web port, all `package main` in repo root)
- `main.go` — HTTP server, static UI serving (`//go:embed all:web`), `/api/file`
  download, `CONFIG_DIR`→`$HOME` redirect, graceful shutdown. **No WriteTimeout**
  (downloads/verification block for minutes). Static served with `Cache-Control:
  no-store` so image updates aren't masked by stale browser JS.
- `rpc.go` — reflection dispatcher: `POST /api/rpc/{Method}` calls any exported
  `*App` method with a JSON array of args. This exposes the entire backend without
  hand-writing handlers. The web UI calls methods through this.
- `events.go` — SSE hub (`/api/events`) replacing Wails `EventsEmit`. Use
  `emitEvent(name, data)`.
- `verify.go` — community Cloudflare verification bridge (see below).
- `queue.go` + `backend/queue_store.go` — durable download queue (see below).
- `navidrome.go` — Subsonic rescan trigger + status (see below).
- `auth.go` — optional `API_TOKEN` bearer auth, off by default (see below).
- `discovery.go` + `backend/discovery.go` — related artists / band members.
- `library.go` — dedup index (see below).
- `app.go` — the old Wails bindings, de-Wailsed (dialogs stubbed, events → SSE).
  Holds `DownloadTrack` and all `*App` methods.
- `backend/` — the download engine. Reused; a few targeted edits (see Gotchas).
- `web/` — vanilla-JS SPA (no build step): `index.html`, `app.js`, `style.css`.
- `tidal-gateway/` — separate Python/Flask service (its own image).

## Build / test / run (locally, macOS)
Toolchain: Go (via brew), no cgo needed.
```bash
export PATH="/opt/homebrew/bin:$PATH"
CGO_ENABLED=0 go build -o /tmp/sf .        # pure-Go: taglib runs as WASM/wazero
CGO_ENABLED=0 go vet ./...
# smoke test: run with a temp config + downloads dir, hit /api/rpc/...
CONFIG_DIR=/tmp/cfg /tmp/sf --addr :8890 --downloads /tmp/dl
curl -s -X POST localhost:8890/api/rpc/GetDownloadProgress -d '[]'
```
`ffmpeg` is only needed at runtime (for no-FLAC transcode); it's absent locally,
present in the image. Don't add cgo — the pure-Go build is why the image is tiny.

## Deploy (CI + registry)
`.github/workflows/docker-publish.yml` builds **two images** on push to `main`
(matrix, linux/amd64) using the Actions `GITHUB_TOKEN`:
- `ghcr.io/akvaithi/spotiflac:latest`
- `ghcr.io/akvaithi/tidal-gateway:latest`

Flow for any change: edit → `CGO_ENABLED=0 go build` to verify → commit → push →
watch `gh run watch` → `docker compose pull && up -d` on the ZimaOS box. Both
ghcr packages must be **public** (user flips this in the GitHub UI once; the CI
token can't set package visibility).

Server access (host, user, compose path, the exact `sshpass`/`sudo -S` command)
lives in the operator's **user-level `~/.claude/CLAUDE.md`**, not here — this
repo is public. The SSH password is read from the macOS Keychain at the moment
of use (`security find-generic-password -s spotiflac-deploy -w`); never write it
into a file, a commit, or the conversation.

After **every** deploy, re-check the saved Tidal gateway URL (see the bridge
gotcha below) — recreating containers silently invalidates it.

## Key mechanisms / gotchas
- **Durable download queue** (`queue.go`, `backend/queue_store.go`): `DownloadTrack`
  is blocking and the queue in `backend/progress.go` is display-only — nothing
  persists or drains it, so `AddToDownloadQueue` alone downloads nothing and a
  client that disconnects loses its work. `EnqueueDownloads([]DownloadRequest)`
  instead persists jobs to the **bolt file shared with history** (bucket
  `DownloadQueueV1`) and a single worker goroutine drains them server-side. Records
  survive a restart: `ResetInterruptedQueueRecords()` requeues anything left
  `downloading` at startup. Failures retry up to 3 times with a growing backoff
  (`NextAttemptAt` gates the claim). RPCs: `GetQueue`, `RetryQueueItem`,
  `CancelQueueItem`, `ClearQueue`. `DownloadTrack` is untouched, so the web UI
  still works exactly as before.
  **The worker must stay serial** — `DownloadTrack` takes no lock and mutates
  package-level progress globals, so concurrent calls interleave and the first to
  finish flips `is_downloading` false for all of them. `CancelQueueItem` on the
  active item calls `ForceStopActiveDownloads`, which cancels *every* in-flight
  download because the cancellation scope is shared.
- **Per-item progress over SSE**: `ProgressInfo` is global and can't describe a
  queue. `backend.SetItemProgressListener` now feeds `download:progress`
  (`{id, progress_mb, total_mb, speed_mbps}`, throttled to 4/s) and the worker
  emits `download:item` on every status change. Two paths report progress and
  **both** need maintaining: `ProgressWriter` (auto-binds to `GetCurrentItemID()`,
  covers Qobuz/Amazon/direct Tidal) and the **DASH segment loop** in
  `backend/tidal.go`, which is the main Tidal route and bypasses `ProgressWriter`
  entirely — it calls `UpdateItemProgress` directly and extrapolates the total from
  the segments fetched so far.
- **Navidrome rescan** (`navidrome.go`): the worker POSTs `/rest/startScan` when
  the queue drains and something landed, so downloads appear without waiting on
  Navidrome's own watcher. Config from `NAVIDROME_URL`/`NAVIDROME_USER`/
  `NAVIDROME_PASSWORD` env, else `navidromeUrl`/`navidromeUser`/`navidromePassword`
  in the server-side `config.json` (never in this repo). Unconfigured is not an
  error. Also exposed as `TriggerNavidromeScan` / `GetNavidromeStatus`.
- **Optional API auth** (`auth.go`): the RPC surface reaches `ListDirectoryFiles`
  on arbitrary paths and had no auth at all. Setting `API_TOKEN` requires a bearer
  token (or `X-API-Token`, or `?token=`); leaving it unset preserves the old
  behaviour exactly. Browsers can't set headers on `EventSource` or the SPA's
  `fetch`, so loading any page with `?token=…` drops a `SameSite=Strict` cookie
  that the rest of the session uses.
- **`GetRelatedArtists`** reuses the *same* `queryArtistOverview` persisted-query
  hash as the discography path and reads `relatedContent.relatedArtists` — no new
  hash to rot. **`GetArtistMembers`** hits MusicBrainz `artist-rels`; it must dedupe,
  since MusicBrainz emits one relation per instrument (a 4-instrument member
  otherwise appears 4×).
- **Community Cloudflare verification** (`verify.go`): the verify service
  *strictly* requires the callback to be `http://127.0.0.1|localhost:<port>/session-grant`.
  We do NOT rewrite it. User solves the challenge, lands on an unreachable
  loopback page, pastes that URL back; we relay the `grant` to the backend's
  loopback listener inside the container. HMAC session is cached in `/config`.
- **Custom gateway URLs must accept `http://`**: upstream `NewTidalDownloader` /
  Qobuz `SetCustomAPIURL` / `CheckCustomQobuzAPI` originally required `https://`
  and silently dropped LAN/Docker `http://` gateways. Patched to accept both. The
  UI (`buildRequest`) reads the gateway URL from the Settings field directly (not
  just saved settings) so it applies even if "Save" wasn't clicked.
- **No-FLAC fallback**: `backend/tidal.go` `DownloadFromManifest` aborted when a
  lossless request got a lossy stream. Now gated by `SetAllowLossyFallback(bool)`
  (request field `allow_lossy_fallback`, UI default on) so it transcodes the
  available stream to a playable `.flac` instead of failing. (A cleaner `.m4a`
  output would require propagating the extension up through `buildTidalOutputPath`
  and the caller — not yet done.)
- **Library dedup** (`library.go`): `ScanLibrary(dir)` walks the download folder
  and keeps **one entry per file** (path/size/mtime + ISRC/title/artist/album,
  filename fallback), persisted to `/config/library-index.json` (v2 format).
  Rescans are incremental — files whose size+mtime match are reused, never
  re-tagged — and `noteLibraryFile()` folds each finished download into the index
  from `DownloadTrack`, so it stays current without a rescan (writes are debounced
  5s). `MatchLibrary(items)` flags tracks already present (ISRC exact, else
  normalized title+first-artist). UI unchecks in-library tracks on album/playlist
  fetch. Note: Spotify album/playlist track metadata usually lacks ISRC at match
  time, so matching is effectively name-based.
- **Duplicate cleanup** (`library_dedup.go`): `FindDuplicates()` unions indexed
  files by ISRC **and** by a *strict* name key (`normStrStrict` keeps
  parentheticals, so "Song (Live)" never merges with the studio take — `nameKey`
  stays loose for download-time matching). Best copy per group = lossless >
  lossy, then largest, then oldest; the rest are suggested for removal.
  `CleanupDuplicates({paths, mode})` defaults to `trash` (move into
  `<library>/.spotiflac-trash/<timestamp>/`, `rename` with copy+remove fallback
  across filesystems) rather than `delete`; it rejects any path outside the
  library dir since it's RPC-reachable, and the scan walk skips dot-directories
  so trashed files don't reappear. `GetLibraryTrash`/`EmptyLibraryTrash` manage
  the trash. UI lives in the Library tab.
- **Spotify→Tidal rematch** (`backend/tidal_rematch.go`): song.link maps a
  Spotify track to *a* Tidal ID, often one the account/region can't stream —
  Tidal 404/401s on `playbackinfopostpaywall`, the gateway turns that into 502,
  and the track fails even though the same song downloads fine when searched
  manually. After all qualities fail, `DownloadByURL` asks the gateway's
  `GET /search/?q=&isrc=` for the same recording under another ID (ISRC match
  first, then normalized title + first artist) and retries up to 3 candidates.
  Needs a custom gateway; the community endpoints expose no search. Tidal API
  errors now carry the response body so the real upstream status is visible.
- **Gateway URL must not be a container IP**: both containers run
  `network_mode: bridge` (the default docker0 bridge), so container IPs shift on
  every recreate and container-name DNS doesn't resolve. A saved
  `http://172.17.0.x:8081` breaks on the next deploy and downloads silently fall
  back to the community servers. Use `http://172.17.0.1:8081` — docker0's host
  gateway plus the gateway's published port — which is stable.
- **Tidal gateway = PKCE**: device-login is capped at HIGH/AAC by Tidal; only
  tidalapi's PKCE flow unlocks lossless/hi-res. Login is a browser paste flow at
  `GET/POST /login`. Session persists in `/config/tidal-session.json` (with
  `is_pkce`). Contract it serves: `GET /track/?id=&quality=` →
  `{data:{manifest:"<base64>", manifestMimeType, assetPresentation}}`.

## Boundaries (do NOT build)
- No bypass of the community servers' **rate limits / scheduled breaks** — that's
  the operator's infra; the sanctioned alternative is the user's own gateway.
- No **Spotify-audio extraction** (librespot/DRM) — Spotify is lossy + DRM; it's
  used only for metadata here. Declined previously.
Keep both community and custom-gateway paths working. Everything is ToS-gray
personal-use tooling; keep that framing.

## Conventions
- Match existing style; keep the UI dependency-free (no build step).
- Add backend methods as `func (a *App) Name(...)` — they're auto-exposed via RPC;
  the JS calls `rpc('Name', ...args)`.
- Per-request backend flags are set via package-level setters before the download
  (downloads are single-active), mirroring `SetDownloading` etc.
