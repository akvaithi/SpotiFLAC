package main

import (
	"context"
	"sync"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

// "Is this actually obtainable?" answered at paste time instead of four minutes
// later as a bot timeout.
//
// Spotify is the catalog and Deezer is the source, and the two are not the same
// set. Until now the difference surfaced only after a track had been queued,
// waited on for 270s, retried three times and failed — with nothing to
// distinguish "Deezer doesn't have this" from "something broke". Checking the
// catalog up front is cheap and turns that into a label on the row.

const (
	// Deezer rate limits around 50 requests / 5s per IP. A playlist can be
	// hundreds of tracks, so the batch is bounded and mildly parallel.
	availabilityConcurrency = 4
	availabilityPerTrack    = 12 * time.Second
	availabilityBudget      = 90 * time.Second
)

type DeezerAvailabilityInput struct {
	Index      int    `json:"index"`
	SpotifyID  string `json:"spotify_id,omitempty"`
	ISRC       string `json:"isrc,omitempty"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMS int    `json:"duration_ms,omitempty"`
}

// CheckDeezerAvailability reports, per track, whether Deezer carries it.
//
// Three outcomes, and the third matters as much as the others: available,
// not available, and **unknown**. A blocked or failing API must never be
// rendered as "not available" — that would tell the user their music doesn't
// exist because of a network condition. Unknown rows are left alone by the UI.
func (a *App) CheckDeezerAvailability(items []DeezerAvailabilityInput) []backend.DeezerAvailability {
	out := make([]backend.DeezerAvailability, len(items))
	if len(items) == 0 {
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), availabilityBudget)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, availabilityConcurrency)

	for i, item := range items {
		wg.Add(1)
		go func(i int, item DeezerAvailabilityInput) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := backend.DeezerAvailability{Index: item.Index}
			if ctx.Err() != nil {
				// Ran out of budget rather than ran out of catalog.
				res.Unknown = true
				res.Reason = "timed out before checking"
				out[i] = res
				return
			}

			lookupCtx, lookupCancel := context.WithTimeout(ctx, availabilityPerTrack)
			defer lookupCancel()

			match, err := backend.FindDeezerTrack(lookupCtx, backend.DeezerLookup{
				SpotifyID:  item.SpotifyID,
				ISRC:       item.ISRC,
				Title:      item.Title,
				Artists:    item.Artist,
				DurationMS: item.DurationMS,
			})
			switch {
			case err == nil && match != nil:
				res.Available = true
				res.DeezerID = match.Track.ID
				res.Method = match.Method
			case err == backend.ErrDeezerSearchUnavailable || lookupCtx.Err() != nil:
				res.Unknown = true
				res.Reason = "could not reach Deezer"
			default:
				res.Reason = "not in Deezer's catalog"
			}
			out[i] = res
		}(i, item)
	}

	wg.Wait()
	return out
}
