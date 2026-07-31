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
	virtualIDPrefix    = "sf:"
	virtualCoverPrefix = "sf-cover:"

	// Spotify's contribution is capped: search results are a list someone is
	// scanning, and a wall of unownable rows under the real ones is worse than
	// a short one.
	maxInjectedSongs = 10

	// How long a search3 will wait for Spotify, and it is two numbers because
	// the right answer depends on what the library already said.
	//
	// Measured 2026-07-31: a Spotify catalog search costs 1.4–1.7s warm, and
	// the limit doesn't move it — it's the round trip to the partner API, not
	// the payload. A single budget therefore can't win. At 1.5s it lands on the
	// median and injection becomes a coin flip; at 2.5s every search in the app
	// slows to Spotify's speed, including searches for music already owned,
	// which is a real regression on the common case.
	//
	// So: if Navidrome already answered well, take whatever is cached and get
	// out of the way. If it came up short, that is exactly when acquisition is
	// the point, and waiting is what the user wants.
	spotifyFastBudget = 250 * time.Millisecond
	spotifySlowBudget = 2500 * time.Millisecond

	// Below this many real hits, the library is treated as having come up short.
	librarySatisfied = 5

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

	mu sync.Mutex
	// spotifyID -> the search hit it came from, so getCoverArt and the
	// enqueue both cost no second round trip. Bounded; oldest evicted first.
	seen      map[string]backend.SearchResult
	seenOrder []string
	// spotifyID -> what to do once the track is really in the library.
	pending map[string]*pendingAcquisition
	// query -> catalog results, so a repeat search never waits.
	searchCache map[string]cachedSearch
	searchOrder []string
}

type cachedSearch struct {
	tracks []backend.SearchResult
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
	SpotifyID string
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
		seen:        map[string]backend.SearchResult{},
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

	switch subsonicEndpoint(r.URL.Path) {
	case "search3":
		f.handleSearch3(w, r)
	case "getCoverArt":
		f.handleGetCoverArt(w, r)
	case "star", "unstar":
		f.handleStar(w, r)
	case "stream", "download":
		f.handleStream(w, r)
	case "createPlaylist", "updatePlaylist":
		f.handlePlaylist(w, r)
	default:
		f.proxy.ServeHTTP(w, r)
	}
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
	spotifyCh := make(chan []backend.SearchResult, 1)
	if !hit {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), spotifySlowBudget)
			defer cancel()
			resp, err := backend.SearchSpotify(ctx, query, maxInjectedSongs*3)
			if err != nil || resp == nil {
				spotifyCh <- nil
				return
			}
			f.cacheSearch(query, resp.Tracks)
			spotifyCh <- resp.Tracks
		}()
	}

	body, ct, err := f.forward(r)
	if err != nil {
		f.proxy.ServeHTTP(w, r)
		return
	}

	// Both formats need the same two facts before deciding anything: how many
	// songs the library returned, and what they were (so a catalog hit for one
	// of them isn't offered twice).
	var (
		envelope  map[string]any
		result    map[string]any
		songs     []any
		songCount int
		ownedKeys map[string]bool
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
		songCount = len(songs)
		ownedKeys = ownedKeySetJSON(songs)
	} else {
		var ok bool
		ownedKeys, songCount, ok = ownedKeySetXML(body)
		if !ok {
			writeRaw(w, ct, body)
			return
		}
	}

	// How long to wait is decided *after* seeing what the library returned —
	// see the budget constants. A well-answered search never pays for Spotify.
	tracks := cached
	if !hit {
		budget := spotifySlowBudget
		if songCount >= librarySatisfied {
			budget = spotifyFastBudget
		}
		select {
		case tracks = <-spotifyCh:
		case <-time.After(budget):
		}
	}
	if len(tracks) == 0 {
		writeRaw(w, ct, body)
		return
	}

	virtual := f.virtualSongs(tracks, ownedKeys)
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

// virtualSong is the one description of an acquirable row. It exists so the
// JSON and XML renderings cannot drift apart — the earlier version built a
// map inline, which would have meant maintaining the field list twice.
type virtualSong struct {
	SpotifyID string
	Title     string
	Artist    string
	Album     string
	HasCover  bool
	Duration  int // seconds
	// Pending means the download is queued or running: the row stays visible
	// so an acquisition never looks lost, but reads as in progress.
	Pending bool
}

// marker distinguishes "you can fetch this" from "this is on its way".
func (v virtualSong) marker() string {
	if v.Pending {
		return "⏳ "
	}
	return "↓ "
}

