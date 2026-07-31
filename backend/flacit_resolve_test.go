package backend

import (
	"sync"
	"sync/atomic"
	"testing"
)

// The resolver is shared between the prewarm goroutines and the download
// worker, so what matters is that concurrent callers for the same id collapse
// into one lookup and all see the same answer — and that nothing deadlocks.
// The network call itself is not exercised here; resolveOnce is swapped for a
// counter so the test stays offline and deterministic.

func withStubResolver(t *testing.T, fn func(spotifyID string) (string, error)) {
	t.Helper()
	deezerResolutionsMu.Lock()
	deezerResolutions = map[string]*deezerResolution{}
	deezerResolutionsMu.Unlock()

	original := resolveDeezerOnce
	resolveDeezerOnce = fn
	t.Cleanup(func() { resolveDeezerOnce = original })
}

func TestResolveDeezerURLCollapsesConcurrentCallers(t *testing.T) {
	var calls int64
	release := make(chan struct{})
	withStubResolver(t, func(string) (string, error) {
		atomic.AddInt64(&calls, 1)
		<-release // hold the lookup open so every caller piles up behind it
		return "https://deezer.com/track/1", nil
	})

	const callers = 20
	var wg sync.WaitGroup
	results := make([]string, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ResolveDeezerURL("spotify-id")
		}()
	}

	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected exactly 1 lookup for 20 concurrent callers, got %d", got)
	}
	for i, got := range results {
		if got != "https://deezer.com/track/1" {
			t.Fatalf("caller %d got %q", i, got)
		}
	}
}

func TestResolveDeezerURLCachesSuccess(t *testing.T) {
	var calls int64
	withStubResolver(t, func(string) (string, error) {
		atomic.AddInt64(&calls, 1)
		return "https://deezer.com/track/2", nil
	})

	for range 3 {
		if got := ResolveDeezerURL("cached"); got != "https://deezer.com/track/2" {
			t.Fatalf("got %q", got)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected the result to be cached after the first call, got %d lookups", got)
	}
}

// A transient song.link failure must not be cached: the queue retries failed
// downloads, and a cached empty result would make every retry silently fall
// back to the bare Spotify link instead of re-resolving.
func TestResolveDeezerURLDoesNotCacheFailure(t *testing.T) {
	var calls int64
	withStubResolver(t, func(string) (string, error) {
		if atomic.AddInt64(&calls, 1) == 1 {
			return "", errStub
		}
		return "https://deezer.com/track/3", nil
	})

	if got := ResolveDeezerURL("flaky"); got != "" {
		t.Fatalf("expected empty on failure, got %q", got)
	}
	if got := ResolveDeezerURL("flaky"); got != "https://deezer.com/track/3" {
		t.Fatalf("expected the retry to re-resolve, got %q", got)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("expected 2 lookups, got %d", got)
	}
}

// A panicking lookup must still release everyone waiting on it rather than
// leaving the download worker blocked forever.
func TestResolveDeezerURLSurvivesPanic(t *testing.T) {
	withStubResolver(t, func(string) (string, error) {
		panic("song.link exploded")
	})

	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- ""
			}
		}()
		done <- ResolveDeezerURL("panics")
	}()

	if got := <-done; got != "" {
		t.Fatalf("got %q", got)
	}
}

var errStub = stubError("song.link unavailable")

type stubError string

func (e stubError) Error() string { return string(e) }
