package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

// The Subsonic facade — see SUBSONIC-FACADE.md for the full design.
//
// SpotiFLAC reverse-proxies Navidrome at /rest/*, so an *unmodified* Subsonic
// client (Cassette, on iOS and macOS) can reach the acquisition loop. Cassette
// speaks exactly one protocol and has no plugin surface, so the only way to
// offer "download this track to the server" is to say it in Subsonic verbs.
//
// The default is a transparent proxy. Injection is opt-in via SUBSONIC_FACADE
// so the pass-through can be proven in production before anything is added to
// it — if the proxy is not perfectly invisible, nothing built on top matters.
//
// Everything here fails open: any error in an intercept falls back to
// proxying. The facade sits in the path of the whole library, so a bug in it
// must degrade to "the feature is missing", never to "music stopped".

const (
	virtualIDPrefix = "sf:"

	// No colon here, unlike the song id. Arpeggi splits the coverArt attribute
	// on ":" and requests the bare remainder — verified in the request log,
	// where every one of its getCoverArt calls carried a raw Spotify id that
	// Navidrome has never heard of, and not one carried the prefix. Song ids
	// survive the round trip intact in all three clients, so only this one
	// changes; a colon is also awkward for any client that caches artwork to
	// disk keyed by id.
	virtualCoverPrefix = "sf-cover-"

	// Still recognised on the way in, so artwork a client cached under the old
	// scheme keeps resolving instead of turning into a broken tile.
	legacyCoverPrefix = "sf-cover:"

	// Deezer-sourced rows. A separate namespace rather than reusing "sf:"
	// because the id means something different — the catalog it indexes into —
	// and a client's cache will hold both for a long time after the switch.
	//
	// Same colon rule as above: the song id may carry one (all three clients
	// round-trip it intact), the cover id may not, because Arpeggi splits the
	// coverArt attribute on ":" and requests the bare remainder.
	deezerIDPrefix    = "sf-dz:"
	deezerCoverPrefix = "sf-dzcover-"

	// Spotify's contribution is capped: search results are a list someone is
	// scanning, and a wall of unownable rows under the real ones is worse than
	// a short one.
	maxInjectedSongs = 10

	// How long a search3 waits for Spotify. One number, deliberately.
	//
	// This was briefly two: 250ms when Navidrome had already returned 5+ songs,
	// 2.5s when it came up short, on the theory that a well-answered search does
	// not need acquisition rows and should not pay for them. That theory is
	// wrong, and it produced a bug that read as "downloads work on my phone but
	// not on my desktop" — the real variable was the *query*. Searching "texas"
	// returned exactly 5 owned songs and so silently lost its acquisition rows,
	// while "hair salon" returned 1 and kept them. Owning some of an artist is
	// the most ordinary reason to want the rest, so the premise was backwards as
	// well as inconsistent.
	//
	// A Spotify search costs 1.4-1.7s warm (measured; the requested limit does
	// not move it, it is the round trip). So a novel query now costs about that,
	// and the cache below makes every repeat instant. Predictable and slightly
	// slow beats fast and intermittent.
	spotifySearchBudget = 2500 * time.Millisecond

	// Repeat searches are common (refining a query, coming back to a screen),
	// and the catalog does not change minute to minute.
	searchCacheTTL     = 10 * time.Minute
	searchCacheEntries = 300
)

type subsonicFacade struct {
	app    *App
	target *url.URL
	proxy  *httputil.ReverseProxy
	client *http.Client
	inject bool
	debug  bool

	mu        sync.Mutex
	acquireMu sync.Mutex
	// spotifyID -> the search hit it came from, so getCoverArt and the
	// enqueue both cost no second round trip. Bounded; oldest evicted first.
	seen      map[string]catalogTrack
	seenOrder []string
	// spotifyID -> what to do once the track is really in the library.
	pending map[string]*pendingAcquisition
	// query -> catalog results, so a repeat search never waits.
	searchCache map[string]cachedSearch
	searchOrder []string
}

type cachedSearch struct {
	tracks []catalogTrack
	at     time.Time
}

// pendingAcquisition is the promise made when a virtual id was starred or
// added to a playlist: the user's action has to end up true of the *real*
// track, not the placeholder that triggered it.
//
// Deliberately in memory. Losing it across a restart means the favourite
// quietly disappears at Cassette's next favourites sync, which is the
// documented fallback anyway (SUBSONIC-FACADE.md §5.3) — not worth a bolt
// bucket and a migration.
type pendingAcquisition struct {
	RowID     string // prefixed, so the catalog it came from survives
	Title     string
	Artist    string
	Star      bool
	Playlists []string
}

var facade *subsonicFacade

func initSubsonicFacade(app *App) *subsonicFacade {
	cfg, ok := loadNavidromeConfig()
	if !ok {
		log.Println("subsonic facade: Navidrome is not configured, /rest/* disabled")
		return nil
	}

	target, err := url.Parse(cfg.URL)
	if err != nil {
		log.Printf("subsonic facade: unusable Navidrome URL %q: %v", cfg.URL, err)
		return nil
	}

	f := &subsonicFacade{
		app:    app,
		target: target,
		proxy:  httputil.NewSingleHostReverseProxy(target),
		// No timeout: /rest/stream is a long-lived body, and Range requests on
		// a large FLAC are ordinary. The proxy handles those itself.
		client:      &http.Client{Timeout: 20 * time.Second},
		inject:      envOr("SUBSONIC_FACADE", "") == "inject",
		debug:       envOr("SUBSONIC_FACADE_DEBUG", "") != "",
		seen:        map[string]catalogTrack{},
		pending:     map[string]*pendingAcquisition{},
		searchCache: map[string]cachedSearch{},
	}

	// A failure reaching Navidrome is Navidrome's answer to give, not ours to
	// invent — propagate it rather than dressing it as a Subsonic error.
	f.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("subsonic facade: proxy error for %s: %v", r.URL.Path, err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}

	mode := "pass-through only"
	if f.inject {
		mode = "injection enabled"
		go f.keepSpotifyWarm()
	}
	log.Printf("subsonic facade: /rest/* -> %s (%s)", cfg.URL, mode)
	return f
}

// keepSpotifyWarm holds a live Spotify access token so no search pays for the
// TOTP handshake on top of the search itself.
//
// This removes one variable rather than being the fix: measurement showed the
// dominant cost is the catalog round trip (1.4–1.7s warm), not the handshake.
// The token lives in a package-level cache in backend/spotfetch.go, so warming
// it here warms it for every caller. The interval is well inside the ~1h expiry.
func (f *subsonicFacade) keepSpotifyWarm() {
	for {
		if err := backend.NewSpotifyClient().Initialize(); err != nil {
			log.Printf("subsonic facade: Spotify token warm-up failed: %v", err)
		}
		time.Sleep(20 * time.Minute)
	}
}

