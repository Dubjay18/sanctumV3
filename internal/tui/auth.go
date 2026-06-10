package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// getAuthHost returns the dynamic protocol and host based on FIREBASE_AUTH_EMULATOR_HOST if set.
func getAuthHost(defaultHost string) string {
	if host := os.Getenv("FIREBASE_AUTH_EMULATOR_HOST"); host != "" {
		return "http://" + host + "/" + defaultHost
	}
	return "https://" + defaultHost
}

// getAPIKey retrieves the Firebase Auth API key from the environment or configuration.
func getAPIKey() string {
	apiKey := os.Getenv("FIREBASE_API_KEY")
	if apiKey == "" {
		if cfg, err := LoadConfig(DefaultConfigPath()); err == nil && cfg.APIKey != "" {
			apiKey = cfg.APIKey
		}
	}
	// Fallback for emulator (API key doesn't matter, but cannot be blank in helpers)
	if apiKey == "" && os.Getenv("FIREBASE_AUTH_EMULATOR_HOST") != "" {
		apiKey = "dummy-api-key"
	}
	return apiKey
}

// SignIn authenticates a user using their email and password.
func SignIn(email, password string) (idToken, refreshToken, uid string, err error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return "", "", "", fmt.Errorf("FIREBASE_API_KEY is not configured")
	}

	apiURL := fmt.Sprintf("%s/v1/accounts:signInWithPassword?key=%s", getAuthHost("identitytoolkit.googleapis.com"), apiKey)

	reqBody, err := json.Marshal(map[string]interface{}{
		"email":             email,
		"password":          password,
		"returnSecureToken": true,
	})
	if err != nil {
		return "", "", "", err
	}

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		errMsg := errResp.Error.Message
		if errMsg == "" {
			errMsg = resp.Status
		}
		return "", "", "", fmt.Errorf("sign-in failed: %s", errMsg)
	}

	var res struct {
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		LocalID      string `json:"localId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", "", "", err
	}

	return res.IDToken, res.RefreshToken, res.LocalID, nil
}

// Register signs up a new user and updates their displayName.
func Register(email, password, displayName string) (idToken, refreshToken, uid string, err error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return "", "", "", fmt.Errorf("FIREBASE_API_KEY is not configured")
	}

	// 1. Sign Up
	signUpURL := fmt.Sprintf("%s/v1/accounts:signUp?key=%s", getAuthHost("identitytoolkit.googleapis.com"), apiKey)
	signUpBody, err := json.Marshal(map[string]interface{}{
		"email":             email,
		"password":          password,
		"returnSecureToken": true,
	})
	if err != nil {
		return "", "", "", err
	}

	resp, err := http.Post(signUpURL, "application/json", bytes.NewBuffer(signUpBody))
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		errMsg := errResp.Error.Message
		if errMsg == "" {
			errMsg = resp.Status
		}
		return "", "", "", fmt.Errorf("sign-up failed: %s", errMsg)
	}

	var signUpRes struct {
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		LocalID      string `json:"localId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&signUpRes); err != nil {
		return "", "", "", err
	}

	// 2. Set Display Name
	updateURL := fmt.Sprintf("%s/v1/accounts:update?key=%s", getAuthHost("identitytoolkit.googleapis.com"), apiKey)
	updateBody, err := json.Marshal(map[string]interface{}{
		"idToken":           signUpRes.IDToken,
		"displayName":       displayName,
		"returnSecureToken": true,
	})
	if err != nil {
		return signUpRes.IDToken, signUpRes.RefreshToken, signUpRes.LocalID, err
	}

	req, err := http.NewRequest("POST", updateURL, bytes.NewBuffer(updateBody))
	if err != nil {
		return signUpRes.IDToken, signUpRes.RefreshToken, signUpRes.LocalID, err
	}
	req.Header.Set("Content-Type", "application/json")

	updateResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return signUpRes.IDToken, signUpRes.RefreshToken, signUpRes.LocalID, err
	}
	defer updateResp.Body.Close()

	if updateResp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(updateResp.Body).Decode(&errResp)
		errMsg := errResp.Error.Message
		if errMsg == "" {
			errMsg = updateResp.Status
		}
		return signUpRes.IDToken, signUpRes.RefreshToken, signUpRes.LocalID, fmt.Errorf("failed to set display name: %s", errMsg)
	}


	// 3. Sign in again to get a fresh token containing the display name claim
	signInToken, signInRefresh, _, err := SignIn(email, password)
	if err == nil && signInToken != "" {
		return signInToken, signInRefresh, signUpRes.LocalID, nil
	}

	return signUpRes.IDToken, signUpRes.RefreshToken, signUpRes.LocalID, nil
}

// RefreshToken requests a new Firebase ID Token using a refresh token.
func RefreshToken(refreshToken string) (newIDToken string, err error) {
	apiKey := getAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("FIREBASE_API_KEY is not configured")
	}

	apiURL := fmt.Sprintf("%s/v1/token?key=%s", getAuthHost("securetoken.googleapis.com"), apiKey)

	reqBody, err := json.Marshal(map[string]interface{}{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return "", err
	}

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		errMsg := errResp.Error.Message
		if errMsg == "" {
			errMsg = resp.Status
		}
		return "", fmt.Errorf("token refresh failed: %s", errMsg)
	}

	var res struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	return res.IDToken, nil
}
