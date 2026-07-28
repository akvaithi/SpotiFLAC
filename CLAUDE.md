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
watch `gh run watch` → user pulls on server. Both ghcr packages must be **public**
(user flips this in the GitHub UI once; the CI token can't set package visibility).

## Key mechanisms / gotchas
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
- **Library dedup** (`library.go`): `ScanLibrary(dir)` walks the download folder,
  reads ISRC + artist/title tags (filename fallback), persists an index to
  `/config/library-index.json`. `MatchLibrary(items)` flags tracks already present
  (ISRC exact, else normalized title+first-artist). UI unchecks in-library tracks
  on album/playlist fetch. Note: Spotify album/playlist track metadata usually
  lacks ISRC at match time, so matching is effectively name-based.
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