// logSubsonicRequest records what a client actually sent, which is the only way
// to settle "the app won't do X" when the endpoint provably works under curl.
//
// Credentials are never logged: u/t/s/p/salt/token are dropped by name, and
// anything unrecognised is logged by key with its value elided rather than
// guessed at. This runs only under SUBSONIC_FACADE_DEBUG.
func logSubsonicRequest(r *http.Request) {
	safe := []string{}
	interesting := map[string]bool{
		"id": true, "albumId": true, "artistId": true, "songId": true,
		"songIdToAdd": true, "songIndexToRemove": true, "playlistId": true,
		"query": true, "name": true, "f": true,
	}

	src := r.URL.Query()
	if r.Method == http.MethodPost {
		// r.ParseForm would consume the body the proxy still has to forward, so
		// read it and put it back. Logging must never change what upstream sees.
		if raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err == nil {
			r.Body = io.NopCloser(bytes.NewReader(raw))
			if vals, e := url.ParseQuery(string(raw)); e == nil {
				src = vals
			}
		}
	}
	for k, vs := range src {
		switch k {
		case "u", "t", "s", "p", "salt", "token", "c", "v":
			continue
		}
		if interesting[k] {
			safe = append(safe, fmt.Sprintf("%s=%v", k, vs))
		} else {
			safe = append(safe, k+"=<set>")
		}
	}
	log.Printf("subsonic facade: %s %s %v", r.Method, subsonicEndpoint(r.URL.Path), safe)
}

// subsonicEndpoint pulls "search3" out of /rest/search3 or /rest/search3.view.
func subsonicEndpoint(path string) string {
	name := strings.TrimPrefix(path, "/rest/")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSuffix(name, ".view")
}

func (f *subsonicFacade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.debug {
		logSubsonicRequest(r)
	}
	if !f.inject {
		f.proxy.ServeHTTP(w, r)
		return
	}
	// Virtual acquisition must authenticate before queueing, just as ordinary
	// Navidrome writes do. Never let a made-up catalog ID bypass upstream auth.
	endpoint := subsonicEndpoint(r.URL.Path)
	if endpoint == "star" || endpoint == "unstar" || endpoint == "stream" || endpoint == "download" || endpoint == "createPlaylist" || endpoint == "updatePlaylist" {
		virtual := false
		for _, key := range []string{"id", "songId", "songIdToAdd"} {
			for _, id := range r.URL.Query()[key] {
				if _, _, ok := parseRowID(id); ok {
					virtual = true
				}
			}
		}
		if virtual && !f.authenticated(r) {
			f.proxy.ServeHTTP(w, r)
			return
		}
	}

	switch subsonicEndpoint(r.URL.Path) {
	case "search3":
		f.handleSearch3(w, r)
	case "getCoverArt":
		f.handleGetCoverArt(w, r)
	case "getPlaylists", "getPlaylist":
		f.handlePlaylistOverlays(w, r)
	case "star", "unstar":
		f.handleStar(w, r)
	case "stream", "download":
		f.handleStream(w, r)
	case "scrobble":
		f.handleVirtualScrobble(w, r)
	case "createPlaylist", "updatePlaylist":
		f.handlePlaylist(w, r)
	default:
		f.proxy.ServeHTTP(w, r)
	}
}

func (f *subsonicFacade) authenticated(r *http.Request) bool {
	q := url.Values{"f": {"json"}}
	for _, key := range []string{"u", "t", "s", "p", "apiKey", "v", "c"} {
		if values := r.URL.Query()[key]; len(values) != 0 {
			q[key] = values
		}
	}
	target := *f.target
	target.Path = "/rest/ping.view"
	target.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var envelope subsonicEnvelope
	return resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope) == nil && envelope.Response.Status == "ok"
}

// -----------------------------------------------------------------------------
// search3 — the only place virtual songs are injected
// -----------------------------------------------------------------------------

func (f *subsonicFacade) handleSearch3(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("query"))

	// Three reasons to stay out of the way entirely:
	//   - POST (the formPost extension): the body carries the params, not the URL.
	//   - An empty query is not a search. Cassette's allSongs() enumerates the
	//     whole library through search3 with an empty query and songOffset
	//     paging; injecting there would scatter placeholders through the
	//     library listing and into everything that caches it.
	//   - Paging past the first page: acquisition rows belong at the end of the
	//     first set of results, not repeated on every page.
	//
	// That last one must test the offset's *value*, not its presence. Amperfy
	// and Arpeggi send `songOffset=0` on every search — perfectly ordinary — and
	// an earlier version skipped on the parameter being set at all, which
	// silently disabled acquisition for both of them.
	//
	// Format is not a reason: both JSON and XML are injected. Subsonic defaults
	// to XML when `f` is absent, which is what these two clients rely on.
	if r.Method != http.MethodGet || query == "" || positiveOffset(q.Get("songOffset")) {
		f.proxy.ServeHTTP(w, r)
		return
	}
	// Subsonic's default response format is XML when `f` is absent.
	wantJSON := q.Get("f") == "json"

	// The catalog lookup runs alongside the upstream request, not after it. A
	// cache hit skips it entirely; a miss keeps filling the cache in the
	// background even if this request gives up waiting, so the next search for
	// the same words is instant.
	cached, hit := f.cachedSearch(query)
	catalogCh := make(chan []catalogTrack, 1)
	if !hit {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), spotifySearchBudget)
			defer cancel()
			tracks, err := searchCatalog(ctx, query, maxInjectedSongs*3)
			if err != nil {
				// Including ErrDeezerSearchUnavailable: a withheld answer is not
				// an empty catalog, and injecting nothing is the right response
				// to both — but only this one gets logged as a problem.
				if err == backend.ErrDeezerSearchUnavailable {
					log.Printf("subsonic facade: %v", err)
				}
				catalogCh <- nil
				return
			}
			f.cacheSearch(query, tracks)
			catalogCh <- tracks
		}()
	}

	body, ct, err := f.forward(r)
	if err != nil {
		f.proxy.ServeHTTP(w, r)
		return
	}

	// Both formats need the same fact before injecting: what the library
	// already returned, so a catalog hit for one of those rows is not offered
	// alongside it.
	var (
		envelope map[string]any
		result   map[string]any
		songs    []any
		owned    ownedSet
	)

	if wantJSON {
		// UseNumber keeps every upstream number byte-identical on the way back
		// out. Without it a re-marshalled float64 can change how an id reads.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&envelope); err != nil {
			writeRaw(w, ct, body)
			return
		}
		resp, _ := envelope["subsonic-response"].(map[string]any)
		if resp == nil || resp["status"] != "ok" {
			writeRaw(w, ct, body)
			return
		}
		result, _ = resp["searchResult3"].(map[string]any)
		if result == nil {
			result = map[string]any{}
			resp["searchResult3"] = result
		}
		songs, _ = result["song"].([]any)
		owned = ownedSetJSON(songs)
	} else {
		var ok bool
		owned, _, ok = ownedSetXML(body)
		if !ok {
			writeRaw(w, ct, body)
			return
		}
	}

	// The wait does not depend on how well the library answered — see the
	// budget constant for why that was tried and abandoned.
	tracks := cached
	if !hit {
		select {
		case tracks = <-catalogCh:
		case <-time.After(spotifySearchBudget):
		}
	}
	if len(tracks) == 0 {
		writeRaw(w, ct, body)
		return
	}

	virtual := f.virtualSongs(query, tracks, owned)
	if len(virtual) == 0 {
		writeRaw(w, ct, body)
		return
	}

	if !wantJSON {
		out, ok := injectXMLSongs(body, virtual)
		if !ok {
			writeRaw(w, ct, body)
			return
		}
		writeRaw(w, ct, out)
		return
	}

	for _, v := range virtual {
		songs = append(songs, v.jsonMap())
	}
	result["song"] = songs

	out, err := json.Marshal(envelope)
	if err != nil {
		writeRaw(w, ct, body)
		return
	}
	writeRaw(w, ct, out)
}

