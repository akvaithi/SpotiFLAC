package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// Library dedup index.
//
// Scans the download/library folder (the user's Navidrome music dir), reads
// each audio file's tags, and keeps one entry per file. The entry list backs
// three things:
//
//   - MatchLibrary: flag tracks that are already here before downloading them.
//   - FindDuplicates/CleanupDuplicates: find copies of the same recording that
//     are already on disk and move the redundant ones to a trash folder.
//   - Live updates: every finished download is folded into the index, so it
//     stays current without a rescan (rescans are also incremental — unchanged
//     files are reused by size+mtime and never re-tagged).

const libraryTrashDirName = ".spotiflac-trash"

type LibraryStats struct {
	Scanning  bool   `json:"scanning"`
	Dir       string `json:"dir"`
	Files     int    `json:"files"`
	ISRCs     int    `json:"isrcs"`
	NameKeys  int    `json:"name_keys"`
	ScannedAt string `json:"scanned_at"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// libraryEntry is one audio file on disk. Size+ModTime let a rescan skip files
// that haven't changed.
type libraryEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
	ISRC    string `json:"isrc,omitempty"`
	Title   string `json:"title,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Album   string `json:"album,omitempty"`
}

type libraryIndex struct {
	mu       sync.RWMutex
	scanning bool
	dir      string
	entries  map[string]*libraryEntry // keyed by absolute path
	isrc     map[string][]string      // ISRC -> paths
	names    map[string][]string      // loose title|artist -> paths
	scanned  time.Time
	updated  time.Time
	lastErr  string

	saveMu    sync.Mutex
	saveTimer *time.Timer
}

var library = &libraryIndex{
	entries: map[string]*libraryEntry{},
	isrc:    map[string][]string{},
	names:   map[string][]string{},
}

var (
	reParen = regexp.MustCompile(`[\(\[][^\)\]]*[\)\]]`)
	reFeat  = regexp.MustCompile(`(?i)\b(feat|ft|featuring|with)\b.*$`)
)

func normStr(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reParen.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	return keepAlphanumeric(s)
}

// normStrStrict keeps parenthetical content, so "Song (Live)" and
// "Song (Radio Edit)" stay distinct. Used for duplicate grouping, where a false
// match would delete a track the user meant to keep.
func normStrStrict(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reFeat.ReplaceAllString(s, "")
	return keepAlphanumeric(s)
}

func keepAlphanumeric(s string) string {
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

func strictKey(title, artist string) string {
	t := normStrStrict(title)
	a := normStrStrict(firstArtist(artist))
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
	Version   int             `json:"version"`
	Dir       string          `json:"dir"`
	ScannedAt string          `json:"scanned_at"`
	UpdatedAt string          `json:"updated_at,omitempty"`
	Entries   []*libraryEntry `json:"entries"`
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
	l.entries = make(map[string]*libraryEntry, len(pi.Entries))
	for _, e := range pi.Entries {
		if e != nil && e.Path != "" {
			l.entries[e.Path] = e
		}
	}
	if t, err := time.Parse(time.RFC3339, pi.ScannedAt); err == nil {
		l.scanned = t
	}
	if t, err := time.Parse(time.RFC3339, pi.UpdatedAt); err == nil {
		l.updated = t
	}
	l.reindexLocked()
}

// reindexLocked rebuilds the lookup maps from entries. Caller holds the lock.
func (l *libraryIndex) reindexLocked() {
	l.isrc = make(map[string][]string, len(l.entries))
	l.names = make(map[string][]string, len(l.entries))
	for path, e := range l.entries {
		if e.ISRC != "" {
			l.isrc[e.ISRC] = append(l.isrc[e.ISRC], path)
		}
		if k := nameKey(e.Title, e.Artist); k != "" {
			l.names[k] = append(l.names[k], path)
		}
	}
}

func (l *libraryIndex) save() {
	p := libraryIndexPath()
	if p == "" {
		return
	}
	l.mu.RLock()
	pi := persistedIndex{
		Version:   2,
		Dir:       l.dir,
		ScannedAt: l.scanned.Format(time.RFC3339),
		Entries:   make([]*libraryEntry, 0, len(l.entries)),
	}
	if !l.updated.IsZero() {
		pi.UpdatedAt = l.updated.Format(time.RFC3339)
	}
	for _, e := range l.entries {
		pi.Entries = append(pi.Entries, e)
	}
	l.mu.RUnlock()

	sort.Slice(pi.Entries, func(i, j int) bool { return pi.Entries[i].Path < pi.Entries[j].Path })
	if data, err := json.Marshal(pi); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			os.Rename(tmp, p)
		}
	}
}

// saveSoon coalesces writes: a playlist download touches the index once per
// track, and the index file is rewritten whole.
func (l *libraryIndex) saveSoon() {
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	if l.saveTimer != nil {
		l.saveTimer.Stop()
	}
	l.saveTimer = time.AfterFunc(5*time.Second, l.save)
}

