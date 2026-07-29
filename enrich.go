package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
	"go.senan.xyz/taglib"
)

// Library enrichment: lyrics and genres, written into the files themselves.
//
// Both belong on the server rather than in a client cache. Navidrome reads tags,
// so once a file carries lyrics its `getLyricsBySongId` works for every client —
// Harmony, the web UI, a phone — and genres populate Navidrome's own index, which
// is empty today because every past download omitted `embed_genre`. A per-client
// cache would solve it once per device and never for anything else.
//
// Sources are free and keyless: LRCLIB for synced lyrics, MusicBrainz for genre
// tags. Both are public services run on donations, so both are rate limited here.

type EnrichOptions struct {
	Lyrics bool `json:"lyrics"`
	Genres bool `json:"genres"`
	// Limit caps how many files are touched in one run. Zero means no limit.
	Limit int `json:"limit"`
	// Overwrite re-fetches even when the tag is already present.
	Overwrite bool `json:"overwrite"`
}

type EnrichStatus struct {
	Running     bool   `json:"running"`
	Total       int    `json:"total"`
	Processed   int    `json:"processed"`
	LyricsAdded int    `json:"lyrics_added"`
	GenresAdded int    `json:"genres_added"`
	Skipped     int    `json:"skipped"`
	Failed      int    `json:"failed"`
	Current     string `json:"current,omitempty"`
	Error       string `json:"error,omitempty"`
	StartedAt   int64  `json:"started_at,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
}

var (
	enrichMu     sync.Mutex
	enrichStatus EnrichStatus
	enrichStop   bool
)

// MusicBrainz asks for one request per second and a contact in the User-Agent.
// Honouring that is the price of a free service with no key.
const (
	musicBrainzDelay = 1100 * time.Millisecond
	lrclibDelay      = 250 * time.Millisecond
	enrichUserAgent  = "SpotiFLAC-Harmony/1.0 ( https://github.com/akvaithi/SpotiFLAC )"
	// Files touched within this window may still be mid-write by the download
	// worker; tagging one while it's being written would corrupt it.
	enrichQuietPeriod = 2 * time.Minute
)

func (a *App) GetEnrichStatus() EnrichStatus {
	enrichMu.Lock()
	defer enrichMu.Unlock()
	return enrichStatus
}

// StopEnrich asks the running pass to finish after the current file.
func (a *App) StopEnrich() {
	enrichMu.Lock()
	enrichStop = true
	enrichMu.Unlock()
}

// EnrichLibrary starts a background pass and returns immediately, like the
// download queue: a full library walk takes far longer than an HTTP request
// should, and the caller shouldn't hold a connection open for it.
func (a *App) EnrichLibrary(opts EnrichOptions) (EnrichStatus, error) {
	enrichMu.Lock()
	if enrichStatus.Running {
		status := enrichStatus
		enrichMu.Unlock()
		return status, fmt.Errorf("an enrichment pass is already running")
	}
	if !opts.Lyrics && !opts.Genres {
		enrichMu.Unlock()
		return EnrichStatus{}, fmt.Errorf("nothing to do: enable lyrics, genres, or both")
	}
	enrichStop = false
	enrichStatus = EnrichStatus{Running: true, StartedAt: time.Now().Unix()}
	enrichMu.Unlock()

	go runEnrichment(opts)

	return a.GetEnrichStatus(), nil
}

func runEnrichment(opts EnrichOptions) {
	defer func() {
		enrichMu.Lock()
		enrichStatus.Running = false
		enrichStatus.Current = ""
		enrichStatus.FinishedAt = time.Now().Unix()
		final := enrichStatus
		enrichMu.Unlock()
		emitEvent("enrich:done", final)

		// Navidrome only learns about new tags on a rescan.
		if final.LyricsAdded > 0 || final.GenresAdded > 0 {
			if err := triggerNavidromeScan(); err != nil {
				fmt.Printf("[enrich] navidrome rescan failed: %v\n", err)
			}
		}
	}()

	files, err := collectAudioFiles(downloadDir)
	if err != nil {
		enrichMu.Lock()
		enrichStatus.Error = err.Error()
		enrichMu.Unlock()
		return
	}

	enrichMu.Lock()
	enrichStatus.Total = len(files)
	enrichMu.Unlock()

	client := &http.Client{Timeout: 20 * time.Second}
	genreCache := map[string][]string{}
	var lastMusicBrainz time.Time

	for _, path := range files {
		enrichMu.Lock()
		stop := enrichStop
		processed := enrichStatus.Processed
		enrichMu.Unlock()
		if stop || (opts.Limit > 0 && processed >= opts.Limit) {
			return
		}

		tags, err := taglib.ReadTags(path)
		if err != nil {
			bumpEnrich(func(s *EnrichStatus) { s.Failed++; s.Processed++ })
			continue
		}

		title := firstTag(tags, taglib.Title)
		artist := firstTag(tags, taglib.AlbumArtist, taglib.Artist)
		album := firstTag(tags, taglib.Album)
		if title == "" || artist == "" {
			bumpEnrich(func(s *EnrichStatus) { s.Skipped++; s.Processed++ })
			continue
		}

		bumpEnrich(func(s *EnrichStatus) { s.Current = title })

		wroteSomething := false

		if opts.Lyrics && (opts.Overwrite || !hasTag(tags, "LYRICS", "UNSYNCEDLYRICS", "SYNCEDLYRICS")) {
			duration := 0
			if raw := firstTag(tags, taglib.Length); raw != "" {
				duration, _ = strconv.Atoi(raw)
			}
			if lyrics := fetchLRCLIB(client, title, primaryArtist(artist), album, duration); lyrics != "" {
				if err := backend.EmbedLyricsOnlyUniversal(path, lyrics); err == nil {
					bumpEnrich(func(s *EnrichStatus) { s.LyricsAdded++ })
					wroteSomething = true
				}
			}
			time.Sleep(lrclibDelay)
		}

		if opts.Genres && (opts.Overwrite || !hasTag(tags, taglib.Genre, "GENRE")) {
			key := strings.ToLower(primaryArtist(artist))
			genres, cached := genreCache[key]
			if !cached {
				if wait := musicBrainzDelay - time.Since(lastMusicBrainz); wait > 0 {
					time.Sleep(wait)
				}
				genres = fetchMusicBrainzGenres(client, primaryArtist(artist))
				lastMusicBrainz = time.Now()
				genreCache[key] = genres
			}
			if len(genres) > 0 {
				// Merge, never Clear: this pass adds a tag, it doesn't own the file.
				update := map[string][]string{taglib.Genre: {genres[0]}}
				if err := taglib.WriteTags(path, update, 0); err == nil {
					bumpEnrich(func(s *EnrichStatus) { s.GenresAdded++ })
					wroteSomething = true
				}
			}
		}

		bumpEnrich(func(s *EnrichStatus) {
			s.Processed++
			if !wroteSomething {
				s.Skipped++
			}
		})

		enrichMu.Lock()
		snapshot := enrichStatus
		enrichMu.Unlock()
		if snapshot.Processed%10 == 0 {
			emitEvent("enrich:progress", snapshot)
		}
	}
}

func bumpEnrich(mutate func(*EnrichStatus)) {
	enrichMu.Lock()
	mutate(&enrichStatus)
	enrichMu.Unlock()
}

// collectAudioFiles walks the library, skipping dot-directories (which is where
// the duplicate-cleanup trash lives) and anything written in the last couple of
// minutes, since the download worker may still have it open.
func collectAudioFiles(root string) ([]string, error) {
	var files []string
	cutoff := time.Now().Add(-enrichQuietPeriod)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".flac", ".mp3", ".m4a":
			if info.ModTime().Before(cutoff) {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}

func firstTag(tags map[string][]string, keys ...string) string {
	for _, key := range keys {
		if values, ok := tags[key]; ok && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func hasTag(tags map[string][]string, keys ...string) bool {
	return firstTag(tags, keys...) != ""
}

// Navidrome joins credited artists; the lyric and genre services want one name.
func primaryArtist(artist string) string {
	for _, sep := range []string{" • ", "•", ";", " & ", ", ", "/"} {
		if index := strings.Index(artist, sep); index > 0 {
			return strings.TrimSpace(artist[:index])
		}
	}
	return strings.TrimSpace(artist)
}

// fetchLRCLIB returns synced lyrics when they exist, falling back to plain.
func fetchLRCLIB(client *http.Client, title, artist, album string, duration int) string {
	endpoint, _ := url.Parse("https://lrclib.net/api/get")
	query := endpoint.Query()
	query.Set("track_name", title)
	query.Set("artist_name", artist)
	if album != "" {
		query.Set("album_name", album)
	}
	if duration > 0 {
		// The version check: a radio edit shouldn't inherit the album cut's timings.
		query.Set("duration", strconv.Itoa(duration))
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", enrichUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var payload struct {
		PlainLyrics  string `json:"plainLyrics"`
		SyncedLyrics string `json:"syncedLyrics"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if payload.SyncedLyrics != "" {
		return payload.SyncedLyrics
	}
	return payload.PlainLyrics
}