// positiveOffset reports whether a paging parameter asks for anything past the
// first page. An absent, empty, zero or unparseable value all mean "page one".
func positiveOffset(raw string) bool {
	if raw == "" {
		return false
	}
	n, err := strconv.Atoi(raw)
	return err == nil && n > 0
}

// catalogTrack is a row from whichever catalog produced it. Deezer is the
// source of new rows; Spotify rows still arrive from clients' caches and from
// pasted links, and both have to reach the same acquisition path.
//
// Keyed on the *prefixed* row id everywhere, not the bare catalog id: the same
// number can exist in both catalogs, and the prefix is what says which one a
// cached client row meant.
type catalogTrack struct {
	Source   string // catalogDeezer | catalogSpotify
	ID       string // id within that catalog
	Title    string
	Artists  string
	Album    string
	CoverURL string
	Duration int // seconds
	ISRC     string
}

const (
	catalogDeezer  = "deezer"
	catalogSpotify = "spotify"
)

func catalogFromDeezer(t backend.DeezerTrack) catalogTrack {
	return catalogTrack{
		Source: catalogDeezer,
		ID:     strconv.FormatInt(t.ID, 10),
		Title:  t.FullTitle(),
		// The full credit, performer first — Deezer bills the composer as
		// `artist`, which is the wrong name for both matching and lyrics.
		Artists:  t.CreditedArtists(" • "),
		Album:    t.Album.Title,
		CoverURL: t.CoverURL(),
		Duration: t.Duration,
		ISRC:     t.ISRC,
	}
}

func catalogFromSpotify(t backend.SearchResult) catalogTrack {
	return catalogTrack{
		Source:   catalogSpotify,
		ID:       t.ID,
		Title:    t.Name,
		Artists:  t.Artists,
		Album:    t.AlbumName,
		CoverURL: t.Images,
		Duration: t.Duration / 1000,
	}
}

// rowID is the id a client sees, and the key for everything server-side.
func (c catalogTrack) rowID() string {
	if c.Source == catalogDeezer {
		return deezerIDPrefix + c.ID
	}
	return virtualIDPrefix + c.ID
}

func (c catalogTrack) coverID() string {
	if c.Source == catalogDeezer {
		return deezerCoverPrefix + c.ID
	}
	return virtualCoverPrefix + c.ID
}

// parseRowID splits a client-supplied id back into catalog and id. Both
// namespaces are accepted forever — a virtual id sitting in a client's cache
// that the server can no longer resolve is a permanently broken row.
func parseRowID(id string) (source, catalogID string, ok bool) {
	switch {
	case strings.HasPrefix(id, deezerIDPrefix):
		return catalogDeezer, strings.TrimPrefix(id, deezerIDPrefix), true
	case strings.HasPrefix(id, virtualIDPrefix):
		return catalogSpotify, strings.TrimPrefix(id, virtualIDPrefix), true
	}
	return "", "", false
}

// virtualSong is the one description of an acquirable row. It exists so the
// JSON and XML renderings cannot drift apart — the earlier version built a
// map inline, which would have meant maintaining the field list twice.
type virtualSong struct {
	RowID    string
	CoverID  string
	AlbumID  string
	ArtistID string
	Suffix   string
	Title    string
	Artist   string
	Album    string
	HasCover bool
	Duration int // seconds
	// Pending means the download is queued or running: the row stays visible
	// so an acquisition never looks lost, but reads as in progress.
	Pending bool
}

// displayTitle indicates an acquisition already in progress.
func (v virtualSong) displayTitle() string {
	if v.Pending {
		return "⏳ " + v.Title
	}
	return v.Title
}

func (v virtualSong) fileSuffix() string {
	if v.Suffix != "" {
		return v.Suffix
	}
	return "flac"
}

func (v virtualSong) jsonMap() map[string]any {
	m := map[string]any{
		"id":     v.RowID,
		"title":  v.displayTitle(),
		"artist": v.Artist,
		"album":  v.Album,
		"isDir":  false,
		"type":   "music",
		"suffix": v.fileSuffix(),
		// Deliberately absent: created / starred / played. They are Date?
		// on the client and a malformed date would fail the decode for the
		// whole response, taking the real results down with it.
	}
	if v.HasCover {
		m["coverArt"] = v.CoverID
	}
	if v.AlbumID != "" {
		m["albumId"], m["parent"] = v.AlbumID, v.AlbumID
	}
	if v.ArtistID != "" {
		m["artistId"] = v.ArtistID
	}
	if v.Duration > 0 {
		m["duration"] = v.Duration
	}
	return m
}

func (v virtualSong) xmlElement(tag string) string {
	var b strings.Builder
	b.WriteByte('<')
	b.WriteString(tag)
	attr := func(name, value string) {
		b.WriteString(" ")
		b.WriteString(name)
		b.WriteString(`="`)
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString(`"`)
	}
	attr("id", v.RowID)
	attr("title", v.displayTitle())
	attr("artist", v.Artist)
	attr("album", v.Album)
	attr("isDir", "false")
	attr("type", "music")
	attr("suffix", v.fileSuffix())
	if v.AlbumID != "" {
		attr("albumId", v.AlbumID)
		attr("parent", v.AlbumID)
	}
	if v.ArtistID != "" {
		attr("artistId", v.ArtistID)
	}
	if v.HasCover {
		attr("coverArt", v.CoverID)
	}
	if v.Duration > 0 {
		attr("duration", strconv.Itoa(v.Duration))
	}
	b.WriteString("/>")
	return b.String()
}

