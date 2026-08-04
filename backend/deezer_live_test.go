package backend

import (
	"context"
	"os"
	"testing"
)

// Opt-in live check: DEEZER_LIVE=1 go test ./backend -run Live
func TestLiveDeezerTranslation(t *testing.T) {
	if os.Getenv("DEEZER_LIVE") == "" {
		t.Skip("set DEEZER_LIVE=1")
	}
	ctx := context.Background()

	// The track that failed: Deezer genuinely lacks this release.
	if _, err := FindDeezerTrack(ctx, DeezerLookup{
		Title: "Chinna Chinna Asai", Artists: "A.R. Rahman, Rajasri", DurationMS: 0,
	}); err != nil {
		t.Logf("MISSING as expected: %v", err)
	} else {
		t.Logf("unexpectedly found a match")
	}

	// The copy that did download.
	m, err := FindDeezerTrack(ctx, DeezerLookup{
		ISRC: "IND292201383", Title: "Chinna Chinna Asai", Artists: "Minmini, A.R. Rahman, Vairamuthu",
	})
	if err != nil {
		t.Fatalf("known-good track not found: %v", err)
	}
	t.Logf("FOUND via %s: id=%d %q dur=%ds credit=%q cover=%s",
		m.Method, m.Track.ID, m.Track.FullTitle(), m.Track.Duration, m.Track.CreditedArtists(" • "), m.Track.CoverURL())

	// Search-only path (no ISRC), must verify and accept.
	m2, err := FindDeezerTrack(ctx, DeezerLookup{
		Title: `Vaa Vaathi (From "Vaathi")`, Artists: "G. V. Prakash Kumar, Shweta Mohan",
	})
	if err != nil {
		t.Fatalf("search path failed: %v", err)
	}
	t.Logf("SEARCH via %s: id=%d %q credit=%q", m2.Method, m2.Track.ID, m2.Track.FullTitle(), m2.Track.CreditedArtists(" • "))
}
