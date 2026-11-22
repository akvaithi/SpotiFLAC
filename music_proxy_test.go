package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

func TestMusicListenerKeepsNavidromeWebAndAPIs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "navidrome:"+r.URL.Path) }))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	f := &subsonicFacade{target: target, proxy: httputil.NewSingleHostReverseProxy(target), client: upstream.Client()}
	handler := musicHandler(f)
	for _, path := range []string{"/", "/app/index.html", "/api/login", "/api/rpc/GetConfig", "/rest/ping.view"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Body.String() != "navidrome:"+path {
			t.Fatalf("%s escaped the Navidrome proxy", path)
		}
	}
}
