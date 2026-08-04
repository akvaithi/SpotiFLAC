# Deezer as the catalog — design

**Status: steps 1–2 landed 2026-08-04. Steps 3–6 not built.**

Landed: `backend/deezer.go` (client), `backend/deezer_translate.go` (verified
translation), `backend/match.go` (comparison helpers, moved down from
`package main` so both packages share one implementation),
`CheckDeezerAvailability` + the "✗ not on Deezer" row state in the web UI, and
the catalog lookup as a last resort inside `resolveTrackURL`.

Verified live from the box: the release that failed three 270s attempts is
reported missing in **0.89s**; the copy that did download resolves by ISRC; the
search path accepts `Vaa Vaathi (From "Vaathi")` only after verification.

Still to do: the `sf-dz:` id namespace, facade `search3` on Deezer, tagging from
Deezer detail (including the lyrics contributor handling below), web UI catalog
paths, related artists / discography.

Today Spotify is the catalog and Deezer is the source. Everything the user can
see comes from Spotify's metadata; everything they can actually *get* comes from
Deezer via `@deezload2bot`. The two sets are not equal, and the gap is only
discovered ~2 minutes after asking, as a failure.

This proposes making Deezer the catalog too, so **offered means obtainable**.

## Why now

On 2026-08-04 a queued "Chinna Chinna Asai" (A.R. Rahman, Rajasri —
ISRC `INL161141228`) failed three 270s attempts. The link resolution bug behind
it is fixed, but the residue is not fixable in the resolver: that release is not
in Deezer's catalog, so no amount of correct resolution produces the track. It
was offered because Spotify has it. Searching Deezer would never have offered it.

Measured the same day: `artist:"A.R. Rahman" track:"Chinna Chinna Asai"` returns
0 on Deezer, while the copy that *did* download returns immediately. The catalog
already knows the answer we spend four minutes discovering.

## Parity — measured, not assumed

All checked 2026-08-04 against the public API from the box.

| Need | Today (Spotify) | Deezer | Verdict |
|---|---|---|---|
| Track search | `searchDesktop` persisted query | `/search` — `isrc`, `link`, `rank`, `duration`, `md5_image` inline | **better** (no hash to rot, ISRC free) |
| Album / artist / playlist search | yes | `/search/album`, `/search/artist`, `/search/playlist` | parity |
| Precise lookup | — | `artist:"X" track:"Y"` (verified: 5 hits, exact first) | **new** |
| Track detail for tagging | Spotify metadata | `isrc`, `duration`, `track_position`, `disk_number`, `release_date`, `contributors[]`, `title_version` | **better** (real artist array) |
| Album detail | yes | title, `release_date`, `upc`, `nb_tracks`, `genres`, `label`, tracks inline (1 call) | **better** (UPC + label) |
| Album art | Spotify images (≤640–1000px) | `cover_xl` 1000px; CDN serves **1400×1400** (198KB, verified) | **better** |
| Related artists | `queryArtistOverview` persisted hash | `/artist/{id}/related` — 20 results | **better** |
| Discography | Spotify | `/artist/{id}/albums` (426 for A.R. Rahman), `/artist/{id}/top` | parity |
| Lyrics | LRCLIB | LRCLIB — provider-independent | parity, **with a catch below** |
| Genres | MusicBrainz | MusicBrainz (unchanged); Deezer genres available as a bonus | parity |
| Auth | TOTP handshake + rotating hashes | none | **better** |

### The lyrics catch — the one real parity risk

LRCLIB is keyed on *performer*. Spotify bills the performer first
(`Minmini, A.R. Rahman, Vairamuthu`); Deezer's `artist` field is often the
composer (`A. R. Rahman`). Measured on the same track:

```
artist_name="Minmini"       album="Roja"                        -> SYNCED
artist_name="A. R. Rahman"  album="Roja (Original Motion ...)"  -> plain only
```

Switching naively to Deezer's `artist` would silently downgrade synced lyrics to
plain on exactly the film-music catalogue this library is full of. **Mitigation:**
drive lyrics from the `contributors[]` array (which contains *both* names) and
keep the existing `FetchLyricsAllSources` preference for synced, which already
falls back to `/api/search` — that search returns `Minmini | Chinna Chinna Asai |
synced: True` regardless of which name we start from. This must be built and
tested as part of the migration, not assumed.

## What changes

1. **`backend/deezer.go`** (new): search (track/album/artist/playlist), track,
   album, artist related/top/albums. Plain JSON, no auth, one HTTP client.
