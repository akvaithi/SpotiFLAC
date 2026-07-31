# DESIGN — the Subsonic facade

**Status:** built in `subsonic.go` and **deployed with `SUBSONIC_FACADE: inject`**.
Phase 1 (pure proxy) passed on the shipped macOS Cassette. Phases 2–6 are verified
server-side against real Navidrome but **not yet exercised through the app**, and
phase 7 (iOS) is untouched.
**Date:** 2026-07-31
**Target client:** [Cassette](https://github.com/CassetteLab/cassette) (iOS + macOS),
**unmodified official builds**.

Injection is opt-in: `SUBSONIC_FACADE=inject`. Unset (the default) is the phase-1
pure reverse proxy, which is what should go to the box first.

---

## 0. The problem this solves

Harmony is macOS-only. Cassette is a native iOS **and** macOS Subsonic client that
is better than Harmony at everything except the one thing Harmony exists for: the
acquisition loop, where `+` on a track that isn't in the library means *download the
lossless file to the server*.

Cassette has no concept of a track it doesn't own. Its only user-configurable
external hook is `ExternalReleaseProvider` — a URL template with a `%s` placeholder
that opens a **browser**. There is no plugin system, no download-provider protocol,
nothing that can be pointed at SpotiFLAC's RPC surface.

Cassette speaks exactly one protocol: Subsonic / OpenSubsonic. Therefore:

> **If the acquisition loop is going to reach an unmodified Cassette, it has to be
> expressed in Subsonic verbs, and every line of code that does so has to live on
> the server.**

That is what this document specifies: a Subsonic facade inside SpotiFLAC that
reverse-proxies Navidrome, injects Spotify catalog results into search, and turns a
Subsonic write verb into an `EnqueueDownloads` call.

---

## 1. Constraints

These are the non-negotiable framing conditions, and several design choices below
exist only because of them.

1. **No client modification, ever.** Not a fork, not a patch, not a sideload.
   Cassette's iOS build is distributed through *their* TestFlight and its macOS build
   through *their* notarized Homebrew cask. A fork would need a separate Apple
   Developer account, re-signing, and a permanent re-merge burden against an actively
   developed repo. MPL-2.0 would permit it; distribution reality does not.

2. **All development and testing happens against the official macOS release.**
   Install via `brew install --cask cassette` and test that build — not a
   locally-compiled checkout. The macOS and iOS apps share the same service layer,
   the same SwiftSonic client and the same view models, so a facade that satisfies
   the official macOS binary is the strongest available evidence that the official
   iOS TestFlight binary will behave identically. A source build could accidentally
   depend on a debug path or an unreleased commit; the shipped artifact cannot.
   **A feature is not accepted until it is demonstrated on the shipped macOS app and
   then confirmed on the shipped iOS app.**

3. **All modification is server-side**, confined to three things already deployed on
   the ZimaOS box: **SpotiFLAC**, **flacit-gateway**, and **Navidrome** (configuration
   only — no Navidrome patches). No new container. No client-side shim.

4. **The facade must never be able to break playback.** It sits in the path of the
   entire library experience. If it fails, music stops. This drives the fail-open
   rule in §9.

---

## 2. System shape

Today:

```
Cassette ──────────────────────────────► Navidrome :4533
SpotiFLAC web UI ──► SpotiFLAC :8080 ──► flacit-gateway :8082 ──► @deezload2bot
                          └──────────► /downloads ──► Navidrome rescan
```

Proposed:

```
                     ┌──────────────────────────────────────────┐
Cassette ──/rest/*──►│  SpotiFLAC :8080  — Subsonic facade      │
                     │                                          │
                     │  pass through ──────────────────────────►│──► Navidrome :4533
                     │  intercept: search3, getCoverArt,        │
                     │             updatePlaylist,              │
                     │             createPlaylist, stream       │
                     │                    │                     │
                     │                    ▼                     │
                     │            EnqueueDownloads ──► worker ──┼──► flacit-gateway
                     └──────────────────────────────────────────┘         │
                                                                          ▼
                                                        /downloads ──► startScan
```

Cassette's server URL becomes SpotiFLAC's. Navidrome stops being addressed directly
by the client.

**SpotiFLAC does not add a second `ServerConfig`.** Cassette's `ServerConfig` model
carries `isActive` and only one server is active at a time — adding SpotiFLAC beside
Navidrome would *swap out* the library rather than augment it. The facade has to
**front** Navidrome, not sit beside it.

---

## 3. Protocol surface

Cassette's service layer calls exactly these Subsonic endpoints (enumerated from the
v1.9-era source):