func (l *libraryIndex) stats() LibraryStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := LibraryStats{
		Scanning: l.scanning,
		Dir:      l.dir,
		Files:    len(l.entries),
		ISRCs:    len(l.isrc),
		NameKeys: len(l.names),
		Error:    l.lastErr,
	}
	if !l.scanned.IsZero() {
		s.ScannedAt = l.scanned.Format(time.RFC3339)
	}
	if !l.updated.IsZero() {
		s.UpdatedAt = l.updated.Format(time.RFC3339)
	}
	return s
}

type scannedFile struct {
	path    string
	size    int64
	modTime int64
}

// walkLibraryFiles lists audio files, skipping hidden directories so the trash
// folder (and anything else dot-prefixed) never lands back in the index.
func walkLibraryFiles(dir string) ([]scannedFile, error) {
	var out []scannedFile
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != dir && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !isLibraryAudioExt(filepath.Ext(name)) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, scannedFile{path: path, size: info.Size(), modTime: info.ModTime().Unix()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk library: %w", err)
	}
	return out, nil
}

func isLibraryAudioExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".flac", ".mp3", ".m4a", ".aac":
		return true
	}
	return false
}

// readLibraryEntry tags one file, falling back to the filename when the file
// has no usable title tag.
func readLibraryEntry(path string, size, modTime int64) *libraryEntry {
	e := &libraryEntry{Path: path, Size: size, ModTime: modTime}
	if meta, err := backend.ReadAudioMetadata(path); err == nil && meta != nil {
		e.ISRC = strings.ToUpper(strings.TrimSpace(meta.ISRC))
		e.Title = strings.TrimSpace(meta.Title)
		e.Artist = strings.TrimSpace(meta.Artist)
		e.Album = strings.TrimSpace(meta.Album)
	}
	if e.Title == "" {
		// fall back to "Title - Artist" / "Artist - Title" filename
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if parts := strings.SplitN(base, " - ", 2); len(parts) == 2 {
			e.Title, e.Artist = parts[0], parts[1]
		} else {
			e.Title = base
		}
	}
	return e
}

// ScanLibrary (re)builds the dedup index by walking dir. Files whose size and
// mtime match the index are reused, so repeat scans are cheap.
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
	previousDir := library.dir
	known := make(map[string]*libraryEntry, len(library.entries))
	if previousDir == dir {
		for path, e := range library.entries {
			known[path] = e
		}
	}
	library.mu.Unlock()

	go func() {
		files, err := walkLibraryFiles(dir)
		entries := make(map[string]*libraryEntry, len(files))
		reused := 0
		for _, f := range files {
			if cached, ok := known[f.path]; ok && cached.Size == f.size && cached.ModTime == f.modTime {
				entries[f.path] = cached
				reused++
				continue
			}
			entries[f.path] = readLibraryEntry(f.path, f.size, f.modTime)
		}

		library.mu.Lock()
		library.scanning = false
		library.dir = dir
		if err != nil {
			library.lastErr = err.Error()
		} else {
			library.entries = entries
			library.scanned = time.Now()
			library.updated = time.Time{}
			library.reindexLocked()
		}
		library.mu.Unlock()

		if err == nil {
			fmt.Printf("[Library] Indexed %d files in %s (%d unchanged)\n", len(entries), dir, reused)
			library.save()
			emitEvent("library:scanned", library.stats())
		}
	}()

	return library.stats(), nil
}

func (a *App) GetLibraryStats() LibraryStats {
	return library.stats()
}

// noteLibraryFile folds a freshly downloaded file into the index so the next
// fetch flags it without waiting for a rescan.
func noteLibraryFile(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || !isLibraryAudioExt(filepath.Ext(abs)) {
		return
	}
	entry := readLibraryEntry(abs, info.Size(), info.ModTime().Unix())

	library.mu.Lock()
	if library.dir == "" {
		library.dir = serverDownloadDir()
	}
	library.entries[abs] = entry
	library.updated = time.Now()
	library.reindexLocked()
	library.mu.Unlock()

	library.saveSoon()
}

// forgetLibraryFiles drops paths that are no longer in the library.
func forgetLibraryFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	library.mu.Lock()
	for _, p := range paths {
		delete(library.entries, p)
	}
	library.updated = time.Now()
	library.reindexLocked()
	library.mu.Unlock()
	library.save()
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
			if len(library.isrc[isrc]) > 0 {
				res.InLibrary = true
				res.MatchType = "isrc"
			}
		}
		if !res.InLibrary {
			if k := nameKey(it.Title, it.Artist); k != "" {
				if len(library.names[k]) > 0 {
					res.InLibrary = true
					res.MatchType = "name"
				}
			}
		}
		out = append(out, res)
	}
	return out
}
