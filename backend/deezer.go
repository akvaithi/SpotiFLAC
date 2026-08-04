package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Deezer's public API — the catalog behind @deezload2bot.
//
// This exists because Spotify is the catalog today while Deezer is the source,
// and the two are not the same set. A track Spotify has and Deezer doesn't is
// offered, queued, and then discovered to be unobtainable about two minutes
// later, as a bot timeout. Asking Deezer directly answers that up front.
//
// No auth, no rotating persisted-query hashes, and search results carry `isrc`
// and `link` inline — so a hit is both matchable against the library and
// downloadable without any further resolution. See DEEZER-MIGRATION.md.

const (
	deezerAPIBase   = "https://api.deezer.com"
	deezerTimeout   = 15 * time.Second
	deezerCoverSize = "1400x1400" // verified larger than cover_xl's 1000x1000
)

// deezerClient is shared; the API is rate limited per IP (~50 requests / 5s),
// so there is nothing to gain from more connections.
var (
	deezerHTTPOnce sync.Once
	deezerHTTP     *http.Client
)

func deezerClient() *http.Client {
	deezerHTTPOnce.Do(func() {
		deezerHTTP = &http.Client{Timeout: deezerTimeout}
	})
	return deezerHTTP
}

// ErrDeezerSearchUnavailable means the API answered, but withheld the results.
//
// This is not hypothetical and it is not an error status: from a blocked IP,
// Deezer returns `{"data": [], "total": 129}` — an empty page with a non-zero
// total. Verified 2026-08-04: a VPN egress gets this for every query while the
// server's own egress gets full results. Treating it as "no results" would make
// a network condition look like an empty catalog, which is precisely the wrong
// conclusion for the thing deciding what the user is allowed to download.
var ErrDeezerSearchUnavailable = fmt.Errorf("deezer search is answering but withholding results (IP likely blocked)")

type DeezerArtistRef struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Picture string `json:"picture_xl"`
}

type DeezerAlbumRef struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Cover       string `json:"cover_xl"`
	MD5Image    string `json:"md5_image"`
	ReleaseDate string `json:"release_date"`
}

type DeezerContributor struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// DeezerTrack covers both the search result shape and the fuller /track/{id}
// detail; fields absent from search stay zero.
type DeezerTrack struct {
	ID            int64               `json:"id"`
	Title         string              `json:"title"`
	TitleShort    string              `json:"title_short"`
	TitleVersion  string              `json:"title_version"`
	Link          string              `json:"link"`
	Duration      int                 `json:"duration"` // seconds
	Rank          int                 `json:"rank"`
	Readable      bool                `json:"readable"`
	ISRC          string              `json:"isrc"`
	TrackPosition int                 `json:"track_position"`
	DiskNumber    int                 `json:"disk_number"`
	ReleaseDate   string              `json:"release_date"`
	ExplicitLyric bool                `json:"explicit_lyrics"`
	MD5Image      string              `json:"md5_image"`
	Artist        DeezerArtistRef     `json:"artist"`
	Album         DeezerAlbumRef      `json:"album"`
	Contributors  []DeezerContributor `json:"contributors"`
}

type DeezerAlbum struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	Link        string          `json:"link"`
	Cover       string          `json:"cover_xl"`
	MD5Image    string          `json:"md5_image"`
	ReleaseDate string          `json:"release_date"`
	UPC         string          `json:"upc"`
	Label       string          `json:"label"`
	NbTracks    int             `json:"nb_tracks"`
	RecordType  string          `json:"record_type"`
	Artist      DeezerArtistRef `json:"artist"`
	Genres      struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	} `json:"genres"`
	Tracks struct {
		Data []DeezerTrack `json:"data"`
	} `json:"tracks"`
}

type DeezerArtist struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Link    string `json:"link"`
	Picture string `json:"picture_xl"`
	NbAlbum int    `json:"nb_album"`
	NbFan   int    `json:"nb_fan"`
}

