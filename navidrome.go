package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// Navidrome integration.
//
// A finished download is invisible until Navidrome rescans, and its own watcher
// can lag by minutes. Poking /rest/startScan when the queue drains is what makes
// "save" feel immediate in a client.
//
// Credentials come from the environment first (natural for Docker), then from
// the server-side config.json — never from this repo, which is public.

type navidromeConfig struct {
	URL      string
	User     string
	Password string
}

func loadNavidromeConfig() (navidromeConfig, bool) {
	cfg := navidromeConfig{
		URL:      strings.TrimRight(os.Getenv("NAVIDROME_URL"), "/"),
		User:     os.Getenv("NAVIDROME_USER"),
		Password: os.Getenv("NAVIDROME_PASSWORD"),
	}

	if cfg.URL == "" || cfg.User == "" || cfg.Password == "" {
		settings, err := backend.LoadConfigSettings()
		if err == nil && settings != nil {
			if cfg.URL == "" {
				cfg.URL = strings.TrimRight(settingString(settings, "navidromeUrl"), "/")
			}
			if cfg.User == "" {
				cfg.User = settingString(settings, "navidromeUser")
			}
			if cfg.Password == "" {
				cfg.Password = settingString(settings, "navidromePassword")
			}
		}
	}

	return cfg, cfg.URL != "" && cfg.User != "" && cfg.Password != ""
}

func settingString(settings map[string]interface{}, key string) string {
	if v, ok := settings[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// subsonicParams builds the salted-token auth Subsonic requires. Sending the
// password itself is also allowed but pointlessly worse.
func subsonicParams(cfg navidromeConfig) url.Values {
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		// A time-derived salt is still fine here: it only has to be unique per
		// request, not unpredictable.
		saltBytes = []byte(fmt.Sprintf("%08d", time.Now().UnixNano()%1e8))
	}
	salt := hex.EncodeToString(saltBytes)
	sum := md5.Sum([]byte(cfg.Password + salt))

	return url.Values{
		"u": {cfg.User},
		"t": {hex.EncodeToString(sum[:])},
		"s": {salt},
		"v": {"1.16.1"},
		"c": {"SpotiFLAC"},
		"f": {"json"},
	}
}

type subsonicEnvelope struct {
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		ScanStatus struct {
			Scanning bool  `json:"scanning"`
			Count    int64 `json:"count"`
		} `json:"scanStatus"`
	} `json:"subsonic-response"`
}

func callSubsonic(cfg navidromeConfig, endpoint string) (*subsonicEnvelope, error) {
	reqURL := fmt.Sprintf("%s/rest/%s?%s", cfg.URL, endpoint, subsonicParams(cfg).Encode())

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("navidrome %s returned %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var env subsonicEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("navidrome %s: unreadable response: %w", endpoint, err)
	}
	if env.Response.Error != nil {
		return nil, fmt.Errorf("navidrome %s: %s (code %d)", endpoint, env.Response.Error.Message, env.Response.Error.Code)
	}
	return &env, nil
}

func triggerNavidromeScan() error {
	cfg, ok := loadNavidromeConfig()
	if !ok {
		// Not configured is not an error — plenty of setups don't run Navidrome.
		return nil
	}
	_, err := callSubsonic(cfg, "startScan")
	return err
}

// TriggerNavidromeScan lets a client force a rescan (RPC).
func (a *App) TriggerNavidromeScan() error {
	cfg, ok := loadNavidromeConfig()
	if !ok {
		return fmt.Errorf("navidrome is not configured (set NAVIDROME_URL/USER/PASSWORD or navidromeUrl/navidromeUser/navidromePassword in config.json)")
	}
	_, err := callSubsonic(cfg, "startScan")
	return err
}

type NavidromeStatus struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Scanning   bool   `json:"scanning"`
	Count      int64  `json:"count"`
	Error      string `json:"error,omitempty"`
}

// GetNavidromeStatus reports whether the scan hook is wired up and working,
// so a client can surface a real problem instead of silently not rescanning.
func (a *App) GetNavidromeStatus() NavidromeStatus {
	cfg, ok := loadNavidromeConfig()
	if !ok {
		return NavidromeStatus{}
	}

	env, err := callSubsonic(cfg, "getScanStatus")
	if err != nil {
		return NavidromeStatus{Configured: true, Error: err.Error()}
	}
	return NavidromeStatus{
		Configured: true,
		Reachable:  true,
		Scanning:   env.Response.ScanStatus.Scanning,
		Count:      env.Response.ScanStatus.Count,
	}
}
