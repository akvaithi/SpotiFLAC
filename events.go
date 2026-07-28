package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// eventHub is a minimal Server-Sent-Events broker that replaces the Wails
// runtime event system. app.go emits events through emitEvent(); the web UI
// subscribes via GET /api/events and receives them as SSE messages.

type sseEvent struct {
	Name string      `json:"name"`
	Data interface{} `json:"data"`
}

type eventHub struct {
	mu      sync.RWMutex
	clients map[chan sseEvent]struct{}
}

var hub = &eventHub{clients: make(map[chan sseEvent]struct{})}

func (h *eventHub) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan sseEvent) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *eventHub) broadcast(ev sseEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- ev:
		default:
			// Drop for slow consumers rather than block the emitter.
		}
	}
}

// emitEvent is the drop-in replacement for runtime.EventsEmit.
func emitEvent(name string, data interface{}) {
	hub.broadcast(sseEvent{Name: name, Data: data})
}

// handleEvents streams events to a browser client over SSE.
func handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Flush the headers now rather than waiting for the first event, so a client
	// can tell "connected, nothing happening yet" from "still waiting to be let in".
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := hub.subscribe()
	defer hub.unsubscribe(ch)

	// Tell the client the current verification state on connect so a page
	// reload during a pending challenge still shows the modal.
	if pending := getPendingVerification(); pending != "" {
		writeSSE(w, flusher, sseEvent{Name: "verification-required", Data: map[string]string{"challenge_url": pending}})
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			writeSSE(w, flusher, ev)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, ev sseEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}
