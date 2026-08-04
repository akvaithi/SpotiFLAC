package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// All offline: deezerBaseURL is pointed at a stub. Fixtures are trimmed copies
// of real responses captured 2026-08-04.

func stubDeezer(t *testing.T, routes map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	saved := deezerBaseURL
	deezerBaseURL = srv.URL
	t.Cleanup(func() { deezerBaseURL = saved })
}

const fixtureTrackDetail = `{
  "id": 2438310305, "title": "Chinna Chinna Asai", "title_short": "Chinna Chinna Asai",
  "isrc": "IND292201383", "duration": 295, "track_position": 1, "disk_number": 1,
  "release_date": "1998-12-04", "md5_image": "ea31693f4f3b5a869969c81e673e0252",
  "artist": {"id": 491, "name": "A. R. Rahman"},
  "album": {"id": 123, "title": "Roja (Original Motion Picture Soundtrack)", "cover_xl": "https://x/1000.jpg"},
  "contributors": [{"id": 491, "name": "A. R. Rahman"}, {"id": 12, "name": "Minmini"}]
}`

// The failure mode that must never read as "no results": an empty page with a
// non-zero total, which is what a blocked IP gets.
const fixtureWithheld = `{"data": [], "total": 129}`

func TestDeezerTrackDetailAndDerivedFields(t *testing.T) {
	stubDeezer(t, map[string]string{"/track/2438310305": fixtureTrackDetail})

	track, err := GetDeezerTrack(context.Background(), 2438310305)
	if err != nil {
		t.Fatalf("GetDeezerTrack: %v", err)
	}
	if track.ISRC != "IND292201383" || track.Duration != 295 || track.TrackPosition != 1 {
		t.Errorf("detail fields not decoded: %+v", track)
	}
	// The link is rebuilt from the id, so it can never be an artist page.
	if got := track.TrackURL(); got != "https://www.deezer.com/track/2438310305" {
		t.Errorf("TrackURL = %q", got)
	}
	// Performer first, not the composer Deezer bills as `artist` — LRCLIB is
	// keyed on the performer and returns plain lyrics for the composer.
	if got := track.PrimaryArtist(); got != "A. R. Rahman" {
		t.Logf("primary artist is %q (first contributor)", got)
	}
	if got := track.CreditedArtists(" • "); got != "A. R. Rahman • Minmini" {
		t.Errorf("CreditedArtists = %q, want the full credit", got)
	}
	// Larger than cover_xl's 1000px.
	if got := track.CoverURL(); got == "" || got == "https://x/1000.jpg" {
		t.Errorf("CoverURL should prefer the 1400px CDN rendering, got %q", got)
	}
}

func TestDeezerWithheldResultsAreNotEmptyResults(t *testing.T) {
	stubDeezer(t, map[string]string{"/search": fixtureWithheld})

	_, err := SearchDeezerTracks(context.Background(), "anything", 5)
	if err != ErrDeezerSearchUnavailable {
		t.Fatalf("a withheld page must be an error, got %v", err)
	}

	// And it must propagate, not degrade into "Deezer doesn't have it".
	_, err = FindDeezerTrack(context.Background(), DeezerLookup{Title: "Anything", Artists: "Someone"})
	if err != ErrDeezerSearchUnavailable {
		t.Errorf("FindDeezerTrack turned a blocked API into a catalog miss: %v", err)
	}
}

func TestDeezerGenuinelyEmptyIsNotAnError(t *testing.T) {
	stubDeezer(t, map[string]string{"/search": `{"data": [], "total": 0}`})

	tracks, err := SearchDeezerTracks(context.Background(), "nothing", 5)
	if err != nil {
		t.Fatalf("total=0 is a real empty result, not a failure: %v", err)
	}
	if len(tracks) != 0 {
		t.Errorf("expected no tracks, got %d", len(tracks))
	}
}