```
ping                getArtists            getArtist            getAlbum
getAlbumList2       getArtistInfo2        getStarred2          getTopSongs
getRandomSongs      getSongsByGenre       getSimilarSongs      getSimilarSongs2
search3             star                  unstar               scrobble
getPlaylists        getPlaylist           createPlaylist       updatePlaylist
deletePlaylist      getInternetRadioStations
getLyricsBySongId   getUser               stream               getCoverArt
```

**Default behaviour is a transparent reverse proxy.** Every request is forwarded to
Navidrome with its query string untouched, and the response body is streamed back
unread. Auth is Navidrome's own `u`/`t`/`s` salted-token scheme, so **the facade
validates nothing and stores no credentials** — it forwards the parameters and
Navidrome accepts or rejects them. This is what keeps the blast radius small: 24 of
the 26 endpoints are `io.Copy`.

Five endpoints are intercepted. Everything else, including any endpoint Cassette adds
in a future release, passes through unmodified and keeps working.

### 3.1 This is not Cassette-specific

Nothing in the facade knows what client is talking to it. Cassette is the *target*,
not a dependency — the contract is the Subsonic protocol, so **any** Subsonic client
pointed at SpotiFLAC gets the same acquisition loop: `↓` rows in search, star to
download. Symfonium, Amperfy, substreamer, play:Sub, Feishin, the CLI clients — all of
them, on any platform, with no per-client work. That is the main structural payoff of
solving this at the protocol layer instead of forking an app.

Three real limits, all in the *injection* path only — pass-through is universal:

- **JSON only.** `f=xml` is forwarded untouched rather than rewritten. An XML client
  gets a fully working library and no `↓` rows.
  **Confirmed in the field 2026-07-31: Amperfy and Arpeggi both show library results
  only.** Amperfy calls `search3` like everyone else but parses XML
  (`ResponseError(type: .xml…)` in `SubsonicServerApi.swift`) and never sends
  `f=json`, so injection is skipped. This is the limit doing what it says, not a bug —
  but note the practical shape of it: **acquisition works in Cassette and in nothing
  else tested.** The "any Subsonic client" claim in this section is about the
  *protocol* being the right seam; it is not a statement that every client is
  currently served.
- **GET only.** A client that submits `search3` over the `formPost` extension gets no
  injection, for the same reason: the parameters are in the body, not the URL. Star
  and playlist writes over POST *are* handled.
- **Marker convention.** `↓` in the title is the only signal a client has that a row
  is an acquisition. A client that renders titles verbatim shows it correctly; one
  that does something clever with titles might not.

Navidrome's **own web UI is unaffected** either way — it talks to Navidrome's private
`/api` endpoints, not `/rest`, so it never passes through the facade.

---

## 4. The virtual id scheme

A Spotify track that is not in the library is represented as a Subsonic song with a
synthetic id:

```
sf:<spotifyTrackID>            e.g. sf:4cOdK2wGLETKBW3PvgPWqT
sf-cover:<spotifyTrackID>      the matching coverArt id
```

The `sf:` prefix is the *only* signal the facade needs. Any request carrying an id
with that prefix is ours; any other id is Navidrome's and is forwarded verbatim.
Navidrome ids are hex ULIDs and never collide with this shape.

Virtual songs are marked in their **title**, not only structurally, because the user
has to be able to tell at a glance that a row is an acquisition and not a play:

```json
{
  "id": "sf:4cOdK2wGLETKBW3PvgPWqT",
  "title": "↓ Nightcall",
  "artist": "Kavinsky",
  "album": "OutRun",
  "coverArt": "sf-cover:4cOdK2wGLETKBW3PvgPWqT",
  "duration": 258,
  "suffix": "flac",
  "isDir": false,
  "type": "music"
}
```

`↓` is chosen over "(not in library)" because Cassette truncates long titles in rows
on iOS and a leading glyph survives truncation.

---

## 5. The five intercepts

### 5.1 `search3` — injection

Forward the query to Navidrome and decode the response. Then call
`backend.SearchSpotify(ctx, query, limit)` (already exists, `app.go:505`) and append
its `Tracks` as virtual songs to the `song` array, **after** every real result.

