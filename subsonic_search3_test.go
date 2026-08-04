package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

// End-to-end cover for handleSearch3 against a stub Navidrome.
//
// The pieces either side of it are tested elsewhere; what was untested is the
// handler that decides whether to inject at all. It sits in front of the whole
// library and its governing rule is that any surprise degrades to plain
// proxying — so most of these cases assert that nothing was touched.

// stubNavidrome answers every request with one canned body.
func stubNavidrome(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testFacade points a facade at the stub and pre-seeds the catalog cache, which
// is the seam that keeps the test off the network: a cache hit means
// handleSearch3 never calls Spotify.
func testFacade(t *testing.T, upstream *httptest.Server, query string, tracks []backend.SearchResult) *subsonicFacade {
	t.Helper()
	// Contain anything that reaches for the app dir (the ISRC and queue
	// databases) inside the test's own directory.
	t.Setenv("HOME", t.TempDir())

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("bad stub URL: %v", err)
	}
	f := &subsonicFacade{
		app:         &App{},
		target:      target,
		proxy:       httputil.NewSingleHostReverseProxy(target),
		client:      &http.Client{Timeout: 5 * time.Second},
		inject:      true,
		seen:        map[string]backend.SearchResult{},
		pending:     map[string]*pendingAcquisition{},
		searchCache: map[string]cachedSearch{},
	}
	f.cacheSearch(query, tracks)
	return f
}

func search3Request(rawQuery string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/rest/search3?"+rawQuery, nil)
}

func emptyLibrary(t *testing.T) {
	t.Helper()
	saved := library
	t.Cleanup(func() { library = saved })
	library = &libraryIndex{
		entries: map[string]*libraryEntry{},
		isrc:    map[string][]string{},
		titles:  map[string][]string{},
	}
}

const jsonOneOwnedSong = `{"subsonic-response":{"status":"ok","version":"1.16.1","searchResult3":{"song":[` +
	`{"id":"real-1","title":"Vaa Vaathi","artist":"G. V. Prakash • Shweta Mohan","album":"Vaathi"}]}}}`

func decodeSongs(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var payload struct {
		Response struct {
			SearchResult3 struct {
				Song []map[string]any `json:"song"`
			} `json:"searchResult3"`
		} `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response is not decodable JSON: %v\n%s", err, body)
	}
	return payload.Response.SearchResult3.Song
}

// The whole point of the facade: a catalog hit the library doesn't have becomes
// an acquirable row, and one it already has does not — even though Navidrome
// spells the credit with bullets and Spotify with commas.
func TestSearch3InjectsOnlyWhatIsMissing(t *testing.T) {
	emptyLibrary(t)
	upstream := stubNavidrome(t, "application/json", jsonOneOwnedSong)
	f := testFacade(t, upstream, "vaa vaathi", []backend.SearchResult{
		{ID: "owned", Name: "Vaa Vaathi", Artists: "Shweta Mohan, G. V. Prakash Kumar", AlbumName: "Vaathi"},
		{ID: "new1", Name: "Vaa Vaathi (Flute)", Artists: "Flute Siva"},
	})

	w := httptest.NewRecorder()
	f.handleSearch3(w, search3Request("query=vaa+vaathi&f=json"))

	songs := decodeSongs(t, w.Body.Bytes())
	if len(songs) != 2 {
		t.Fatalf("expected the real row plus one injected, got %d: %v", len(songs), songs)
	}
	if songs[0]["id"] != "real-1" {
		t.Errorf("upstream row was not preserved first: %v", songs[0])
	}
	if songs[1]["id"] != virtualIDPrefix+"new1" {
		t.Errorf("expected the missing track injected, got %v", songs[1])
	}
	if title, _ := songs[1]["title"].(string); !strings.HasPrefix(title, "↓ ") {
		t.Errorf("injected row is not marked acquirable: %q", title)
	}
	for _, s := range songs {
		if s["id"] == virtualIDPrefix+"owned" {
			t.Error("offered a download for a track the library already has")
		}
	}
}

// XML is the default format, and it must be spliced rather than re-serialised.
func TestSearch3InjectsIntoXML(t *testing.T) {
	emptyLibrary(t)
	const body = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<subsonic-response status="ok" version="1.16.1"><searchResult3>` +
		`<song id="real-1" title="Vaa Vaathi" artist="G. V. Prakash • Shweta Mohan"/>` +
		`</searchResult3></subsonic-response>`
	upstream := stubNavidrome(t, "application/xml", body)
	f := testFacade(t, upstream, "vaa vaathi", []backend.SearchResult{
		{ID: "new1", Name: "Vaa Vaathi (Flute)", Artists: "Flute Siva"},
	})

	w := httptest.NewRecorder()
	f.handleSearch3(w, search3Request("query=vaa+vaathi"))

	out := w.Body.String()
	if !strings.Contains(out, `id="real-1"`) {
		t.Error("upstream row did not survive the splice")
	}
	if !strings.Contains(out, `id="sf:new1"`) {
		t.Errorf("no virtual row was spliced in:\n%s", out)
	}
	if !strings.HasPrefix(out, `<?xml`) {
		t.Error("the XML declaration was lost — the body was re-serialised, not spliced")
	}
}

// Everything below is the fail-open rule: on any surprise the client gets
// exactly what Navidrome said.
func TestSearch3PassesThroughUnchanged(t *testing.T) {
	cases := []struct {
		name, contentType, body, rawQuery string
	}{
		{
			name:        "upstream reported failure",
			contentType: "application/json",
			body:        `{"subsonic-response":{"status":"failed","error":{"code":40}}}`,
			rawQuery:    "query=vaa+vaathi&f=json",
		},
		{
			name:        "upstream sent something undecodable",
			contentType: "application/json",
			body:        `{"subsonic-response":{"status":"ok",`,
			rawQuery:    "query=vaa+vaathi&f=json",
		},
		{
			name:        "XML that is not a search3 response",
			contentType: "application/xml",
			body:        `<subsonic-response status="ok"/>`,
			rawQuery:    "query=vaa+vaathi",
		},
		{
			name:        "paging past the first page",
			contentType: "application/json",
			body:        jsonOneOwnedSong,
			rawQuery:    "query=vaa+vaathi&f=json&songOffset=10",
		},
		{
			name:        "an empty query is a library enumeration, not a search",
			contentType: "application/json",
			body:        jsonOneOwnedSong,
			rawQuery:    "query=&f=json",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			emptyLibrary(t)
			upstream := stubNavidrome(t, c.contentType, c.body)
			f := testFacade(t, upstream, "vaa vaathi", []backend.SearchResult{
				{ID: "new1", Name: "Vaa Vaathi (Flute)", Artists: "Flute Siva"},
			})

			w := httptest.NewRecorder()
			f.handleSearch3(w, search3Request(c.rawQuery))

			if got := w.Body.String(); got != c.body {
				t.Errorf("body was modified.\n got: %s\nwant: %s", got, c.body)
			}
		})
	}
}