// fetchMusicBrainzGenres reads an artist's genres, falling back to community tags
// — which are messier but far better populated. Spotify's own artist genres come
// back empty through the pathfinder API, so this is the only live source.
func fetchMusicBrainzGenres(client *http.Client, artist string) []string {
	endpoint, _ := url.Parse("https://musicbrainz.org/ws/2/artist")
	query := endpoint.Query()
	query.Set("query", artist)
	query.Set("fmt", "json")
	query.Set("limit", "1")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", enrichUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var payload struct {
		Artists []struct {
			Name   string `json:"name"`
			Score  int    `json:"score"`
			Genres []struct {
				Name  string `json:"name"`
				Count int    `json:"count"`
			} `json:"genres"`
			Tags []struct {
				Name  string `json:"name"`
				Count int    `json:"count"`
			} `json:"tags"`
		} `json:"artists"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &payload) != nil || len(payload.Artists) == 0 {
		return nil
	}

	match := payload.Artists[0]
	// A weak name match is worse than no genre: it would file a Tamil film
	// composer under whatever a similarly-spelled band plays.
	if match.Score < 90 || !strings.EqualFold(match.Name, artist) {
		return nil
	}

	// Ordered by how many people agreed, not by whatever order the API returned.
	// NewJeans' raw tags are "kpop, girl group, k-pop, 4th gen k-pop" — the first
	// one is arbitrary, the most-voted one is the actual genre.
	type weighted struct {
		name  string
		count int
	}
	var ranked []weighted
	for _, genre := range match.Genres {
		ranked = append(ranked, weighted{genre.Name, genre.Count})
	}
	if len(ranked) == 0 {
		for _, tag := range match.Tags {
			if tag.Count > 0 {
				ranked = append(ranked, weighted{tag.Name, tag.Count})
			}
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].count > ranked[j].count })

	var out []string
	for _, entry := range ranked {
		out = append(out, titleCaseGenre(entry.name))
	}
	return out
}

// MusicBrainz tags are a folksonomy: "kpop", "k-pop" and "K-Pop" all occur. This
// normalizes just enough that Navidrome's genre list doesn't show three of each.
func titleCaseGenre(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	switch name {
	case "kpop", "k pop":
		name = "k-pop"
	case "jpop", "j pop":
		name = "j-pop"
	case "cpop", "c pop":
		name = "c-pop"
	case "randb", "r and b", "rnb":
		name = "r&b"
	}
	parts := strings.Split(name, " ")
	for index, part := range parts {
		if part == "" {
			continue
		}
		runes := []rune(part)
		parts[index] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	return strings.Join(parts, " ")
}
