package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config represents the client-side session configuration.
type Config struct {
	ServerURL    string `json:"server_url"`
	UID          string `json:"uid"`
	DisplayName  string `json:"display_name"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	AIProvider   string `json:"ai_provider"`
	AIAPIKey     string `json:"ai_api_key"`
	APIKey       string `json:"api_key"`
}

// DefaultConfigPath returns the default configuration file path (~/.sanctum/config.json).
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".sanctum", "config.json")
}

// LoadConfig loads the client configuration from the specified path.
// It returns an empty Config if the file does not exist.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// SaveConfig saves the client configuration to the specified path.
func SaveConfig(config Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// IsTokenExpired checks if a JWT token is expired by decoding its payload segment.
func IsTokenExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return true // invalid JWT format
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return true
	}

	return time.Now().Unix() >= claims.Exp
}

// RefreshFirebaseToken sends a refresh request to Firebase secure token API.
func RefreshFirebaseToken(apiKey, refreshToken string) (string, string, error) {
	apiURL := fmt.Sprintf("https://securetoken.googleapis.com/v1/token?key=%s", apiKey)

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	resp, err := http.Post(apiURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token refresh failed: %s", resp.Status)
	}

	var res struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", "", err
	}

	return res.IDToken, res.RefreshToken, nil
}
