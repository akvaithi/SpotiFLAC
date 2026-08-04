package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FlacItDownloader is SpotiFLAC's only download source: it talks to the
// flacit-gateway sidecar, which drives @deezload2bot over Telegram MTProto to
// fetch a Deezer-sourced lossless FLAC and pulls it down with 16 parallel
// connections. The Tidal/Qobuz/Amazon engines this replaced are gone — no
// fallback by design, see CLAUDE.md.
type FlacItDownloader struct {
	gatewayURL  string
	client      *http.Client
	SourceURL   string
	SourceLabel string
}

func NewFlacItDownloader(gatewayURL string) *FlacItDownloader {
	return &FlacItDownloader{
		gatewayURL: strings.TrimRight(strings.TrimSpace(gatewayURL), "/"),
		client:     &http.Client{Timeout: 3 * time.Minute},
	}
}

type flacItJobView struct {
	State      string  `json:"state"`
	Filename   string  `json:"filename"`
	Size       int64   `json:"size"`
	Downloaded float64 `json:"downloaded"`
	SpeedMBps  float64 `json:"speed_mbps"`
	Error      string  `json:"error"`
}

const (
	flacItPollInterval = 500 * time.Millisecond
	flacItJobDeadline  = 5 * time.Minute
)

// Download resolves spotifyID to a Deezer (preferred) or Spotify track link,
// fetches the FLAC through the gateway's job API, writes it to the expected
// path, and tags it. filenameFormat is assumed already token-substituted by
// the caller — app.go's DownloadTrack runs ApplyArtistFilenameTokens /
// ApplyExtraFilenameTokens / ApplyFilenameContextTokens before dispatch, same
// as it did for the downloaders this replaces.
func (f *FlacItDownloader) Download(
	spotifyID, outputDir, filenameFormat string,
	includeTrackNumber bool, position int,
	trackName, artistName, albumName, albumArtist, releaseDate string,
	useAlbumTrackNumber bool,
	coverURL string, embedMaxQualityCover bool,
	spotifyTrackNumber, spotifyDiscNumber, spotifyTotalTracks, spotifyTotalDiscs int,
	copyrightText, publisher, composer, metadataSeparator, isrcOverride, spotifyURL string,
	useFirstArtistOnly, useSingleGenre, embedGenre bool,
) (string, error) {
	if f.gatewayURL == "" {
		return "", fmt.Errorf("no Telegram gateway configured (FLACIT_GATEWAY_URL / flacitApiUrl)")
	}
	if outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	isrc := strings.TrimSpace(isrcOverride)

	metaChan := make(chan Metadata, 1)
	if embedGenre && isrc != "" {
		go func() {
			if ShouldSkipMusicBrainzMetadataFetch() {
				fmt.Println("Skipping MusicBrainz metadata fetch because status check is offline.")
				metaChan <- Metadata{}
				return
			}
			fmt.Println("Fetching MusicBrainz metadata...")
			if fetchedMeta, err := FetchMusicBrainzMetadata(isrc, trackName, artistName, albumName, useSingleGenre, embedGenre); err == nil {
				metaChan <- fetchedMeta
			} else {
				fmt.Printf("Warning: failed to fetch MusicBrainz metadata: %v\n", err)
				metaChan <- Metadata{}
			}
		}()
	} else {
		close(metaChan)
	}

	filenameArtist := artistName
	filenameAlbumArtist := albumArtist
	if useFirstArtistOnly {
		filenameArtist = GetFirstArtist(artistName)
		filenameAlbumArtist = GetFirstArtist(albumArtist)
	}
	expectedFilename := BuildExpectedFilename(trackName, filenameArtist, albumName, filenameAlbumArtist, releaseDate, filenameFormat, "", "", includeTrackNumber, position, spotifyDiscNumber, useAlbumTrackNumber, isrc)
	if !strings.EqualFold(filepath.Ext(expectedFilename), ".flac") {
		expectedFilename = strings.TrimSuffix(expectedFilename, filepath.Ext(expectedFilename)) + ".flac"
	}
	expectedPath := filepath.Join(outputDir, expectedFilename)
	expectedPath, alreadyExists := ResolveOutputPathForDownload(expectedPath, GetRedownloadWithSuffixSetting())
	if alreadyExists {
		fmt.Printf("File already exists: %s (%.2f MB)\n", expectedPath, float64(mustFileSize(expectedPath))/(1024*1024))
		return "EXISTS:" + expectedPath, nil
	}

	// Both of these are pure metadata lookups that don't need the FLAC file to
	// exist yet, and the gateway wait below is dominated by tens of seconds of
	// bot round-trip (see awaitReady) during which the Go process is otherwise
	// idle. Kicking them off now instead of after downloadFile hides their
	// latency entirely behind that wait rather than adding to the end of it —
	// the same trick already used for the MusicBrainz fetch above.
	identChan := make(chan struct {
		identifiers SpotifyTrackIdentifiers
		err         error
	}, 1)
	go func() {
		identifiers, err := GetSpotifyTrackIdentifiersDirect(spotifyURL)
		identChan <- struct {
			identifiers SpotifyTrackIdentifiers
			err         error
		}{identifiers, err}
	}()

	coverChan := make(chan string, 1)
	if coverURL != "" {
		go func() {
			path := expectedPath + ".cover.jpg"
			coverClient := NewCoverClient()
			if err := coverClient.DownloadCoverToPath(coverURL, path, embedMaxQualityCover); err != nil {
				fmt.Printf("Warning: failed to download Spotify cover: %v\n", err)
				coverChan <- ""
				return
			}
			coverChan <- path
		}()
	} else {
		coverChan <- ""
	}

	// The cover is a temp file that must be removed on *every* path, including
	// the gateway failures below that return before it's ever read. Waiting on
	// the channel first is what makes that safe: the goroutine may still be
	// mid-write, and removing the path before it finishes would leave the file.
	coverPath := ""
	coverReceived := false
	defer func() {
		if !coverReceived {
			coverPath = <-coverChan
		}
		if coverPath != "" {
			os.Remove(coverPath)
		}
	}()

	trackURL := f.resolveTrackURL(spotifyID)
	f.SourceURL = trackURL
	f.SourceLabel = fmt.Sprintf("%s - %s", artistName, trackName)

	fmt.Printf("Fetching via Telegram gateway: %s\n", trackURL)
	jobID, err := f.submitFetch(trackURL, trackName, artistName)
	if err != nil {
		return "", fmt.Errorf("gateway: %w", err)
	}
	defer f.deleteJob(jobID)

	if err := f.awaitReady(jobID); err != nil {
		return "", err
	}

	if err := f.downloadFile(jobID, expectedPath); err != nil {
		return "", err
	}

	// The goroutine above only runs (and only sends) when isrc was non-empty at
	// the start; otherwise metaChan was closed immediately and there's nothing
	// to drain.
	var mbMeta Metadata
	if isrc != "" {
		mbMeta = <-metaChan
	}

	upc := ""
	identResult := <-identChan
	if identResult.err == nil || identResult.identifiers.ISRC != "" || identResult.identifiers.UPC != "" {
		if isrc == "" && strings.TrimSpace(identResult.identifiers.ISRC) != "" {
			isrc = strings.TrimSpace(identResult.identifiers.ISRC)
		}
		upc = strings.TrimSpace(identResult.identifiers.UPC)
	}

	coverPath = <-coverChan
	coverReceived = true

	trackNumberToEmbed := spotifyTrackNumber
	if trackNumberToEmbed == 0 {
		trackNumberToEmbed = 1
	}

	metadata := Metadata{
		Title:       trackName,
		Artist:      artistName,
		Album:       albumName,
		AlbumArtist: albumArtist,
		Date:        releaseDate,
		TrackNumber: trackNumberToEmbed,
		TotalTracks: spotifyTotalTracks,
		DiscNumber:  spotifyDiscNumber,
		TotalDiscs:  spotifyTotalDiscs,
		URL:         spotifyURL,
		Comment:     spotifyURL,
		Copyright:   copyrightText,
		Publisher:   publisher,
		Composer:    composer,
		Separator:   metadataSeparator,
		Description: "https://github.com/akvaithi/SpotiFLAC",
		ISRC:        isrc,
		UPC:         upc,
		Genre:       mbMeta.Genre,
	}

	if err := TagFile(expectedPath, metadata, coverPath); err != nil {
		fmt.Printf("Warning: failed to embed metadata: %v\n", err)
	} else {
		fmt.Println("Metadata embedded successfully")
	}

	return expectedPath, nil
}