Two filters before appending:

- **Drop anything already owned.** Run the candidates through `MatchLibrary`
  (`library.go:449`) — ISRC exact, else normalized title + first artist. Note the
  known limitation recorded in `CLAUDE.md`: Spotify search results usually lack ISRC,
  so this is effectively name-based, and it is *stricter* than Navidrome's own
  matching. A false "not owned" shows a `↓` row for a track you already have;
  enqueueing it is harmless (the duplicate finder catches it) but it is noise.
  **Open question in §12.**
- **Cap at 10.** Search results are a list someone is scanning; a wall of
  unownable rows below the real ones is worse than a short one.

**Latency, measured 2026-07-31 — this is the part that needed rethinking.**

A Spotify catalog search costs **1.4–1.7s warm**, and the requested limit doesn't
move it (10 vs 30 results: no difference). It is the round trip to the partner API.
Warming the access token helps marginally and is done anyway (`keepSpotifyWarm`), but
it is not the cost.

That kills the original single-budget plan. At 1.5s the budget sits on the median and
injection becomes a coin flip — observed directly: the same query returned 0 rows then
10. At 2.5s every search in the app slows to Spotify's speed, *including* searches for
music already owned, which is a real regression on the common case.

So the wait is decided **after** seeing what Navidrome returned:

| Navidrome returned | Budget | Rationale |
|---|---|---|
| ≥ 5 songs | **250ms** | The library answered. Take whatever is cached and get out of the way. |
| < 5 songs | **2.5s** | The library came up short, which is exactly when acquisition is the point. |

A **10-minute, 300-entry query cache** sits in front of both, and the background fetch
keeps filling it even when the request gave up waiting — so a refined or repeated
search is instant. Measured: 2.04s first, **0.01s** repeat; a well-answered search
stays at 0.25s and injects nothing.

### 5.2 `getCoverArt` — virtual artwork

`id=sf-cover:<spotifyID>` → resolve the Spotify image URL and proxy the bytes with a
long `Cache-Control`. Everything else forwards.

The image URL is already available from the search result's `Images` field, so this
is a cache lookup rather than a second Spotify round trip; the facade keeps a
bounded in-memory map of `spotifyID → imageURL` populated by §5.1.

### 5.3 `star` — **the primary trigger: "add to my library"**

This is the mechanism the whole design turns on.

```
star?id=sf:4cOdK2wGLETKBW3PvgPWqT
```

The facade:
1. Recognises the `sf:` prefix, does **not** forward to Navidrome.
2. Builds a `DownloadRequest` from the cached Spotify metadata and calls
   `EnqueueDownloads` (`queue.go:313`), which returns immediately, persists to bolt,
   prewarms the Deezer link, and nudges the serial worker. Idempotent: a second star
   for an id already queued is a no-op.
3. Returns Subsonic `<subsonic-response status="ok">`.

`unstar` on an `sf:` id cancels the queue item if it hasn't started.

**Why star is the right verb.** "Add to Favorites" sits in the *same* context menu as
Add to Playlist, on every song row including search results
(`SongRow.swift:226–241`), and it is one action with no destination to choose. It is
also the honest semantics: the user is saying *put this in my library*, and the file
landing in `/downloads` and being indexed by Navidrome is exactly that — it becomes a
first-class library track under Artists / Albums / Songs, not an entry in some list.

**The stale favourite is self-cleaning, and that is what makes this safe.**
`FavoritesService.star` optimistically inserts a local `FavoriteRecord` before the
server call and only rolls it back if the call *throws* — so returning ok leaves a
persisted record keyed `song:sf:4cOd…`. Ordinarily that is exactly the poisoned-cache
problem §7 exists to prevent. It isn't, here, because `syncFromServer` reconciles
against `getStarred2` and **deletes every local record the server doesn't return**.
The virtual favourite therefore disappears at the next sync on its own.

Better still, the facade closes the loop: once the download lands and the rescan
completes, it looks the new track up in Navidrome and issues a **real** `star` for it.
The favourite the user set is then genuinely true, pointing at a real, playable track.
Matching is by `songMatches`, not the library's `nameKey` — see the comment on that
function for why, and §5.3a for the bug that forced it.

**Known consequence, accepted 2026-07-31: a freshly-acquired favourite cannot be
removed until the app relaunches.**

