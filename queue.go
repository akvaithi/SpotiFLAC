package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// Durable download queue worker.
//
// DownloadTrack is a blocking call that a client has to hold open for the whole
// download, and the in-memory queue in backend/progress.go is display-only —
// nothing drains it. That's fine for the bundled web UI, which downloads one
// track per request and stays open, but it means a client that disconnects
// loses its work.
//
// EnqueueDownloads persists jobs instead, and this worker drains them
// server-side whether or not any client is connected. DownloadTrack is left
// exactly as it was so the existing web UI keeps working unchanged.
//
// Serial execution is a requirement, not a preference: DownloadTrack takes no
// lock and mutates package-level progress state, so concurrent calls interleave
// meaninglessly and the first one to finish flips is_downloading false for all
// of them.

const (
	queueMaxAttempts    = 3
	queueRetryBackoff   = 20 * time.Second
	queueIdlePoll       = 30 * time.Second
	queueProgressWindow = 250 * time.Millisecond
)

type downloadWorker struct {
	app  *App
	wake chan struct{}

	mu        sync.Mutex
	currentID string
	cancelled map[string]bool

	scanPending bool
}

var worker *downloadWorker

func startDownloadWorker(app *App) {
	worker = &downloadWorker{
		app:       app,
		wake:      make(chan struct{}, 1),
		cancelled: make(map[string]bool),
	}

	// Anything still marked "downloading" was interrupted by a restart; put it
	// back on the queue rather than stranding it.
	if n, err := backend.ResetInterruptedQueueRecords(); err != nil {
		log.Printf("queue: could not reset interrupted records: %v", err)
	} else if n > 0 {
		log.Printf("queue: requeued %d download(s) interrupted by restart", n)
	}

	backend.SetItemProgressListener(emitItemProgress)

	go worker.run()
}

func (w *downloadWorker) nudge() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *downloadWorker) run() {
	idle := time.NewTicker(queueIdlePoll)
	defer idle.Stop()

	for {
		rec, ok := backend.ClaimNextQueueRecord()
		if !ok {
			// Queue is drained. Tell Navidrome to pick up whatever landed.
			w.flushScan()
			select {
			case <-w.wake:
			case <-idle.C:
			}
			continue
		}
		w.process(rec)
	}
}

func (w *downloadWorker) process(rec backend.QueueRecord) {
	var req DownloadRequest
	if err := json.Unmarshal(rec.Request, &req); err != nil {
		w.finish(rec, backend.QueueFailed, "", "malformed request: "+err.Error())
		return
	}

	if req.OutputDir == "" {
		req.OutputDir = downloadDir
	}
	req.ItemID = rec.ID

	w.mu.Lock()
	w.currentID = rec.ID
	cancelled := w.cancelled[rec.ID]
	delete(w.cancelled, rec.ID)
	w.mu.Unlock()

	if cancelled {
		w.mu.Lock()
		w.currentID = ""
		w.mu.Unlock()
		w.finish(rec, backend.QueueCancelled, "", "cancelled")
		return
	}

	// Give the in-memory display queue a row too, so the bundled web UI shows
	// worker downloads alongside its own. DownloadTrack skips AddToQueue when
	// ItemID is already set, which is why we do it here.
	backend.AddToQueue(rec.ID, req.TrackName, req.ArtistName, req.AlbumName, req.SpotifyID)

	rec.Status = backend.QueueDownloading
	emitEvent("download:item", queueItemView(rec))

	resp, err := w.app.DownloadTrack(req)

	w.mu.Lock()
	w.currentID = ""
	cancelled = w.cancelled[rec.ID]
	delete(w.cancelled, rec.ID)
	w.mu.Unlock()

	switch {
	case cancelled || resp.Cancelled:
		w.finish(rec, backend.QueueCancelled, "", "cancelled")

	case err == nil && resp.Success:
		w.mu.Lock()
		w.scanPending = true
		w.mu.Unlock()
		w.finish(rec, backend.QueueCompleted, resp.File, "")

	default:
		msg := resp.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = resp.Message
		}
		if msg == "" {
			msg = "download failed"
		}
		w.retryOrFail(rec, msg)
	}
}