// Deezer link resolution is 0.7-3.5s of song.link/Deezer API round-trips, and
// it sits on the critical path *before* the bot is even contacted. Since the
// download worker is serial and each track takes several seconds, resolving the
// next few tracks' links while the current one is in flight hides that cost
// entirely — by the time the worker reaches them the answer is already cached.
//
// Entries are shared between the prewarm goroutine and the worker, so a link is
// looked up once even when both want it at the same time: the second caller
// blocks on the same `done` channel rather than firing a duplicate request.
type deezerResolution struct {
	done chan struct{}
	url  string
}

var (
	deezerResolutions   = map[string]*deezerResolution{}
	deezerResolutionsMu sync.Mutex

	// song.link has no documented rate limit and this code does no backoff, so
	// concurrency is capped rather than letting a 600-track backfill open 600
	// connections at once. Three is plenty to stay ahead of a serial worker.
	deezerResolveSem = make(chan struct{}, 3)
)

// deezerResolutionsMax bounds the cache. Downloads are one-shot, so an entry is
// dead weight once used; clearing wholesale is simpler than tracking ages and
// costs at most one repeated lookup per track after a very long run.
const deezerResolutionsMax = 5000

// Indirected so the caching and concurrency behaviour above can be tested
// without reaching song.link.
var resolveDeezerOnce = func(spotifyID string) (string, error) {
	return NewSongLinkClient().GetDeezerURLFromSpotify(spotifyID)
}

