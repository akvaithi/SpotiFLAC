# CLAUDE.md — working in this repo

This is a **headless web/Docker rebuild of SpotiFLAC** (originally a Wails desktop
app). The desktop GUI was replaced with an HTTP server + embedded web UI; the
download engine in `backend/` is reused mostly unchanged. Personal/self-hosted use.

## What it does
Paste a Spotify track/album/playlist URL (or search) → resolves to a Deezer track
link → downloads lossless FLAC to a mounted volume via the **flacit-gateway**
sidecar, which drives Telegram's `@deezload2bot` over MTProto (Telethon) and pulls
the file with 16 parallel connections. Runs on the user's ZimaOS box in Docker.

This is SpotiFLAC's **only** download source — the Tidal/Qobuz/Amazon engines, the
community Cloudflare verification bridge, and the Tidal PKCE gateway were removed
entirely (2026-07-30) in favor of this pipeline, ported from
[FlacIt](https://github.com/BunnY-exe/FlacIt). Consequences worth remembering:
16-bit/44.1kHz only (no hi-res tier), and no fallback source — a track Deezer
doesn't carry fails into the queue's failed list rather than retrying elsewhere.

## Architecture (files added for the web port, all `package main` in repo root)
- `main.go` — HTTP server, static UI serving (`//go:embed all:web`), `/api/file`
  download, `CONFIG_DIR`→`$HOME` redirect, graceful shutdown. **No WriteTimeout**
  (downloads can block for minutes). Static served with `Cache-Control: no-store`
  so image updates aren't masked by stale browser JS.
- `rpc.go` — reflection dispatcher: `POST /api/rpc/{Method}` calls any exported
  `*App` method with a JSON array of args. This exposes the entire backend without
  hand-writing handlers. The web UI calls methods through this.
- `events.go` — SSE hub (`/api/events`) replacing Wails `EventsEmit`. Use
  `emitEvent(name, data)`.
- `queue.go` + `backend/queue_store.go` — durable download queue (see below).
- `navidrome.go` — Subsonic rescan trigger + status (see below).
- `subsonic.go` — the **Subsonic facade** at `/rest/*` (see below, and
  `SUBSONIC-FACADE.md` for the full design).
- `auth.go` — optional `API_TOKEN` bearer auth, off by default (see below).
- `discovery.go` + `backend/discovery.go` — related artists / band members.
- `library.go` — dedup index (see below).
- `app.go` — the old Wails bindings, de-Wailsed (dialogs stubbed, events → SSE).
  Holds `DownloadTrack` and all `*App` methods.
- `backend/flacit.go` — the download engine: resolves a Deezer link, drives the
  gateway's job API, tags the result. `GetFlacItGatewayURL()` in `backend/config.go`
  resolves the gateway address (env → setting → docker0 default).
- `backend/` (rest) — Spotify metadata/catalog, tagging, lyrics, enrichment,
  library index/dedup. Provider-agnostic; untouched by the download-source swap.
- `web/` — vanilla-JS SPA (no build step): `index.html`, `app.js`, `style.css`.
- `flacit-gateway/` — separate Python/Flask/Telethon service (its own image); see
  its own README for the contract and one-time Telegram session bootstrap.

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
- `ghcr.io/akvaithi/flacit-gateway:latest`

Flow for any change: edit → `CGO_ENABLED=0 go build` to verify → commit → push →
watch `gh run watch` → `docker compose pull && up -d` on the ZimaOS box. Both
ghcr packages must be **public** (user flips this in the GitHub UI once; the CI
token can't set package visibility).

Server access (host, user, compose path, the exact `sshpass`/`sudo -S` command)
lives in the operator's **user-level `~/.claude/CLAUDE.md`**, not here — this
repo is public. The SSH password is read from the macOS Keychain at the moment
of use (`security find-generic-password -s spotiflac-deploy -w`); never write it
into a file, a commit, or the conversation.

After **every** deploy, re-check the saved flacit-gateway URL (see the gateway
IP gotcha below) — recreating containers silently invalidates it.

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
- **Most of a download is the bot, not the transfer** (re-measured 2026-07-31
  from the gateway's own log, bracketing `POST /fetch` → `GET /fetch/<job>/file`):
  **6s, 97s, 118s, 178s**. The gateway→server copy of an 11MB FLAC is ~0.05s and
  Navidrome's rescan is ~370ms, so essentially all of it is `@deezload2bot`
  replying. An earlier note here said 5–13s; that is still a *fast* reply but no
  longer the range. Two things were ruled out by measurement and shouldn't be
  re-suspected: LRCLIB answers in 0.3–0.6s (its five-call chain looks alarming and
  isn't the cost, and it runs concurrently with the download), and MusicBrainz's
  1.1s courtesy delay is an order of magnitude too small to matter.
  `flacit-gateway`'s `FLAC_RETRY_AFTER`/`FLAC_TIMEOUT` were retuned 35s/120s →
  90s/270s to match: at 35s the resend fired on nearly every download, which both
  doubled load on the bot and let a duplicate reply be consumed by the *next* job. Neither the box (39MB RSS, 0% CPU, no memory pressure)
  nor Navidrome (~500ms scan, plus its own filesystem watcher) is a factor.
  Two consequences are already handled and shouldn't be re-litigated:
  `EnqueueDownloads` **prewarms the Deezer link** through
  `backend.ResolveDeezerURL` (singleflight + cache, capped at 3 concurrent
  because `songlink.go` has no backoff, and failures are deliberately *not*
  cached so a queue retry re-resolves); and the gateway waits on a Telethon
  `NewMessage` event rather than a 2s poll, with the poll kept as a backstop.
  `_parallel_download` honours `FloodWaitError` — 16 senders trip Telegram's
  limiter on long tracks, and not waiting failed the whole job.
- **Per-item progress over SSE**: `ProgressInfo` is global and can't describe a
  queue. There is **no byte count during the bot wait** — the gateway doesn't
  know the size until the document arrives — so that phase is reported as
  `download:phase` (`{id, phase}`, `resolving`|`downloading`) instead of a fake
  percentage; a client should label it rather than show an empty bar.
  `backend.SetItemProgressListener` now feeds `download:progress`
  (`{id, progress_mb, total_mb, speed_mbps}`, throttled to 4/s) and the worker
  emits `download:item` on every status change. `backend/flacit.go` reports
  progress two ways depending on phase: `awaitReady` polls the gateway's job
  status and calls `SetItemTotalSize`/`UpdateItemProgress` directly (the same
  "bypass `ProgressWriter`" pattern the old Tidal DASH loop used, since there's no
  `io.Writer` to hang one off during that phase), then `downloadFile` uses
  `NewProgressWriterWithID` for the actual gateway→server byte copy.
- **Library enrichment** (`enrich.go`): `EnrichLibrary({lyrics,genres,covers,limit,overwrite})`
  starts a background pass that writes **synced lyrics (LRCLIB)** and **genres
  (MusicBrainz)** into the files themselves, then triggers a Navidrome rescan.
  `GetEnrichStatus` / `StopEnrich` manage it; progress rides the SSE hub as
  `enrich:progress` / `enrich:done`. Tags belong in the files rather than a client
  cache — Navidrome then serves them to every client at once. Rules that matter:
  genre is written with a **merge, never `taglib.Clear`**; files modified in the
  last 2 minutes are skipped (the download worker may still hold them open); a
  MusicBrainz match below score 90 or with a non-exact name is discarded; and
  genres are ranked by tag vote count, not API order. Rate limits are deliberate —
  1.1s for MusicBrainz as they ask, with per-artist caching.
  **Covers** were added 2026-07-29 and are the same shape: skip files that already
  carry a picture, cache per `artist|album` so an album costs one lookup, source from
  Spotify because every file here was acquired from a Spotify track id. Half the
  library had no artwork at all — there are **no `cover.*` sidecars on disk**, so a
  file without an embedded picture leaves Navidrome nothing but its own placeholder.
  Recovered 621 files, 0 failures. The root cause is closed separately: **`DownloadTrack`
  now resolves `cover_url` from `spotify_id`** when a caller omits it, because a client
  enqueueing from a Spotify listening-history export has the id but no image URL.
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
- **Deezer link resolution** (`backend/flacit.go` `resolveTrackURL`): prefers a
  Deezer track URL over the bare Spotify one, via the existing
  `SongLinkClient.GetDeezerURLFromSpotify` — it already chains song.link's Deezer
  match, an ISRC lookup, and Deezer's ISRC API as internal fallbacks, so there's
  nothing left to retry at the call site. Falls back to the Spotify link only if
  every one of those fails. Deliberately **no fuzzy bot-side search** — a wrong
  inline-query match could file a remix or live cut under the right track's name.
- **The gateway serializes fetches**: `flacit-gateway`'s `/fetch` queues jobs and
  a single worker drains them one at a time, because the bot chat is one stateful
  conversation — a reply is matched by "a new inbound message after the id
  recorded before sending", which two concurrent fetches would race for.
  SpotiFLAC's own download worker is already serial, so this costs nothing.
- **Library dedup** (`library.go`): `ScanLibrary(dir)` walks the download folder
  and keeps **one entry per file** (path/size/mtime + ISRC/title/artist/album,
  filename fallback), persisted to `/config/library-index.json` (v2 format).
  Rescans are incremental — files whose size+mtime match are reused, never
  re-tagged — and `noteLibraryFile()` folds each finished download into the index
  from `DownloadTrack`, so it stays current without a rescan (writes are debounced
  5s). `MatchLibrary(items)` flags tracks already present (ISRC exact, else
  **loose title bucket + artist-set overlap**). UI unchecks in-library tracks on
  album/playlist fetch. Note: Spotify album/playlist track metadata usually lacks
  ISRC at match time, so matching is effectively name-based.
  The name test is `titleKey` + `creditsMatch`/`artistsOverlap` (`library.go`),
  **not** a `title|firstArtist` key — that key was replaced 2026-08-04 because it
  required both sides to bill the same artist first. SpotiFLAC tags credits with
  `•`, Spotify sends `, `, and the order differs, so a track the user owned was
  reported missing and offered for download again (hit constantly on Indian film
  songs, where multi-singer credits are the norm). `titleKey` also collapses
  `Song - From "Film"` onto `Song (From "Film")` — Spotify writes the same
  qualifier both ways, and `normStr` already stripped the bracketed form. Two
  rules to keep: `firstArtist`/`strictKey` are **still** untouched (they feed
  duplicate detection, where collapsing a credit deletes music), and
  `creditsMatch` refuses a library file with **no** artist tag rather than
  letting it claim every track sharing its title.
- **Duplicate cleanup** (`library_dedup.go`): `FindDuplicates()` unions indexed
  files by ISRC **and** by a *strict* name key (`normStrStrict` keeps
  parentheticals, so "Song (Live)" never merges with the studio take — the
  `titleKey`/`artistsOverlap` pair stays loose for download-time matching, and
  dedup deliberately does not use it). Best copy per group = lossless >
  lossy, then largest, then oldest; the rest are suggested for removal.
  `CleanupDuplicates({paths, mode})` defaults to `trash` (move into
  `<library>/.spotiflac-trash/<timestamp>/`, `rename` with copy+remove fallback
  across filesystems) rather than `delete`; it rejects any path outside the
  library dir since it's RPC-reachable, and the scan walk skips dot-directories
  so trashed files don't reappear. `GetLibraryTrash`/`EmptyLibraryTrash` manage
  the trash. UI lives in the Library tab.
- **The Subsonic facade** (`subsonic.go`, full design in `SUBSONIC-FACADE.md`):
  SpotiFLAC reverse-proxies Navidrome at `/rest/*` so an **unmodified** Subsonic
  client reaches the acquisition loop. Point the client at SpotiFLAC instead of
  Navidrome; it must *front* Navidrome, not sit beside it, because clients keep one
  active server. Default is a transparent proxy; `SUBSONIC_FACADE=inject` turns on
  injection. Verified working in Cassette (iOS + macOS), Amperfy and Arpeggi.
  Rules that cost real debugging time and shouldn't be relearned:
  - **Everything fails open.** Any error in an intercept falls back to proxying.
    It sits in the path of the whole library: a bug must degrade to "feature
    missing", never "music stopped". Never write to the response before the last
    fallible step.
  - **Inject into `search3` and nowhere else.** Clients cache aggressively
    (SwiftData in Cassette), and a virtual id in a cached list is a permanently
    broken row the server can't reach. `FavoriteRecord` is the one exception, and
    only because `getStarred2` reconciles it.
  - **Both JSON and XML**, and Subsonic defaults to XML when `f` is absent — that
    default is what Amperfy and Arpeggi use. XML rows are *spliced* in, never
    re-serialised, so upstream bytes survive byte for byte.
  - **`songOffset=0` is not paging.** Test the offset's value, not its presence;
    the presence check silently disabled acquisition for two clients.
  - **No colon in the cover id** (`sf-cover-`, not `sf-cover:`) — Arpeggi splits on
    it and requests the bare remainder. Song ids keep `sf:` and survive intact.
  - **One search budget, not an adaptive one.** Waiting less when the library
    already answered well seems clever and produced "works on my phone, not my
    desktop": the real variable was the query, and owning some of an artist is the
    most ordinary reason to want the rest.
  - **An in-flight row stays visible** as `⏳` rather than being suppressed —
    otherwise a track is in neither list for the ~2 minutes before Navidrome
    indexes it, and reads as lost.
  - Reconciliation matches with `songMatches`, which since 2026-08-04 shares its
    artist test (`artistsOverlap`) with the library index — one notion of who made
    a record, so "already owned" and "found after download" can't disagree. Do not
    "fix" `firstArtist` — it still feeds duplicate detection, where collapsing an
    artist list would offer to delete real music.
  - **Known gap:** the gateway checks a returned document *is* a FLAC, never that
    it's the one requested, so a duplicate reply can in principle be attributed to
    the next job. Retuning the resend made it unlikely, not impossible.
- **Gateway URL must not be a container IP**: both containers run
  `network_mode: bridge` (the default docker0 bridge), so container IPs shift on
  every recreate and container-name DNS doesn't resolve. Use
  `http://172.17.0.1:8082` — docker0's host gateway plus flacit-gateway's
  published port — which is stable. `GetFlacItGatewayURL()` resolves it as
  `FLACIT_GATEWAY_URL` env → `flacitApiUrl` setting → that default.
- **Telegram session bootstrap**: `flacit-gateway` needs an authenticated
  Telethon session at `/config/telegram-session.session`, and a fresh login isn't
  enough by itself — the account must also have manually started
  `@deezload2bot` and joined its channel once, or every fetch times out. The
  sanctioned path is copying an already-bootstrapped session onto the box (see
  `flacit-gateway/README.md`); `/login` is the recovery flow, not the default one.
- **FLAC quality is bot-side state, not per-request**: `@deezload2bot` defaults to
  MP3 320kbps and has to be switched to FLAC once via its `/settings` menu — the
  gateway automates this on first successful connection (`_ensure_flac_quality`)
  and records it in `/config/.flac_quality_set` so it only runs once. `_is_flac()`
  double-checks every incoming document (mime **and** filename) anyway, since a
  quality flag can be silently reset bot-side.

## Boundaries (do NOT build)
- No fuzzy bot-side search fallback for `resolveTrackURL` — a wrong inline-query
  match could file a remix or live cut under the right track's name. Link-only
  was a deliberate choice, not an oversight.
- No **Spotify-audio extraction** (librespot/DRM) — Spotify is lossy + DRM; it's
  used only for metadata here. Declined previously.
Everything is ToS-gray personal-use tooling; keep that framing.

## Conventions
- Match existing style; keep the UI dependency-free (no build step).
- Add backend methods as `func (a *App) Name(...)` — they're auto-exposed via RPC;
  the JS calls `rpc('Name', ...args)`.
- Per-request backend flags are set via package-level setters before the download
  (downloads are single-active), mirroring `SetDownloading` etc.
