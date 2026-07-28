package main

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// Optional shared-secret auth for the API.
//
// The RPC surface reaches methods like ListDirectoryFiles that read arbitrary
// paths, and it has never had any authentication — acceptable when the only
// client was the bundled UI on a trusted LAN, less so once other clients talk
// to it. Setting API_TOKEN turns on a bearer check; leaving it unset keeps the
// existing behaviour exactly, so no deployment breaks by upgrading.
//
// The token is also accepted as a query parameter, because EventSource can't
// set headers and the SSE stream needs to be reachable from a browser.
//
// Native clients send a bearer header. The bundled web UI can't — its fetch()
// calls know nothing about tokens — so loading any page once with ?token=…
// sets a cookie and the UI keeps working unchanged. The cookie is SameSite
// strict, so another site can't ride it to reach the API.

const apiTokenCookie = "spotiflac_token"

var apiToken string

func initAPIToken() {
	apiToken = strings.TrimSpace(os.Getenv("API_TOKEN"))
}

func apiAuthEnabled() bool { return apiToken != "" }

func requestHasValidToken(r *http.Request) bool {
	if !apiAuthEnabled() {
		return true
	}

	presented := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			presented = strings.TrimSpace(after)
		}
	}
	if presented == "" {
		presented = r.Header.Get("X-API-Token")
	}
	if presented == "" {
		presented = r.URL.Query().Get("token")
	}
	if presented == "" {
		if c, err := r.Cookie(apiTokenCookie); err == nil {
			presented = c.Value
		}
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(apiToken)) == 1
}

// rememberTokenCookie lets a browser that arrived with ?token=… keep working
// without the query string, so the bundled UI's own fetch() calls authenticate.
func rememberTokenCookie(w http.ResponseWriter, r *http.Request) {
	if !apiAuthEnabled() || r.URL.Query().Get("token") == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     apiTokenCookie,
		Value:    apiToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60 * 60 * 24 * 365,
	})
}

// requireAPIToken guards a handler when API_TOKEN is set.
func requireAPIToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requestHasValidToken(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="spotiflac"`)
			writeErr(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next(w, r)
	}
}
