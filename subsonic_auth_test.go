package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestVirtualAcquisitionRequiresNavidromeAuthentication(t *testing.T) {
	pings := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/ping.view" {
			pings++
		}
		fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":"Wrong username or password"}}}`)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	facade := &subsonicFacade{target: target, proxy: httputil.NewSingleHostReverseProxy(target), client: upstream.Client(), inject: true}
	for _, endpoint := range []string{"star", "stream", "download", "updatePlaylist", "createPlaylist"} {
		param := "id"
		if endpoint == "updatePlaylist" {
			param = "songIdToAdd"
		}
		if endpoint == "createPlaylist" {
			param = "songId"
		}
		request := httptest.NewRequest("GET", "/rest/"+endpoint+"?"+param+"="+url.QueryEscape(deezerIDPrefix+"123"), nil)
		response := httptest.NewRecorder()
		facade.ServeHTTP(response, request)
		if response.Body.String() == "" {
			t.Fatal("missing upstream authentication failure")
		}
	}
	if pings != 5 {
		t.Fatalf("expected 5 authentication checks, got %d", pings)
	}
}