`FavoritesService.unstar` begins:

```swift
guard let record = try? backgroundContext.fetch(descriptor).first else { return }
```

If there is no *local* `FavoriteRecord` it returns early and **never calls the
server** — no request, no error. The facade's re-star happens server-side under
SpotiFLAC's own credentials, so Cassette never made that call and has no local
record. The Favorites list still shows the track, because that list comes from
`getStarred2`, but the unfavorite button silently does nothing.

It self-heals: `syncFromServer` inserts a local record for everything `getStarred2`
returns, after which unfavorite works normally. The catch is *when* that runs —
`MainTabView` calls it from `.task(id: serverState.isOnline)`, so **only on launch
and on connectivity changes**. Not pull-to-refresh, not on a timer. A user who leaves
the app open all day keeps the stuck row all day; force-quitting and reopening clears
it immediately.

The alternative was to stop re-starring altogether, which trades a favourite that
can't be removed for a favourite that silently disappears. Re-starring was chosen
because it honours what the user actually asked for, and because the failure mode is
visible and recoverable rather than silent. Nothing server-side can shorten the
window — only the client decides when to sync.

### 5.3a Why matching does not use `nameKey`

The first real acquisition through the app looked like it worked — the FLAC landed,
Navidrome indexed it, the track was playable — and the favourite was silently dropped.
The log said `"London Thumakda" never appeared in Navidrome` for a file sitting on
disk.

**SpotiFLAC tags multiple artists separated by `•`.** Navidrome therefore reported
`Labh Janjua • Sonu Kakkar • Neha Kakkar` while Spotify had said the same three names
comma-separated. `firstArtist` (`library.go:108`) splits on `, ; & /` and ` x ` — not
on bullets — so the two sides reduced to `labhjanjua` and
`labhjanjuasonukakkarnehakakkar`, and never matched.

The one-line fix — adding `•` to `firstArtist` — is **wrong** and was deliberately
not made. That helper also feeds `normStrStrict` behind duplicate detection
(`library_dedup.go`), where collapsing an artist list to its first name would make
*Nightcall — Kavinsky* and *Nightcall — Kavinsky • Angèle • Phoenix* hash alike and
offer to delete one as a duplicate. A matching bug in a favourite is an annoyance; a
matching bug in dedup deletes music.

So `songMatches` is local to the facade: exact normalized title, then substring
containment on artists, which is separator-agnostic because `normStr` strips the
separators themselves. Safe here because the title must already match exactly and the
candidate set is one search's worth of rows.

**Still outstanding:** `MatchLibrary` has the same blind spot, so §5.1's ownership
filter can miss a multi-artist track you already own and offer a `↓` row for it.
Harmless — enqueueing a duplicate is caught downstream — but it is the same root
cause and is not fixed.

### 5.4 `createPlaylist` / `updatePlaylist` — secondary trigger, with a destination

```
updatePlaylist?playlistId=<real>&songIdToAdd=sf:4cOdK2...
```

Same as §5.3, plus: split `songIdToAdd` into real and `sf:` ids, forward only the
real ones, and record `spotifyID → playlistID` in a **playlist-destination table** in
the same bolt file as the queue. After the rescan, add the real track to that
playlist for real.

This is the "get me this track *and* put it in my running mix" case. Star is the
plain acquisition; playlist add is acquisition with a destination. Both are one
context-menu action from a search result.

"Add to Playlist…" is at `SongRow.swift:201`, immediately above the favourite action
in the same menu. Any playlist works; there is no need for a designated drop-box
playlist, because §5.3 already covers plain acquisition.

### 5.5 `stream` — the graceful failure

`stream?id=sf:...` means the user pressed Play on a virtual row.

The facade enqueues the download (idempotently — a second request for an id already
queued is a no-op) and returns Subsonic **error 70, "data not found"**. Cassette
surfaces this as a playback error.

**Blocking the stream until the file exists is explicitly rejected.** It is the
seductive option — "press play and it just appears" — and it does not work:
`CustomHeadersTransport` sets `timeoutIntervalForRequest = 30` and
`timeoutIntervalForResource = 30`, while a download is 5–13s of `@deezload2bot`
latency plus transfer plus tagging. It would succeed often enough to look correct and
fail often enough to be untrustworthy, which is the worst of both. The measured
latency profile is in `CLAUDE.md`; nothing about it has changed.