2. **Catalog calls** in the facade and web UI point at it. `backend.SearchSpotify`
   stays for the translation path only.
3. **Identity becomes the Deezer track id.** The virtual row id becomes
   `sf-dz:<deezerID>`; `DownloadRequest` gains `DeezerID`. When present,
   `resolveTrackURL` is skipped entirely — the link is `deezer.com/track/<id>`.
   The whole song.link / ISRC / Songstats resolution chain becomes a *fallback for
   Spotify-originated requests only*.
4. **Cover proxy** serves Deezer's CDN (1400×1400) for `sf-dz:` rows.
5. **Tagging** reads Deezer track+album detail: contributors, UPC, label,
   release_date, positions.

## What does NOT change

- The gateway, the bot contract, the reply-matching check, the serial worker.
- The durable queue, retries, SSE progress, Navidrome rescan.
- Library index, dedup, `artistsOverlap`/`strictKey`, the trash.
- MusicBrainz genres, LRCLIB as the lyrics source, cover embedding rules.
- **Pasting a Spotify URL keeps working** (see below). The web UI is unchanged
  in shape.
- Every boundary in `CLAUDE.md`, including no bot-side fuzzy search.

## Spotify compatibility

Pasted Spotify links and the listening-history import are primary entry points
and must keep working. Translation chain, in order:

1. Spotify id → ISRC (cached; `GetCachedISRCsOnly`, else resolve).
2. ISRC → `api.deezer.com/track/isrc:{isrc}` — exact, already in use today.
3. **If both fail:** `artist:"X" track:"Y"` search. *This is the one decision
   that needs a call — see below.*
4. Otherwise: report "not available on Deezer" **at paste time**, not after a
   four-minute bot timeout. This is the user-visible win.

## Decisions — settled 2026-08-04

1. **Fuzzy translation fallback: build it, verified strictly.** Step 3 falls back
   to `artist:"X" track:"Y"` and accepts a candidate **only** on all three of:
   exact normalized title (`normStr`), artist overlap (`artistsOverlap`), and
   duration within ±2s. Anything else reports unavailable.
   This does **not** reopen the bot-side search boundary, and the distinction is
   the whole reason it is allowed here: the bot picks a match inside its own
   inline query and hands back a file we cannot check against what was asked. A
   Deezer candidate is a structured record we verify *before* committing to it,
   and the resulting download is by exact track id. The boundary in `CLAUDE.md`
   stands as written — it is about the bot, not about verifiable matching.
2. **Artist credit: `contributors[]`, performer first**, joined with the existing
   `•` convention. This matches how the current 1348 files are tagged
   (first-artist-only from Spotify = the performer), so library matching, dedup's
   best-copy logic and filenames all stay consistent across the boundary — and it
   keeps the performer available for the LRCLIB lookup above.
   Where Deezer's `artist` (composer) and the first contributor differ, the
   performer wins for tagging; the composer is still carried in the full credit.
3. **Existing Spotify-keyed records** (queue history, `pending`, the ISRC cache):
   left alone. `deezer_id` is added alongside, history is never rewritten, and old
   rows keep working through the translation chain.

## Risks

- **Deezer's search fails *silently*.** From a blocked IP it returns
  `{"data": [], "total": 129}` — an empty page with a non-zero total, not an
  error. Verified: my machine gets this, the box does not. As the *primary*
  catalog this must be read as "search unavailable → fall back / report", never
  as "no results". A `total > 0 && len(data) == 0` check is the tell.
- **Catalog shrink is the point, but it is still a shrink.** Tracks Spotify has
  and Deezer doesn't will disappear from search rather than failing late. That is
  the requested behaviour; noting it so it is not later reported as a regression.
- **Rate limit** ~50 req/5s per IP. An album is one call (tracks inline). Fine.
- **Scope.** `SpotifyID` appears 63 times across 9 files. The id-namespace change
  is the bulk of the work and the main regression surface.

## Build order

1. `backend/deezer.go` + tests against recorded fixtures (no network in tests).
2. Translation chain + "unavailable on Deezer" reporting at paste time.
3. Facade `search3` switched to Deezer; `sf-dz:` ids; cover proxy.
4. Tagging from Deezer detail, including the lyrics contributor handling.
5. Web UI search/album/playlist/artist paths.
6. Related artists / discography.

Steps 1–2 are independently useful and low-risk: they fix "offered but
unobtainable" for pasted links even if nothing else lands.
