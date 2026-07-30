# HANDOFF — SpotiFLAC self-hosted web app

Snapshot of where things stand. Read `CLAUDE.md` for architecture/build details.

## 2026-07-30 — Download engine replaced: Telegram/Deezer via flacit-gateway

Ripped out the Tidal/Qobuz/Amazon downloaders, the community Cloudflare
verification bridge, and the Tidal PKCE gateway entirely — replaced with a
single new source ported from [FlacIt](https://github.com/BunnY-exe/FlacIt):
a Telethon session drives `@deezload2bot` over Telegram MTProto, and the
resulting Deezer-sourced FLAC is pulled down with 16 parallel connections
(~3.5-4.6 MB/s vs. the ~0.3 MB/s a single MTProto stream is throttled to).
New sidecar `flacit-gateway/` (own image, own `/config`, same shape as the old
`tidal-gateway/`) exposes a small job API (`POST /fetch`, poll `GET
/fetch/<id>`, `GET /fetch/<id>/file`) that `backend/flacit.go` drives.

Kept: the whole Spotify catalog / queue / tagging / library index / dedup /
enrichment / Navidrome-rescan layer — none of it was provider-specific.
`SongLinkClient.GetDeezerURLFromSpotify` (already existed, used for ISRC) now
also supplies the Deezer link the bot needs, so no new resolution code was
required there.

Two consequences accepted going in, not bugs: **16-bit/44.1kHz only** (no
hi-res tier — Deezer doesn't offer one), and **no fallback source** — a track
Deezer doesn't carry now fails into the queue's failed list instead of being
retried against another provider.

**Not yet deployed to the ZimaOS box as of this entry** — see "Pending" below
for what's left before the live server is running this.

## 2026-07-29 — Harmony backfill, and library enrichment

- **662-track backfill** queued from Harmony out of six years of Spotify listening
  history: 634 completed, 31 failed (4.7%). 26 of the failures are tracks song.link
  can't find on Tidal at all; 4 are Tidal 401s where the account/region can't stream
  that particular ID; 1 other. Qobuz as a second source would recover most of the
  first group.
- **Deployed `enrich.go`** (image `75db2d6`) and ran a full pass over 1,348 files,
  writing LRCLIB lyrics and MusicBrainz genres into the tags so Navidrome serves
  them to every client. Navidrome's genre index was empty before this because every
  download predating Harmony omitted `embed_genre`.
- Post-deploy check done: saved Tidal gateway URL is still `http://172.17.0.1:8081`.

## 2026-07-29 (later) — API token turned on

The public hostname `https://spotiflac.akvaithi.page` was answering the internet
**unauthenticated**, and the RPC surface includes `ListDirectoryFiles` on arbitrary
paths. `API_TOKEN` is now set in the compose file's `spotiflac` service and the
container recreated. Verified: `GetDefaults`, `/api/events` and `ListDirectoryFiles`
all return `401 {"error":"missing or invalid API token"}` without it, 200 with it.

The token is **not in this repo and must never be**. It lives in the operator's
macOS Keychain under service `spotiflac-api` (and under `spotiflac` for Harmony's own
lookup). The previous compose file is backed up next to it as
`docker-compose.yml.bak.<timestamp>`. The bundled web UI still works — load it once
with `?token=...` and `auth.go` drops a `SameSite=Strict` cookie for the session.

## 2026-07-29 (later still) — cover art recovered

Roughly half the library had **no embedded artwork**, so Navidrome served its own
grey placeholder and every client showed it. Measured before: 88 of 150 sampled
FLACs carried a picture block, 48 of 90 albums had real art, and there were **no
`cover.*` files anywhere on disk** for Navidrome to fall back to.

The cause was acquisition, not tagging. Tracks queued from a Spotify
listening-history export have a track id but no image URL — the export contains
none — so `cover_url` arrived empty and nothing was embedded. Tracks downloaded
from a search were unaffected, which is why the split was roughly even and why it
looked like Navidrome had regressed.

Two changes, both deployed:
- **`EnrichLibrary` gained a `covers` option**, alongside lyrics and genres: skips
  files that already have a picture, caches per `artist|album` so an album costs one
  lookup, rate limited. Spotify is the source because every one of these files was
  acquired from a Spotify track id.
- **`DownloadTrack` resolves the cover from `spotify_id`** when the caller supplies
  no `cover_url`, so this cannot recur for any client.

Result: **621 covers added, 717 already had one, 0 failures**; 150/150 sampled files
now carry a picture block and **90/90 albums show real art**. Post-deploy checks
done — gateway URL still `http://172.17.0.1:8081`, token auth still returning 401.

## Status: download engine swapped locally, redeploy pending
The user self-hosts this on a **ZimaOS** box (CasaOS-managed Docker). The
previously-working flow was Spotify URL → Tidal (via the user's own PKCE
gateway) → hi-res/lossless FLAC into their Navidrome music folder. As of
2026-07-30 that engine has been replaced locally (code complete, builds clean)
with Spotify URL → Deezer via `@deezload2bot`/Telegram → 16-bit/44.1kHz FLAC.
**The live server is still running the old Tidal-gateway image** until the
deploy steps below are completed.

## Where things live
- **Repo**: https://github.com/akvaithi/SpotiFLAC (public, `main`)
- **Images** (public, ghcr, amd64, built by GitHub Actions on push):
  - `ghcr.io/akvaithi/spotiflac:latest`
  - `ghcr.io/akvaithi/flacit-gateway:latest` (replaces `tidal-gateway`, which is
    removed from `main` as of 2026-07-30 — the ghcr package can stay published,
    just unused, or be deleted once the new one is confirmed working)
- **Server**: ZimaOS, compose at `/DATA/.casaos/apps/spotiflac/docker-compose.yml`
  - App: `http://<zimaos-ip>:8080`  ·  Gateway: `http://<zimaos-ip>:8082`
    (was `8081` for `tidal-gateway`)
  - `spotiflac`'s config volume: `/DATA/AppData/spotiflac/config`. The gateway
    now has its **own** volume, `/DATA/AppData/flacit-gateway`, holding
    `telegram-session.session` — not shared with `spotiflac`'s config.
  - Music/library + downloads: `/DATA/Media/Music` → `/downloads`
  - Both containers use `network_mode: bridge` (the default docker0 bridge), so
    **container IPs shift on every recreate** and container-name DNS does not
    work. This bit the old Tidal gateway URL more than once; the fix carries
    over unchanged — the saved `flacitApiUrl` must be `http://172.17.0.1:8082`,
    docker0's host gateway plus the new gateway's published port, never a
    `172.17.0.x` container IP.

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
- **Always re-check the saved flacit-gateway URL after a deploy.** Recreating
  containers changes their bridge IPs; the saved URL must stay
  `http://172.17.0.1:8082`, never a `172.17.0.x` container IP.

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
11. **Download engine swap to Telegram/Deezer** (2026-07-30, code-complete —
    see top of file): removed Tidal/Qobuz/Amazon + the Cloudflare verification
    bridge + the Tidal PKCE gateway; added `flacit-gateway/` and
    `backend/flacit.go`, ported from FlacIt's `@deezload2bot` MTProto pipeline.

## Pending / not yet deployed
- **The 2026-07-30 Telegram/flacit-gateway swap (see top of file) — not yet on
  the live server.** Still needed: copy a working Telethon session onto the box
  at `/DATA/AppData/flacit-gateway/telegram-session.session`, update the
  server's compose file (drop `tidal-gateway`, add `flacit-gateway`, set
  `FLACIT_GATEWAY_URL`), push to `main` so CI builds `flacit-gateway:latest`,
  flip that ghcr package public, then `docker compose pull && up -d` and verify
  `GetFlacItStatus` reports `logged_in: true` before trusting it with a real
  download.
- **Navidrome credentials are configured** in
  `/DATA/AppData/spotiflac/config/.spotiflac/config.json` (`navidromeUrl` =
  `http://172.17.0.1:4533` — docker0 host gateway, *not* a container IP —
  plus `navidromeUser` / `navidromePassword`; the password is also in the operator's
  Keychain under service `navidrome`). `loadNavidromeConfig` re-reads the file on
  every call, so changing these needs **no container restart**.
- The pre-v2 library index on the server was discarded by design — **the user needs
  to hit "Scan library" once** to rebuild it.

## Possible next steps (user may ask)
- Dedup matching tuning if false matches/misses show up (normalization lives in
  `library.go`: `normStr` / `firstArtist` / `nameKey`).
- A second download source for tracks Deezer doesn't have — deliberately not
  built with the 2026-07-30 swap (see Boundaries in `CLAUDE.md`); would need a
  new decision, not a resurrection of the removed Tidal/Qobuz/Amazon engines.

## Known caveats
- **16-bit/44.1kHz only, no fallback source.** Accepted trade-off of the
  2026-07-30 swap, not a bug — see `CLAUDE.md` and this file's 2026-07-30 entry.
- The gateway processes one fetch at a time (bot chat is a single stateful
  conversation), same as SpotiFLAC's own download worker being serial —
  matches, doesn't add a new bottleneck.
- flacit-gateway's Telegram session can be revoked/expire independent of
  anything in this repo; `GetFlacItStatus` / the gateway's `GET /` is the way
  to check before assuming a stuck queue is this repo's fault.
- Local smoke tests on macOS still can't finish a *full* download end-to-end if
  `ffmpeg` isn't installed for any conversion step the request asks for — that's
  environmental (the image ships ffmpeg); the flacit-gateway ↔ backend/flacit.go
  path itself has no ffmpeg dependency.
- Everything is ToS-gray personal-use tooling; the user has paid Spotify.
