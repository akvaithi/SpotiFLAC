package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func SanitizeSettingsMap(settings map[string]interface{}) map[string]interface{} {
	if settings == nil {
		return nil
	}

	sanitized := make(map[string]interface{}, len(settings))
	for key, value := range settings {
		sanitized[key] = value
	}

	return sanitized
}

func SanitizePersistedConfigSettings() error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}

	sanitized := SanitizeSettingsMap(settings)
	payload, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, payload, 0o644)
}

func GetDefaultMusicPath() string {

	homeDir, err := os.UserHomeDir()
	if err != nil {

		return "C:\\Users\\Public\\Music"
	}

	return filepath.Join(homeDir, "Music")
}

func GetConfigPath() (string, error) {
	dir, err := EnsureAppDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "config.json"), nil
}

func LoadConfigSettings() (map[string]interface{}, error) {
	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}

	return SanitizeSettingsMap(settings), nil
}

// The browser source runs inside the existing Chromium container on a shared
// Docker network. Keep legacy settings readable for existing callers.
const defaultFlacItGatewayURL = "http://chromium:8782"

func GetFlacItGatewayURL() string {
	if source := strings.TrimSpace(os.Getenv("DOUBLEDOUBLE_GATEWAY_URL")); source != "" {
		return strings.TrimRight(source, "/")
	}
	if fromEnv := strings.TrimSpace(os.Getenv("FLACIT_GATEWAY_URL")); fromEnv != "" {
		return strings.TrimRight(fromEnv, "/")
	}

	settings, err := LoadConfigSettings()
	if err == nil && settings != nil {
		if fromConfig, ok := settings["flacitApiUrl"].(string); ok {
			if trimmed := strings.TrimRight(strings.TrimSpace(fromConfig), "/"); trimmed != "" {
				return trimmed
			}
		}
	}

	return defaultFlacItGatewayURL
}

func GetRedownloadWithSuffixSetting() bool {
	settings, err := LoadConfigSettings()
	if err != nil || settings == nil {
		return true
	}

	enabled, ok := settings["redownloadWithSuffix"].(bool)
	if !ok {
		return true
	}
	return enabled
}

func normalizeExistingFileCheckMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "isrc", "upc":
		return "isrc"
	case "hybrid":
		return "hybrid"
	default:
		return "filename"
	}
}

func GetExistingFileCheckModeSetting() string {
	settings, err := LoadConfigSettings()
	if err != nil || settings == nil {
		return "filename"
	}

	rawMode, _ := settings["existingFileCheckMode"].(string)
	return normalizeExistingFileCheckMode(rawMode)
}

func GetLinkResolverSetting() string {
	settings, err := LoadConfigSettings()
	if err != nil || settings == nil {
		return linkResolverProviderDeezerSongLink
	}

	resolver, _ := settings["linkResolver"].(string)
	switch strings.TrimSpace(strings.ToLower(resolver)) {
	case "songlink", linkResolverProviderDeezerSongLink:
		return linkResolverProviderDeezerSongLink
	case "songstats":
		return linkResolverProviderSongstats
	case "":
		return linkResolverProviderDeezerSongLink
	default:
		return linkResolverProviderDeezerSongLink
	}
}

func GetLinkResolverAllowFallback() bool {
	settings, err := LoadConfigSettings()
	if err != nil || settings == nil {
		return true
	}

	allowFallback, ok := settings["allowResolverFallback"].(bool)
	if !ok {
		return true
	}

	return allowFallback
}
