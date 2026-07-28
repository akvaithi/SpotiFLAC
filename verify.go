package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Verification bridge.
//
// The community verify service issues a Cloudflare challenge whose callback
// ("cb") is STRICTLY validated: it must be a loopback host (127.0.0.1 or
// localhost) with the exact path /session-grant. It rejects any other host or
// path (e.g. a LAN IP, or /verify/callback) with an error page. So we cannot
// point the callback at this web app.
//
// Instead we keep the backend's real loopback callback unchanged (the service
// accepts it and renders the Turnstile checkbox). The user solves the challenge
// in their browser; the challenge then redirects the browser to that loopback
// URL — which is unreachable from a remote browser, so the browser shows a
// "can't connect" page whose address bar contains ...&grant=<grant>.
//
// The user pastes that address back into the web UI. We extract the grant and
// relay it to the backend's loopback listener from INSIDE the container (where
// 127.0.0.1 is reachable), completing the exact same exchange as on desktop.

type verifyBridge struct {
	mu         sync.Mutex
	loopbackCB string // backend's real loopback callback (with its state)
	pending    string // challenge URL currently shown to the user
}

var bridge = &verifyBridge{}

// setPublicBase is retained for compatibility with main.go's middleware but is
// no longer used to build callbacks (the callback must stay loopback).
var (
	publicBaseMu sync.RWMutex
	publicBase   string
)

func setPublicBase(u string) {
	publicBaseMu.Lock()
	if publicBase == "" && u != "" {
		publicBase = u
	}
	publicBaseMu.Unlock()
}

// webOpenBrowser is registered with backend.SetCommunityVerificationHandlers.
// It runs on the backend's verification goroutine and must not block.
func webOpenBrowser(challengeURL string) {
	loopbackCB := ""
	if u, err := url.Parse(challengeURL); err == nil {
		loopbackCB = u.Query().Get("cb")
	}
	bridge.mu.Lock()
	bridge.loopbackCB = loopbackCB
	bridge.pending = challengeURL
	bridge.mu.Unlock()

	emitEvent("verification-required", map[string]string{"challenge_url": challengeURL})
}

func getPendingVerification() string {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.pending
}

// handleVerifyComplete receives the grant the user captured after solving the
// challenge and relays it to the backend's loopback listener.
//
//	POST /api/verify/complete   body: {"value": "<pasted URL or bare grant>"}
func handleVerifyComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	value := extractJSONValue(string(body))
	grant := extractGrant(value)
	if grant == "" {
		writeErr(w, http.StatusBadRequest, "could not find a grant in the pasted value")
		return
	}

	bridge.mu.Lock()
	cb := bridge.loopbackCB
	bridge.mu.Unlock()
	if cb == "" {
		writeErr(w, http.StatusBadRequest, "no verification is in progress")
		return
	}

	relay, err := url.Parse(cb)
	if err != nil || !isLoopbackHost(relay.Host) || relay.Path != "/session-grant" {
		writeErr(w, http.StatusBadRequest, "invalid loopback callback")
		return
	}
	q := relay.Query()
	q.Set("grant", grant)
	relay.RawQuery = q.Encode()

	client := &http.Client{Timeout: 20 * time.Second}
	resp, relayErr := client.Get(relay.String())
	ok := false
	if relayErr == nil {
		ok = resp.StatusCode == http.StatusOK
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
	}

	bridge.mu.Lock()
	bridge.pending = ""
	bridge.mu.Unlock()

	emitEvent("verification-complete", map[string]bool{"ok": ok})
	if !ok {
		msg := "relay failed"
		if relayErr != nil {
			msg = relayErr.Error()
		}
		writeErr(w, http.StatusBadGateway, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// extractJSONValue pulls the "value" field out of a small JSON body without a
// struct, tolerating a bare string body too.
func extractJSONValue(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	// crude but dependency-free: look for "value"
	if i := strings.Index(body, "\"value\""); i >= 0 {
		rest := body[i+len("\"value\""):]
		if c := strings.Index(rest, ":"); c >= 0 {
			rest = strings.TrimSpace(rest[c+1:])
			if strings.HasPrefix(rest, "\"") {
				rest = rest[1:]
				if e := strings.Index(rest, "\""); e >= 0 {
					return unescapeJSON(rest[:e])
				}
			}
		}
	}
	return strings.Trim(body, "\"")
}

func unescapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\\/", "/")
	s = strings.ReplaceAll(s, "\\u0026", "&")
	s = strings.ReplaceAll(s, "\\\"", "\"")
	return s
}

// extractGrant accepts a full pasted URL (…?state=…&grant=XYZ) or a bare grant.
func extractGrant(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if u, err := url.Parse(value); err == nil && u.Query().Get("grant") != "" {
		return u.Query().Get("grant")
	}
	// maybe they pasted just the query string
	if strings.Contains(value, "grant=") {
		if vals, err := url.ParseQuery(strings.TrimLeft(value, "?")); err == nil {
			if g := vals.Get("grant"); g != "" {
				return g
			}
		}
	}
	// otherwise treat the whole thing as the grant token
	if !strings.Contains(value, "://") && !strings.Contains(value, "=") {
		return value
	}
	return ""
}

func isLoopbackHost(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
