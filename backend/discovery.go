package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Discovery helpers for clients that browse rather than just download.
//
// Neither of these is needed by the bundled web UI — it takes a Spotify URL and
// fetches it. A client that wants to *find* music needs an artist graph, and
// this is the only place with a working Spotify session to ask.

type RelatedArtist struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image,omitempty"`
	URI         string `json:"uri,omitempty"`
	ExternalURL string `json:"external_url,omitempty"`
}

// GetRelatedArtists returns Spotify's "fans also like" for an artist.
//
// It reuses the queryArtistOverview persisted query that the discography path
// already relies on: the response carries relatedContent alongside the albums,
// so this costs one request and no new query hash to keep working.
func GetRelatedArtists(artistID string) ([]RelatedArtist, error) {
	artistID = strings.TrimSpace(artistID)
	if artistID == "" {
		return nil, fmt.Errorf("artist id is required")
	}
	// Accept a bare id, a spotify: URI, or an open.spotify.com URL.
	if idx := strings.LastIndex(artistID, "/"); idx >= 0 {
		artistID = artistID[idx+1:]
	}
	if idx := strings.LastIndex(artistID, ":"); idx >= 0 {
		artistID = artistID[idx+1:]
	}
	if idx := strings.Index(artistID, "?"); idx >= 0 {
		artistID = artistID[:idx]
	}

	client := NewSpotifyClient()
	if err := client.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize spotify client: %w", err)
	}

	payload := map[string]interface{}{
		"variables": map[string]interface{}{
			"uri":    fmt.Sprintf("spotify:artist:%s", artistID),
			"locale": "",
		},
		"operationName": "queryArtistOverview",
		"extensions": map[string]interface{}{
			"persistedQuery": map[string]interface{}{
				"version":    1,
				"sha256Hash": "446130b4a0aa6522a686aafccddb0ae849165b5e0436fd802f96e0243617b5d8",
			},
		},
	}

	data, err := client.Query(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to query artist overview: %w", err)
	}

	artistUnion := getMap(getMap(data, "data"), "artistUnion")
	relatedArtists := getMap(getMap(artistUnion, "relatedContent"), "relatedArtists")
	items := getSlice(relatedArtists, "items")

	results := make([]RelatedArtist, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		uri := getString(item, "uri")
		id := getString(item, "id")
		if id == "" && uri != "" {
			if idx := strings.LastIndex(uri, ":"); idx >= 0 {
				id = uri[idx+1:]
			}
		}
		if id == "" {
			continue
		}

		artist := RelatedArtist{
			ID:          id,
			Name:        getString(getMap(item, "profile"), "name"),
			URI:         uri,
			ExternalURL: fmt.Sprintf("https://open.spotify.com/artist/%s", id),
		}

		if sources := getSlice(getMap(getMap(item, "visuals"), "avatarImage"), "sources"); len(sources) > 0 {
			if first, ok := sources[0].(map[string]interface{}); ok {
				artist.Image = getString(first, "url")
			}
		}

		results = append(results, artist)
	}

	return results, nil
}

// ---------------------------------------------------------------- MusicBrainz

type ArtistMember struct {
	Name        string   `json:"name"`
	MBID        string   `json:"mbid,omitempty"`
	Instruments []string `json:"instruments,omitempty"`
	Original    bool     `json:"original,omitempty"`
	Begin       string   `json:"begin,omitempty"`
	End         string   `json:"end,omitempty"`
	Ended       bool     `json:"ended"`
}

type ArtistMembersResult struct {
	MBID    string         `json:"mbid,omitempty"`
	Name    string         `json:"name,omitempty"`
	Type    string         `json:"type,omitempty"`
	Area    string         `json:"area,omitempty"`
	Begin   string         `json:"begin,omitempty"`
	End     string         `json:"end,omitempty"`
	Members []ArtistMember `json:"members"`
}

type mbArtistSearchResponse struct {
	Artists []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Type  string `json:"type"`
		Score int    `json:"score"`
	} `json:"artists"`
}

type mbArtistLookupResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	LifeSpan struct {
		Begin string `json:"begin"`
		End   string `json:"end"`
		Ended bool   `json:"ended"`
	} `json:"life-span"`
	Area *struct {
		Name string `json:"name"`
	} `json:"area"`
	Relations []struct {
		Type       string   `json:"type"`
		Direction  string   `json:"direction"`
		Begin      string   `json:"begin"`
		End        string   `json:"end"`
		Ended      bool     `json:"ended"`
		Attributes []string `json:"attributes"`
		Artist     *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"relations"`
}

func musicBrainzGet(path string, out interface{}) error {
	req, err := http.NewRequest(http.MethodGet, musicBrainzAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("SpotiFLAC/%s ( support@spotbye.qzz.io )", AppVersion))
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: musicBrainzRequestTimeout}

	// Share the global MusicBrainz rate limiter — one request per second is
	// their published limit and the tagging path already respects it.
	waitForMusicBrainzRequestSlot()

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		noteMusicBrainzThrottle()
	}
	if resp.StatusCode != http.StatusOK {
		return &musicBrainzStatusError{StatusCode: resp.StatusCode}
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// GetArtistMembers resolves a band's lineup from MusicBrainz.
//
// Artist pages elsewhere show "similar artists" but never who is actually in the
// band; MusicBrainz models that as a "member of band" relation, which is what
// this reads.
func GetArtistMembers(artistName string) (ArtistMembersResult, error) {
	var result ArtistMembersResult

	artistName = strings.TrimSpace(artistName)
	if artistName == "" {
		return result, fmt.Errorf("artist name is required")
	}

	var search mbArtistSearchResponse
	searchPath := fmt.Sprintf("/artist?query=%s&fmt=json&limit=1", url.QueryEscape(artistName))
	if err := musicBrainzGet(searchPath, &search); err != nil {
		return result, err
	}
	if len(search.Artists) == 0 {
		return result, nil
	}

	mbid := search.Artists[0].ID

	var lookup mbArtistLookupResponse
	lookupPath := fmt.Sprintf("/artist/%s?inc=artist-rels&fmt=json", url.PathEscape(mbid))
	if err := musicBrainzGet(lookupPath, &lookup); err != nil {
		return result, err
	}

	result.MBID = lookup.ID
	result.Name = lookup.Name
	result.Type = lookup.Type
	result.Begin = lookup.LifeSpan.Begin
	result.End = lookup.LifeSpan.End
	if lookup.Area != nil {
		result.Area = lookup.Area.Name
	}
	result.Members = []ArtistMember{}

	// MusicBrainz emits one relation per instrument, so a four-instrument
	// member arrives four times. Collapse by MBID and collect the instruments.
	index := map[string]int{}
	for _, rel := range lookup.Relations {
		// "backward" is the band's view of the relation: this person is a
		// member of us, rather than us being a member of something else.
		if rel.Type != "member of band" || rel.Direction != "backward" || rel.Artist == nil {
			continue
		}

		pos, seen := index[rel.Artist.ID]
		if !seen {
			result.Members = append(result.Members, ArtistMember{
				Name:  rel.Artist.Name,
				MBID:  rel.Artist.ID,
				Begin: rel.Begin,
				End:   rel.End,
				Ended: rel.Ended,
			})
			pos = len(result.Members) - 1
			index[rel.Artist.ID] = pos
		}

		member := &result.Members[pos]
		for _, attr := range rel.Attributes {
			attr = strings.TrimSpace(attr)
			if attr == "" {
				continue
			}
			// "original" is a founding-member marker, not an instrument.
			if strings.EqualFold(attr, "original") {
				member.Original = true
				continue
			}
			if slices.Contains(member.Instruments, attr) {
				continue
			}
			member.Instruments = append(member.Instruments, attr)
		}

		// Widen the span across the merged relations, and treat the member as
		// current if any stint is still open.
		if member.Begin == "" || (rel.Begin != "" && rel.Begin < member.Begin) {
			member.Begin = rel.Begin
		}
		if rel.End > member.End {
			member.End = rel.End
		}
		if !rel.Ended {
			member.Ended = false
			member.End = ""
		}
	}

	return result, nil
}
