package firebase

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestAuthMiddleware_ExpiredToken(t *testing.T) {
	// Start a mock server to capture emulator requests if any
	mockAuthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"users": [{"localId": "test-uid", "disabled": false}]}`))
	}))
	defer mockAuthServer.Close()

	emulatorHost := strings.TrimPrefix(mockAuthServer.URL, "http://")
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", emulatorHost)

	credentialsPath := "../../secrets/credentials.json"
	_, authClient, err := InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}

	// Generate a dummy RSA key for RS256 signing
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA private key: %v", err)
	}

	// 1. Create a mock token that is expired
	expiredClaims := jwt.MapClaims{
		"iss":  "https://securetoken.google.com/sanctum-d89ce",
		"aud":  "sanctum-d89ce",
		"sub":  "test-uid",
		"exp":  time.Now().Unix() - 3600, // 1 hour ago
		"iat":  time.Now().Unix() - 7200,
		"name": "Test User",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, expiredClaims)
	expiredTokenString, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	// Verify VerifyIDToken returns error
	_, err = authClient.VerifyIDToken(context.Background(), expiredTokenString)
	if err == nil {
		t.Errorf("expected error for expired token, got nil")
	}

	// 2. Set up httptest server with AuthMiddleware
	handler := AuthMiddleware(authClient, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+expiredTokenString)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 Unauthorized, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	// Start a mock server to capture emulator requests and return valid user lookup
	mockAuthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"users": [
				{
					"localId": "test-uid-valid",
					"displayName": "Valid User",
					"disabled": false
				}
			]
		}`))
	}))
	defer mockAuthServer.Close()

	emulatorHost := strings.TrimPrefix(mockAuthServer.URL, "http://")
	t.Setenv("FIREBASE_AUTH_EMULATOR_HOST", emulatorHost)

	credentialsPath := "../../secrets/credentials.json"
	_, authClient, err := InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}

	// Generate a dummy RSA key for RS256 signing
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA private key: %v", err)
	}

	// 1. Create a mock token that is valid (expires in the future)
	validClaims := jwt.MapClaims{
		"iss":  "https://securetoken.google.com/sanctum-d89ce",
		"aud":  "sanctum-d89ce",
		"sub":  "test-uid-valid",
		"exp":  time.Now().Unix() + 1000, // valid in future
		"iat":  time.Now().Unix() - 10,
		"name": "Valid User",
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, validClaims)
	validTokenString, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	// Verify VerifyIDToken succeeds
	decoded, err := authClient.VerifyIDToken(context.Background(), validTokenString)
	if err != nil {
		t.Fatalf("expected valid token verification to succeed, got: %v", err)
	}
	if decoded.UID != "test-uid-valid" {
		t.Errorf("expected UID 'test-uid-valid', got '%s'", decoded.UID)
	}

	// 2. Set up httptest server with AuthMiddleware and verify context claims injection
	handler := AuthMiddleware(authClient, func(w http.ResponseWriter, r *http.Request) {
		uid, _ := r.Context().Value(ContextKeyUID).(string)
		name, _ := r.Context().Value(ContextKeyName).(string)
		w.Header().Set("X-Test-UID", uid)
		w.Header().Set("X-Test-Name", name)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+validTokenString)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200 OK, got %d", rec.Code)
	}
	if rec.Header().Get("X-Test-UID") != "test-uid-valid" {
		t.Errorf("expected injected UID 'test-uid-valid', got '%s'", rec.Header().Get("X-Test-UID"))
	}
	if rec.Header().Get("X-Test-Name") != "Valid User" {
		t.Errorf("expected injected name 'Valid User', got '%s'", rec.Header().Get("X-Test-Name"))
	}
}
