package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

var playlistURLPattern = regexp.MustCompile(`^https://open\.spotify\.com/(?:intl-[a-zA-Z-]+/)?playlist/([A-Za-z0-9]{22})/?(?:\?[^\s#]*)?(?:#[^\s]*)?$`)
var spotifyTrackID = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
var backfillMu sync.Mutex

const backfillBatchLimit = 200

func playlistBackfillHandler(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := sha256.Sum256([]byte("akvcloud-playlist-backfill-v1:" + apiToken))
		if apiToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+hex.EncodeToString(token[:]))) != 1 {
			writeErr(w, http.StatusUnauthorized, "Invalid backfill credentials")
			return
		}
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "Use POST")
			return
		}
		var input struct {
			URL   string `json:"url"`
			Queue bool   `json:"queue"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input) != nil {
			writeErr(w, http.StatusBadRequest, "Invalid request")
			return
		}
		match := playlistURLPattern.FindStringSubmatch(strings.TrimSpace(input.URL))
		if match == nil {
			writeErr(w, http.StatusBadRequest, "Paste an open.spotify.com playlist link")
			return
		}
		if !backfillMu.TryLock() {
			writeErr(w, http.StatusConflict, "Another playlist is being checked. Try again shortly.")
			return
		}
		defer backfillMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		result, err := app.preparePlaylistBackfill(ctx, match[1], input.Queue)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

type playlistBackfillResult struct {
	Name       string `json:"name"`
	Tracks     int    `json:"tracks"`
	Owned      int    `json:"owned"`
	Pending    int    `json:"pending"`
	Skipped    int    `json:"skipped"`
	Missing    int    `json:"missing"`
	Queued     int    `json:"queued"`
	BatchLimit int    `json:"batch_limit"`
	Error      string `json:"error,omitempty"`
}

func (a *App) preparePlaylistBackfill(ctx context.Context, id string, queue bool) (playlistBackfillResult, error) {
	var out playlistBackfillResult
	data, err := backend.GetFilteredSpotifyData(ctx, "https://open.spotify.com/playlist/"+id, false, time.Second, ", ", nil)
	if err != nil {
		return out, fmt.Errorf("Spotify could not load this playlist. Check that the link opens publicly and try again.")
	}
	playlist, ok := data.(backend.PlaylistResponsePayload)
	if !ok || len(playlist.TrackList) == 0 {
		return out, fmt.Errorf("Spotify returned no tracks. Use a public playlist link.")
	}
	out.Name = playlist.PlaylistInfo.Owner.Name
	out.Tracks = len(playlist.TrackList)
	out.BatchLimit = backfillBatchLimit
	matches := make([]LibMatchInput, 0, len(playlist.TrackList))
	for i, t := range playlist.TrackList {
		matches = append(matches, LibMatchInput{Index: i, SpotifyID: t.SpotifyID, Title: t.Name, Artist: t.Artists})
	}
	owned := a.MatchLibrary(matches)
	records, err := backend.GetQueueRecords()
	if err != nil {
		return out, fmt.Errorf("Could not read the download queue")
	}
	active := make(map[string]bool)
	for _, rec := range records {
		if rec.Status == backend.QueueQueued || rec.Status == backend.QueueDownloading {
			active[rec.SpotifyID] = true
		}
	}
	seen := make(map[string]bool)
	requests := make([]DownloadRequest, 0, backfillBatchLimit)
	for i, t := range playlist.TrackList {
		if !spotifyTrackID.MatchString(t.SpotifyID) || t.Name == "" || t.Artists == "" || seen[t.SpotifyID] {
			out.Skipped++
			continue
		}
		seen[t.SpotifyID] = true
		if owned[i].InLibrary {
			out.Owned++
			continue
		}
		if active[t.SpotifyID] {
			out.Pending++
			continue
		}
		out.Missing++
		if len(requests) >= backfillBatchLimit {
			continue
		}
		requests = append(requests, DownloadRequest{Service: "doubledouble", SpotifyID: t.SpotifyID, TrackName: t.Name, ArtistName: t.Artists, AlbumName: t.AlbumName, AlbumArtist: t.AlbumArtist, CoverURL: strings.SplitN(strings.TrimSpace(t.Images), " ", 2)[0], Duration: t.DurationMS / 1000})
	}
	if queue && len(requests) > 0 {
		if ctx.Err() != nil {
			return out, fmt.Errorf("Playlist timed out before queueing")
		}
		ids, err := a.EnqueueDownloads(requests)
		out.Queued = len(ids)
		if err != nil {
			out.Error = "The download queue could not save every track. Preview the playlist again before retrying."
		}
	}
	return out, nil
}