// virtualSongs turns catalog hits into acquirable rows, dropping anything
// already owned or fetched. In-flight results stay visible.
//
// Source-agnostic: Deezer supplies new rows, but a Spotify-sourced row still
// arrives from a client's cache or a pasted link, and both take this path.
func (f *subsonicFacade) virtualSongs(query string, tracks []catalogTrack, owned ownedSet) []virtualSong {
	states := f.acquisitionStates()

	// Two ownership signals, unioned. Navidrome's own results are the
	// authority on what is playable, but its search only returned a page of
	// them; MatchLibrary sees the whole index and is stricter (it false-
	// negatives on name differences), so either saying "owned" is enough to
	// drop the row. A duplicate result for a track you already have is noise, and
	// suppressing it is worth one cheap extra check.
	inputs := make([]LibMatchInput, 0, len(tracks))
	for i, t := range tracks {
		// ISRC comes free on a Deezer hit, which makes the ownership test exact
		// rather than a name comparison — the branch that used to be dead
		// because Spotify search never carries one.
		in := LibMatchInput{Index: i, ISRC: t.ISRC, Title: t.Title, Artist: t.Artists}
		if t.Source == catalogSpotify {
			in.SpotifyID = t.ID
		}
		inputs = append(inputs, in)
	}
	indexed := map[int]bool{}
	for _, m := range f.app.MatchLibrary(inputs) {
		indexed[m.Index] = m.InLibrary
	}

	eligible := make([]catalogTrack, 0, len(tracks))
	pending := make(map[string]bool, len(tracks))
	for i, t := range tracks {
		if t.ID == "" || t.Title == "" {
			continue
		}
		if indexed[i] || owned.has(t.Title, t.Artists) {
			continue
		}

		// A track already fetched is dropped — the real row is in the results
		// beside it. One still in flight is *kept*, marked as pending.
		//
		// It used to be dropped too, and that made an acquisition disappear for
		// the ~2 minutes between starring it and Navidrome indexing it: the
		// placeholder was suppressed because it was queued, and the real track
		// did not exist yet, so the track was in neither list and looked lost.
		state := states[t.rowID()]
		if state == backend.QueueCompleted {
			continue
		}
		pending[t.rowID()] = state != ""
		eligible = append(eligible, t)
	}

	// Order by how directly each hit answers what was typed, before the cap
	// takes the top ten. Catalog relevance ranks a partial word match
	// ("Vaathi Raid", "Vaaji Vaaji" for "Vaa Vaathi") alongside tracks actually
	// called that, and the cap then spends its budget on them.
	//
	// Ordering, never filtering, and it looks only at the query — nothing here
	// may depend on how well the library answered. That premise is what made
	// acquisition rows silently vanish for well-covered searches once already.
	sort.SliceStable(eligible, func(i, j int) bool {
		return queryRelevance(query, eligible[i].Title) > queryRelevance(query, eligible[j].Title)
	})

	out := make([]virtualSong, 0, maxInjectedSongs)
	for _, t := range eligible {
		if len(out) >= maxInjectedSongs {
			break
		}
		f.remember(t)
		out = append(out, virtualSong{
			RowID:    t.rowID(),
			CoverID:  t.coverID(),
			Title:    t.Title,
			Artist:   t.Artists,
			Album:    t.Album,
			HasCover: t.CoverURL != "",
			Duration: t.Duration,
			Pending:  pending[t.rowID()],
		})
	}
	return out
}

// queryRelevance scores how directly a title answers the text that was typed.
// Coarse on purpose — three tiers, and Spotify's own ordering breaks ties
// within each, because a cleverer score is a thing to debug later.
func queryRelevance(query, title string) int {
	q, t := normStr(query), normStr(title)
	if q == "" || t == "" {
		return 0
	}
	switch {
	case t == q:
		return 2 // called exactly what was asked for
	case strings.Contains(t, q):
		return 1 // the query plus a qualifier, a cover, a version
	default:
		return 0 // matched on a word or two — a neighbour, not the thing
	}
}

// ownedSet indexes the songs Navidrome just returned by loose title, keeping
// every artist credit seen under each one. A title bucket plus an artist test,
// rather than one title|artist key, because the two sides spell an artist list
// differently and don't agree on who is billed first — see artistsOverlap.
type ownedSet map[string][]string

func (s ownedSet) add(title, artist string) {
	if k := titleKey(title); k != "" {
		s[k] = append(s[k], artist)
	}
}

func (s ownedSet) has(title, artist string) bool {
	k := titleKey(title)
	if k == "" {
		return false
	}
	for _, a := range s[k] {
		if creditsMatch(a, artist) {
			return true
		}
	}
	return false
}

// ownedSetJSON indexes the songs Navidrome just returned, so a catalog hit
// for something already in that same result list never appears twice.
func ownedSetJSON(songs []any) ownedSet {
	owned := ownedSet{}
	for _, raw := range songs {
		s, _ := raw.(map[string]any)
		if s == nil {
			continue
		}
		title, _ := s["title"].(string)
		artist, _ := s["artist"].(string)
		if title != "" {
			owned.add(title, artist)
		}
	}
	return owned
}

// ownedSetXML does the same by scanning tokens, and doubles as the
// validity check for the XML path: it reports ok only for a well-formed
// status="ok" response, so anything surprising falls through to pass-through.
func ownedSetXML(body []byte) (owned ownedSet, songCount int, ok bool) {
	owned = ownedSet{}
	dec := xml.NewDecoder(bytes.NewReader(body))
	sawResponse, statusOK := false, false

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, isStart := tok.(xml.StartElement)
		if !isStart {
			continue
		}
		switch se.Name.Local {
		case "subsonic-response":
			sawResponse = true
			for _, a := range se.Attr {
				if a.Name.Local == "status" && a.Value == "ok" {
					statusOK = true
				}
			}
		case "song":
			songCount++
			var title, artist string
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "title":
					title = a.Value
				case "artist":
					artist = a.Value
				}
			}
			if title != "" {
				owned.add(title, artist)
			}
		}
	}
	return owned, songCount, sawResponse && statusOK
}

