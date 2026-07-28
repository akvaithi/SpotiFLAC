package backend

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Durable download queue.
//
// The original queue (progress.go) is an in-memory slice that only ever holds
// display rows: nothing persists it and nothing drains it, so a restart loses
// everything and AddToQueue on its own downloads nothing. This store is the
// persistent half — it lives in the same bolt file as the download history, and
// the worker in queue.go is what actually drains it.
//
// Records carry the DownloadRequest as opaque JSON so this package doesn't need
// to know the main package's request type.

const queueBucket = "DownloadQueueV1"

const (
	QueueQueued      = "queued"
	QueueDownloading = "downloading"
	QueueCompleted   = "completed"
	QueueFailed      = "failed"
	QueueCancelled   = "cancelled"
)

type QueueRecord struct {
	ID      string          `json:"id"`
	Batch   string          `json:"batch,omitempty"`
	Request json.RawMessage `json:"request"`

	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
	FilePath string `json:"file_path,omitempty"`

	// Denormalized for display, so listing the queue never has to unmarshal
	// every request body.
	TrackName  string `json:"track_name,omitempty"`
	ArtistName string `json:"artist_name,omitempty"`
	AlbumName  string `json:"album_name,omitempty"`
	SpotifyID  string `json:"spotify_id,omitempty"`
	CoverURL   string `json:"cover_url,omitempty"`

	EnqueuedAt    int64 `json:"enqueued_at"`
	StartedAt     int64 `json:"started_at,omitempty"`
	FinishedAt    int64 `json:"finished_at,omitempty"`
	NextAttemptAt int64 `json:"next_attempt_at,omitempty"`
}

// IsFinished reports whether a record has reached a terminal state.
func (r QueueRecord) IsFinished() bool {
	return r.Status == QueueCompleted || r.Status == QueueFailed || r.Status == QueueCancelled
}

func ensureQueueDB() error {
	if historyDB == nil {
		if err := InitHistoryDB("SpotiFLAC"); err != nil {
			return err
		}
	}
	return historyDB.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(queueBucket))
		return err
	})
}

// AppendQueueRecord assigns an ID and persists the record. Keys are zero-padded
// so bolt's lexicographic cursor order is also FIFO order.
func AppendQueueRecord(rec *QueueRecord) error {
	if err := ensureQueueDB(); err != nil {
		return err
	}
	return historyDB.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(queueBucket))
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		rec.ID = fmt.Sprintf("q%016d", seq)
		rec.Status = QueueQueued
		rec.EnqueuedAt = time.Now().Unix()

		buf, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(rec.ID), buf)
	})
}

func PutQueueRecord(rec QueueRecord) error {
	if err := ensureQueueDB(); err != nil {
		return err
	}
	return historyDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		buf, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(rec.ID), buf)
	})
}

func GetQueueRecord(id string) (QueueRecord, bool) {
	var rec QueueRecord
	found := false
	if err := ensureQueueDB(); err != nil {
		return rec, false
	}
	_ = historyDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(id))
		if v == nil {
			return nil
		}
		if json.Unmarshal(v, &rec) == nil {
			found = true
		}
		return nil
	})
	return rec, found
}

// GetQueueRecords returns every record in enqueue order.
func GetQueueRecords() ([]QueueRecord, error) {
	if err := ensureQueueDB(); err != nil {
		return nil, err
	}
	var records []QueueRecord
	err := historyDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec QueueRecord
			if json.Unmarshal(v, &rec) == nil {
				records = append(records, rec)
			}
		}
		return nil
	})
	sort.SliceStable(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, err
}

// ClaimNextQueueRecord atomically moves the oldest eligible queued record to
// "downloading" and returns it. Doing the read and the write in one bolt
// transaction is what makes it safe to call from more than one place.
func ClaimNextQueueRecord() (QueueRecord, bool) {
	var claimed QueueRecord
	ok := false
	if err := ensureQueueDB(); err != nil {
		return claimed, false
	}
	now := time.Now().Unix()
	_ = historyDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec QueueRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			if rec.Status != QueueQueued {
				continue
			}
			// Held back by retry backoff.
			if rec.NextAttemptAt > now {
				continue
			}
			rec.Status = QueueDownloading
			rec.StartedAt = now
			rec.Error = ""
			buf, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := b.Put(k, buf); err != nil {
				return err
			}
			claimed = rec
			ok = true
			return nil
		}
		return nil
	})
	return claimed, ok
}

// ResetInterruptedQueueRecords puts anything left mid-flight by a crash or a
// container restart back on the queue. Called once at startup.
func ResetInterruptedQueueRecords() (int, error) {
	if err := ensureQueueDB(); err != nil {
		return 0, err
	}
	count := 0
	err := historyDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec QueueRecord
			if json.Unmarshal(v, &rec) != nil || rec.Status != QueueDownloading {
				continue
			}
			rec.Status = QueueQueued
			rec.StartedAt = 0
			rec.NextAttemptAt = 0
			buf, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := b.Put(k, buf); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// DeleteFinishedQueueRecords drops terminal records, optionally keeping the
// most recent `keep` of them so the UI can still show recent history.
func DeleteFinishedQueueRecords(keep int) (int, error) {
	records, err := GetQueueRecords()
	if err != nil {
		return 0, err
	}
	var finished []QueueRecord
	for _, rec := range records {
		if rec.IsFinished() {
			finished = append(finished, rec)
		}
	}
	if keep > 0 && len(finished) > keep {
		finished = finished[:len(finished)-keep]
	} else if keep > 0 {
		return 0, nil
	}

	removed := 0
	err = historyDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(queueBucket))
		if b == nil {
			return nil
		}
		for _, rec := range finished {
			if err := b.Delete([]byte(rec.ID)); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}