---

## 6. Lifecycle of one acquisition

```
1. User searches "nightcall" in Cassette (iOS)
2. facade → Navidrome search3          (real results)
   facade → SearchSpotify              (concurrent, 1.5s budget)
   facade → ownership filter           (drop owned)
   ← song[] = [ …real… , "↓ Nightcall" (sf:4cOd…) ]
3. User long-presses the ↓ row → Add to Favorites
4. Cassette → star?id=sf:4cOd…
   Cassette also writes a local FavoriteRecord "song:sf:4cOd…" (see §5.3)
5. facade: EnqueueDownloads([DownloadRequest{SpotifyID:"4cOd…", …}])
   ← status="ok"                        (Cassette shows its normal starred state)
6. worker: resolve Deezer link → flacit-gateway → @deezload2bot → FLAC
7. worker: tag, write to /downloads, noteLibraryFile
8. queue drains → TriggerNavidromeScan
9. facade: find the new song id in Navidrome by ISRC, star it for real
10. Cassette's next favourites sync drops the sf: record, keeps the real one.
    The track is in Artists / Albums / Songs / Favorites, playable, lossless.
```

Steps 6–8 are entirely existing code. The facade adds steps 2, 5 and 9.

Note what step 10 means: **the track is in the library proper**, indexed by Navidrome
like anything else — not parked in a playlist. That is the difference between this
design and one built on a magic drop-box playlist.

---

## 7. What Cassette persists, and why injection is search-only

Cassette caches aggressively in SwiftData: `CachedTrack`, `DownloadedTrack`,
`FavoriteRecord`, `PinnedItem`, `QueueSnapshot`, `PlaybackSession`. A virtual id that
gets into one of those generally becomes a broken row that survives relaunch and that
the facade cannot reach in to clean up.

**Therefore virtual songs are injected into `search3` and nowhere else.** Not
`getAlbumList2`, not `getStarred2`, not `getRandomSongs`, not `getSongsByGenre`. A
search result is transient; an album list is cached and pinned and shown on the home
screen.

`FavoriteRecord` is the one deliberate exception, and only because it is the one
model Cassette **reconciles against the server**: `syncFromServer` deletes every
local record `getStarred2` doesn't return, so an `sf:` favourite evaporates on its
own (§5.3). Do not generalise from it — nothing else in that list self-heals. Before
any future intercept writes a virtual id into a model, check whether that model has a
server-authoritative sync path; if it doesn't, the answer is no.

Two consequences accepted deliberately:

- There is **no "browse the Spotify catalog"** experience in Cassette — no artist
  discography of unowned albums, no charts, no new releases. Search is the only door.
- **Album-level acquisition is not offered.** `songIdToAdd` is per-track;
  a virtual *album* would have to appear in `getAlbum`, which Cassette caches. Adding
  ten tracks means ten context-menu actions, or one multi-select in `AddMusicSheet`.

If the user wants catalog browsing, SpotiFLAC's own web UI already does it and works
in Safari on the phone. That is the honest division of labour: Cassette for
listening, the web UI for bulk acquisition, the facade for the "I just heard of this
band, get me this track" case that happens while you're already in the player.

---

## 8. Auth

Two independent layers, and they must not be confused:

- **Subsonic auth is Navidrome's.** Forwarded untouched. The facade holds no
  Navidrome password for the *client's* requests. It already has service credentials
  for `startScan` (`navidrome.go`, env → `config.json`) and reuses those only for
  its own server-initiated calls in step 9.
- **`API_TOKEN` protects `/api/*` and must not gate `/rest/*`.** Cassette cannot
  send a SpotiFLAC bearer token on Subsonic calls. The new route is registered
  **outside** `requireAPIToken`:

  ```go
  mux.HandleFunc("/rest/", handleSubsonic(app))   // no requireAPIToken
  ```

  Harmony's token stays in force for `/api/rpc/` and `/api/events`, unchanged.

If a shared secret on `/rest/*` is wanted later, Cassette's **custom HTTP headers**
feature (built for Cloudflare Access / Authelia) can carry one — configured in the
app's own server form, still no code change. Not required for v1; the box is behind
Cloudflare already.

---

## 9. Failure modes, and the fail-open rule

**The facade must degrade to a plain reverse proxy under every error it can
encounter.**