// injectXMLSongs splices rows into <searchResult3>, by insertion rather than
// re-serialising: parsing and re-emitting the whole document would risk
// changing attributes, entity forms or namespace prefixes on results that were
// already correct. Everything upstream sent is preserved byte for byte.
//
// Returns ok=false whenever the shape isn't what's expected, so a surprising
// response is passed through untouched rather than half-rewritten — malformed
// XML doesn't degrade for a client, it breaks the parse outright.
func injectXMLSongs(body []byte, songs []virtualSong) ([]byte, bool) {
	var rows strings.Builder
	for _, s := range songs {
		rows.WriteString(s.xmlElement("song"))
	}

	// Normal case: a populated element to append to.
	if i := bytes.LastIndex(body, []byte("</searchResult3>")); i >= 0 {
		out := make([]byte, 0, len(body)+rows.Len())
		out = append(out, body[:i]...)
		out = append(out, rows.String()...)
		out = append(out, body[i:]...)
		return out, true
	}

	// Empty library result, which Navidrome emits self-closing. It has to be
	// opened up before anything can go inside it.
	for _, empty := range []string{"<searchResult3/>", "<searchResult3 />"} {
		if i := bytes.Index(body, []byte(empty)); i >= 0 {
			out := make([]byte, 0, len(body)+rows.Len()+32)
			out = append(out, body[:i]...)
			out = append(out, "<searchResult3>"...)
			out = append(out, rows.String()...)
			out = append(out, "</searchResult3>"...)
			out = append(out, body[i+len(empty):]...)
			return out, true
		}
	}
	return nil, false
}

// acquisitionStates maps row id -> queue status for anything this facade has
// already been asked to fetch, so search can tell the three cases apart:
// already here, on its way, or never asked for.
//
// A record is indexed under whichever catalog ids it carries. Both, when it has
// both: a track queued from a Spotify row before the switch must still read as
// "on its way" when the same recording now appears as a Deezer row, or starring
// it again would queue a second copy.
func (f *subsonicFacade) acquisitionStates() map[string]string {
	states := map[string]string{}
	records, err := backend.GetQueueRecords()
	if err != nil {
		return states
	}
	for _, rec := range records {
		switch rec.Status {
		case backend.QueueQueued, backend.QueueDownloading, backend.QueueCompleted:
			if rec.SpotifyID != "" {
				states[virtualIDPrefix+rec.SpotifyID] = rec.Status
			}
			if rec.DeezerID != "" {
				states[deezerIDPrefix+rec.DeezerID] = rec.Status
			}
		}
	}
	return states
}

// -----------------------------------------------------------------------------
// getCoverArt — artwork for a track that isn't on disk yet
// -----------------------------------------------------------------------------

func (f *subsonicFacade) handleGetCoverArt(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	lookup := ""
	switch {
	case strings.HasPrefix(id, deezerCoverPrefix):
		lookup = deezerIDPrefix + strings.TrimPrefix(id, deezerCoverPrefix)
	case strings.HasPrefix(id, virtualCoverPrefix):
		lookup = virtualIDPrefix + strings.TrimPrefix(id, virtualCoverPrefix)
	case strings.HasPrefix(id, legacyCoverPrefix):
		lookup = virtualIDPrefix + strings.TrimPrefix(id, legacyCoverPrefix)
	default:
		// Belt and braces for clients that mangle the id on the way out: a bare
		// id we have seen in a catalog search is ours, whatever the client did
		// to the prefix. Safe because this map only ever holds catalog track
		// ids, so a real Navidrome cover id cannot be in it.
		if _, known := f.recall(id); known {
			lookup = id
		}
	}

	if lookup == "" {
		f.proxy.ServeHTTP(w, r)
		return
	}

	imageURL := f.coverURL(lookup)
	if imageURL == "" {
		writeSubsonicError(w, r, 70, "Cover art not found")
		return
	}

	resp, err := f.client.Get(imageURL)
	if err != nil {
		writeSubsonicError(w, r, 0, "cover fetch failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeSubsonicError(w, r, 70, "Cover art not found")
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
}

func (f *subsonicFacade) cachedSearch(query string) ([]catalogTrack, bool) {
	key := strings.ToLower(strings.TrimSpace(query))
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.searchCache[key]
	if !ok || time.Since(entry.at) > searchCacheTTL {
		return nil, false
	}
	return entry.tracks, true
}

func (f *subsonicFacade) cacheSearch(query string, tracks []catalogTrack) {
	key := strings.ToLower(strings.TrimSpace(query))
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.searchCache[key]; !exists {
		f.searchOrder = append(f.searchOrder, key)
	}
	f.searchCache[key] = cachedSearch{tracks: tracks, at: time.Now()}
	for len(f.searchOrder) > searchCacheEntries {
		delete(f.searchCache, f.searchOrder[0])
		f.searchOrder = f.searchOrder[1:]
	}
}

// searchCatalog asks the source of truth for what can actually be downloaded.
//
// Deezer is the catalog now because it is also the source: a Spotify hit that
// Deezer doesn't carry is a row the pipeline can never satisfy, and offering it
// costs the user a queued track, three 270s bot timeouts and a failure. Asking
// the catalog that feeds the bot means offered implies obtainable.
//
// Spotify is not consulted here at all. It remains the metadata source for
// pasted links (backend.FindDeezerTrack translates those), and older Spotify
// rows still resolve — but nothing new is offered from a catalog that cannot
// deliver.
func searchCatalog(ctx context.Context, query string, limit int) ([]catalogTrack, error) {
	hits, err := backend.SearchDeezerTracks(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]catalogTrack, 0, len(hits))
	for _, h := range hits {
		// `readable` is Deezer saying this is not playable in this region, which
		// means the bot cannot fetch it either.
		if h.ID == 0 || !h.Readable {
			continue
		}
		out = append(out, catalogFromDeezer(h))
	}
	return out, nil
}

func (f *subsonicFacade) remember(t catalogTrack) {
	if t.ID == "" {
		return
	}
	key := t.rowID()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.seen[key]; ok {
		return
	}
	f.seen[key] = t
	f.seenOrder = append(f.seenOrder, key)
	for len(f.seenOrder) > 2000 {
		delete(f.seen, f.seenOrder[0])
		f.seenOrder = f.seenOrder[1:]
	}
}

// recall looks a row up by its full id, and falls back to a bare catalog id for
// clients that strip the prefix on the way out.
func (f *subsonicFacade) recall(id string) (catalogTrack, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.seen[id]; ok {
		return t, true
	}
	// A bare id: try both namespaces before giving up.
	if t, ok := f.seen[deezerIDPrefix+id]; ok {
		return t, true
	}
	if t, ok := f.seen[virtualIDPrefix+id]; ok {
		return t, true
	}
	return catalogTrack{}, false
}

func (f *subsonicFacade) coverURL(id string) string {
	t, _ := f.recall(id)
	return t.CoverURL
}

// -----------------------------------------------------------------------------
// star / unstar — the primary acquisition trigger
// -----------------------------------------------------------------------------

func (f *subsonicFacade) handleStar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	virtual, real := splitVirtual(q["id"])
	if len(virtual) == 0 {
		f.proxy.ServeHTTP(w, r)
		return
	}

	starring := subsonicEndpoint(r.URL.Path) == "star"
	for _, spotifyID := range virtual {
		if starring {
			// "Add to Favorites" on a virtual row is the user saying: put this in my
			// library. The file landing in /downloads and being indexed by
			// Navidrome is exactly that.
			if err := f.acquire(r.Context(), spotifyID, func(p *pendingAcquisition) { p.Star = true }); err != nil {
				log.Printf("subsonic facade: could not enqueue %s: %v", spotifyID, err)
				writeSubsonicError(w, r, 0, "could not queue download")
				return
			}
		} else {
			f.cancelPending(spotifyID)
		}
	}

	f.forwardRemaining(w, r, "id", real)
}