// TrackURL is what gets handed to the bot. Built from the id rather than the
// API's own `link` field so it can never be an artist or album page — the
// failure that motivated all of this.
func (t DeezerTrack) TrackURL() string {
	if t.ID <= 0 {
		return ""
	}
	return fmt.Sprintf("https://www.deezer.com/track/%d", t.ID)
}

// FullTitle re-joins the version qualifier Deezer splits out, so
// `title_short`="Vaa Vaathi" + `title_version`=`(From "Vaathi")` reads the way
// Spotify would have written it.
func (t DeezerTrack) FullTitle() string {
	if t.Title != "" {
		return t.Title
	}
	if t.TitleVersion == "" {
		return t.TitleShort
	}
	return strings.TrimSpace(t.TitleShort + " " + t.TitleVersion)
}

// CreditedArtists is the full credit, performer first, joined the way this
// repo's tagger already writes multiple artists.
//
// Deezer's `artist` field is often the *composer* — "A. R. Rahman" where the
// performer is "Minmini" — and `contributors` carries both. Performer-first
// matters beyond taste: LRCLIB is keyed on the performer, and starting from the
// composer measurably downgrades synced lyrics to plain.
func (t DeezerTrack) CreditedArtists(separator string) string {
	if separator == "" {
		separator = " • "
	}
	names := t.contributorNames()
	if len(names) == 0 {
		return t.Artist.Name
	}
	return strings.Join(names, separator)
}

// PrimaryArtist is the performer — the first contributor, falling back to the
// artist field when a record has no contributors at all.
func (t DeezerTrack) PrimaryArtist() string {
	if names := t.contributorNames(); len(names) > 0 {
		return names[0]
	}
	return t.Artist.Name
}

