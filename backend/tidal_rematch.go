package backend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// song.link resolves a Spotify track to *a* Tidal ID, but that ID is often one
// the account/region can't stream (Tidal answers playbackinfopostpaywall with
// 404/401, which a self-hosted gateway surfaces as 502). Tidal usually carries
// the same recording under several IDs, so when the resolved one is dead we ask
// the gateway's /search/ endpoint for alternates and retry with those.

type tidalSearchCandidate struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Version      string `json:"version"`
	Artist       string `json:"artist"`
	ISRC         string `json:"isrc"`
	Duration     int    `json:"duration"`
	AudioQuality string `json:"audioQuality"`
	StreamReady  bool   `json:"streamReady"`
}

type tidalSearchResponse struct {
	Items []tidalSearchCandidate `json:"items"`
}

var (
	tidalFeaturedArtistRegex = regexp.MustCompile(`(?i)\s*[\(\[]\s*(feat\.?|ft\.?|featuring|with)\s[^\)\]]*[\)\]]`)
	tidalTitleNoiseRegex     = regexp.MustCompile(`(?i)\s+-\s+(remaster(ed)?|.*version|.*edit|.*mix|mono|stereo)\b.*$`)
	tidalNonAlnumRegex       = regexp.MustCompile(`[^a-z0-9]+`)
)

// findAlternateTidalTrackIDs returns up to maxCandidates Tidal track IDs that
// look like the same recording as the one that failed. Only works when a custom
// gateway is configured (the community endpoints expose no search).
func (t *TidalDownloader) findAlternateTidalTrackIDs(failedID int64, isrc, title, artist string, maxCandidates int) []int64 {
	if strings.TrimSpace(t.apiURL) == "" || strings.TrimSpace(title) == "" {
		return nil
	}

	query := strings.TrimSpace(firstTidalArtist(artist) + " " + stripTidalFeatured(title))
	if query == "" {
		return nil
	}

	endpoint := fmt.Sprintf("%s/search/?q=%s&limit=25", t.apiURL, url.QueryEscape(query))
	if isrc = strings.ToUpper(strings.TrimSpace(isrc)); isrc != "" {
		endpoint += "&isrc=" + url.QueryEscape(isrc)
	}

	req, err := NewRequestWithDefaultHeaders(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	resp, err := t.client.Do(req)
	if err != nil {
		fmt.Printf("Tidal rematch search failed: %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		fmt.Printf("Tidal rematch search returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(preview)))
		return nil
	}

	var results tidalSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		fmt.Printf("Tidal rematch search decode failed: %v\n", err)
		return nil
	}

	wantTitle := normalizeTidalMatchText(stripTidalFeatured(title))
	wantArtist := normalizeTidalMatchText(firstTidalArtist(artist))

	var isrcMatches, nameMatches []int64
	for _, candidate := range results.Items {
		if candidate.ID == 0 || candidate.ID == failedID || !candidate.StreamReady {
			continue
		}
		if isrc != "" && strings.EqualFold(candidate.ISRC, isrc) {
			isrcMatches = append(isrcMatches, candidate.ID)
			continue
		}
		if !tidalCandidateMatchesName(candidate, wantTitle, wantArtist) {
			continue
		}
		nameMatches = append(nameMatches, candidate.ID)
	}

	ids := append(isrcMatches, nameMatches...)
	if len(ids) > maxCandidates {
		ids = ids[:maxCandidates]
	}
	return ids
}

// resolveTidalRematchISRC falls back to Spotify's ISRC when the caller didn't
// carry one (playlist/album track lists usually don't). Only called on the
// rematch path, so the extra lookup costs nothing in the common case.
func resolveTidalRematchISRC(isrcOverride, spotifyURL string) string {
	if isrc := strings.TrimSpace(isrcOverride); isrc != "" {
		return isrc
	}
	trackID, err := extractSpotifyTrackID(spotifyURL)
	if err != nil {
		return ""
	}
	identifiers, err := GetSpotifyTrackIdentifiersDirect(trackID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(identifiers.ISRC)
}

func tidalCandidateMatchesName(candidate tidalSearchCandidate, wantTitle, wantArtist string) bool {
	if wantTitle == "" || wantArtist == "" {
		return false
	}
	gotTitle := normalizeTidalMatchText(stripTidalFeatured(candidate.Title))
	if gotTitle != wantTitle {
		return false
	}
	gotArtist := normalizeTidalMatchText(candidate.Artist)
	if gotArtist == "" {
		return false
	}
	return gotArtist == wantArtist ||
		strings.Contains(gotArtist, wantArtist) ||
		strings.Contains(wantArtist, gotArtist)
}

func firstTidalArtist(artist string) string {
	artist = strings.TrimSpace(artist)
	for _, sep := range []string{",", ";", " & ", " feat. ", " ft. ", " featuring "} {
		if index := strings.Index(strings.ToLower(artist), strings.ToLower(sep)); index > 0 {
			artist = artist[:index]
		}
	}
	return strings.TrimSpace(artist)
}

func stripTidalFeatured(title string) string {
	return strings.TrimSpace(tidalFeaturedArtistRegex.ReplaceAllString(title, ""))
}

// normalizeTidalMatchText lowercases, drops "- Remastered"-style suffixes and
// strips everything but letters/digits, so "Move (Brb.)" and "move brb" compare
// equal without matching a genuinely different song.
func normalizeTidalMatchText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = tidalTitleNoiseRegex.ReplaceAllString(value, "")
	return tidalNonAlnumRegex.ReplaceAllString(value, "")
}
