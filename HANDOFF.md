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
  - Both containers use `network_mode: bridge` (the default docker0 bridge), so
    **container IPs shift on every recreate** and container-name DNS does not
    work. The gateway moved `172.17.0.12` → `172.17.0.11` on the 2026-07-28
    deploy and silently broke the saved "Custom Tidal API URL". The saved URL is
    now `http://172.17.0.1:8081` — docker0's host gateway plus the gateway's
    published port — which survives recreates. Don't put a container IP back.

## Access / deploy
- A deploy is CI building the images, then pulling them on the server:
  ```bash
  sudo docker compose -f /DATA/.casaos/apps/spotiflac/docker-compose.yml pull
  sudo docker compose -f /DATA/.casaos/apps/spotiflac/docker-compose.yml up -d
  ```
- That runs over SSH with `sshpass` from the user's Mac; `sudo` on the box
  has no TTY, so the password is piped into `sudo -S`. Force password auth
  (`-o PreferredAuthentications=password -o PubkeyAuthentication=no`) — a bare
  `ssh` sometimes fails with `Permission denied (publickey,password)` first.
- **Credentials are not in this repo and must never be.** Host, user and the full
  command live in the operator's user-level `~/.claude/CLAUDE.md`; the password is
  read from the macOS Keychain at the moment of use
  (`security find-generic-password -s spotiflac-deploy -w`). If that lookup fails,
  ask the user — don't put the value in a file or a commit.
- **Always re-check the saved Tidal gateway URL after a deploy.** Recreating
  containers changes their bridge IPs; the saved URL must stay
  `http://172.17.0.1:8081`, never a `172.17.0.x` container IP.

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
8. **Spotify→Tidal rematch** (`backend/tidal_rematch.go` + gateway `/search/`):
   song.link's Tidal ID is often unplayable for this account, which surfaced as
   `502` / "all requested Tidal qualities failed"; failed tracks now retry
   against alternate IDs found by ISRC/name search.
9. **Duplicate cleanup** (`library_dedup.go`): per-file index (v2), incremental
   rescans, index auto-updates as downloads finish, and a Library-tab review
   list that trashes redundant copies into `<library>/.spotiflac-trash/`.

10. **Durable download queue + client API** (2026-07-28), built for the Harmony
    macOS client (`~/Developer/Harmony`, https://github.com/akvaithi/Harmony) but
    useful on its own: `EnqueueDownloads` persists jobs to bolt and a server-side
    worker drains them, so downloads survive a client disconnecting *and* a
    container restart; per-item `download:item` / `download:progress` SSE events
    replace polling; the worker triggers a Navidrome `/rest/startScan` when the
    queue drains; `GetRelatedArtists` / `GetArtistMembers` for browsing;
    and an optional `API_TOKEN` (off by default) for the previously wide-open API.

## Pending / not yet deployed
- Nothing. Deployed and verified on 2026-07-28: both images pulled, containers
  recreated, `/search/` confirmed working against live Tidal, and the new
  library RPCs answering. The pre-v2 library index on the server was discarded
  by design — **the user needs to hit "Scan library" once** to rebuild it.

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
- Single active download queue (matches upstream); fine for one user. The durable
  queue worker is deliberately serial for the same reason — `DownloadTrack` shares
  package-level progress state, so parallel downloads would corrupt it.
- Local smoke tests can't finish a download on macOS: `ffmpeg` isn't installed, so
  the FLAC conversion step fails after the bytes arrive. That's environmental (the
  image ships ffmpeg), but it means "download failed" locally is not a real signal.
- Everything is ToS-gray personal-use tooling; the user has paid Tidal + Spotify.