// ResolveDeezerURL returns the Deezer track URL for a Spotify id, blocking on an
// in-flight lookup if one is already running. Empty string means no Deezer match.
func ResolveDeezerURL(spotifyID string) string {
	if spotifyID == "" {
		return ""
	}

	deezerResolutionsMu.Lock()
	entry, found := deezerResolutions[spotifyID]
	if !found {
		if len(deezerResolutions) >= deezerResolutionsMax {
			deezerResolutions = map[string]*deezerResolution{}
		}
		entry = &deezerResolution{done: make(chan struct{})}
		deezerResolutions[spotifyID] = entry
	}
	deezerResolutionsMu.Unlock()

	if !found {
		func() {
			// Closed unconditionally: if the lookup panics, every caller
			// blocked on this entry would otherwise wait forever.
			defer close(entry.done)

			deezerResolveSem <- struct{}{}
			defer func() { <-deezerResolveSem }()

			url, err := resolveDeezerOnce(spotifyID)
			if err == nil {
				entry.url = url
				return
			}
			// A failure is not cached. song.link timeouts and rate limits are
			// transient, and the queue retries a failed download — but it would
			// hit this cache, skip the lookup, and fall back to the bare Spotify
			// link every time, turning a blip into a permanent downgrade.
			deezerResolutionsMu.Lock()
			delete(deezerResolutions, spotifyID)
			deezerResolutionsMu.Unlock()
		}()
	}

	<-entry.done
	return entry.url
}

// PrewarmDeezerURL starts a resolution without waiting for it.
func PrewarmDeezerURL(spotifyID string) {
	if spotifyID == "" {
		return
	}
	go ResolveDeezerURL(spotifyID)
}

// resolveTrackURL prefers a Deezer track link over the Spotify one — the bot
// resolves Deezer links directly with no ambiguity, whereas a bare Spotify
// link relies on the bot's own (unverified) resolution. GetDeezerURLFromSpotify
// already chains song.link's Deezer match, an ISRC lookup, and Deezer's ISRC
// API as internal fallbacks, so there's nothing left to retry here.
func (f *FlacItDownloader) resolveTrackURL(spotifyID string) string {
	if url := ResolveDeezerURL(spotifyID); url != "" {
		// Last check before the bot sees it. A resolver that answers with an
		// artist or album page produces a link the bot cannot act on, and the
		// only symptom is a fetch that waits out its whole timeout and retries —
		// so the Spotify link, which the bot does understand, is strictly better
		// than a Deezer link to the wrong kind of thing.
		if isDeezerTrackURL(url) {
			return url
		}
		fmt.Printf("Resolver returned a non-track Deezer link (%s); using the Spotify link instead\n", url)
	}
	return "https://open.spotify.com/track/" + spotifyID
}