// retryOrFail puts a failed record back on the queue with a backoff until it
// runs out of attempts. Most failures here are transient — a Tidal ID the
// account can't stream, a rate limit — and the rematch path in the downloader
// often succeeds on a later try.
func (w *downloadWorker) retryOrFail(rec backend.QueueRecord, msg string) {
	rec.Attempts++
	if rec.Attempts >= queueMaxAttempts {
		w.finish(rec, backend.QueueFailed, "", msg)
		return
	}

	rec.Status = backend.QueueQueued
	rec.Error = msg
	rec.StartedAt = 0
	rec.NextAttemptAt = time.Now().Add(queueRetryBackoff * time.Duration(rec.Attempts)).Unix()
	if err := backend.PutQueueRecord(rec); err != nil {
		log.Printf("queue: could not persist retry for %s: %v", rec.ID, err)
	}
	emitEvent("download:item", queueItemView(rec))
}

func (w *downloadWorker) finish(rec backend.QueueRecord, status, filePath, errMsg string) {
	rec.Status = status
	rec.FilePath = filePath
	rec.Error = errMsg
	rec.FinishedAt = time.Now().Unix()
	rec.NextAttemptAt = 0
	if err := backend.PutQueueRecord(rec); err != nil {
		log.Printf("queue: could not persist %s for %s: %v", status, rec.ID, err)
	}
	emitEvent("download:item", queueItemView(rec))
}

