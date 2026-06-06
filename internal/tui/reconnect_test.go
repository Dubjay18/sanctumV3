package tui

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"

	"github.com/Dubjay/sanctum/internal/crypto"
)

type dummyModel struct {
	msgs chan tea.Msg
}

func (m dummyModel) Init() tea.Cmd { return nil }
func (m dummyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	select {
	case m.msgs <- msg:
	default:
	}
	if _, ok := msg.(WsConnectedMsg); ok {
		return m, tea.Quit
	}
	return m, nil
}
func (m dummyModel) View() string { return "" }

func TestIntegration_Reconnection(t *testing.T) {
	var connectCount int
	var mu sync.Mutex

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectCount++
		mu.Unlock()

		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					break
				}
			}
		}
	})

	// Bind to a free port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := l.Addr().String()

	ts1 := httptest.NewUnstartedServer(handler)
	ts1.Listener = l
	ts1.Start()

	wsURL := "ws://" + addr

	// Connect client
	pub, priv, _ := crypto.GenerateKeypair()
	wsClient, err := Connect(wsURL)
	if err != nil {
		t.Fatalf("failed to connect initial client: %v", err)
	}
	wsClient.PublicKey = pub
	wsClient.PrivateKey = priv

	mu.Lock()
	if connectCount != 1 {
		t.Errorf("expected 1 connection, got %d", connectCount)
	}
	mu.Unlock()

	// Stop server 1 (simulate crash)
	ts1.Close()

	// Create a dummy Bubble Tea program structure to receive messages
	msgsChan := make(chan tea.Msg, 10)
	var in bytes.Buffer
	var out bytes.Buffer
	p := tea.NewProgram(dummyModel{msgs: msgsChan}, tea.WithInput(&in), tea.WithOutput(&out))
	wsClient.Program = p

	go func() {
		_, _ = p.Run()
	}()

	// Start reconnectLoop in a background context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go wsClient.Listen() // returns because connection is closed
	go wsClient.reconnectLoop(ctx)

	// Confirm client enters reconnect state
	var reconnecting bool
	deadline := time.After(3 * time.Second)
	for !reconnecting {
		select {
		case msg := <-msgsChan:
			if _, ok := msg.(WsReconnectingMsg); ok {
				reconnecting = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for reconnect state")
		}
	}

	// Start server 2 on the same address
	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("failed to listen on same port: %v", err)
	}
	ts2 := httptest.NewUnstartedServer(handler)
	ts2.Listener = l2
	ts2.Start()
	defer ts2.Close()

	// Wait for successful reconnection message
	deadline2 := time.After(8 * time.Second)
	var reconnected bool
	for !reconnected {
		select {
		case msg := <-msgsChan:
			if _, ok := msg.(WsConnectedMsg); ok {
				reconnected = true
			}
		case <-deadline2:
			t.Fatalf("timed out waiting for reconnection")
		}
	}

	mu.Lock()
	if connectCount != 2 {
		t.Errorf("expected 2 connections total, got %d", connectCount)
	}
	mu.Unlock()
}