| Failure | Behaviour |
|---|---|
| Spotify search errors or exceeds 1.5s | Return Navidrome's results alone. No error to the client. |
| Response body isn't parseable JSON | Stream the original bytes through untouched. Never synthesise an error. |
| `MatchLibrary` panics or is slow | Skip the filter, inject unfiltered. |
| `EnqueueDownloads` fails | Return Subsonic error 0 (generic) — the one place an error is honest, because the user's action genuinely didn't happen. |
| Navidrome unreachable | Propagate its status verbatim. The facade adds no retry. |
| Facade container down | **Total outage** — this is the cost of being in the path. Mitigated only by the code above it being small and by the pass-through path having no logic in it. |

`f=xml` must be handled or refused explicitly. Cassette requests `f=json`, but the
pass-through path must not assume it: **only intercept when `f=json`**, and forward
anything else untouched rather than trying to rewrite XML.

---

## 10. What this does *not* recover from Harmony

Stated plainly so the trade is made with open eyes. Switching to Cassette gives up:

- **The on-device recommender** — FoundationModels + Accelerate DSP acoustic ranking,
  "Picked for you", the prompt box, track radio. Not replicable through a Subsonic
  facade at all. The substitute is ListenBrainz (§11.1–11.2): scrobbling, similar
  artists, fresh releases. Note this is a real downgrade in *kind* — Harmony ranked
  candidates on measured acoustic features, ListenBrainz ranks on what other people
  listened to. Nothing on the table restores the former.
- **Charts / New Releases** from Spotify.
- **Spotify playlist and album *import*** — though this one is recoverable
  server-side and needs no client support: SpotiFLAC can create the playlist in
  Navidrome directly, add what it owns, and enqueue the rest. Worth doing as a
  separate piece of work.
- **Ownership badges everywhere.** Cassette will show `↓` rows in search and nothing
  else; there is no "you own 8 of these 12 tracks" on an artist page.
- **Library maintenance UI** — `EnrichLibrary`, `FindDuplicates`, trash. These stay
  in SpotiFLAC's web UI, reachable from any browser including the phone's.

