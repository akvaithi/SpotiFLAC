package backend

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var songstatsScriptPattern = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld\+json["'][^>]*>(.*?)</script>`)

func (s *SongLinkClient) populateLinksFromSongstats(links *resolvedTrackLinks, isrc string) error {
	pageURL := fmt.Sprintf("https://songstats.com/%s?ref=ISRCFinder", strings.ToUpper(strings.TrimSpace(isrc)))

	req, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", songLinkUserAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch Songstats page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Songstats returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read Songstats response: %w", err)
	}

	matches := songstatsScriptPattern.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return fmt.Errorf("Songstats JSON-LD not found")
	}

	found := false
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}

		scriptBody := strings.TrimSpace(html.UnescapeString(match[1]))
		if scriptBody == "" {
			continue
		}

		var payload interface{}
		if err := json.Unmarshal([]byte(scriptBody), &payload); err != nil {
			continue
		}

		before := *links
		collectSongstatsLinks(payload, links)
		if *links != before {
			found = true
		}
	}

	if !found && !hasAnySongLinkData(links) {
		return fmt.Errorf("no platform links found in Songstats")
	}

	return nil
}

func collectSongstatsLinks(value interface{}, links *resolvedTrackLinks) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if sameAs, ok := typed["sameAs"]; ok {
			applySongstatsSameAs(sameAs, links)
		}
		for _, nested := range typed {
			collectSongstatsLinks(nested, links)
		}
	case []interface{}:
		for _, nested := range typed {
			collectSongstatsLinks(nested, links)
		}
	}
}

func applySongstatsSameAs(value interface{}, links *resolvedTrackLinks) {
	switch typed := value.(type) {
	case string:
		assignSongstatsLink(typed, links)
	case []interface{}:
		for _, item := range typed {
			if link, ok := item.(string); ok {
				assignSongstatsLink(link, links)
			}
		}
	}
}

func assignSongstatsLink(rawLink string, links *resolvedTrackLinks) {
	link := strings.TrimSpace(rawLink)
	if link == "" {
		return
	}

	switch {
	case strings.Contains(link, "listen.tidal.com/track"):
		if links.TidalURL == "" {
			links.TidalURL = link
			fmt.Println("Tidal URL found via Songstats")
		}
	case strings.Contains(link, "music.amazon.com"):
		if links.AmazonURL == "" {
			if normalized := normalizeAmazonMusicURL(link); normalized != "" {
				links.AmazonURL = normalized
				fmt.Println("Amazon URL found via Songstats")
			}
		}
	case strings.Contains(link, "deezer.com"):
		// Songstats lists an artist page under the same platform as the track —
		// `deezer.com/us/artist/491` was accepted here as if it were a track and
		// sent to the bot, which is why "Chinna Chinna Asai" downloaded nothing.
		// normalizeDeezerTrackURL returns empty for anything that isn't a track.
		if links.DeezerURL == "" {
			if normalized := normalizeDeezerTrackURL(link); normalized != "" {
				links.DeezerURL = normalized
				fmt.Println("Deezer URL found via Songstats")
			} else {
				fmt.Printf("Songstats returned a non-track Deezer link, ignoring: %s\n", link)
			}
		}
	}
}
