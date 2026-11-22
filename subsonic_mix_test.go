package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"testing"
	"time"
)

func TestDiscoveryMixPreservesPlaylistAccessAndUsesPlayableEntries(t *testing.T) {
	rows := []virtualSong{{RowID: "sf-dz:123", Title: "Rock & Roll", Duration: 45}}
	published := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	list := []byte(`<subsonic-response status="ok"><playlists><playlist id="other" songCount="0"/><playlist id="mix" public="false" songCount="0"/></playlists></subsonic-response>`)
	patched := addDiscoveryMixXML(list, "mix", false, rows, published)
	if !bytes.Contains(patched, []byte(`id="mix" public="false" songCount="1" duration="45"`)) ||
		!bytes.Contains(patched, []byte(`id="other" songCount="0"`)) ||
		!bytes.Contains(patched, []byte(`changed="2026-11-22T00:00:00Z"`)) {
		t.Fatalf("unexpected playlist listing: %s", patched)
	}
	detail := []byte(`<subsonic-response status="ok"><playlist id="mix" public="false" songCount="0"></playlist></subsonic-response>`)
	patched = addDiscoveryMixXML(detail, "mix", true, rows, published)
	var decoded struct {
		Playlist struct {
			Entry struct {
				ID    string `xml:"id,attr"`
				Title string `xml:"title,attr"`
			} `xml:"entry"`
		} `xml:"playlist"`
	}
	if xml.Unmarshal(patched, &decoded) != nil || decoded.Playlist.Entry.ID != "sf-dz:123" || decoded.Playlist.Entry.Title != "Rock & Roll" {
		t.Fatalf("expected a playable, escaped entry: %s", patched)
	}

	raw := []byte(`{"subsonic-response":{"status":"ok","playlist":{"id":"mix","public":false,"songCount":0}}}`)
	patched = addDiscoveryMixJSON(raw, "mix", true, rows, published)
	var envelope struct {
		Response struct {
			Playlist struct {
				SongCount int
				Entry     []struct{ ID string }
			}
		} `json:"subsonic-response"`
	}
	if json.Unmarshal(patched, &envelope) != nil || envelope.Response.Playlist.SongCount != 1 ||
		len(envelope.Response.Playlist.Entry) != 1 || envelope.Response.Playlist.Entry[0].ID != "sf-dz:123" {
		t.Fatalf("unexpected JSON playlist detail: %s", patched)
	}
	for _, blocked := range []string{
		`{"subsonic-response":{"status":"failed","error":{}}}`,
		`{"subsonic-response":{"status":"ok","playlist":{"id":"mix","public":true}}}`,
		`{"subsonic-response":{"status":"ok","playlists":{"playlist":[]}}}`,
	} {
		if patched := addDiscoveryMixJSON([]byte(blocked), "mix", true, rows, published); patched != nil {
			t.Fatal("changed a playlist response without private playlist access")
		}
	}
}
