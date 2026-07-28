# HANDOFF — SpotiFLAC self-hosted web app

Snapshot of where things stand. Read `CLAUDE.md` for architecture/build details.

## Status: working in production
The user self-hosts this on a **ZimaOS** box (CasaOS-managed Docker). Full flow
works end-to-end: Spotify URL → Tidal (via the user's own PKCE gateway) →
**hi-res/lossless FLAC** into their Navidrome music folder.

## Where things live
- **Repo**: https://github.com/akvaithi/SpotiFLAC (public, `main`)
- **Images** (public, ghcr, amd64, built by GitHub Actions on push):
  - `ghcr.io/akvaithi/spotiflac:latest`
  - `ghcr.io/akvaithi/tidal-gateway:latest`
- **Server**: ZimaOS, compose at `/DATA/.casaos/apps/spotiflac/docker-compose.yml`
  - App: `http://<zimaos-ip>:8080`  ·  Gateway: `http://<zimaos-ip>:8081`
  - Shared config volume: `/DATA/AppData/spotiflac/config` (both containers)
  - Music/library + downloads: `/DATA/Media/Music` → `/downloads`
  - Gateway container IP has been stable at `172.17.0.12`; the user's saved
    "Custom Tidal API URL" points there.

## Access / deploy
- Deploys are done by pulling the new images on the server:
  ```bash
  sudo docker compose -f /DATA/.casaos/apps/spotiflac/docker-compose.yml pull
  sudo docker compose -f /DATA/.casaos/apps/spotiflac/docker-compose.yml up -d
  ```
- Earlier deploys were done over SSH with `sshpass` from the user's Mac. **The SSH
  password was rotated** (user was advised to), so that access no longer works and
  is not stored anywhere. Assume manual deploy by the user, or ask them to re-share
  access if needed. Do NOT keep credentials in the repo.

## Done so far (chronological highlights)
1. Ported Wails desktop app → HTTP server + web UI + reflection RPC + SSE.
2. Community Cloudflare verification bridge (paste-the-loopback-URL flow).
3. Published both images via CI to ghcr; wrote `SETUP.md`.
4. Fixed custom-gateway URLs being ignored (accept `http://`); UI reads gateway
   field directly; added no-cache headers for the UI.
5. Built the **Tidal gateway** and switched it to **PKCE** login to unlock
   lossless/hi-res (device login was AAC-capped).
6. **Library dedup** (`library.go`): scan Navidrome folder, flag/skip tracks
   already owned. Removed the audio-tools (convert/resample) tab.
7. **No-FLAC fallback**: `allow_lossy_fallback` (default on) so tracks without a
   FLAC download the best available instead of failing.

## Pending / not yet deployed
- **Deploy the latest image** — the no-FLAC-fallback change (commit "fall back to
  best available quality when a track has no FLAC") is pushed and built, but the
  final `docker compose pull && up -d` on the server was NOT run by us (SSH
  password had changed). The user needs to run it.

## Possible next steps (user may ask)
- **Honest `.m4a` for no-FLAC tracks**: currently the no-FLAC path transcodes the
  lossy stream into a `.flac` container (playable, but not true lossless). Saving
  as real `.m4a` needs the extension to propagate through `buildTidalOutputPath`
  and the Tidal downloader's return path. User was offered this; not built yet.
- **Qobuz gateway**: same pattern as the Tidal one (contract:
  `GET /api/download-music?track_id=&quality=27` → `{success, data:{url}}`) so
  downloads can fall back Tidal → Qobuz for coverage. Stubbed in docs only.
- Dedup matching tuning if false matches/misses show up (normalization lives in
  `library.go`: `normStr` / `firstArtist` / `nameKey`).

## Known caveats
- Community servers run rate limits + scheduled "breaks"; the gateway path avoids
  them. We deliberately do NOT bypass those limits.
- No-FLAC fallback files are `.flac` but sourced from AAC (Tidal has no lossless
  for that track) — playable, not audiophile-lossless.
- Single active download queue (matches upstream); fine for one user.
- Everything is ToS-gray personal-use tooling; the user has paid Tidal + Spotify.
