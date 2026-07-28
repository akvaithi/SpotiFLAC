package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Duplicate cleanup for files already on disk.
//
// Files are grouped into "same recording" sets by ISRC and by a strict
// title|artist key (parentheticals kept, so "Song (Live)" never merges with the
// studio take). Within a group the best copy is picked — lossless first, then
// biggest file, then oldest — and the rest are offered for removal. Removal
// defaults to moving files into <library>/.spotiflac-trash/<timestamp>/ rather
// than deleting, so a bad call is recoverable; the scan skips dot-directories
// so trashed files don't reappear in the index.

type DuplicateFile struct {
	Path     string  `json:"path"`
	RelPath  string  `json:"rel_path"`
	Size     int64   `json:"size"`
	SizeMB   float64 `json:"size_mb"`
	Format   string  `json:"format"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	ISRC     string  `json:"isrc,omitempty"`
	Modified string  `json:"modified"`
	Keep     bool    `json:"keep"`
	Reason   string  `json:"reason,omitempty"`
}

type DuplicateGroup struct {
	Key          string          `json:"key"`
	MatchType    string          `json:"match_type"` // isrc | name
	Title        string          `json:"title"`
	Artist       string          `json:"artist"`
	MixedAlbums  bool            `json:"mixed_albums"`
	ReclaimBytes int64           `json:"reclaim_bytes"`
	Files        []DuplicateFile `json:"files"`
}

type DuplicateReport struct {
	Dir          string           `json:"dir"`
	Indexed      int              `json:"indexed"`
	Groups       []DuplicateGroup `json:"groups"`
	TotalDupes   int              `json:"total_dupes"`
	ReclaimBytes int64            `json:"reclaim_bytes"`
	ReclaimMB    float64          `json:"reclaim_mb"`
	ScannedAt    string           `json:"scanned_at,omitempty"`
	Stale        bool             `json:"stale"`
}

// formatRank ranks containers by how much we want to keep them.
func formatRank(path string) int {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return 3
	case ".m4a", ".aac":
		return 2
	case ".mp3":
		return 1
	}
	return 0
}

// FindDuplicates groups indexed files that are the same recording.
func (a *App) FindDuplicates() DuplicateReport {
	library.mu.RLock()
	dir := library.dir
	scanned := library.scanned
	entries := make([]*libraryEntry, 0, len(library.entries))
	for _, e := range library.entries {
		entries = append(entries, e)
	}
	library.mu.RUnlock()

	report := DuplicateReport{Dir: dir, Indexed: len(entries), Groups: []DuplicateGroup{}}
	if !scanned.IsZero() {
		report.ScannedAt = scanned.Format(time.RFC3339)
	}
	report.Stale = scanned.IsZero()

	// Union files that share an ISRC or a strict name key.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		root := find(parent[x])
		parent[x] = root
		return root
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	byISRC := map[string]string{}
	byName := map[string]string{}
	for _, e := range entries {
		find(e.Path)
		if e.ISRC != "" {
			if other, ok := byISRC[e.ISRC]; ok {
				union(e.Path, other)
			} else {
				byISRC[e.ISRC] = e.Path
			}
		}
		if k := strictKey(e.Title, e.Artist); k != "" {
			if other, ok := byName[k]; ok {
				union(e.Path, other)
			} else {
				byName[k] = e.Path
			}
		}
	}

	byRoot := map[string][]*libraryEntry{}
	for _, e := range entries {
		root := find(e.Path)
		byRoot[root] = append(byRoot[root], e)
	}

	for _, members := range byRoot {
		if len(members) < 2 {
			continue
		}
		group := buildDuplicateGroup(dir, members)
		report.Groups = append(report.Groups, group)
		report.TotalDupes += len(group.Files) - 1
		report.ReclaimBytes += group.ReclaimBytes
	}

	// Biggest win first.
	sort.Slice(report.Groups, func(i, j int) bool {
		if report.Groups[i].ReclaimBytes != report.Groups[j].ReclaimBytes {
			return report.Groups[i].ReclaimBytes > report.Groups[j].ReclaimBytes
		}
		return report.Groups[i].Title < report.Groups[j].Title
	})
	report.ReclaimMB = float64(report.ReclaimBytes) / (1024 * 1024)
	return report
}

func buildDuplicateGroup(dir string, members []*libraryEntry) DuplicateGroup {
	// Best copy first: lossless > lossy, then largest, then oldest on disk.
	sort.Slice(members, func(i, j int) bool {
		ri, rj := formatRank(members[i].Path), formatRank(members[j].Path)
		if ri != rj {
			return ri > rj
		}
		if members[i].Size != members[j].Size {
			return members[i].Size > members[j].Size
		}
		if members[i].ModTime != members[j].ModTime {
			return members[i].ModTime < members[j].ModTime
		}
		return members[i].Path < members[j].Path
	})

	group := DuplicateGroup{
		Title:     members[0].Title,
		Artist:    members[0].Artist,
		MatchType: "name",
	}
	if members[0].ISRC != "" {
		group.Key = members[0].ISRC
		group.MatchType = "isrc"
	} else {
		group.Key = strictKey(members[0].Title, members[0].Artist)
	}

	albums := map[string]struct{}{}
	for index, e := range members {
		albums[strings.ToLower(e.Album)] = struct{}{}
		rel := e.Path
		if dir != "" {
			if r, err := filepath.Rel(dir, e.Path); err == nil {
				rel = r
			}
		}
		file := DuplicateFile{
			Path:     e.Path,
			RelPath:  rel,
			Size:     e.Size,
			SizeMB:   float64(e.Size) / (1024 * 1024),
			Format:   strings.TrimPrefix(strings.ToLower(filepath.Ext(e.Path)), "."),
			Title:    e.Title,
			Artist:   e.Artist,
			Album:    e.Album,
			ISRC:     e.ISRC,
			Modified: time.Unix(e.ModTime, 0).Format(time.RFC3339),
			Keep:     index == 0,
		}
		if index == 0 {
			file.Reason = fmt.Sprintf("best copy — %s, %.1f MB", strings.ToUpper(file.Format), file.SizeMB)
		} else {
			group.ReclaimBytes += e.Size
		}
		group.Files = append(group.Files, file)
	}
	// Same song across different albums is usually a real duplicate, but it's
	// also where a "single vs album" judgement call lives — surface it.
	group.MixedAlbums = len(albums) > 1
	return group
}

type CleanupDuplicatesRequest struct {
	Paths  []string `json:"paths"`
	Mode   string   `json:"mode"` // "trash" (default) or "delete"
	DryRun bool     `json:"dry_run"`
}

type CleanupDuplicatesResult struct {
	Mode       string   `json:"mode"`
	DryRun     bool     `json:"dry_run"`
	Removed    []string `json:"removed"`
	Failed     []string `json:"failed,omitempty"`
	FreedBytes int64    `json:"freed_bytes"`
	FreedMB    float64  `json:"freed_mb"`
	TrashDir   string   `json:"trash_dir,omitempty"`
}

// CleanupDuplicates removes the given files, defaulting to a reversible move
// into the library's trash folder.
func (a *App) CleanupDuplicates(req CleanupDuplicatesRequest) (CleanupDuplicatesResult, error) {
	result := CleanupDuplicatesResult{Mode: "trash", DryRun: req.DryRun}
	if strings.EqualFold(strings.TrimSpace(req.Mode), "delete") {
		result.Mode = "delete"
	}
	if len(req.Paths) == 0 {
		return result, nil
	}

	library.mu.RLock()
	dir := library.dir
	library.mu.RUnlock()
	if strings.TrimSpace(dir) == "" {
		dir = serverDownloadDir()
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return result, fmt.Errorf("invalid library dir: %w", err)
	}

	var trashRoot string
	if result.Mode == "trash" && !req.DryRun {
		trashRoot = filepath.Join(root, libraryTrashDirName, time.Now().Format("2006-01-02T15-04-05"))
		if err := os.MkdirAll(trashRoot, 0o755); err != nil {
			return result, fmt.Errorf("failed to create trash folder: %w", err)
		}
		result.TrashDir = trashRoot
	}

	var removed []string
	for _, p := range req.Paths {
		abs, err := filepath.Abs(strings.TrimSpace(p))
		if err != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		// Never touch anything outside the library folder — this is reachable
		// over RPC.
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: outside the library folder", p))
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if info.IsDir() {
			result.Failed = append(result.Failed, fmt.Sprintf("%s: is a directory", p))
			continue
		}

		if req.DryRun {
			removed = append(removed, abs)
			result.FreedBytes += info.Size()
			continue
		}

		if result.Mode == "delete" {
			if err := os.Remove(abs); err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p, err))
				continue
			}
		} else {
			target := filepath.Join(trashRoot, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			if err := moveFile(abs, target); err != nil {
				result.Failed = append(result.Failed, fmt.Sprintf("%s: %v", p, err))
				continue
			}
		}
		removed = append(removed, abs)
		result.FreedBytes += info.Size()
	}

	result.Removed = removed
	result.FreedMB = float64(result.FreedBytes) / (1024 * 1024)
	if !req.DryRun && len(removed) > 0 {
		forgetLibraryFiles(removed)
		fmt.Printf("[Library] %s %d duplicate file(s), freed %.1f MB\n", result.Mode+"d", len(removed), result.FreedMB)
		emitEvent("library:cleaned", result)
	}
	return result, nil
}

// moveFile renames, falling back to copy+remove when the trash folder is on a
// different filesystem than the file.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	in.Close()
	return os.Remove(src)
}

type LibraryTrashInfo struct {
	Dir    string  `json:"dir"`
	Files  int     `json:"files"`
	Bytes  int64   `json:"bytes"`
	SizeMB float64 `json:"size_mb"`
}

// GetLibraryTrash reports what's sitting in the trash folder.
func (a *App) GetLibraryTrash() LibraryTrashInfo {
	info := LibraryTrashInfo{Dir: libraryTrashPath()}
	filepath.WalkDir(info.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, statErr := d.Info(); statErr == nil {
			info.Files++
			info.Bytes += fi.Size()
		}
		return nil
	})
	info.SizeMB = float64(info.Bytes) / (1024 * 1024)
	return info
}

// EmptyLibraryTrash permanently deletes everything previously trashed.
func (a *App) EmptyLibraryTrash() (LibraryTrashInfo, error) {
	before := a.GetLibraryTrash()
	if before.Dir == "" {
		return before, fmt.Errorf("no library folder configured")
	}
	if err := os.RemoveAll(before.Dir); err != nil {
		return before, fmt.Errorf("failed to empty trash: %w", err)
	}
	fmt.Printf("[Library] Emptied trash: %d file(s), %.1f MB\n", before.Files, before.SizeMB)
	return LibraryTrashInfo{Dir: before.Dir}, nil
}

func libraryTrashPath() string {
	library.mu.RLock()
	dir := library.dir
	library.mu.RUnlock()
	if strings.TrimSpace(dir) == "" {
		dir = serverDownloadDir()
	}
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, libraryTrashDirName)
}