// submitFetch queues a fetch, telling the gateway what the reply should be.
// The expectation is what lets it reject a duplicate or late reply for some
// other track instead of filing it under this one's name; a gateway that
// predates the field ignores it and behaves as before.
func (f *FlacItDownloader) submitFetch(trackURL, trackName, artistName string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"url":    trackURL,
		"title":  trackName,
		"artist": artistName,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ActiveDownloadContext(), http.MethodPost, f.gatewayURL+"/fetch", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.JobID == "" {
		return "", fmt.Errorf("gateway returned an unrecognizable job id: %s", string(data))
	}
	return out.JobID, nil
}

func (f *FlacItDownloader) jobStatus(jobID string) (flacItJobView, error) {
	req, err := http.NewRequestWithContext(ActiveDownloadContext(), http.MethodGet, f.gatewayURL+"/fetch/"+jobID, nil)
	if err != nil {
		return flacItJobView{}, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return flacItJobView{}, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return flacItJobView{}, fmt.Errorf("gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var view flacItJobView
	if err := json.Unmarshal(data, &view); err != nil {
		return flacItJobView{}, fmt.Errorf("could not decode job status: %w", err)
	}
	return view, nil
}

// awaitReady polls the gateway until the job is ready or failed, relaying
// progress the same way the Tidal DASH loop did — directly through
// SetItemTotalSize/UpdateItemProgress rather than a ProgressWriter, since there
// is no io.Writer to hang one off of during this phase.
func (f *FlacItDownloader) awaitReady(jobID string) error {
	itemID := GetCurrentItemID()
	deadline := time.Now().Add(flacItJobDeadline)

	for time.Now().Before(deadline) {
		if err := CheckDownloadCancelled(); err != nil {
			return err
		}

		view, err := f.jobStatus(jobID)
		if err != nil {
			return fmt.Errorf("gateway: %w", err)
		}

		switch view.State {
		case "ready":
			return nil
		case "failed":
			if view.Error != "" {
				return fmt.Errorf("%s", view.Error)
			}
			return fmt.Errorf("gateway job failed")
		case "resolving":
			// The bulk of a fetch's wall time lives here — the bot searching
			// Deezer and replying over Telegram, tens of seconds by design
			// (see flacit-gateway/gateway.py) — and there's no byte count to
			// report yet. Surfacing the phase at least tells a client this
			// isn't stuck, since progress/total stay 0/0 the whole time.
			if itemID != "" {
				SetItemPhase(itemID, "resolving")
			}
		case "downloading":
			if itemID != "" {
				SetItemPhase(itemID, "downloading")
				if view.Size > 0 {
					SetItemTotalSize(itemID, float64(view.Size)/(1024*1024))
				}
				UpdateItemProgress(itemID, view.Downloaded, view.SpeedMBps)
			}
			SetDownloadProgress(view.Downloaded)
			SetDownloadSpeed(view.SpeedMBps)
		}

		if err := SleepWithDownloadContext(flacItPollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("timed out waiting for the Telegram gateway")
}

func (f *FlacItDownloader) downloadFile(jobID, savePath string) error {
	req, err := http.NewRequestWithContext(ActiveDownloadContext(), http.MethodGet, f.gatewayURL+"/fetch/"+jobID+"/file", nil)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway returned %d fetching the file: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	out, err := os.Create(savePath)
	if err != nil {
		return err
	}
	defer out.Close()

	pw := NewProgressWriterWithID(out, GetCurrentItemID())
	pw.SetTotalBytes(resp.ContentLength)
	if _, err := io.Copy(pw, resp.Body); err != nil {
		os.Remove(savePath)
		return WrapDownloadCancelled(err)
	}
	return nil
}

func (f *FlacItDownloader) deleteJob(jobID string) {
	req, err := http.NewRequest(http.MethodDelete, f.gatewayURL+"/fetch/"+jobID, nil)
	if err != nil {
		return
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
