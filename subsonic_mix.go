package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

type discoveryMix struct {
	PlaylistID string         `json:"playlist_id"`
	Tracks     []catalogTrack `json:"tracks"`
}

func loadDiscoveryMix() (discoveryMix, time.Time) {
	var mix discoveryMix
	config := os.Getenv("CONFIG_DIR")
	if config == "" {
		return mix, time.Time{}
	}
	file, err := os.Open(filepath.Join(config, "listenbrainz-mix.json"))
	if err != nil {
		return mix, time.Time{}
	}
	defer file.Close()
	if json.NewDecoder(io.LimitReader(file, 256<<10)).Decode(&mix) != nil {
		return discoveryMix{}, time.Time{}
	}
	if info, err := file.Stat(); err == nil {
		return mix, info.ModTime()
	}
	return mix, time.Time{}
}

// The mix uses an ordinary private Navidrome playlist for its ID and access
// control. Only decorate playlist rows returned to this caller by Navidrome.
func (f *subsonicFacade) handleDiscoveryMix(w http.ResponseWriter, r *http.Request) {
	mix, published := loadDiscoveryMix()
	detail := subsonicEndpoint(r.URL.Path) == "getPlaylist"
	if r.Method != http.MethodGet || mix.PlaylistID == "" || len(mix.Tracks) == 0 ||
		(detail && r.URL.Query().Get("id") != mix.PlaylistID) {
		f.proxy.ServeHTTP(w, r)
		return
	}
	body, contentType, err := f.forward(r)
	if err != nil {
		f.proxy.ServeHTTP(w, r)
		return
	}

	rows := make([]virtualSong, 0, len(mix.Tracks))
	for _, track := range mix.Tracks {
		id, err := strconv.ParseInt(track.ID, 10, 64)
		if track.Source != catalogDeezer || err != nil || id <= 0 || track.Title == "" || track.Artists == "" {
			continue
		}
		rows = append(rows, virtualSong{
			RowID: track.rowID(), CoverID: track.coverID(), Title: track.Title,
			Artist: track.Artists, Album: track.Album, HasCover: track.CoverURL != "",
			Duration: track.Duration,
		})
	}
	if len(rows) != 0 {
		var patched []byte
		if strings.Contains(contentType, "json") {
			patched = addDiscoveryMixJSON(body, mix.PlaylistID, detail, rows, published)
		} else {
			patched = addDiscoveryMixXML(body, mix.PlaylistID, detail, rows, published)
		}
		if patched != nil {
			if detail {
				for _, track := range mix.Tracks {
					f.remember(track)
				}
			}
			body = patched
		}
	}
	writeRaw(w, contentType, body)
}

func addDiscoveryMixJSON(body []byte, id string, detail bool, rows []virtualSong, published time.Time) []byte {
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	response, _ := envelope["subsonic-response"].(map[string]any)
	if response["status"] != "ok" {
		return nil
	}
	var playlists []any
	if detail {
		playlists = []any{response["playlist"]}
	} else {
		list, _ := response["playlists"].(map[string]any)
		playlists, _ = list["playlist"].([]any)
	}
	for _, value := range playlists {
		playlist, _ := value.(map[string]any)
		if playlist["id"] != id || playlist["public"] == true {
			continue
		}
		count, _ := playlist["songCount"].(float64)
		duration, _ := playlist["duration"].(float64)
		playlist["songCount"] = int(count) + len(rows)
		playlist["duration"] = int(duration) + mixDuration(rows)
		changed, _ := playlist["changed"].(string)
		if value := newerMixDate(changed, published); value != "" {
			playlist["changed"] = value
		}
		if detail {
			entries, _ := playlist["entry"].([]any)
			for _, row := range rows {
				entries = append(entries, row.jsonMap())
			}
			playlist["entry"] = entries
		}
		patched, _ := json.Marshal(envelope)
		return patched
	}
	return nil
}

