package backend

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	isrcCacheDBFile = "isrc_cache.db"
	isrcCacheBucket = "SpotifyTrackISRC"
)

type isrcCacheEntry struct {
	TrackID   string `json:"track_id"`
	ISRC      string `json:"isrc"`
	UpdatedAt int64  `json:"updated_at"`
}

var (
	isrcCacheDB   *bolt.DB
	isrcCacheDBMu sync.Mutex
)

func InitISRCCacheDB() error {
	isrcCacheDBMu.Lock()
	defer isrcCacheDBMu.Unlock()

	if isrcCacheDB != nil {
		return nil
	}

	appDir, err := EnsureAppDir()
	if err != nil {
		return err
	}

	dbPath := filepath.Join(appDir, isrcCacheDBFile)
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return err
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(isrcCacheBucket))
		return err
	}); err != nil {
		db.Close()
		return err
	}

	isrcCacheDB = db
	return nil
}

func CloseISRCCacheDB() {
	isrcCacheDBMu.Lock()
	defer isrcCacheDBMu.Unlock()

	if isrcCacheDB != nil {
		_ = isrcCacheDB.Close()
		isrcCacheDB = nil
	}
}

func GetCachedISRC(trackID string) (string, error) {
	normalizedTrackID := strings.TrimSpace(trackID)
	if normalizedTrackID == "" {
		return "", nil
	}

	if err := InitISRCCacheDB(); err != nil {
		return "", err
	}

	var cachedISRC string
	err := isrcCacheDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(isrcCacheBucket))
		if bucket == nil {
			return nil
		}

		value := bucket.Get([]byte(normalizedTrackID))
		if len(value) == 0 {
			return nil
		}

		var entry isrcCacheEntry
		if err := json.Unmarshal(value, &entry); err != nil {
			return err
		}

		cachedISRC = strings.ToUpper(strings.TrimSpace(entry.ISRC))
		return nil
	})
	if err != nil {
		return "", err
	}

	return cachedISRC, nil
}

// GetCachedISRCsOnly looks several track ids up in one read transaction and
// returns only the ones already known. Never fetches: this exists for the
// matching paths, which run inside a search budget and must not make a network
// call per row — a miss just falls back to matching by name.
func GetCachedISRCsOnly(trackIDs []string) map[string]string {
	found := map[string]string{}
	if len(trackIDs) == 0 {
		return found
	}
	if err := InitISRCCacheDB(); err != nil {
		return found
	}

	_ = isrcCacheDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(isrcCacheBucket))
		if bucket == nil {
			return nil
		}
		for _, id := range trackIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			value := bucket.Get([]byte(id))
			if len(value) == 0 {
				continue
			}
			var entry isrcCacheEntry
			if json.Unmarshal(value, &entry) != nil {
				continue
			}
			if isrc := strings.ToUpper(strings.TrimSpace(entry.ISRC)); isrc != "" {
				found[id] = isrc
			}
		}
		return nil
	})
	return found
}

func PutCachedISRC(trackID string, isrc string) error {
	normalizedTrackID := strings.TrimSpace(trackID)
	normalizedISRC := strings.ToUpper(strings.TrimSpace(isrc))
	if normalizedTrackID == "" || normalizedISRC == "" {
		return nil
	}

	if err := InitISRCCacheDB(); err != nil {
		return err
	}

	entry := isrcCacheEntry{
		TrackID:   normalizedTrackID,
		ISRC:      normalizedISRC,
		UpdatedAt: time.Now().Unix(),
	}

	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("failed to encode ISRC cache entry: %w", err)
	}

	return isrcCacheDB.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(isrcCacheBucket))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(normalizedTrackID), payload)
	})
}
