package main

import (
	"encoding/xml"
	"strings"
	"testing"
)

const xmlPopulated = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<subsonic-response xmlns="http://subsonic.org/restapi" status="ok" version="1.16.1" type="navidrome">` +
	`<searchResult3>` +
	`<song id="nav1" title="Owned &amp; Loved" artist="Kavinsky" album="OutRun" duration="258"/>` +
	`</searchResult3></subsonic-response>`

const xmlEmpty = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<subsonic-response xmlns="http://subsonic.org/restapi" status="ok" version="1.16.1">` +
	`<searchResult3/></subsonic-response>`

const xmlFailed = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<subsonic-response xmlns="http://subsonic.org/restapi" status="failed" version="1.16.1">` +
	`<error code="40" message="Wrong username or password"/></subsonic-response>`

func sample() []virtualSong {
	return []virtualSong{{
		SpotifyID: "abc123",
		// Deliberately nasty: the characters that break naive string building.
		Title:    `Rock & "Roll" <b>`,
		Artist:   "Kavinsky",
		Album:    "OutRun",
		HasCover: true,
		Duration: 258,
	}}
}

// The whole point of the XML path: the result must still parse. Malformed XML
// doesn't degrade for a client, it breaks the response outright.
func mustParse(t *testing.T, body []byte) int {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	songs := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "song" {
			songs++
		}
	}
	if songs == 0 {
		t.Fatalf("no songs parsed from %s", body)
	}
	return songs
}

func TestInjectXMLPopulated(t *testing.T) {
	out, ok := injectXMLSongs([]byte(xmlPopulated), sample())
	if !ok {
		t.Fatal("injection reported failure on a populated result")
	}
	if n := mustParse(t, out); n != 2 {
		t.Errorf("expected 2 songs (1 real + 1 virtual), got %d", n)
	}
	if !strings.Contains(string(out), `id="sf:abc123"`) {
		t.Error("virtual id missing")
	}
	// The real row must survive untouched, entity form and all.
	if !strings.Contains(string(out), `title="Owned &amp; Loved"`) {
		t.Error("upstream row was altered")
	}
	// Attributes must be escaped, not passed through raw.
	if strings.Contains(string(out), `Rock & "Roll"`) {
		t.Error("attribute value was not escaped")
	}
}

func TestInjectXMLEmptySelfClosing(t *testing.T) {
	out, ok := injectXMLSongs([]byte(xmlEmpty), sample())
	if !ok {
		t.Fatal("injection reported failure on a self-closing empty result")
	}
	if n := mustParse(t, out); n != 1 {
		t.Errorf("expected 1 virtual song, got %d", n)
	}
	if strings.Contains(string(out), "<searchResult3/>") {
		t.Error("self-closing element was left in place")
	}
}

func TestOwnedSetXML(t *testing.T) {
	owned, count, ok := ownedSetXML([]byte(xmlPopulated))
	if !ok || count != 1 {
		t.Fatalf("ok=%v count=%d, want true/1", ok, count)
	}
	// The ampersand must have been decoded before indexing, or the owned-track
	// filter would never match the catalog's plain-text title.
	if !owned.has("Owned & Loved", "Kavinsky") {
		t.Errorf("owned song not indexed: %v", owned)
	}
}

// A non-ok response must not be treated as injectable.
func TestOwnedSetXMLRejectsFailure(t *testing.T) {
	if _, _, ok := ownedSetXML([]byte(xmlFailed)); ok {
		t.Error("a status=failed response was accepted for injection")
	}
}

// Anything unrecognised passes through rather than being half-rewritten.
func TestInjectXMLRefusesUnknownShape(t *testing.T) {
	if _, ok := injectXMLSongs([]byte(`<subsonic-response status="ok"/>`), sample()); ok {
		t.Error("injected into a response with no searchResult3")
	}
}

// songOffset=0 is what Amperfy and Arpeggi send on every search. Treating its
// presence as "this is a paging request" silently disabled acquisition for both.
func TestPositiveOffset(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"", false}, {"0", false}, {"00", false}, {"junk", false}, {"-1", false},
		{"1", true}, {"500", true},
	} {
		if got := positiveOffset(c.in); got != c.want {
			t.Errorf("positiveOffset(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// An acquisition must never be invisible. Between starring a row and Navidrome
// indexing the file there is a ~2 minute window where the real track does not
// exist yet; the placeholder has to stay, marked as in progress.
func TestPendingRowStaysVisible(t *testing.T) {
	pending := virtualSong{SpotifyID: "x", Title: "Hair Salon", Artist: "Megan Moroney", Pending: true}
	fresh := virtualSong{SpotifyID: "y", Title: "Hair Salon", Artist: "Megan Moroney"}

	if got := pending.jsonMap()["title"].(string); !strings.HasPrefix(got, "⏳") {
		t.Errorf("pending row title = %q, want the in-progress marker", got)
	}
	if got := fresh.jsonMap()["title"].(string); !strings.HasPrefix(got, "↓") {
		t.Errorf("acquirable row title = %q, want the download marker", got)
	}
	if !strings.Contains(pending.xmlElement(), "⏳") {
		t.Error("XML rendering lost the pending marker")
	}
}

// Arpeggi splits the coverArt id on ":" and requests the bare remainder, so
// the prefix must not contain one — and a mangled id must still resolve.
func TestCoverPrefixHasNoColon(t *testing.T) {
	if strings.Contains(virtualCoverPrefix, ":") {
		t.Errorf("virtualCoverPrefix %q contains a colon; clients split on it", virtualCoverPrefix)
	}
	v := virtualSong{SpotifyID: "abc123", Title: "T", HasCover: true}
	got := v.jsonMap()["coverArt"].(string)
	if strings.Contains(got, ":") {
		t.Errorf("coverArt id %q contains a colon", got)
	}
	if !strings.HasSuffix(got, "abc123") {
		t.Errorf("coverArt id %q lost the spotify id", got)
	}
}
