package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// Library dedup index.
//
// Scans the download/library folder (the user's Navidrome music dir), reads
// each audio file's tags, and builds an index keyed by ISRC and by a normalized
// "title|artist" fingerprint. New downloads are matched against it so tracks
// already in the library can be flagged and skipped instead of re-downloaded.

type LibraryStats struct {
	Scanning  bool   `json:"scanning"`
	Dir       string `json:"dir"`
	Files     int    `json:"files"`
	ISRCs     int    `json:"isrcs"`
	NameKeys  int    `json:"name_keys"`
	ScannedAt string `json:"scanned_at"`
	Error     string `json:"error,omitempty"`
}

type libraryIndex struct {
	mu       sync.RWMutex
	scanning bool
	dir      string
	isrc     map[string]struct{}
	names    map[string]struct{}
	files    int
	scanned  time.Time
	lastErr  string
}

var library = &libraryIndex{isrc: map[string]struct{}{}, names: map[string]struct{}{}}

var (
	reParen = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)
	reFeat  = regexp.MustCompile(`(?i)\b(feat|ft|featuring|with)\b.*$`)
)

func normStr(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reParen.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstArtist(artist string) string {
	for _, sep := range []string{",", ";", "&", "/", " x ", " X "} {
		if i := strings.Index(artist, sep); i >= 0 {
			artist = artist[:i]
		}
	}
	return artist
}

func nameKey(title, artist string) string {
	t := normStr(title)
	a := normStr(firstArtist(artist))
	if t == "" {
		return ""
	}
	return t + "|" + a
}

func libraryIndexPath() string {
	dir, err := backend.EnsureAppDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "library-index.json")
}

type persistedIndex struct {
	Dir       string   `json:"dir"`
	ScannedAt string   `json:"scanned_at"`
	Files     int      `json:"files"`
	ISRCs     []string `json:"isrcs"`
	NameKeys  []string `json:"name_keys"`
}

func (l *libraryIndex) load() {
	p := libraryIndexPath()
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var pi persistedIndex
	if json.Unmarshal(data, &pi) != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dir = pi.Dir
	l.files = pi.Files
	l.isrc = make(map[string]struct{}, len(pi.ISRCs))
	for _, s := range pi.ISRCs {
		l.isrc[s] = struct{}{}
	}
	l.names = make(map[string]struct{}, len(pi.NameKeys))
	for _, s := range pi.NameKeys {
		l.names[s] = struct{}{}
	}
	if t, err := time.Parse(time.RFC3339, pi.ScannedAt); err == nil {
		l.scanned = t
	}
}

func (l *libraryIndex) save() {
	p := libraryIndexPath()
	if p == "" {
		return
	}
	l.mu.RLock()
	pi := persistedIndex{Dir: l.dir, Files: l.files, ScannedAt: l.scanned.Format(time.RFC3339)}
	for k := range l.isrc {
		pi.ISRCs = append(pi.ISRCs, k)
	}
	for k := range l.names {
		pi.NameKeys = append(pi.NameKeys, k)
	}
	l.mu.RUnlock()
	if data, err := json.Marshal(pi); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			os.Rename(tmp, p)
		}
	}
}

func (l *libraryIndex) stats() LibraryStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := LibraryStats{
		Scanning: l.scanning,
		Dir:      l.dir,
		Files:    l.files,
		ISRCs:    len(l.isrc),
		NameKeys: len(l.names),
		Error:    l.lastErr,
	}
	if !l.scanned.IsZero() {
		s.ScannedAt = l.scanned.Format(time.RFC3339)
	}
	return s
}

// ScanLibrary (re)builds the dedup index by walking dir and reading tags.
func (a *App) ScanLibrary(dir string) (LibraryStats, error) {
	if strings.TrimSpace(dir) == "" {
		dir = serverDownloadDir()
	}
	library.mu.Lock()
	if library.scanning {
		library.mu.Unlock()
		return library.stats(), nil
	}
	library.scanning = true
	library.lastErr = ""
	library.mu.Unlock()

	go func() {
		isrc := map[string]struct{}{}
		names := map[string]struct{}{}
		count := 0

		files, err := backend.ListAudioFiles(dir)
		if err == nil {
			for _, f := range files {
				if f.IsDir {
					continue
				}
				count++
				meta, mErr := backend.ReadAudioMetadata(f.Path)
				title, artist := "", ""
				if mErr == nil && meta != nil {
					if strings.TrimSpace(meta.ISRC) != "" {
						isrc[strings.ToUpper(strings.TrimSpace(meta.ISRC))] = struct{}{}
					}
					title, artist = meta.Title, meta.Artist
				}
				if title == "" {
					// fall back to "Title - Artist" / "Artist - Title" filename
					base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
					if parts := strings.SplitN(base, " - ", 2); len(parts) == 2 {
						title, artist = parts[0], parts[1]
					} else {
						title = base
					}
				}
				if k := nameKey(title, artist); k != "" {
					names[k] = struct{}{}
				}
			}
		}

		library.mu.Lock()
		library.scanning = false
		library.dir = dir
		if err != nil {
			library.lastErr = err.Error()
		} else {
			library.isrc = isrc
			library.names = names
			library.files = count
			library.scanned = time.Now()
		}
		library.mu.Unlock()
		if err == nil {
			library.save()
		}
	}()

	return library.stats(), nil
}

func (a *App) GetLibraryStats() LibraryStats {
	return library.stats()
}

type LibMatchInput struct {
	Index  int    `json:"index"`
	ISRC   string `json:"isrc,omitempty"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
}

type LibMatchResult struct {
	Index     int    `json:"index"`
	InLibrary bool   `json:"in_library"`
	MatchType string `json:"match_type,omitempty"`
}

// MatchLibrary reports which of the given tracks already exist in the index.
func (a *App) MatchLibrary(items []LibMatchInput) []LibMatchResult {
	library.mu.RLock()
	defer library.mu.RUnlock()
	out := make([]LibMatchResult, 0, len(items))
	for _, it := range items {
		res := LibMatchResult{Index: it.Index}
		if isrc := strings.ToUpper(strings.TrimSpace(it.ISRC)); isrc != "" {
			if _, ok := library.isrc[isrc]; ok {
				res.InLibrary = true
				res.MatchType = "isrc"
			}
		}
		if !res.InLibrary {
			if k := nameKey(it.Title, it.Artist); k != "" {
				if _, ok := library.names[k]; ok {
					res.InLibrary = true
					res.MatchType = "name"
				}
			}
		}
		out = append(out, res)
	}
	return out
}
