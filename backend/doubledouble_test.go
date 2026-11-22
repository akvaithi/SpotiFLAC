package backend

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoubleDoubleSourceIdentity(t *testing.T) {
	cases := []struct {
		name    string
		meta    AudioMetadata
		isrc    string
		wantErr bool
	}{
		{"same recording", AudioMetadata{Title: "Let It Happen", Artist: "Tame Impala"}, "", false},
		{"wrong recording", AudioMetadata{Title: "Let It Happen (Live)", Artist: "Tame Impala"}, "", true},
		{"wrong artist", AudioMetadata{Title: "Let It Happen", Artist: "Other Artist"}, "", true},
		{"missing identity", AudioMetadata{}, "", true},
		{"isrc matches", AudioMetadata{ISRC: "AUXX12345678"}, "AUXX12345678", false},
		{"isrc differs", AudioMetadata{ISRC: "AUXX12345679"}, "AUXX12345678", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceIdentity(&tc.meta, "Let It Happen", "Tame Impala", tc.isrc)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected identity result: %v", err)
			}
		})
	}
}

func TestDoubleDoubleWaitsForVerificationThenResumes(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		state := "waiting_verification"
		if calls > 1 {
			state = "ready"
		}
		fmt.Fprintf(w, `{"state":%q}`, state)
	}))
	defer server.Close()
	downloader := NewFlacItDownloader(server.URL)
	if err := downloader.awaitReady("test"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected two status checks, got %d", calls)
	}
}