func (v virtualSong) jsonMap() map[string]any {
	m := map[string]any{
		"id":     virtualIDPrefix + v.SpotifyID,
		"title":  v.marker() + v.Title,
		"artist": v.Artist,
		"album":  v.Album,
		"isDir":  false,
		"type":   "music",
		"suffix": "flac",
		// Deliberately absent: created / starred / played. They are Date?
		// on the client and a malformed date would fail the decode for the
		// whole response, taking the real results down with it.
	}
	if v.HasCover {
		m["coverArt"] = virtualCoverPrefix + v.SpotifyID
	}
	if v.Duration > 0 {
		m["duration"] = v.Duration
	}
	return m
}

func (v virtualSong) xmlElement() string {
	var b strings.Builder
	b.WriteString("<song")
	attr := func(name, value string) {
		b.WriteString(" ")
		b.WriteString(name)
		b.WriteString(`="`)
		_ = xml.EscapeText(&b, []byte(value))
		b.WriteString(`"`)
	}
	attr("id", virtualIDPrefix+v.SpotifyID)
	attr("title", v.marker()+v.Title)
	attr("artist", v.Artist)
	attr("album", v.Album)
	attr("isDir", "false")
	attr("type", "music")
	attr("suffix", "flac")
	if v.HasCover {
		attr("coverArt", virtualCoverPrefix+v.SpotifyID)
	}
	if v.Duration > 0 {
		attr("duration", strconv.Itoa(v.Duration))
	}
	b.WriteString("/>")
	return b.String()
}

// virtualSongs turns Spotify hits into acquirable rows, dropping anything
// already owned or already on its way.
func (f *subsonicFacade) virtualSongs(tracks []backend.SearchResult, ownedKeys map[string]bool) []virtualSong {
	states := f.acquisitionStates()

	// Two ownership signals, unioned. Navidrome's own results are the
	// authority on what is playable, but its search only returned a page of
	// them; MatchLibrary sees the whole index and is stricter (it false-
	// negatives on name differences), so either saying "owned" is enough to
	// drop the row. A stray ↓ on a track you already have is noise, and
	// suppressing it is worth one cheap extra check.
	inputs := make([]LibMatchInput, 0, len(tracks))
	for i, t := range tracks {
		inputs = append(inputs, LibMatchInput{Index: i, Title: t.Name, Artist: firstArtist(t.Artists)})
	}
	indexed := map[int]bool{}
	for _, m := range f.app.MatchLibrary(inputs) {
		indexed[m.Index] = m.InLibrary
	}

	out := make([]virtualSong, 0, maxInjectedSongs)
	for i, t := range tracks {
		if len(out) >= maxInjectedSongs {
			break
		}
		if t.ID == "" || t.Name == "" {
			continue
		}
		if indexed[i] || ownedKeys[nameKey(t.Name, t.Artists)] {
			continue
		}

		// A track already fetched is dropped — the real row is in the results
		// beside it. One still in flight is *kept*, marked as pending.
		//
		// It used to be dropped too, and that made an acquisition disappear for
		// the ~2 minutes between starring it and Navidrome indexing it: the
		// placeholder was suppressed because it was queued, and the real track
		// did not exist yet, so the track was in neither list and looked lost.
		state := states[t.ID]
		if state == backend.QueueCompleted {
			continue
		}

		f.remember(t)
		out = append(out, virtualSong{
			SpotifyID: t.ID,
			Title:     t.Name,
			Artist:    t.Artists,
			Album:     t.AlbumName,
			HasCover:  t.Images != "",
			Duration:  t.Duration / 1000,
			Pending:   state != "",
		})
	}
	return out
}

// ownedKeySetJSON indexes the songs Navidrome just returned, so a catalog hit
// for something already in that same result list never appears twice.
func ownedKeySetJSON(songs []any) map[string]bool {
	keys := map[string]bool{}
	for _, raw := range songs {
		s, _ := raw.(map[string]any)
		if s == nil {
			continue
		}
		title, _ := s["title"].(string)
		artist, _ := s["artist"].(string)
		if title != "" {
			keys[nameKey(title, artist)] = true
		}
	}
	return keys
}

// ownedKeySetXML does the same by scanning tokens, and doubles as the
// validity check for the XML path: it reports ok only for a well-formed
// status="ok" response, so anything surprising falls through to pass-through.
func ownedKeySetXML(body []byte) (keys map[string]bool, songCount int, ok bool) {
	keys = map[string]bool{}
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
				keys[nameKey(title, artist)] = true
			}
		}
	}
	return keys, songCount, sawResponse && statusOK
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
		rows.WriteString(s.xmlElement())
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

