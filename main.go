package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/akvaithi/SpotiFLAC/backend"
)

//go:embed all:web
var webAssets embed.FS

var (
	downloadDir string
	appVersion  = "web"
)

// serverDownloadDir is referenced from app.go (e.g. ExportFailedDownloads).
func serverDownloadDir() string { return downloadDir }

func main() {
	var addr string
	flag.StringVar(&addr, "addr", envOr("ADDR", ":8080"), "listen address")
	flag.StringVar(&downloadDir, "downloads", envOr("DOWNLOAD_DIR", "/downloads"), "download output directory")
	flag.Parse()

	// Redirect the backend's home-relative config dir (~/.spotiflac) to a
	// persistent volume when CONFIG_DIR is set (Docker mounts it at /config).
	if cfg := os.Getenv("CONFIG_DIR"); cfg != "" {
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			log.Printf("warning: could not create CONFIG_DIR %s: %v", cfg, err)
		}
		os.Setenv("HOME", cfg)        // unix: os.UserHomeDir()
		os.Setenv("USERPROFILE", cfg) // windows fallback
	}

	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		log.Printf("warning: could not create download dir %s: %v", downloadDir, err)
	}

	backend.AppVersion = appVersion

	initAPIToken()
	if apiAuthEnabled() {
		log.Println("API_TOKEN set: /api/* requires a bearer token")
	}

	app := NewApp()
	app.startup()
	defer app.shutdown(context.Background())

	// Drains the durable download queue so downloads survive a client
	// disconnecting or the container restarting (see queue.go).
	startDownloadWorker(app)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/rpc/", requireAPIToken(handleRPC(app)))
	mux.HandleFunc("/api/events", requireAPIToken(handleEvents))
	mux.HandleFunc("/api/server-info", handleServerInfo)
	mux.HandleFunc("/api/file", requireAPIToken(handleFileDownload))

	// The Subsonic facade is deliberately NOT behind requireAPIToken: a
	// Subsonic client can't send a SpotiFLAC bearer token, and /rest/* carries
	// Navidrome's own u/t/s auth, which Navidrome itself validates.
	if facade = initSubsonicFacade(app); facade != nil {
		mux.Handle("/rest/", facade)
	}

	mux.Handle("/", staticHandler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: downloads can block for minutes.
		IdleTimeout: 5 * time.Minute,
	}

	go func() {
		log.Printf("SpotiFLAC web listening on %s  (downloads -> %s)", addr, downloadDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"download_dir": downloadDir,
		"version":      appVersion,
	})
}

func staticHandler() http.Handler {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never cache the UI, so pulling a new image always serves fresh
		// HTML/JS/CSS instead of a stale browser copy.
		w.Header().Set("Cache-Control", "no-store, must-revalidate")

		// When API_TOKEN is set the UI is behind it too; arriving with
		// ?token=… drops a cookie so the SPA's own API calls authenticate.
		rememberTokenCookie(w, r)
		if !requestHasValidToken(r) {
			http.Error(w, "missing or invalid API token", http.StatusUnauthorized)
			return
		}
		// FileServer serves index.html for "/" automatically. For unknown
		// non-asset paths, fall back to index.html (SPA-style routing).
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path[1:], ".") {
			if _, err := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// handleFileDownload streams a finished file from the downloads directory to
// the browser. Restricted to downloadDir to avoid arbitrary filesystem reads.
func handleFileDownload(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	if raw == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	clean := filepath.Clean(raw)
	absDL, _ := filepath.Abs(downloadDir)
	absFile, err := filepath.Abs(clean)
	if err != nil || !strings.HasPrefix(absFile, absDL+string(os.PathSeparator)) {
		http.Error(w, "path outside download directory", http.StatusForbidden)
		return
	}
	info, err := os.Stat(absFile)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(absFile)+"\"")
	http.ServeFile(w, r, absFile)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

var _ = fmt.Sprintf
