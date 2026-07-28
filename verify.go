package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Verification proxy bridge.
//
// Upstream SpotiFLAC solves the community Cloudflare challenge by opening the
// challenge URL in a local browser; the challenge page then redirects to a
// loopback callback (http://127.0.0.1:<port>/session-grant) that the desktop
// app is listening on. In a headless container the user's browser is on a
// different host and cannot reach that loopback address.
//
// This bridge keeps the backend completely unchanged. When the backend asks us
// to "open the browser", we:
//  1. take the challenge URL it hands us (which embeds cb=<loopback callback>),
//  2. stash that loopback callback under a random token,
//  3. rewrite cb to point at THIS server's /verify/callback?token=... ,
//  4. publish the rewritten challenge URL to the web UI (which opens it in a
//     new browser tab on the user's device).
//
// When the user finishes the Cloudflare challenge, their browser is redirected
// to /verify/callback?token=...&grant=... . We then relay that grant to the
// backend's loopback listener (reachable from inside the container), which
// completes the session exchange exactly as on desktop.

type verifyBridge struct {
	mu       sync.Mutex
	loopback map[string]string // token -> loopback callback URL
	pending  string            // current rewritten challenge URL (for late SSE subscribers)
}

var bridge = &verifyBridge{loopback: make(map[string]string)}

// publicBaseURL is derived per-request from the Host header, but the backend's
// openBrowser callback has no request context, so we capture the most recent
// base URL seen by an incoming HTTP request. Defaults are overridden in main.
var (
	publicBaseMu sync.RWMutex
	publicBase   = ""
)

func setPublicBase(u string) {
	publicBaseMu.Lock()
	if publicBase == "" && u != "" {
		publicBase = u
	}
	publicBaseMu.Unlock()
}

func currentPublicBase() string {
	publicBaseMu.RLock()
	defer publicBaseMu.RUnlock()
	return publicBase
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// webOpenBrowser is registered with backend.SetCommunityVerificationHandlers.
// It runs on the backend's verification goroutine and must not block.
func webOpenBrowser(challengeURL string) {
	u, err := url.Parse(challengeURL)
	if err != nil {
		// Fall back to surfacing the raw URL so the user can still solve it.
		bridge.setPending(challengeURL)
		emitEvent("verification-required", map[string]string{"challenge_url": challengeURL})
		return
	}

	q := u.Query()
	loopbackCB := q.Get("cb")

	base := currentPublicBase()
	if loopbackCB == "" || base == "" {
		// Without a loopback cb or a known public address we cannot proxy;
		// surface the original URL unchanged.
		bridge.setPending(challengeURL)
		emitEvent("verification-required", map[string]string{"challenge_url": challengeURL})
		return
	}

	token := randToken()
	bridge.mu.Lock()
	bridge.loopback[token] = loopbackCB
	bridge.mu.Unlock()

	q.Set("cb", base+"/verify/callback?token="+token)
	u.RawQuery = q.Encode()
	rewritten := u.String()

	bridge.setPending(rewritten)
	emitEvent("verification-required", map[string]string{"challenge_url": rewritten})
}

func (b *verifyBridge) setPending(v string) {
	b.mu.Lock()
	b.pending = v
	b.mu.Unlock()
}

func getPendingVerification() string {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.pending
}

// handleVerifyCallback receives the user's browser after they solve the
// Cloudflare challenge, relays the grant to the backend's loopback listener,
// then shows a "Verified" page that auto-closes the tab.
func handleVerifyCallback(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	grant := r.URL.Query().Get("grant")

	bridge.mu.Lock()
	loopbackCB := bridge.loopback[token]
	delete(bridge.loopback, token)
	bridge.pending = ""
	bridge.mu.Unlock()

	if loopbackCB == "" || grant == "" {
		http.Error(w, "invalid or expired verification callback", http.StatusBadRequest)
		return
	}

	relayURL, err := url.Parse(loopbackCB)
	if err != nil {
		http.Error(w, "invalid loopback callback", http.StatusBadRequest)
		return
	}
	rq := relayURL.Query()
	rq.Set("grant", grant)
	relayURL.RawQuery = rq.Encode()

	client := &http.Client{Timeout: 15 * time.Second}
	resp, relayErr := client.Get(relayURL.String())
	if relayErr == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
	}

	emitEvent("verification-complete", map[string]bool{"ok": relayErr == nil})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, verifiedPageHTML)
}

const verifiedPageHTML = `<!doctype html><html><head><meta charset="utf-8">` +
	`<meta name="viewport" content="width=device-width,initial-scale=1"><title>Verified</title>` +
	`<style>*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:20px;background:#000;background-image:radial-gradient(circle,rgba(255,255,255,.2) 1.5px,transparent 1.5px);background-size:30px 30px;color:#f5f5f5;font:14px/1.5 system-ui,sans-serif}main{text-align:center}.icon{width:48px;height:48px;margin:0 auto 20px;display:grid;place-items:center;border-radius:50%;background:#fff;color:#000;font-size:22px}h1{margin:0 0 6px;font-size:24px;letter-spacing:-.035em}p{margin:0;color:#888}</style></head>` +
	`<body><main><div class="icon">&#10003;</div><h1>Verified</h1><p>You can close this tab and return to SpotiFLAC.</p></main>` +
	`<script>setTimeout(()=>window.close(),1200)</script></body></html>`