// -----------------------------------------------------------------------------
// createPlaylist / updatePlaylist — acquisition with a destination
// -----------------------------------------------------------------------------

func (f *subsonicFacade) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	param := "songIdToAdd"
	if subsonicEndpoint(r.URL.Path) == "createPlaylist" {
		param = "songId"
	}

	virtual, real := splitVirtual(q[param])
	if len(virtual) == 0 {
		f.proxy.ServeHTTP(w, r)
		return
	}

	for _, spotifyID := range virtual {
		if err := f.acquire(r.Context(), spotifyID, func(*pendingAcquisition) {}); err != nil {
			log.Printf("subsonic facade: could not enqueue %s: %v", spotifyID, err)
			writeSubsonicError(w, r, 0, "could not queue download")
			return
		}
	}

	// Forward first, then record the destination, because on createPlaylist the
	// id only exists once Navidrome has answered — a playlist being created has
	// no id to file anything under yet. Without reading it back, a ↓ track added
	// while making a new playlist would be acquired and then never filed.
	body, ct, err := f.forwardWithout(r, param, real)
	if err != nil {
		// The acquisition still happened; only the filing is lost.
		writeSubsonicOK(w, r)
		return
	}

	playlistID := q.Get("playlistId")
	if playlistID == "" {
		playlistID = createdPlaylistID(body)
	}
	if playlistID != "" {
		f.mu.Lock()
		for _, spotifyID := range virtual {
			if p := f.pending[spotifyID]; p != nil && !contains(p.Playlists, playlistID) {
				p.Playlists = append(p.Playlists, playlistID)
			}
		}
		f.mu.Unlock()
	}

	writeRaw(w, ct, body)
}