func (t DeezerTrack) contributorNames() []string {
	names := make([]string, 0, len(t.Contributors))
	seen := map[string]bool{}
	for _, c := range t.Contributors {
		name := strings.TrimSpace(c.Name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		names = append(names, name)
	}
	return names
}

// CoverURL prefers the larger CDN rendering over the API's cover_xl. Deezer
// serves 1400x1400 from the same md5 (measured: 198KB vs 119KB at 1000px);
// cover_xl is the fallback when no md5 came back.
func (t DeezerTrack) CoverURL() string {
	if md5 := firstNonEmpty(t.MD5Image, t.Album.MD5Image); md5 != "" {
		return fmt.Sprintf("https://cdn-images.dzcdn.net/images/cover/%s/%s-000000-80-0-0.jpg", md5, deezerCoverSize)
	}
	return t.Album.Cover
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// deezerListResponse is the envelope every list endpoint uses.
type deezerListResponse[T any] struct {
	Data  []T `json:"data"`
	Total int `json:"total"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// deezerBaseURL is a variable so tests can point the whole client at a stub.
var deezerBaseURL = deezerAPIBase

func deezerGet(ctx context.Context, path string, query url.Values, out any) error {
	endpoint := deezerBaseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := deezerClient().Do(req)
	if err != nil {
		return fmt.Errorf("deezer request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deezer returned status %d for %s", resp.StatusCode, path)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading deezer response: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding deezer response: %w", err)
	}
	return nil
}

func deezerList[T any](ctx context.Context, path string, query url.Values) ([]T, error) {
	var payload deezerListResponse[T]
	if err := deezerGet(ctx, path, query, &payload); err != nil {
		return nil, err
	}
	if payload.Error != nil && payload.Error.Message != "" {
		return nil, fmt.Errorf("deezer error: %s", payload.Error.Message)
	}
	// The withheld-results case. A genuinely empty result set reports total 0.
	if len(payload.Data) == 0 && payload.Total > 0 {
		return nil, ErrDeezerSearchUnavailable
	}
	return payload.Data, nil
}

func deezerLimit(limit int) string {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	return strconv.Itoa(limit)
}

// SearchDeezerTracks runs a plain text query.
func SearchDeezerTracks(ctx context.Context, query string, limit int) ([]DeezerTrack, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty search query")
	}
	return deezerList[DeezerTrack](ctx, "/search", url.Values{
		"q":     {query},
		"limit": {deezerLimit(limit)},
	})
}

// SearchDeezerTracksPrecise uses Deezer's field syntax, which is far stricter
// than a bare query: `artist:"X" track:"Y"` matches the pair rather than any
// record sharing a word with either.
func SearchDeezerTracksPrecise(ctx context.Context, artist, track string, limit int) ([]DeezerTrack, error) {
	artist, track = strings.TrimSpace(artist), strings.TrimSpace(track)
	if track == "" {
		return nil, fmt.Errorf("empty track title")
	}
	q := fmt.Sprintf("track:%q", track)
	if artist != "" {
		q = fmt.Sprintf("artist:%q %s", artist, q)
	}
	return deezerList[DeezerTrack](ctx, "/search", url.Values{
		"q":     {q},
		"limit": {deezerLimit(limit)},
	})
}

func SearchDeezerAlbums(ctx context.Context, query string, limit int) ([]DeezerAlbum, error) {
	return deezerList[DeezerAlbum](ctx, "/search/album", url.Values{
		"q":     {strings.TrimSpace(query)},
		"limit": {deezerLimit(limit)},
	})
}

func SearchDeezerArtists(ctx context.Context, query string, limit int) ([]DeezerArtist, error) {
	return deezerList[DeezerArtist](ctx, "/search/artist", url.Values{
		"q":     {strings.TrimSpace(query)},
		"limit": {deezerLimit(limit)},
	})
}

// GetDeezerTrack fetches the detail record, which carries the fields search
// omits: contributors, track/disk position, release date.
func GetDeezerTrack(ctx context.Context, id int64) (*DeezerTrack, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid deezer track id")
	}
	var track DeezerTrack
	if err := deezerGet(ctx, "/track/"+strconv.FormatInt(id, 10), nil, &track); err != nil {
		return nil, err
	}
	if track.ID == 0 {
		return nil, fmt.Errorf("deezer has no track %d", id)
	}
	return &track, nil
}

// GetDeezerTrackByISRC is the exact translation from any other catalog: an ISRC
// identifies the recording, so a hit here is not a guess.
func GetDeezerTrackByISRC(ctx context.Context, isrc string) (*DeezerTrack, error) {
	isrc = strings.ToUpper(strings.TrimSpace(isrc))
	if isrc == "" {
		return nil, fmt.Errorf("empty ISRC")
	}
	var track DeezerTrack
	if err := deezerGet(ctx, "/track/isrc:"+isrc, nil, &track); err != nil {
		return nil, err
	}
	if track.ID == 0 {
		return nil, fmt.Errorf("deezer has no track for ISRC %s", isrc)
	}
	return &track, nil
}

// GetDeezerAlbum returns the album with its tracklist inline — one call, not
// one per track.
func GetDeezerAlbum(ctx context.Context, id int64) (*DeezerAlbum, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid deezer album id")
	}
	var album DeezerAlbum
	if err := deezerGet(ctx, "/album/"+strconv.FormatInt(id, 10), nil, &album); err != nil {
		return nil, err
	}
	if album.ID == 0 {
		return nil, fmt.Errorf("deezer has no album %d", id)
	}
	return &album, nil
}

// GetDeezerRelatedArtists replaces the Spotify persisted-query hash behind
// GetRelatedArtists — same answer, nothing to rot.
func GetDeezerRelatedArtists(ctx context.Context, artistID int64, limit int) ([]DeezerArtist, error) {
	return deezerList[DeezerArtist](ctx, fmt.Sprintf("/artist/%d/related", artistID), url.Values{
		"limit": {deezerLimit(limit)},
	})
}

func GetDeezerArtistTopTracks(ctx context.Context, artistID int64, limit int) ([]DeezerTrack, error) {
	return deezerList[DeezerTrack](ctx, fmt.Sprintf("/artist/%d/top", artistID), url.Values{
		"limit": {deezerLimit(limit)},
	})
}

func GetDeezerArtistAlbums(ctx context.Context, artistID int64, limit int) ([]DeezerAlbum, error) {
	return deezerList[DeezerAlbum](ctx, fmt.Sprintf("/artist/%d/albums", artistID), url.Values{
		"limit": {deezerLimit(limit)},
	})
}