// acquisitionStates maps spotify id -> queue status for anything this facade
// has already been asked to fetch, so search can tell the three cases apart:
// already here, on its way, or never asked for.
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
				states[rec.SpotifyID] = rec.Status
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
	if !strings.HasPrefix(id, virtualCoverPrefix) {
		f.proxy.ServeHTTP(w, r)
		return
	}

	imageURL := f.coverURL(strings.TrimPrefix(id, virtualCoverPrefix))
	if imageURL == "" {
		writeSubsonicError(w, 70, "Cover art not found")
		return
	}

	resp, err := f.client.Get(imageURL)
	if err != nil {
		writeSubsonicError(w, 0, "cover fetch failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeSubsonicError(w, 70, "Cover art not found")
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
}

func (f *subsonicFacade) cachedSearch(query string) ([]backend.SearchResult, bool) {
	key := strings.ToLower(strings.TrimSpace(query))
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.searchCache[key]
	if !ok || time.Since(entry.at) > searchCacheTTL {
		return nil, false
	}
	return entry.tracks, true
}

func (f *subsonicFacade) cacheSearch(query string, tracks []backend.SearchResult) {
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

func (f *subsonicFacade) remember(t backend.SearchResult) {
	if t.ID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.seen[t.ID]; ok {
		return
	}
	f.seen[t.ID] = t
	f.seenOrder = append(f.seenOrder, t.ID)
	for len(f.seenOrder) > 2000 {
		delete(f.seen, f.seenOrder[0])
		f.seenOrder = f.seenOrder[1:]
	}
}

func (f *subsonicFacade) recall(spotifyID string) (backend.SearchResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.seen[spotifyID]
	return t, ok
}

func (f *subsonicFacade) coverURL(spotifyID string) string {
	t, _ := f.recall(spotifyID)
	return t.Images
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
			// "Add to Favorites" on a ↓ row is the user saying: put this in my
			// library. The file landing in /downloads and being indexed by
			// Navidrome is exactly that.
			if err := f.acquire(spotifyID, func(p *pendingAcquisition) { p.Star = true }); err != nil {
				log.Printf("subsonic facade: could not enqueue %s: %v", spotifyID, err)
				writeSubsonicError(w, 0, "could not queue download")
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
		if err := f.acquire(spotifyID, nil); err != nil {
			log.Printf("subsonic facade: could not enqueue %s: %v", spotifyID, err)
			writeSubsonicError(w, 0, "could not queue download")
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
		writeSubsonicOK(w)
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
	if !strings.HasPrefix(id, virtualIDPrefix) {
		f.proxy.ServeHTTP(w, r)
		return
	}

	// Blocking until the file exists is the tempting version and it does not
	// work: the client's transports time out at 30s while a download is 5–13s
	// of bot latency plus transfer plus tagging. It would succeed often enough
	// to look correct and fail often enough to be untrustworthy. Enqueue and
	// say so instead.
	spotifyID := strings.TrimPrefix(id, virtualIDPrefix)
	if err := f.acquire(spotifyID, func(p *pendingAcquisition) { p.Star = true }); err != nil {
		log.Printf("subsonic facade: could not enqueue %s: %v", spotifyID, err)
	}
	writeSubsonicError(w, 70, "Queued for download — it will appear in your library shortly")
}

// -----------------------------------------------------------------------------
// Acquisition
// -----------------------------------------------------------------------------

// acquire enqueues a Spotify track if it isn't already on its way, and records
// what should become true of it once it lands. Idempotent: a second star for
// an id already queued only updates the promise.
func (f *subsonicFacade) acquire(spotifyID string, note func(*pendingAcquisition)) error {
	f.mu.Lock()
	existing := f.pending[spotifyID]
	f.mu.Unlock()

	if existing != nil {
		if note != nil {
			f.mu.Lock()
			note(existing)
			f.mu.Unlock()
		}
		return nil
	}

	meta, err := f.trackMetadata(spotifyID)
	if err != nil {
		return err
	}

	p := &pendingAcquisition{SpotifyID: spotifyID, Title: meta.Name, Artist: firstArtist(meta.Artists)}
	if note != nil {
		note(p)
	}

	f.mu.Lock()
	f.pending[spotifyID] = p
	f.mu.Unlock()

	// Already fetched or already fetching: the promise above is recorded, but
	// there is nothing new to enqueue.
	if f.acquisitionStates()[spotifyID] != "" {
		return nil
	}

	_, err = f.app.EnqueueDownloads([]DownloadRequest{{
		Service:    "flacit",
		SpotifyID:  spotifyID,
		Query:      "https://open.spotify.com/track/" + spotifyID,
		TrackName:  meta.Name,
		ArtistName: meta.Artists,
		AlbumName:  meta.AlbumName,
		CoverURL:   meta.Images,
		// Ask for lyrics and genre in the file itself. The web UI leaves these
		// to a checkbox, but a track acquired from a phone has no follow-up
		// step — nobody is going to run EnrichLibrary from a bus — and
		// tags-in-files is how everything else here works, because Navidrome
		// then serves them to every client at once.
		EmbedLyrics: true,
		EmbedGenre:  true,
	}})
	if err != nil {
		f.mu.Lock()
		delete(f.pending, spotifyID)
		f.mu.Unlock()
	}
	return err
}

func (f *subsonicFacade) cancelPending(spotifyID string) {
	f.mu.Lock()
	delete(f.pending, spotifyID)
	f.mu.Unlock()
}

// trackMetadata resolves the fields the downloader tags with. The normal path
// is a cache hit — the user starred a row they just searched for — so the
// fetch is only for a star on a row that outlived the cache (a restart, or a
// result the client had held on to).
func (f *subsonicFacade) trackMetadata(spotifyID string) (backend.SearchResult, error) {
	if t, ok := f.recall(spotifyID); ok {
		return t, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	raw, err := backend.GetFilteredSpotifyData(ctx, "https://open.spotify.com/track/"+spotifyID, false, 0, "", nil)
	if err != nil {
		return backend.SearchResult{}, err
	}
	payload, ok := raw.(backend.TrackResponse)
	if !ok {
		if ptr, isPtr := raw.(*backend.TrackResponse); isPtr && ptr != nil {
			payload = *ptr
		} else {
			return backend.SearchResult{}, fmt.Errorf("unexpected metadata payload for %s", spotifyID)
		}
	}

	t := backend.SearchResult{
		ID:        spotifyID,
		Name:      payload.Track.Name,
		Type:      "track",
		Artists:   payload.Track.Artists,
		AlbumName: payload.Track.AlbumName,
		Images:    payload.Track.Images,
		Duration:  payload.Track.DurationMS,
	}
	if t.Name == "" {
		return backend.SearchResult{}, fmt.Errorf("no metadata for %s", spotifyID)
	}
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
			f.cancelPending(p.SpotifyID)
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
		f.cancelPending(p.SpotifyID)
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

// songMatches is deliberately more forgiving than the library index's nameKey,
// and it exists because that key got this wrong in production.
//
// SpotiFLAC tags multiple artists separated by "•", so Navidrome reported
// "Labh Janjua • Sonu Kakkar • Neha Kakkar" while Spotify had said
// "Labh Janjua, Sonu Kakkar, Neha Kakkar". `firstArtist` splits on commas and
// slashes but not bullets, so the two sides reduced to "labhjanjua" and
// "labhjanjuasonukakkarnehakakkar" and the track was never found — the download
// landed, and the favourite that asked for it was silently dropped.
//
// The shared `nameKey` is deliberately NOT changed to fix this: it also feeds
// the strict key behind duplicate detection, where collapsing an artist list to
// its first name would make "Nightcall — Kavinsky" and "Nightcall — Kavinsky •
// Angèle • Phoenix" look like the same recording and offer to delete one.
// Substring containment is safe here because the title already had to match
// exactly and the candidate set is one search's worth of results.
func songMatches(navTitle, navArtist, wantTitle, wantArtist string) bool {
	if normStr(navTitle) == "" || normStr(navTitle) != normStr(wantTitle) {
		return false
	}
	if wantArtist == "" {
		return true
	}
	// normStr strips the separators themselves, so a bullet-, comma- or
	// ampersand-joined list all reduce to the same run of letters.
	have, want := normStr(navArtist), normStr(wantArtist)
	return have == want || strings.Contains(have, want) || strings.Contains(want, have)
}

// -----------------------------------------------------------------------------
// Plumbing
// -----------------------------------------------------------------------------

func splitVirtual(ids []string) (virtual, real []string) {
	for _, id := range ids {
		if strings.HasPrefix(id, virtualIDPrefix) {
			virtual = append(virtual, strings.TrimPrefix(id, virtualIDPrefix))
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
		writeSubsonicOK(w)
		return
	}
	body, ct, err := f.forwardWithout(r, param, real)
	if err != nil {
		writeSubsonicOK(w)
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

func writeSubsonicOK(w http.ResponseWriter) {
	writeSubsonicResponse(w, map[string]any{
		"status":  "ok",
		"version": "1.16.1",
		"type":    "navidrome",
	})
}

func writeSubsonicError(w http.ResponseWriter, code int, message string) {
	writeSubsonicResponse(w, map[string]any{
		"status":  "failed",
		"version": "1.16.1",
		"type":    "navidrome",
		"error":   map[string]any{"code": code, "message": message},
	})
}

// Subsonic reports its own errors inside a 200 — an HTTP error status is for
// transport failures, and a client that sees one may retry or go offline.
func writeSubsonicResponse(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"subsonic-response": response})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