// flushScan triggers a Navidrome rescan once the queue drains, but only if
// something actually landed since the last one.
func (w *downloadWorker) flushScan() {
	w.mu.Lock()
	pending := w.scanPending
	w.scanPending = false
	w.mu.Unlock()

	if !pending {
		return
	}
	if err := triggerNavidromeScan(); err != nil {
		log.Printf("navidrome: scan trigger failed: %v", err)
		emitEvent("navidrome:scan", map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	emitEvent("navidrome:scan", map[string]interface{}{"ok": true})
}

// emitItemProgress relays per-item byte progress to SSE clients, throttled so a
// large FLAC doesn't produce hundreds of messages.
var (
	progressThrottleMu   sync.Mutex
	progressThrottleLast = map[string]time.Time{}
)

func emitItemProgress(id string, progressMB, speedMBps float64) {
	progressThrottleMu.Lock()
	last, seen := progressThrottleLast[id]
	if seen && time.Since(last) < queueProgressWindow {
		progressThrottleMu.Unlock()
		return
	}
	progressThrottleLast[id] = time.Now()
	progressThrottleMu.Unlock()

	var totalMB float64
	for _, item := range backend.GetDownloadQueue().Queue {
		if item.ID == id {
			totalMB = item.TotalSize
			break
		}
	}

	emitEvent("download:progress", map[string]interface{}{
		"id":          id,
		"progress_mb": progressMB,
		"total_mb":    totalMB,
		"speed_mbps":  speedMBps,
	})
}

// ---------------------------------------------------------------- RPC surface

type QueueItem struct {
	ID         string  `json:"id"`
	Batch      string  `json:"batch,omitempty"`
	Status     string  `json:"status"`
	Attempts   int     `json:"attempts"`
	Error      string  `json:"error,omitempty"`
	FilePath   string  `json:"file_path,omitempty"`
	TrackName  string  `json:"track_name,omitempty"`
	ArtistName string  `json:"artist_name,omitempty"`
	AlbumName  string  `json:"album_name,omitempty"`
	SpotifyID  string  `json:"spotify_id,omitempty"`
	CoverURL   string  `json:"cover_url,omitempty"`
	ProgressMB float64 `json:"progress_mb"`
	TotalMB    float64 `json:"total_mb"`
	SpeedMBps  float64 `json:"speed_mbps"`
	EnqueuedAt int64   `json:"enqueued_at"`
	StartedAt  int64   `json:"started_at,omitempty"`
	FinishedAt int64   `json:"finished_at,omitempty"`
}

type QueueInfo struct {
	Items     []QueueItem `json:"items"`
	Queued    int         `json:"queued"`
	Active    int         `json:"active"`
	Completed int         `json:"completed"`
	Failed    int         `json:"failed"`
	Cancelled int         `json:"cancelled"`
}

func queueItemView(rec backend.QueueRecord) QueueItem {
	return QueueItem{
		ID:         rec.ID,
		Batch:      rec.Batch,
		Status:     rec.Status,
		Attempts:   rec.Attempts,
		Error:      rec.Error,
		FilePath:   rec.FilePath,
		TrackName:  rec.TrackName,
		ArtistName: rec.ArtistName,
		AlbumName:  rec.AlbumName,
		SpotifyID:  rec.SpotifyID,
		CoverURL:   rec.CoverURL,
		EnqueuedAt: rec.EnqueuedAt,
		StartedAt:  rec.StartedAt,
		FinishedAt: rec.FinishedAt,
	}
}

// EnqueueDownloads persists download jobs and returns their ids immediately.
// Unlike DownloadTrack the caller doesn't hold the connection open, and the
// work survives both the client disconnecting and the server restarting.
func (a *App) EnqueueDownloads(reqs []DownloadRequest) ([]string, error) {
	if len(reqs) == 0 {
		return []string{}, nil
	}

	batch := fmt.Sprintf("b%d", time.Now().UnixNano())
	ids := make([]string, 0, len(reqs))

	for _, req := range reqs {
		if req.OutputDir == "" {
			req.OutputDir = downloadDir
		}
		body, err := json.Marshal(req)
		if err != nil {
			return ids, fmt.Errorf("could not encode request for %q: %w", req.TrackName, err)
		}
		rec := &backend.QueueRecord{
			Batch:      batch,
			Request:    body,
			TrackName:  req.TrackName,
			ArtistName: req.ArtistName,
			AlbumName:  req.AlbumName,
			SpotifyID:  req.SpotifyID,
			CoverURL:   req.CoverURL,
		}
		if err := backend.AppendQueueRecord(rec); err != nil {
			return ids, fmt.Errorf("could not queue %q: %w", req.TrackName, err)
		}
		ids = append(ids, rec.ID)
		emitEvent("download:item", queueItemView(*rec))
	}

	if worker != nil {
		worker.nudge()
	}
	return ids, nil
}

// GetQueue returns the durable queue, with live progress folded in for whatever
// is currently downloading.
func (a *App) GetQueue() (QueueInfo, error) {
	records, err := backend.GetQueueRecords()
	if err != nil {
		return QueueInfo{}, err
	}

	live := map[string]backend.DownloadItem{}
	for _, item := range backend.GetDownloadQueue().Queue {
		live[item.ID] = item
	}

	info := QueueInfo{Items: make([]QueueItem, 0, len(records))}
	for _, rec := range records {
		view := queueItemView(rec)
		if item, ok := live[rec.ID]; ok {
			view.ProgressMB = item.Progress
			view.TotalMB = item.TotalSize
			view.SpeedMBps = item.Speed
		}
		switch rec.Status {
		case backend.QueueQueued:
			info.Queued++
		case backend.QueueDownloading:
			info.Active++
		case backend.QueueCompleted:
			info.Completed++
		case backend.QueueFailed:
			info.Failed++
		case backend.QueueCancelled:
			info.Cancelled++
		}
		info.Items = append(info.Items, view)
	}
	return info, nil
}

// RetryQueueItem puts a failed or cancelled item back on the queue.
func (a *App) RetryQueueItem(id string) error {
	rec, ok := backend.GetQueueRecord(id)
	if !ok {
		return fmt.Errorf("no such queue item: %s", id)
	}
	if rec.Status == backend.QueueQueued || rec.Status == backend.QueueDownloading {
		return nil
	}

	rec.Status = backend.QueueQueued
	rec.Attempts = 0
	rec.Error = ""
	rec.StartedAt = 0
	rec.FinishedAt = 0
	rec.NextAttemptAt = 0
	if err := backend.PutQueueRecord(rec); err != nil {
		return err
	}
	emitEvent("download:item", queueItemView(rec))

	if worker != nil {
		worker.nudge()
	}
	return nil
}

// CancelQueueItem drops a queued item, or stops it if it's the one currently
// downloading.
//
// Stopping the active download cancels *all* in-flight downloads, because the
// backend's cancellation scope is shared. The worker is serial so that's
// normally just this item — but a concurrent DownloadTrack from the web UI
// would be caught too.
func (a *App) CancelQueueItem(id string) error {
	rec, ok := backend.GetQueueRecord(id)
	if !ok {
		return fmt.Errorf("no such queue item: %s", id)
	}
	if rec.IsFinished() {
		return nil
	}

	isCurrent := false
	if worker != nil {
		worker.mu.Lock()
		isCurrent = worker.currentID == id
		worker.cancelled[id] = true
		worker.mu.Unlock()
	}

	if isCurrent {
		backend.ForceStopActiveDownloads()
		return nil
	}

	rec.Status = backend.QueueCancelled
	rec.FinishedAt = time.Now().Unix()
	rec.Error = "cancelled"
	if err := backend.PutQueueRecord(rec); err != nil {
		return err
	}
	emitEvent("download:item", queueItemView(rec))
	return nil
}

// ClearQueue removes finished records. Anything queued or downloading is left
// alone — use CancelQueueItem for those.
func (a *App) ClearQueue() (int, error) {
	return backend.DeleteFinishedQueueRecords(0)
}
