package main

import (
	"strings"
	"testing"

	"github.com/akvaithi/SpotiFLAC/backend"
)

// The id namespace is the contract with every client's cache. A row id that
// stops resolving is a permanently broken row the server can't reach, so the
// old namespace has to keep working forever alongside the new one.
func TestRowIDRoundTrip(t *testing.T) {
	cases := []struct {
		track      catalogTrack
		wantRow    string
		wantCover  string
		wantSource string
	}{
		{
			track:      catalogTrack{Source: catalogDeezer, ID: "2438310305"},
			wantRow:    "sf-dz:2438310305",
			wantCover:  "sf-dzcover-2438310305",
			wantSource: catalogDeezer,
		},
		{
			track:      catalogTrack{Source: catalogSpotify, ID: "4cOdK2wGLETKBW3PvgPWqT"},
			wantRow:    "sf:4cOdK2wGLETKBW3PvgPWqT",
			wantCover:  "sf-cover-4cOdK2wGLETKBW3PvgPWqT",
			wantSource: catalogSpotify,
		},
	}

	for _, c := range cases {
		if got := c.track.rowID(); got != c.wantRow {
			t.Errorf("rowID() = %q, want %q", got, c.wantRow)
		}
		if got := c.track.coverID(); got != c.wantCover {
			t.Errorf("coverID() = %q, want %q", got, c.wantCover)
		}
		source, id, ok := parseRowID(c.track.rowID())
		if !ok || source != c.wantSource || id != c.track.ID {
			t.Errorf("parseRowID(%q) = (%q, %q, %v)", c.track.rowID(), source, id, ok)
		}
		// Cover ids must stay colon-free: Arpeggi splits on ":" and requests
		// the bare remainder.
		if strings.Contains(c.track.coverID(), ":") {
			t.Errorf("cover id %q contains a colon", c.track.coverID())
		}
	}

	// Navidrome's own ids must not be mistaken for ours.
	for _, id := range []string{"", "al-123", "4cOdK2wGLETKBW3PvgPWqT", "sf-cover-x"} {
		if _, _, ok := parseRowID(id); ok {
			t.Errorf("parseRowID(%q) claimed a non-virtual id", id)
		}
	}
}

// The prefix is what distinguishes the catalogs, so ids must not collide: the
// same number can name a track in either.
func TestSplitVirtualKeepsThePrefix(t *testing.T) {
	virtual, real := splitVirtual([]string{"sf-dz:123", "nav-1", "sf:123", ""})

	if len(real) != 1 || real[0] != "nav-1" {
		t.Errorf("real ids = %v, want [nav-1]", real)
	}
	if len(virtual) != 2 || virtual[0] != "sf-dz:123" || virtual[1] != "sf:123" {
		t.Fatalf("virtual ids = %v, want both prefixed forms preserved", virtual)
	}
	// Stripping the prefix here is what would make these two indistinguishable.
	if virtual[0] == virtual[1] {
		t.Error("two catalogs' ids collapsed into one")
	}
}

func TestCatalogFromDeezerUsesPerformerCredit(t *testing.T) {
	track := backend.DeezerTrack{
		ID:       2438310305,
		Title:    "Chinna Chinna Asai",
		Duration: 295,
		ISRC:     "IND292201383",
		Artist:   backend.DeezerArtistRef{Name: "A. R. Rahman"},
		Album:    backend.DeezerAlbumRef{Title: "Roja"},
		Contributors: []backend.DeezerContributor{
			{Name: "A. R. Rahman"}, {Name: "Minmini"},
		},
	}

	c := catalogFromDeezer(track)
	if c.Source != catalogDeezer || c.ID != "2438310305" {
		t.Errorf("wrong identity: %+v", c)
	}
	// The full credit, not just Deezer's `artist` field — the performer has to
	// be in there for lyrics and for library matching.
	if !strings.Contains(c.Artists, "Minmini") {
		t.Errorf("credit %q dropped the performer", c.Artists)
	}
	// ISRC rides along, which is what makes the ownership test exact.
	if c.ISRC != "IND292201383" {
		t.Errorf("ISRC lost: %q", c.ISRC)
	}
	if c.Duration != 295 {
		t.Errorf("duration lost: %d", c.Duration)
	}
}

// A cached Spotify row from before the switch must still reach the acquisition
// path, and must not be confused for a Deezer id.
func TestRecallAcceptsBothNamespacesAndBareIDs(t *testing.T) {
	f := &subsonicFacade{seen: map[string]catalogTrack{}}

	dz := catalogTrack{Source: catalogDeezer, ID: "123", Title: "Deezer Row"}
	sp := catalogTrack{Source: catalogSpotify, ID: "abc", Title: "Spotify Row"}
	f.remember(dz)
	f.remember(sp)

	for _, c := range []struct{ lookup, want string }{
		{"sf-dz:123", "Deezer Row"},
		{"sf:abc", "Spotify Row"},
		// Clients that strip the prefix on the way out (Arpeggi does this to
		// cover ids) still have to resolve.
		{"123", "Deezer Row"},
		{"abc", "Spotify Row"},
	} {
		got, ok := f.recall(c.lookup)
		if !ok || got.Title != c.want {
			t.Errorf("recall(%q) = %q/%v, want %q", c.lookup, got.Title, ok, c.want)
		}
	}

	if _, ok := f.recall("something-navidrome-owns"); ok {
		t.Error("recall claimed an id it has never seen")
	}
}