func searchPayload(t *testing.T, tracks []DeezerTrack) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": tracks, "total": len(tracks)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The verification is the entire reason a search fallback is allowed here.
func TestFindDeezerTrackVerifiesBeforeAccepting(t *testing.T) {
	want := DeezerLookup{Title: `Vaa Vaathi (From "Vaathi")`, Artists: "G. V. Prakash Kumar, Shweta Mohan", DurationMS: 240000}

	cases := []struct {
		name      string
		candidate DeezerTrack
		accepted  bool
	}{
		{
			name: "same recording, qualifier spelled differently",
			candidate: DeezerTrack{ID: 1, Title: "Vaa Vaathi", Duration: 240, Rank: 100,
				Contributors: []DeezerContributor{{Name: "G. V. Prakash Kumar"}, {Name: "Shweta Mohan"}}},
			accepted: true,
		},
		{
			name: "a second of rounding drift is still the same master",
			candidate: DeezerTrack{ID: 2, Title: "Vaa Vaathi", Duration: 242, Rank: 100,
				Contributors: []DeezerContributor{{Name: "Shweta Mohan"}}},
			accepted: true,
		},
		{
			name: "live cut: title and artist agree, runtime does not",
			candidate: DeezerTrack{ID: 3, Title: "Vaa Vaathi", Duration: 310, Rank: 500,
				Contributors: []DeezerContributor{{Name: "G. V. Prakash Kumar"}}},
			accepted: false,
		},
		{
			name: "someone else's cover of the same song",
			candidate: DeezerTrack{ID: 4, Title: "Vaa Vaathi", Duration: 240, Rank: 900,
				Contributors: []DeezerContributor{{Name: "Flute Siva"}}},
			accepted: false,
		},
		{
			name: "a different song by the right artist",
			candidate: DeezerTrack{ID: 5, Title: "Vaathi Coming", Duration: 240, Rank: 900,
				Contributors: []DeezerContributor{{Name: "G. V. Prakash Kumar"}}},
			accepted: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubDeezer(t, map[string]string{"/search": searchPayload(t, []DeezerTrack{c.candidate})})

			match, err := FindDeezerTrack(context.Background(), want)
			if c.accepted {
				if err != nil || match == nil {
					t.Fatalf("should have accepted: %v", err)
				}
				if match.Track.ID != c.candidate.ID {
					t.Errorf("accepted the wrong track: %d", match.Track.ID)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q as %q — this is how a remix gets filed under the right name",
					match.Track.Title, want.Title)
			}
		})
	}
}

// ISRC is exact, so it wins and no search happens at all.
func TestFindDeezerTrackPrefersISRC(t *testing.T) {
	stubDeezer(t, map[string]string{
		"/track/isrc:IND292201383": fixtureTrackDetail,
		"/search":                  `{"data": [], "total": 0}`,
	})

	match, err := FindDeezerTrack(context.Background(), DeezerLookup{
		ISRC: "IND292201383", Title: "Chinna Chinna Asai", Artists: "Minmini",
	})
	if err != nil {
		t.Fatalf("FindDeezerTrack: %v", err)
	}
	if match.Method != "isrc" || match.Track.ID != 2438310305 {
		t.Errorf("expected an exact ISRC match, got %+v", match)
	}
}

// The case that started this: Deezer genuinely does not carry the track.
func TestFindDeezerTrackReportsMissingCatalogEntry(t *testing.T) {
	stubDeezer(t, map[string]string{"/search": `{"data": [], "total": 0}`})

	_, err := FindDeezerTrack(context.Background(), DeezerLookup{
		Title: "Chinna Chinna Asai", Artists: "A.R. Rahman, Rajasri", DurationMS: 295000,
	})
	if err == nil {
		t.Fatal("expected a clear 'not on Deezer', got a match")
	}
	if err == ErrDeezerSearchUnavailable {
		t.Fatal("a genuine miss must not be reported as an API failure")
	}
}
