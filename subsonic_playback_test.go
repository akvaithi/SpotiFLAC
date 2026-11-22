package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/akvaithi/SpotiFLAC/backend"
)

func TestIndexedDownloadUsesExactPathAndCallerCredentials(t *testing.T) {
	previous := downloadDir
	downloadDir = t.TempDir()
	t.Cleanup(func() { downloadDir = previous })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/search3.view" || r.URL.Query().Get("u") != "listener" ||
			r.URL.Query().Get("apiKey") != "example" || r.URL.Query().Get("query") != "Song" {
			http.Error(w, "unexpected search", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"subsonic-response":{"searchResult3":{"song":[{"id":"other","path":"Artist/Live/Song.flac"},{"id":"exact","path":"Artist/Album/Song.flac"}]}}}`)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	facade := &subsonicFacade{target: target, client: upstream.Client()}
	request := httptest.NewRequest("GET", "/rest/stream.view?u=listener&apiKey=example", nil)
	record := backend.QueueRecord{TrackName: "Song", FilePath: filepath.Join(downloadDir, "Artist", "Album", "Song.flac")}
	if id := facade.indexedDownload(request, record); id != "exact" {
		t.Fatalf("got ID %q, want exact", id)
	}
	record.FilePath = filepath.Join(downloadDir, "..", "Artist", "Album", "Song.flac")
	if id := facade.indexedDownload(request, record); id != "" {
		t.Fatalf("got ID %q for a file outside the music directory", id)
	}
}
