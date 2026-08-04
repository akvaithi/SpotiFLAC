package backend

import (
	"context"
	"fmt"
	"strings"
)

// Translating a Spotify track to the Deezer recording the bot can actually
// fetch.
//
// Order matters, and so does what each step is willing to accept:
//
//  1. ISRC — identifies the recording. A hit is not a guess.
//  2. A precise `artist:"X" track:"Y"` search, accepted only after the
//     candidate is verified against what was asked for.
//
// Step 2 is why this file is careful. Picking a track by name is the same
// *class* of thing the "no bot-side fuzzy search" boundary forbids, and the
// rationale — a wrong match files a remix or live cut under the right track's
// name — applies exactly. The difference is inspectability: the bot resolves a
// query inside its own black box and hands back a file, whereas a Deezer
// candidate is a structured record that can be checked on title, artist and
// duration *before* anything is downloaded, and then fetched by exact id.
// Loosen the verification below and that distinction disappears.

// deezerDurationSlackSeconds is how far a candidate's runtime may differ.
//
// Not zero: Deezer and Spotify disagree by a second or two on the same
// recording through rounding and differing masters. Not large either — a live
// cut, an extended mix or a radio edit of the same song is the failure this
// catches, and those differ by far more.
const deezerDurationSlackSeconds = 2

type DeezerLookup struct {
	SpotifyID  string
	ISRC       string
	Title      string
	Artists    string
	DurationMS int
}

type DeezerMatch struct {
	Track  DeezerTrack
	Method string // "isrc" | "search"
}

// FindDeezerTrack resolves a track to its Deezer recording, or reports that
// Deezer does not have it. The error is the useful part for callers that want to
// tell the user *before* a four-minute bot timeout does.
func FindDeezerTrack(ctx context.Context, req DeezerLookup) (*DeezerMatch, error) {
	isrc := strings.ToUpper(strings.TrimSpace(req.ISRC))
	if isrc == "" && strings.TrimSpace(req.SpotifyID) != "" {
		// Cache only here; resolving an ISRC over the network is the caller's
		// choice to make, since this runs in latency-sensitive paths.
		if cached := GetCachedISRCsOnly([]string{strings.TrimSpace(req.SpotifyID)}); len(cached) > 0 {
			isrc = cached[strings.TrimSpace(req.SpotifyID)]
		}
	}

	if isrc != "" {
		if track, err := GetDeezerTrackByISRC(ctx, isrc); err == nil && track.ID > 0 {
			return &DeezerMatch{Track: *track, Method: "isrc"}, nil
		}
	}

	if strings.TrimSpace(req.Title) == "" {
		return nil, fmt.Errorf("not on Deezer: no ISRC match and no title to search with")
	}

	candidates, err := SearchDeezerTracksPrecise(ctx, FirstCredit(req.Artists), req.Title, 10)
	if err != nil {
		// A withheld-results answer must not read as "Deezer doesn't have it".
		if err == ErrDeezerSearchUnavailable {
			return nil, err
		}
		candidates = nil
	}
	if match := pickVerifiedDeezerTrack(candidates, req); match != nil {
		return &DeezerMatch{Track: *match, Method: "search"}, nil
	}

	// A plain query as a second shape — the field syntax misses when the artist
	// is credited differently. Same verification, so this loosens the *query*,
	// never what counts as a match.
	plain, err := SearchDeezerTracks(ctx, strings.TrimSpace(req.Title+" "+FirstCredit(req.Artists)), 15)
	if err != nil {
		if err == ErrDeezerSearchUnavailable {
			return nil, err
		}
		plain = nil
	}
	if match := pickVerifiedDeezerTrack(plain, req); match != nil {
		return &DeezerMatch{Track: *match, Method: "search"}, nil
	}

	return nil, fmt.Errorf("not on Deezer: %q by %q", req.Title, req.Artists)
}

// pickVerifiedDeezerTrack returns the best candidate that passes every check, or
// nil. All three conditions must hold — this is the guard that keeps a search
// fallback from filing the wrong recording under the right name.
func pickVerifiedDeezerTrack(candidates []DeezerTrack, req DeezerLookup) *DeezerTrack {
	wantTitle := NormalizeTitle(req.Title)
	if wantTitle == "" {
		return nil
	}
	wantSeconds := req.DurationMS / 1000

	var best *DeezerTrack
	for i := range candidates {
		c := candidates[i]
		if c.ID <= 0 {
			continue
		}
		if NormalizeTitle(c.FullTitle()) != wantTitle {
			continue
		}
		if req.Artists != "" && !ArtistsOverlap(c.CreditedArtists(", "), req.Artists) {
			continue
		}
		// Duration is the check that separates the studio take from the live
		// cut once title and artist already agree. Skipped only when the caller
		// genuinely doesn't know it.
		if wantSeconds > 0 && c.Duration > 0 {
			if diff := c.Duration - wantSeconds; diff > deezerDurationSlackSeconds || diff < -deezerDurationSlackSeconds {
				continue
			}
		}
		// Deezer's own popularity, so a compilation re-release doesn't win over
		// the canonical issue.
		if best == nil || c.Rank > best.Rank {
			candidate := c
			best = &candidate
		}
	}
	return best
}

// DeezerAvailability is the paste-time answer: does the source actually have
// this, before it is queued and waited on.
type DeezerAvailability struct {
	Index     int    `json:"index"`
	Available bool   `json:"available"`
	DeezerID  int64  `json:"deezer_id,omitempty"`
	Method    string `json:"method,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Unknown marks "could not tell" as distinct from "not available" — a
	// blocked or failing API must never be rendered as an empty catalog.
	Unknown bool `json:"unknown,omitempty"`
}