// songOffset=0 is what Amperfy and Arpeggi send on every search. Testing the
// parameter's presence rather than its value silently disabled acquisition for
// both of them once.
func TestSearch3TreatsZeroOffsetAsFirstPage(t *testing.T) {
	emptyLibrary(t)
	upstream := stubNavidrome(t, "application/json", jsonOneOwnedSong)
	f := testFacade(t, upstream, "vaa vaathi", []backend.SearchResult{
		{ID: "new1", Name: "Vaa Vaathi (Flute)", Artists: "Flute Siva"},
	})

	w := httptest.NewRecorder()
	f.handleSearch3(w, search3Request("query=vaa+vaathi&f=json&songOffset=0"))

	if len(decodeSongs(t, w.Body.Bytes())) != 2 {
		t.Errorf("songOffset=0 suppressed acquisition: %s", w.Body.String())
	}
}

// Titles that are literally what was asked for come before ones that merely
// share a word, so the ten-row cap isn't spent on neighbours.
func TestSearch3RanksLiteralTitleMatchesFirst(t *testing.T) {
	emptyLibrary(t)
	upstream := stubNavidrome(t, "application/json",
		`{"subsonic-response":{"status":"ok","searchResult3":{}}}`)
	f := testFacade(t, upstream, "vaa vaathi", []backend.SearchResult{
		// Spotify's own order: neighbours first, the literal match last.
		{ID: "neighbour1", Name: "Vaathi Raid", Artists: "Anirudh Ravichander"},
		{ID: "neighbour2", Name: "Vaaji Vaaji", Artists: "Hariharan"},
		{ID: "exact", Name: "Vaa Vaathi", Artists: "Shamitha John"},
		{ID: "qualified", Name: "Vaa Vaathi (Flute)", Artists: "Flute Siva"},
	})

	w := httptest.NewRecorder()
	f.handleSearch3(w, search3Request("query=vaa+vaathi&f=json"))

	songs := decodeSongs(t, w.Body.Bytes())
	if len(songs) != 4 {
		t.Fatalf("expected 4 injected rows, got %d", len(songs))
	}
	if songs[0]["id"] != virtualIDPrefix+"exact" {
		t.Errorf("exact title match did not rank first: %v", songs[0]["id"])
	}
	if songs[1]["id"] != virtualIDPrefix+"qualified" {
		t.Errorf("qualified title match did not rank second: %v", songs[1]["id"])
	}
	// Spotify's relative order survives within a tier.
	if songs[2]["id"] != virtualIDPrefix+"neighbour1" || songs[3]["id"] != virtualIDPrefix+"neighbour2" {
		t.Errorf("upstream ordering was not stable within the tier: %v %v", songs[2]["id"], songs[3]["id"])
	}
}