func addDiscoveryMixXML(body []byte, id string, detail bool, rows []virtualSong, published time.Time) []byte {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		offset := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		attrs := make(map[string]string, len(start.Attr))
		for _, attr := range start.Attr {
			attrs[attr.Name.Local] = attr.Value
		}
		if start.Name.Local == "subsonic-response" && attrs["status"] != "ok" {
			return nil
		}
		if start.Name.Local != "playlist" || attrs["id"] != id || attrs["public"] == "true" {
			continue
		}

		count, _ := strconv.Atoi(attrs["songCount"])
		duration, _ := strconv.Atoi(attrs["duration"])
		patch := struct {
			XMLName xml.Name   `xml:"playlist"`
			Attrs   []xml.Attr `xml:",any,attr"`
			Entries string     `xml:",innerxml"`
		}{}
		changed := newerMixDate(attrs["changed"], published)
		for _, attr := range start.Attr {
			if attr.Name.Local != "songCount" && attr.Name.Local != "duration" &&
				(attr.Name.Local != "changed" || changed == "") {
				patch.Attrs = append(patch.Attrs, attr)
			}
		}
		patch.Attrs = append(patch.Attrs,
			xml.Attr{Name: xml.Name{Local: "songCount"}, Value: strconv.Itoa(count + len(rows))},
			xml.Attr{Name: xml.Name{Local: "duration"}, Value: strconv.Itoa(duration + mixDuration(rows))},
		)
		if changed != "" {
			patch.Attrs = append(patch.Attrs, xml.Attr{Name: xml.Name{Local: "changed"}, Value: changed})
		}
		if detail {
			var entries strings.Builder
			for _, row := range rows {
				entries.WriteString(row.xmlElement("entry"))
			}
			patch.Entries = entries.String()
		}
		updated, err := xml.Marshal(patch)
		if err != nil {
			return nil
		}
		end := decoder.InputOffset()
		if !bytes.HasSuffix(bytes.TrimSpace(body[offset:end]), []byte("/>")) {
			updated = bytes.TrimSuffix(updated, []byte("</playlist>"))
		}
		var result bytes.Buffer
		result.Write(body[:offset])
		result.Write(updated)
		result.Write(body[end:])
		return result.Bytes()
	}
}

func mixDuration(rows []virtualSong) int {
	total := 0
	for _, row := range rows {
		total += row.Duration
	}
	return total
}

// Clients use the playlist's changed timestamp to invalidate their cache.
func newerMixDate(current string, published time.Time) string {
	changed, _ := time.Parse(time.RFC3339Nano, current)
	if published.After(changed) {
		return published.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

// A now-playing scrobble is distinct from the media requests Arpeggi may
// issue while pre-caching. It can prefetch one song without starting a chain
// that downloads the rest of the playlist.
func (f *subsonicFacade) handleVirtualScrobble(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ids := q["id"]
	if len(ids) != 1 {
		f.proxy.ServeHTTP(w, r)
		return
	}
	id := ids[0]
	if _, _, virtual := parseRowID(id); !virtual || !f.authenticated(r) {
		f.proxy.ServeHTTP(w, r)
		return
	}
	rec := playbackQueueRecord(id)
	if rec.Status != backend.QueueCompleted {
		f.proxy.ServeHTTP(w, r)
		return
	}
	realID := f.indexedDownload(r, rec)
	if realID == "" {
		f.proxy.ServeHTTP(w, r)
		return
	}
	if q.Get("submission") == "false" {
		f.prefetchDiscoveryNext(r, id)
	}
	proxied := r.Clone(r.Context())
	q.Set("id", realID)
	proxied.URL.RawQuery = q.Encode()
	f.proxy.ServeHTTP(w, proxied)
}

func discoverySuccessor(mix discoveryMix, current string) (catalogTrack, bool) {
	for i := 1; i < len(mix.Tracks); i++ {
		if mix.Tracks[i-1].rowID() == current {
			next := mix.Tracks[i]
			id, err := strconv.ParseInt(next.ID, 10, 64)
			return next, next.Source == catalogDeezer && err == nil && id > 0 && next.Title != "" && next.Artists != ""
		}
	}
	return catalogTrack{}, false
}

func (f *subsonicFacade) prefetchDiscoveryNext(r *http.Request, current string) {
	mix, _ := loadDiscoveryMix()
	next, ok := discoverySuccessor(mix, current)
	if !ok || mix.PlaylistID == "" || playbackQueueRecord(next.rowID()).Status != "" {
		return
	}
	// Never infer access from the requested track, which is a public catalog ID.
	q := r.URL.Query()
	q.Del("time")
	q.Del("submission")
	q.Set("f", "json")
	q.Set("id", mix.PlaylistID)
	probe := r.Clone(r.Context())
	probe.URL.Path = "/rest/getPlaylist.view"
	probe.URL.RawQuery = q.Encode()
	body, _, err := f.forward(probe)
	if err != nil {
		return
	}
	var reply struct {
		Response struct {
			Status   string
			Playlist struct {
				ID     string
				Public bool
			}
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.Response.Status != "ok" ||
		reply.Response.Playlist.ID != mix.PlaylistID || reply.Response.Playlist.Public {
		return
	}
	f.remember(next)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := f.acquire(ctx, next.rowID(), nil); err != nil {
		log.Printf("subsonic facade: could not prefetch the next discovery track: %v", err)
	}
}
