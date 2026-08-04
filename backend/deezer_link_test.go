package backend

import "testing"

// Regression: a resolver answered "Chinna Chinna Asai" with
// https://deezer.com/us/artist/491 — A.R. Rahman's *artist* page. It was
// accepted as a track link and sent to the bot, which cannot act on it, so the
// fetch waited out its timeout and retried. Worse, because a Deezer URL had
// "already been found", the ISRC fallback that resolves the track correctly was
// never reached.
func TestNormalizeDeezerTrackURLRejectsNonTracks(t *testing.T) {
	tracks := map[string]string{
		"https://www.deezer.com/track/2438310305":   "https://www.deezer.com/track/2438310305",
		"https://deezer.com/us/track/2438310305":    "https://www.deezer.com/track/2438310305",
		"https://www.deezer.com/en/track/123?utm=x": "https://www.deezer.com/track/123",
		"https://www.deezer.com/track/456/":         "https://www.deezer.com/track/456",
	}
	for in, want := range tracks {
		if got := normalizeDeezerTrackURL(in); got != want {
			t.Errorf("normalizeDeezerTrackURL(%q) = %q, want %q", in, got, want)
		}
	}

	// Empty means "no Deezer link", never the input echoed back.
	nonTracks := []string{
		"https://deezer.com/us/artist/491",
		"https://www.deezer.com/album/12345",
		"https://www.deezer.com/us/playlist/999",
		"https://www.deezer.com/artist/491/top_track",
		"",
	}
	for _, in := range nonTracks {
		if got := normalizeDeezerTrackURL(in); got != "" {
			t.Errorf("normalizeDeezerTrackURL(%q) = %q, want \"\" (a non-track link must not pass as a track)", in, got)
		}
	}
}

func TestIsDeezerTrackURL(t *testing.T) {
	yes := []string{
		"https://www.deezer.com/track/2438310305",
		"https://deezer.com/us/track/2438310305",
	}
	for _, u := range yes {
		if !isDeezerTrackURL(u) {
			t.Errorf("isDeezerTrackURL(%q) = false, want true", u)
		}
	}

	no := []string{
		"https://deezer.com/us/artist/491",
		"https://www.deezer.com/album/12345",
		"https://open.spotify.com/track/abc",
		"",
	}
	for _, u := range no {
		if isDeezerTrackURL(u) {
			t.Errorf("isDeezerTrackURL(%q) = true, want false", u)
		}
	}
}

// The last check before the bot sees a link: a non-track Deezer URL must fall
// back to the Spotify link, which the bot does understand.
func TestResolveTrackURLFallsBackWhenResolverReturnsNonTrack(t *testing.T) {
	f := &FlacItDownloader{}

	withStubResolver(t, func(string) (string, error) {
		return "https://deezer.com/us/artist/491", nil
	})
	if got := f.resolveTrackURL("abc123"); got != "https://open.spotify.com/track/abc123" {
		t.Errorf("an artist link was passed to the bot as a track: %q", got)
	}

	withStubResolver(t, func(string) (string, error) {
		return "https://www.deezer.com/track/2438310305", nil
	})
	if got := f.resolveTrackURL("abc123"); got != "https://www.deezer.com/track/2438310305" {
		t.Errorf("a good track link was not used: %q", got)
	}
}