And a warning about what Cassette will look like on day one: on Navidrome 0.61.2
`getSimilarArtists2` **404s** and `getTopSongs` returns nothing (verified during
Harmony's Phase 3). Cassette's `SubsonicRecommendationProvider` will therefore come
back empty, and Discover will look broken until the companion services in §11 are
connected. Those are prerequisites of the switch, not facade concerns.

---

## 11. Companion services

Cassette's discovery surfaces are only as good as the services behind them, and on
this box none of them is connected today. This section is separate work from the
facade — no shared code — but it gates whether the switch is worth making, so it is
specified here and should be done **first**, since it needs no new SpotiFLAC code at
all and immediately tells us how good Cassette can get.

**The decision is ListenBrainz only** (11.1 + 11.2). AudioMuse is declined; 11.3
records why.

**Done 2026-07-31:** token stored in the Keychain (`listenbrainz`), and the Spotify
history imported — **82,937 listens, 2020-05-08 to 2026-07-27**, submitted through
`/1/submit-listens` as `listen_type: "import"`. The account was empty beforehand, so
there was no duplication risk. Filters applied: needs a track name (drops 61
podcast/local rows) and ≥30s played (drops 34,602 skips), deduped on
(timestamp, track, artist). Two things worth knowing if this is ever repeated:
ListenBrainz's rate limit is **30 requests per 5s** and it is shared with *reads*, so
polling the listen-count endpoint during an import is what exhausted the importer's
retries at offset 77,000 the first time; and `listen-count` lags the ingestion queue
by minutes, so a short count right after a submission is not a failure.

### 11.1 ListenBrainz — scrobbling and recommendations

Powers Cassette's Discover: Fresh Releases and Similar Artists, plus scrobbling.
Nothing exists yet — the Keychain has no `listenbrainz` entry.

1. Create the account at listenbrainz.org, take the user token from the profile page.
2. Store it: `security add-generic-password -s listenbrainz -a akvaithi -w '<token>' -T /usr/bin/security -U`. **Never into a file, a commit, or the conversation.**
3. Enter it in Cassette → Settings → ListenBrainz. Cassette keeps its own copy at
   Keychain key `app.cassette.listenbrainz.token` (service group
   `app.cassette.server-credentials`) — the entry above is the operator's record.
4. Scrobbling goes **only** through Cassette. Do **not** also enable Navidrome's
   ListenBrainz agent: Navidrome relays scrobbles itself, and running both
   double-counts every listen and poisons the recommendation signal. This is the same
   rule Harmony follows.

### 11.2 The Spotify listening history — seeding the cold start

This is the piece that makes 11.1 useful on day one instead of in six months, and
it's the reason to do it in this order.

A fresh ListenBrainz account has no history, so Similar Artists and Fresh Releases
have nothing to reason about. The extended streaming history is already on disk at
`~/Developer/Harmony/Spotify Extended Streaming History/` — **`Streaming_History_Audio_*.json`, 2020 through 2026, eleven files.**

**ListenBrainz has imported Spotify extended streaming history natively since August
2025**; no third-party importer is needed. Upload the audio JSONs (not the
`Streaming_History_Video_*` ones) through the site's own import flow.

Two notes:
- Filter plays under 30 seconds, the conventional threshold — skips inflate an
  artist's weight without reflecting taste.
- This is the *same corpus* Harmony's `TasteProfile` reads. Importing it into
  ListenBrainz doesn't move it out of Harmony's reach; both can use it.

### 11.3 AudioMuse-AI — **decided against, 2026-07-31**

Not being deployed. ListenBrainz only. Recorded here with the reasoning so it isn't
re-litigated every time Cassette's Moods tab looks thin.

**Why.** AudioMuse adds four containers (Postgres 15, Redis, Flask, worker) and wants
8 GB RAM. Measured against the actual box on 2026-07-31: CPU and disk are comfortable
(Ryzen 7 5700U, 16 threads, AVX2 present; 260 G free on `/DATA`), but memory is
**14 Gi total, 9.2 Gi used, 5.8 Gi available, with 3.3 Gi of swap already in use** —
the box is under pressure before AudioMuse arrives. It is the least load-bearing of
the three companions, and ListenBrainz carries Discover without it.

**What is actually lost, measured rather than assumed** (probed against Navidrome
0.61.2 on the box, same date):

- **Moods still works, on genres.** `MoodPlaylistService` falls back to
  `LibraryTagTrackProvider`, which gathers candidates per mood via `getSongsByGenre`
  and ranks on genre + `MOOD` + `bpm` tags. Genres are **populated** — `getGenres`
  returns a real distribution (K-Pop 273, Pop 208, Hip Hop 46, …), because
  `EnrichLibrary`'s MusicBrainz pass wrote them into the files and Navidrome indexed
  them. So the candidate-gathering half of the fallback is healthy.
  *(Note: this supersedes Harmony's `CLAUDE.md` note of 2026-07-29 that genres were
  empty everywhere. That was true then and is not true now.)*
- **Mood ranking is weak**, because `bpm` is `0` and `moods` is `[]` on every song
  sampled. The provider will log `(0 tagged, 0 with BPM)` and rank on genre alone.
  Moods will be genre playlists wearing mood names.
- **Instant Mix and the endless queue degrade** to `similarBackfillQueue` — the
  history/discography/genre heuristic — since there is no AudioMuse *and* Navidrome's
  own similarity agent is absent (`getSimilarArtists2` 404s on 0.61.2). Not dead,
  just not sonic.

**The cheap lever if Moods ever matters enough.** Writing `BPM` and `MOOD` tags into
the files would fix the ranking half without any container, and tags-in-files is
already the pattern `EnrichLibrary` follows for lyrics, genres and covers — one pass
fixes Cassette, the web UI and a phone at once. That is a possible future extension
to `EnrichLibrary`, explicitly **not** in scope here, and much cheaper than 8 GB of
Postgres.

---

## 12. Build order

Each phase is independently verifiable on the shipped app.

| Phase | Deliverable | Verified by |
|---|---|---|
| **1** | Pure reverse proxy at `/rest/*`, zero interception — **built** | Point the official macOS Cassette at SpotiFLAC. Everything works exactly as before: browse, play, star, playlists, lyrics, offline downloads. **No regression is the whole test.** |
| **2** | `search3` injection + `getCoverArt` — **built** | `↓` rows appear below real results, with correct artwork, in the official app. Search latency unchanged when Spotify is slow. |
| **3** | `star` / `unstar` trigger — **built** | Add to Favorites on a `↓` row → item appears in `GetQueue` → FLAC on disk → track is in Artists/Albums/Songs. |
| **4** | Post-download reconciliation: star the real track — **built, untested end to end** | The favourite points at a real playable track; the `sf:` record is gone after a sync. |
| **5** | `updatePlaylist` / `createPlaylist` + destination table — **built, untested end to end** | Add to Playlist on a `↓` row acquires *and* lands in the chosen playlist after the rescan. |
| **6** | `stream` intercept — **built** | Pressing play on a `↓` row enqueues and errors cleanly rather than hanging. |
| **7** | iOS confirmation | Install the TestFlight build, repeat phases 1–6 end to end on the phone. |

Phase 1 is the load-bearing one and should sit in production for several days before
phase 2 lands. If phase 1 is not perfectly transparent, nothing after it matters.

**Phase 0 is §11**, and it comes before all of this. Connect ListenBrainz, import the
Spotify history, decide on AudioMuse, and *live with the official Cassette pointed
straight at Navidrome for a while* — no facade in the path at all. That answers the
question this whole document is downstream of: is Cassette, fully fed, actually the
player worth switching to? If the answer is no, none of phases 1–7 need writing.

## 13. Testing

- **Test the shipped artifacts only.** `brew install --cask cassette` for macOS; the
  TestFlight build for iOS. Never a local Xcode build. The reason is in §1.2.
- Phase 1's acceptance criterion is *behavioural identity* — the app must be unable
  to tell it is talking to a proxy. Check playback, seek, offline download, star,
  playlist edit, lyrics, and cold launch with the session restore.
- **Verify the companion services actually round-trip**, don't assume them:
  - Play a track → confirm the listen appears on the ListenBrainz profile, and
    confirm it appears **once**, not twice (§11.1's double-count trap).
  - After the history import, confirm Discover's Similar Artists and Fresh Releases
    return something. Empty here means the import didn't take, not that Cassette is
    broken.
  - Expect Moods to be genre-shaped and Instant Mix to be heuristic (§11.3). That is
    the accepted state, not a bug to chase.
- Keep a way back: the facade is a URL change in Cassette's server settings.
  Reverting means editing one field, which is also the rollback plan.
- The gateway URL gotcha in `CLAUDE.md` applies unchanged — after every deploy,
  re-check the saved flacit-gateway URL is `http://172.17.0.1:8082` and not a
  container IP.

---

## 14. Open questions

1. **Ownership filtering fidelity.** `MatchLibrary` is stricter than Navidrome and
   produced a 26-of-36 false-negative rate on a playlist import during Harmony's
   work. Should §5.1 instead ask *Navidrome* whether the track is owned — a
   `search3` on title+artist against the results already in hand — the way Harmony's
   `refreshOwnershipIndex` does? That is more accurate and costs nothing extra, and
   is probably the right answer.
2. **Does `AddMusicSheet` multi-select reach search results?** If it does, album-scale
   acquisition is one flow rather than ten context menus, and §7's second consequence
   softens considerably. Needs checking against the shipped app.
3. **Does the favourite need to survive at all?** §5.3 re-stars the real track so the
   user's action stays true. The alternative is to treat star purely as a trigger and
   let the favourite vanish — simpler, but it silently discards something the user
   explicitly asked for. Re-starring is the default here; worth a look at how it feels
   in practice.
4. **Duplicate suppression across sessions** — the facade should not re-offer a `↓`
   row for something already sitting in the queue. A queue lookup in §5.1 is easy;
   it is listed here because it changes what the search response means.

---

## 15. Boundaries

Carried over from `CLAUDE.md`, and restated because a facade makes them tempting
again:

- **No fuzzy bot-side search.** Resolution stays link-only. A wrong inline-query match
  files a remix or live cut under the right track's name.
- **No Spotify audio extraction.** Spotify is metadata only.
- **No Navidrome patches.** Configuration only. Navidrome must remain replaceable.
- **No new container *for the facade*.** It is a route in the existing binary.
  AudioMuse (§11.3) is separate infrastructure under a separate decision, and does
  not license the facade to grow a sidecar.
- **No modification to any client**, which is the entire premise.
- **No secret in this repo.** It is public. ListenBrainz and AudioMuse tokens live in
  the Keychain and in the app's own settings, never in a file or a commit.
