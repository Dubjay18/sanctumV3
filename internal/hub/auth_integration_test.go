package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/internal/tui"
)

func randomEmail() string {
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	return fmt.Sprintf("test-%s@example.com", hex.EncodeToString(bytes))
}

func TestIntegration_AuthHandshake(t *testing.T) {
	// Verify no goroutine leaks after the test ends
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)

	defer http.DefaultClient.CloseIdleConnections()
	defer func() {
		if tr, ok := http.DefaultTransport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}()

	t.Setenv("ENV", "development")

	credentialsPath := "../../secrets/credentials.json"
	fsClient, authClient, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}
	defer fsClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize Hub
	chatHub := NewHub()
	chatHub.SetFirestoreClient(fsClient)
	go chatHub.RunWithContext(ctx)

	// 1. Register a new user via the REST API helper
	email := randomEmail()
	password := "secure-password"
	displayName := "Integration User"

	t.Logf("Registering test user via REST API: %s", email)
	idToken, _, uid, err := tui.Register(email, password, displayName)
	if err != nil {
		t.Fatalf("REST registration failed: %v", err)
	}

	if idToken == "" || uid == "" {
		t.Fatalf("registration succeeded but returned empty token/uid")
	}
	t.Logf("Registration successful: uid=%s", uid)

	// 2. Set up WebSocket server with AuthMiddleware
	wsHandler := func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}

		tokenUID, _ := r.Context().Value(firebase.ContextKeyUID).(string)
		name, _ := r.Context().Value(firebase.ContextKeyName).(string)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		client := NewClient(chatHub, conn, tokenUID, name, "")
		client.Start()
	}

	server := httptest.NewServer(firebase.AuthMiddleware(authClient, wsHandler))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse server URL: %v", err)
	}
	serverURL.Scheme = "ws"
	serverURL.Path = "/ws"

	// 3. Connect to WS server with the valid ID token
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+idToken)

	t.Log("Connecting to WebSocket with valid token...")
	conn, resp, err := websocket.DefaultDialer.Dial(serverURL.String(), headers)
	if err != nil {
		t.Fatalf("failed to connect with valid token: %v", err)
	}
	defer conn.Close()
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected switching protocols status (101), got: %d", resp.StatusCode)
	}

	// Verify that client is registered on the Hub and its UID is correctly populated
	time.Sleep(100 * time.Millisecond) // wait for registration propagation
	client, exists := chatHub.GetClient(uid)
	if !exists {
		t.Errorf("expected client with uid %s to be registered on the hub, but wasn't found", uid)
	} else {
		if client.ID != uid {
			t.Errorf("client ID mismatch: expected %s, got %s", uid, client.ID)
		}
		if client.Name != displayName {
			t.Errorf("client Name mismatch: expected %s, got %s", displayName, client.Name)
		}
	}

	// Close valid connection to trigger unregister cleanup
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	time.Sleep(50 * time.Millisecond)

	// 4. Connect with a tampered token (change one character) — verify 401 rejection
	tamperedToken := "invalid_token_format_to_fail_parsing"
	headersTampered := http.Header{}
	headersTampered.Set("Authorization", "Bearer "+tamperedToken)

	t.Log("Connecting to WebSocket with tampered token...")
	_, resp2, err := websocket.DefaultDialer.Dial(serverURL.String(), headersTampered)
	if resp2 != nil && resp2.Body != nil {
		defer resp2.Body.Close()
	}
	if err == nil {
		t.Errorf("expected connection with tampered token to fail, but it succeeded")
	} else {
		if resp2 == nil {
			t.Fatalf("expected dial response but got nil")
		}
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized, got %d", resp2.StatusCode)
		}
	}
}