// createPlaylistID digs the new playlist's id out of Navidrome's response.
func createdPlaylistID(body []byte) string {
	var payload struct {
		Response struct {
			Playlist struct {
				ID string `json:"id"`
			} `json:"playlist"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Response.Playlist.ID
}

// -----------------------------------------------------------------------------
// stream — pressing play on something that isn't here yet
// -----------------------------------------------------------------------------

func (f *subsonicFacade) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if _, _, ok := parseRowID(id); !ok {
		f.proxy.ServeHTTP(w, r)
		return
	}

	if r.Method == http.MethodHead {
		f.proxy.ServeHTTP(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if rec := playbackQueueRecord(id); rec.Status == backend.QueueCompleted {
		if f.proxyIndexedDownload(w, r, ctx, rec) {
			return
		}
	}
	if err := f.acquire(ctx, id, nil); err != nil {
		log.Printf("subsonic facade: could not enqueue %s: %v", id, err)
		writeSubsonicError(w, r, 0, "Could not queue download")
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for ctx.Err() == nil {
		rec := playbackQueueRecord(id)
		switch rec.Status {
		case backend.QueueCompleted:
			if f.proxyIndexedDownload(w, r, ctx, rec) {
				return
			}
		case backend.QueueFailed, backend.QueueCancelled:
			writeSubsonicError(w, r, 70, "Download did not finish. Tap to retry.")
			return
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	if r.Context().Err() == nil {
		writeSubsonicError(w, r, 70, "Download is still processing. Tap again once it appears in your library.")
	}
}

func (f *subsonicFacade) proxyIndexedDownload(w http.ResponseWriter, r *http.Request, ctx context.Context, rec backend.QueueRecord) bool {
	songID := f.indexedDownload(r.Clone(ctx), rec)
	if songID == "" {
		return false
	}
	proxied := r.Clone(r.Context())
	q := proxied.URL.Query()
	q.Set("id", songID)
	proxied.URL.RawQuery = q.Encode()
	f.proxy.ServeHTTP(w, proxied)
	return true
}

func playbackQueueRecord(rowID string) backend.QueueRecord {
	records, err := backend.GetQueueRecords()
	if err != nil {
		return backend.QueueRecord{}
	}
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if (rec.DeezerID != "" && deezerIDPrefix+rec.DeezerID == rowID) ||
			(rec.SpotifyID != "" && virtualIDPrefix+rec.SpotifyID == rowID) {
			return rec
		}
	}
	return backend.QueueRecord{}
}

func (f *subsonicFacade) indexedDownload(r *http.Request, rec backend.QueueRecord) string {
	// Both containers mount the same music directory at different paths.
	// Compare its relative path so a different release or remix cannot play.
	relative, err := filepath.Rel(serverDownloadDir(), rec.FilePath)
	if err != nil || !filepath.IsLocal(relative) || relative == "." {
		return ""
	}

	search := r.Clone(r.Context())
	search.URL.Path = "/rest/search3.view"
	q := search.URL.Query()
	for _, key := range []string{"id", "format", "maxBitRate", "timeOffset", "estimatedContentLength", "albumOffset", "artistOffset", "songOffset"} {
		q.Del(key)
	}
	q.Set("query", rec.TrackName)
	q.Set("songCount", "50")
	q.Set("artistCount", "0")
	q.Set("albumCount", "0")
	q.Set("f", "json")
	search.URL.RawQuery = q.Encode()

	body, _, err := f.forward(search)
	if err != nil {
		return ""
	}
	var payload struct {
		Response struct {
			SearchResult3 struct {
				Songs []struct {
					ID, Path, Title, Artist, Album string
					ISRC                           []string
				} `json:"song"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	var request DownloadRequest
	_ = json.Unmarshal(rec.Request, &request)
	wantISRC := strings.ToUpper(strings.TrimSpace(request.ISRC))
	match := ""
	ambiguous := false
	for _, song := range payload.Response.SearchResult3.Songs {
		if song.Path == filepath.ToSlash(relative) {
			return song.ID
		}
		// Media Check can reorganize completed downloads. Match their recording
		// code and the release, or require an unambiguous metadata match for
		// older queue records carrying no recording code.
		if wantISRC != "" && !contains(song.ISRC, wantISRC) {
			continue
		}
		if rec.AlbumName == "" || rec.ArtistName == "" ||
			!strings.EqualFold(strings.TrimSpace(song.Title), strings.TrimSpace(rec.TrackName)) ||
			!strings.EqualFold(strings.TrimSpace(song.Album), strings.TrimSpace(rec.AlbumName)) ||
			!artistsOverlap(song.Artist, rec.ArtistName) {
			continue
		}
		ambiguous = ambiguous || match != ""
		match = song.ID
	}
	if !ambiguous {
		return match
	}
	return ""
}

// -----------------------------------------------------------------------------
// Acquisition
// -----------------------------------------------------------------------------

// acquire enqueues a track if it isn't already on its way, and records what
// should become true of it once it lands. Idempotent: a second star for an id
// already queued only updates the promise.
//
// rowID carries its own catalog, so a Deezer row enqueues with an exact source
// id — no link resolution at all — while a Spotify row from a client's cache
// still resolves the old way.
func (f *subsonicFacade) acquire(ctx context.Context, rowID string, note func(*pendingAcquisition)) error {
	if note == nil && isAcquiredOrQueued(playbackQueueRecord(rowID)) {
		return nil
	}
	meta, err := f.trackMetadata(ctx, rowID)
	if err != nil {
		return err
	}

	f.acquireMu.Lock()
	defer f.acquireMu.Unlock()

	alreadyQueued := isAcquiredOrQueued(playbackQueueRecord(rowID))
	if alreadyQueued && note == nil {
		return nil
	}

	if note != nil {
		f.mu.Lock()
		p := f.pending[rowID]
		if p == nil {
			p = &pendingAcquisition{RowID: rowID, Title: meta.Title, Artist: meta.Artists}
			f.pending[rowID] = p
		}
		note(p)
		f.mu.Unlock()
	}
	if alreadyQueued {
		return nil
	}

	req := DownloadRequest{
		Service:    "doubledouble",
		TrackName:  meta.Title,
		ArtistName: meta.Artists,
		AlbumName:  meta.Album,
		CoverURL:   meta.CoverURL,
		ISRC:       meta.ISRC,
		// Ask for lyrics and genre in the file itself. The web UI leaves these
		// to a checkbox, but a track acquired from a phone has no follow-up
		// step — nobody is going to run EnrichLibrary from a bus — and
		// tags-in-files is how everything else here works, because Navidrome
		// then serves them to every client at once.
		EmbedLyrics: true,
		EmbedGenre:  true,
	}
	if meta.Source == catalogDeezer {
		req.DeezerID = meta.ID
		req.Query = "https://www.deezer.com/track/" + meta.ID
	} else {
		req.SpotifyID = meta.ID
		req.Query = "https://open.spotify.com/track/" + meta.ID
	}

	if _, err = f.app.EnqueueDownloads([]DownloadRequest{req}); err != nil {
		f.mu.Lock()
		delete(f.pending, rowID)
		f.mu.Unlock()
	}
	return err
}

func isAcquiredOrQueued(rec backend.QueueRecord) bool {
	if rec.Status == backend.QueueCompleted {
		_, err := os.Stat(rec.FilePath)
		return err == nil
	}
	return rec.Status == backend.QueueQueued || rec.Status == backend.QueueDownloading
}

func (f *subsonicFacade) cancelPending(rowID string) {
	f.mu.Lock()
	delete(f.pending, rowID)
	f.mu.Unlock()
}

// trackMetadata resolves the fields the downloader tags with. The normal path
// is a cache hit — the user starred a row they just searched for — so a fetch
// only happens for a star on a row that outlived the cache (a restart, or a
// result the client had held on to).
//
// The fetch is per-catalog, and a Spotify row can still arrive years after the
// switch from a client's cache, which is why that path stays.
func (f *subsonicFacade) trackMetadata(ctx context.Context, rowID string) (catalogTrack, error) {
	if t, ok := f.recall(rowID); ok {
		return t, nil
	}

	source, catalogID, ok := parseRowID(rowID)
	if !ok {
		return catalogTrack{}, fmt.Errorf("unrecognised row id %q", rowID)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if source == catalogDeezer {
		id, err := strconv.ParseInt(catalogID, 10, 64)
		if err != nil {
			return catalogTrack{}, fmt.Errorf("bad deezer id %q", catalogID)
		}
		track, err := backend.GetDeezerTrack(ctx, id)
		if err != nil {
			return catalogTrack{}, err
		}
		t := catalogFromDeezer(*track)
		f.remember(t)
		return t, nil
	}

	raw, err := backend.GetFilteredSpotifyData(ctx, "https://open.spotify.com/track/"+catalogID, false, 0, "", nil)
	if err != nil {
		return catalogTrack{}, err
	}
	payload, ok := raw.(backend.TrackResponse)
	if !ok {
		if ptr, isPtr := raw.(*backend.TrackResponse); isPtr && ptr != nil {
			payload = *ptr
		} else {
			return catalogTrack{}, fmt.Errorf("unexpected metadata payload for %s", catalogID)
		}
	}
	if payload.Track.Name == "" {
		return catalogTrack{}, fmt.Errorf("no metadata for %s", catalogID)
	}

	t := catalogFromSpotify(backend.SearchResult{
		ID:        catalogID,
		Name:      payload.Track.Name,
		Type:      "track",
		Artists:   payload.Track.Artists,
		AlbumName: payload.Track.AlbumName,
		Images:    payload.Track.Images,
		Duration:  payload.Track.DurationMS,
	})
	f.remember(t)
	return t, nil
}

// reconcile makes the user's action true of the real track. Called after the
// queue drains and Navidrome has been told to rescan.
//
// The rescan is asynchronous and a fresh file is not indexed the instant it
// lands, so this polls rather than assuming. It gives up quietly: a favourite
// that never gets re-applied disappears at the client's next sync, which is a
// visible-but-harmless outcome, not something to retry forever.
func (f *subsonicFacade) reconcile() {
	f.mu.Lock()
	pending := make([]*pendingAcquisition, 0, len(f.pending))
	for _, p := range f.pending {
		pending = append(pending, p)
	}
	f.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	cfg, ok := loadNavidromeConfig()
	if !ok {
		return
	}

	deadline := time.Now().Add(3 * time.Minute)
	for _, p := range pending {
		var songID string
		for time.Now().Before(deadline) {
			if songID = findNavidromeSong(cfg, p.Title, p.Artist); songID != "" {
				break
			}
			time.Sleep(10 * time.Second)
		}
		if songID == "" {
			log.Printf("subsonic facade: %q never appeared in Navidrome, giving up on reconciliation", p.Title)
			f.cancelPending(p.RowID)
			continue
		}

		if p.Star {
			if _, err := callSubsonicWith(cfg, "star", url.Values{"id": {songID}}); err != nil {
				log.Printf("subsonic facade: could not star %s: %v", songID, err)
			}
		}
		for _, playlistID := range p.Playlists {
			v := url.Values{"playlistId": {playlistID}, "songIdToAdd": {songID}}
			if _, err := callSubsonicWith(cfg, "updatePlaylist", v); err != nil {
				log.Printf("subsonic facade: could not add %s to playlist %s: %v", songID, playlistID, err)
			}
		}
		f.cancelPending(p.RowID)
	}
}

// findNavidromeSong looks the freshly-indexed track up by name. ISRC would be
// exact and Navidrome does expose it, but search3 has no ISRC index — so this
// searches on title and confirms the artist.
func findNavidromeSong(cfg navidromeConfig, title, artist string) string {
	v := url.Values{"query": {title}, "songCount": {"20"}, "artistCount": {"0"}, "albumCount": {"0"}}
	body, err := rawSubsonic(cfg, "search3", v)
	if err != nil {
		return ""
	}

	var payload struct {
		Response struct {
			SearchResult3 struct {
				Song []struct {
					ID     string `json:"id"`
					Title  string `json:"title"`
					Artist string `json:"artist"`
				} `json:"song"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	for _, s := range payload.Response.SearchResult3.Song {
		if songMatches(s.Title, s.Artist, title, artist) {
			return s.ID
		}
	}
	return ""
}

// songMatches decides whether a Navidrome row is the track we asked for. Title
// has to match loosely and the credits have to share a performer — the same
// test the library index uses, so "already owned" and "found after download"
// can never disagree about who made a record.
//
// It exists because the old title|firstArtist key got this wrong in production:
// SpotiFLAC tags multiple artists separated by "•", so Navidrome reported
// "Labh Janjua • Sonu Kakkar • Neha Kakkar" while Spotify had said
// "Labh Janjua, Sonu Kakkar, Neha Kakkar", the two sides reduced to
// "labhjanjua" and "labhjanjuasonukakkarnehakakkar", and the track was never
// found — the download landed and the favourite that asked for it was dropped.
func songMatches(navTitle, navArtist, wantTitle, wantArtist string) bool {
	if normStr(navTitle) == "" || normStr(navTitle) != normStr(wantTitle) {
		return false
	}
	return artistsOverlap(navArtist, wantArtist)
}

// -----------------------------------------------------------------------------
// Plumbing
// -----------------------------------------------------------------------------

// splitVirtual separates our placeholder ids from Navidrome's own. Virtual ids
// are returned **with** their prefix: it names the catalog, and dropping it
// would make a Deezer id indistinguishable from a Spotify one.
func splitVirtual(ids []string) (virtual, real []string) {
	for _, id := range ids {
		if _, _, ok := parseRowID(id); ok {
			virtual = append(virtual, id)
		} else if id != "" {
			real = append(real, id)
		}
	}
	return virtual, real
}

// forwardRemaining passes the non-virtual ids on to Navidrome so a mixed
// request still does what it says, and answers ok when nothing is left for it.
func (f *subsonicFacade) forwardRemaining(w http.ResponseWriter, r *http.Request, param string, real []string) {
	if len(real) == 0 {
		writeSubsonicOK(w, r)
		return
	}
	body, ct, err := f.forwardWithout(r, param, real)
	if err != nil {
		writeSubsonicOK(w, r)
		return
	}
	writeRaw(w, ct, body)
}

// forwardWithout replays the client's request with `param` reduced to the real
// ids, so Navidrome never sees a virtual one.
func (f *subsonicFacade) forwardWithout(r *http.Request, param string, real []string) ([]byte, string, error) {
	q := r.URL.Query()
	q.Del(param)
	for _, id := range real {
		q.Add(param, id)
	}
	forwarded := *r.URL
	forwarded.RawQuery = q.Encode()

	proxied := r.Clone(r.Context())
	proxied.URL = &forwarded
	return f.forward(proxied)
}

// forward performs the client's own request against Navidrome, credentials and
// all, and returns the body. Used where the response has to be read rather than
// streamed.
func (f *subsonicFacade) forward(r *http.Request) ([]byte, string, error) {
	upstream := *f.target
	upstream.Path = strings.TrimRight(f.target.Path, "/") + r.URL.Path
	upstream.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		return nil, "", err
	}
	for _, h := range []string{"Authorization", "Cookie", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("navidrome returned %d", resp.StatusCode)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// rawSubsonic makes a server-initiated call with SpotiFLAC's own credentials,
// for the reconciliation pass — nothing to do with the client's request.
func rawSubsonic(cfg navidromeConfig, endpoint string, extra url.Values) ([]byte, error) {
	params := subsonicParams(cfg)
	for k, vs := range extra {
		for _, v := range vs {
			params.Add(k, v)
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/rest/%s?%s", cfg.URL, endpoint, params.Encode()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("navidrome %s returned %d", endpoint, resp.StatusCode)
	}
	return body, nil
}

func callSubsonicWith(cfg navidromeConfig, endpoint string, extra url.Values) (*subsonicEnvelope, error) {
	body, err := rawSubsonic(cfg, endpoint, extra)
	if err != nil {
		return nil, err
	}
	var env subsonicEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.Response.Error != nil {
		return nil, fmt.Errorf("%s: %s (code %d)", endpoint, env.Response.Error.Message, env.Response.Error.Code)
	}
	return &env, nil
}

func writeRaw(w http.ResponseWriter, contentType string, body []byte) {
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeSubsonicOK(w http.ResponseWriter, r *http.Request) {
	writeSubsonicResponse(w, r, map[string]any{
		"status":  "ok",
		"version": "1.16.1",
		"type":    "navidrome",
	})
}

func writeSubsonicError(w http.ResponseWriter, r *http.Request, code int, message string) {
	writeSubsonicResponse(w, r, map[string]any{
		"status":  "failed",
		"version": "1.16.1",
		"type":    "navidrome",
		"error":   map[string]any{"code": code, "message": message},
	})
}

// Subsonic reports its own errors inside a 200 — an HTTP error status is for
// transport failures, and a client that sees one may retry or go offline.
func writeSubsonicResponse(w http.ResponseWriter, r *http.Request, response map[string]any) {
	if r.URL.Query().Get("f") == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"subsonic-response": response})
		return
	}
	type errorElement struct {
		Code    int    `xml:"code,attr"`
		Message string `xml:"message,attr"`
	}
	envelope := struct {
		XMLName   xml.Name      `xml:"subsonic-response"`
		Namespace string        `xml:"xmlns,attr"`
		Status    string        `xml:"status,attr"`
		Version   string        `xml:"version,attr"`
		Type      string        `xml:"type,attr"`
		Error     *errorElement `xml:"error,omitempty"`
	}{Namespace: "http://subsonic.org/restapi", Status: fmt.Sprint(response["status"]), Version: "1.16.1", Type: "navidrome"}
	if problem, ok := response["error"].(map[string]any); ok {
		code, _ := problem["code"].(int)
		envelope.Error = &errorElement{Code: code, Message: fmt.Sprint(problem["message"])}
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(envelope)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
