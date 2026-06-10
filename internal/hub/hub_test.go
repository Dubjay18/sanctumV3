package hub

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/Dubjay/sanctum/pkg/types"
)

const sendBufferSize = 256

func TestHubRegisterAddsToRoom(t *testing.T) {
	hub, stopHub := startHub(t)
	client, cleanup := newTestClient(t, hub, "client-1", "Alice", "room-1")
	defer cleanup()

	hub.register <- client
	readWithTimeout(t, client.send, 200*time.Millisecond)
	drainWithTimeout(client.send, 50*time.Millisecond)

	stopHub()
	if roomClients, ok := hub.rooms["room-1"]; !ok || !roomClients[client] {
		t.Fatalf("expected client to be registered in room")
	}
}

func TestHubBroadcastToRoom(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	sender, cleanupSender := newTestClient(t, hub, "sender", "Sender", "room-1")
	defer cleanupSender()
	recipientA, cleanupA := newTestClient(t, hub, "recv-1", "Recv1", "room-1")
	defer cleanupA()
	recipientB, cleanupB := newTestClient(t, hub, "recv-2", "Recv2", "room-1")
	defer cleanupB()

	hub.register <- sender
	hub.register <- recipientA
	hub.register <- recipientB

	readWithTimeout(t, sender.send, 200*time.Millisecond)
	readWithTimeout(t, recipientA.send, 200*time.Millisecond)
	readWithTimeout(t, recipientB.send, 200*time.Millisecond)

	drainWithTimeout(sender.send, 50*time.Millisecond)
	drainWithTimeout(recipientA.send, 50*time.Millisecond)
	drainWithTimeout(recipientB.send, 50*time.Millisecond)

	hub.broadcast <- BroadcastMsg{Envelope: &types.Envelope{Type: types.TypeText, RoomID: "room-1", Payload: "hello"}, Sender: sender}

	ackMsg := readWithTimeout(t, sender.send, 200*time.Millisecond)
	ackEnv, _ := types.Unmarshal(ackMsg)
	if ackEnv.Type != types.TypeAck {
		t.Fatalf("expected TypeAck, got %s", ackEnv.Type)
	}

	msgA := readWithTimeout(t, recipientA.send, 200*time.Millisecond)
	msgB := readWithTimeout(t, recipientB.send, 200*time.Millisecond)

	assertEnvelopePayload(t, msgA, "hello")
	assertEnvelopePayload(t, msgB, "hello")
	assertNoMessage(t, sender.send, 50*time.Millisecond)
}

func TestHubUnregisterRemovesAndCleansRoom(t *testing.T) {
	hub, stopHub := startHub(t)
	client, cleanup := newTestClient(t, hub, "client-1", "Alice", "room-1")
	defer cleanup()

	hub.register <- client
	readWithTimeout(t, client.send, 200*time.Millisecond)
	drainWithTimeout(client.send, 50*time.Millisecond)

	hub.unregister <- client
	waitForClosed(t, client.send, 200*time.Millisecond)

	stopHub()
	if _, ok := hub.rooms["room-1"]; ok {
		t.Fatalf("expected room to be removed when empty")
	}
	if _, ok := hub.clients[client.ID]; ok {
		t.Fatalf("expected client to be removed from client map")
	}
}

func TestHubDMQueuesWhenOffline(t *testing.T) {
	hub, stopHub := startHub(t)
	client, cleanup := newTestClient(t, hub, "sender", "Sender", "room-1")
	defer cleanup()

	hub.register <- client
	readWithTimeout(t, client.send, 200*time.Millisecond)
	drainWithTimeout(client.send, 50*time.Millisecond)

	hub.broadcast <- BroadcastMsg{Envelope: &types.Envelope{Type: types.TypeDM, ToUID: "offline", Payload: "ping"}, Sender: client}
	readWithTimeout(t, client.send, 200*time.Millisecond)

	stopHub()
	if pending := hub.pendingDMs["offline"]; len(pending) != 1 {
		t.Fatalf("expected pending DM for offline user")
	}
}

func TestHubDMDeliveredOnNextConnect(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	sender, cleanupSender := newTestClient(t, hub, "sender", "Sender", "room-1")
	defer cleanupSender()

	hub.register <- sender
	readWithTimeout(t, sender.send, 200*time.Millisecond)
	drainWithTimeout(sender.send, 50*time.Millisecond)

	hub.broadcast <- BroadcastMsg{Envelope: &types.Envelope{Type: types.TypeDM, ToUID: "recipient", Payload: "queued"}, Sender: sender}
	readWithTimeout(t, sender.send, 200*time.Millisecond)

	recipient, cleanupRecipient := newTestClient(t, hub, "recipient", "Recipient", "room-1")
	defer cleanupRecipient()
	hub.register <- recipient

	found := false
	deadline := time.After(500 * time.Millisecond)
	for !found {
		select {
		case data := <-recipient.send:
			if env, err := types.Unmarshal(data); err == nil && env.Type == types.TypeDM && env.Payload == "queued" {
				found = true
			}
		case <-deadline:
			t.Fatalf("expected queued DM to be delivered after recipient connects")
		}
	}
}

func TestHubTypeTextOverwritesTimestampAndGeneratesID(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	sender, cleanupSender := newTestClient(t, hub, "sender", "Sender", "room-1")
	defer cleanupSender()
	recipient, cleanupRecipient := newTestClient(t, hub, "recipient", "Recipient", "room-1")
	defer cleanupRecipient()

	hub.register <- sender
	hub.register <- recipient

	readWithTimeout(t, sender.send, 200*time.Millisecond)
	readWithTimeout(t, recipient.send, 200*time.Millisecond)

	drainWithTimeout(sender.send, 50*time.Millisecond)
	drainWithTimeout(recipient.send, 50*time.Millisecond)

	// Send message with custom ID and a distinct non-zero timestamp
	customTimestamp := int64(12345)
	hub.broadcast <- BroadcastMsg{
		Envelope: &types.Envelope{
			Type:      types.TypeText,
			RoomID:    "room-1",
			Payload:   "test-timestamp",
			Timestamp: customTimestamp,
		},
		Sender: sender,
	}

	msg := readWithTimeout(t, recipient.send, 200*time.Millisecond)
	env, err := types.Unmarshal(msg)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if env.Timestamp == customTimestamp {
		t.Fatalf("expected timestamp to be overwritten by server, got %d", env.Timestamp)
	}
	if env.Timestamp == 0 {
		t.Fatalf("expected non-zero server-assigned timestamp, got 0")
	}
	if env.ID == "" {
		t.Fatalf("expected server to assign a UUID to message ID, got empty string")
	}

	// Send message with already set ID to verify it is NOT overwritten
	customID := "already-set-id"
	hub.broadcast <- BroadcastMsg{
		Envelope: &types.Envelope{
			ID:      customID,
			Type:    types.TypeText,
			RoomID:  "room-1",
			Payload: "test-id",
		},
		Sender: sender,
	}

	msg2 := readWithTimeout(t, recipient.send, 200*time.Millisecond)
	env2, err := types.Unmarshal(msg2)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if env2.ID != customID {
		t.Fatalf("expected ID to be preserved as %q, got %q", customID, env2.ID)
	}
}

func TestHubDMIsolation(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	alice, cleanupAlice := newTestClient(t, hub, "alice", "Alice", "general")
	defer cleanupAlice()
	bob, cleanupBob := newTestClient(t, hub, "bob", "Bob", "general")
	defer cleanupBob()
	charlie, cleanupCharlie := newTestClient(t, hub, "charlie", "Charlie", "general")
	defer cleanupCharlie()

	hub.register <- alice
	hub.register <- bob
	hub.register <- charlie

	// Drain register snapshots and presence updates
	readWithTimeout(t, alice.send, 200*time.Millisecond)
	readWithTimeout(t, bob.send, 200*time.Millisecond)
	readWithTimeout(t, charlie.send, 200*time.Millisecond)

	drainWithTimeout(alice.send, 50*time.Millisecond)
	drainWithTimeout(bob.send, 50*time.Millisecond)
	drainWithTimeout(charlie.send, 50*time.Millisecond)

	// Alice sends DM to Bob
	dmEnv := &types.Envelope{
		Type:    types.TypeDM,
		ToUID:   "bob",
		FromUID: "alice",
		Payload: "secret-dm",
	}
	hub.broadcast <- BroadcastMsg{Envelope: dmEnv, Sender: alice}

	// Confirm: bob receives it
	msgBob := readWithTimeout(t, bob.send, 200*time.Millisecond)
	assertEnvelopePayload(t, msgBob, "secret-dm")

	// Confirm: charlie receives nothing, alice receives TypeAck
	assertNoMessage(t, charlie.send, 100*time.Millisecond)
	ackMsg := readWithTimeout(t, alice.send, 200*time.Millisecond)
	ackEnv, _ := types.Unmarshal(ackMsg)
	if ackEnv.Type != types.TypeAck {
		t.Fatalf("expected TypeAck, got %s", ackEnv.Type)
	}
	assertNoMessage(t, alice.send, 100*time.Millisecond)

	// Confirm: room broadcast from alice still reaches Bob and Charlie (but not Alice herself)
	broadcastEnv := &types.Envelope{
		Type:    types.TypeText,
		RoomID:  "general",
		FromUID: "alice",
		Payload: "hello-room",
	}
	hub.broadcast <- BroadcastMsg{Envelope: broadcastEnv, Sender: alice}

	// Confirm: bob and charlie receive it
	msgBobBroadcast := readWithTimeout(t, bob.send, 200*time.Millisecond)
	msgCharlieBroadcast := readWithTimeout(t, charlie.send, 200*time.Millisecond)

	assertEnvelopePayload(t, msgBobBroadcast, "hello-room")
	assertEnvelopePayload(t, msgCharlieBroadcast, "hello-room")

	// Confirm: alice (sender) receives TypeAck
	ackMsg2 := readWithTimeout(t, alice.send, 200*time.Millisecond)
	ackEnv2, _ := types.Unmarshal(ackMsg2)
	if ackEnv2.Type != types.TypeAck {
		t.Fatalf("expected TypeAck, got %s", ackEnv2.Type)
	}
	assertNoMessage(t, alice.send, 100*time.Millisecond)
}

func TestHubGoroutineLeakCheck(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	hub, stopHub := startHub(t)
	client, cleanup := newTestClient(t, hub, "client", "Client", "room-1")
	defer cleanup()

	go client.readPump()
	go client.writePump()

	hub.register <- client
	readWithTimeout(t, client.send, 200*time.Millisecond)
	drainWithTimeout(client.send, 50*time.Millisecond)

	hub.unregister <- client
	waitForClosed(t, client.send, 200*time.Millisecond)
	stopHub()

	time.Sleep(100 * time.Millisecond)
}

func BenchmarkHubBroadcast(b *testing.B) {
	hub, stopHub := startHub(b)
	defer stopHub()

	clients := make([]*Client, 0, 100)
	drainDone := make(chan struct{})
	for i := 0; i < 100; i++ {
		client, cleanup := newTestClient(b, hub, "client-"+strconv.Itoa(i), "User", "room-bench")
		b.Cleanup(cleanup)
		clients = append(clients, client)
		hub.register <- client
		readWithTimeout(b, client.send, 200*time.Millisecond)
		go drainUntil(drainDone, client.send)
	}

	sender := clients[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env := &types.Envelope{Type: types.TypeText, RoomID: "room-bench", Payload: "bench"}
		hub.broadcast <- BroadcastMsg{Envelope: env, Sender: sender}
	}
	b.StopTimer()

	close(drainDone)
}

func startHub(tb testing.TB) (*Hub, func()) {
	tb.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := NewHub()
	go hub.RunWithContext(ctx)
	return hub, func() {
		cancel()
		<-hub.Done()
	}
}

func newTestClient(tb testing.TB, hub *Hub, id, name, roomID string) (*Client, func()) {
	tb.Helper()
	conn, cleanup := newMockWebsocketConn(tb)
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		ID:     id,
		Name:   name,
		conn:   conn,
		send:   make(chan []byte, sendBufferSize),
		hub:    hub,
		roomID: roomID,
		ctx:    ctx,
		cancel: cancel,
	}
	return client, cleanup
}

func newMockWebsocketConn(tb testing.TB) (*websocket.Conn, func()) {
	tb.Helper()
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan struct{})

	go func() {
		defer close(serverDone)
		reader := bufio.NewReader(serverConn)
		tp := textproto.NewReader(reader)
		_, err := tp.ReadLine()
		if err != nil {
			return
		}
		headers, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		key := headers.Get("Sec-WebSocket-Key")
		accept := websocketAcceptKey(key)
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		_, _ = serverConn.Write([]byte(response))
		_, _ = io.Copy(io.Discard, serverConn)
	}()

	ws, _, err := websocket.NewClient(clientConn, &url.URL{Scheme: "ws", Host: "example.com", Path: "/"}, http.Header{}, 1024, 1024)
	if err != nil {
		tb.Fatalf("failed to create websocket client: %v", err)
	}

	cleanup := func() {
		_ = ws.Close()
		_ = serverConn.Close()
		<-serverDone
	}

	return ws, cleanup
}

func websocketAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func readWithTimeout(tb testing.TB, ch <-chan []byte, timeout time.Duration) []byte {
	tb.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		tb.Fatalf("timed out waiting for message")
		return nil
	}
}

func assertNoMessage(t *testing.T, ch <-chan []byte, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("did not expect message")
	case <-time.After(timeout):
	}
}

func drainWithTimeout(ch <-chan []byte, timeout time.Duration) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			continue
		case <-time.After(timeout):
			return
		}
	}
}

func drainUntil(done <-chan struct{}, ch <-chan []byte) {
	for {
		select {
		case <-done:
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
		}
	}
}

func waitForClosed(tb testing.TB, ch <-chan []byte, timeout time.Duration) {
	tb.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			select {
			case <-time.After(timeout):
				tb.Fatalf("expected channel to close")
			case _, ok = <-ch:
				if ok {
					tb.Fatalf("expected channel to close")
				}
			}
		}
	case <-time.After(timeout):
		tb.Fatalf("expected channel to close")
	}
}

func assertEnvelopePayload(t *testing.T, msg []byte, expected string) {
	t.Helper()
	env, err := types.Unmarshal(msg)
	if err != nil {
		t.Fatalf("failed to unmarshal envelope: %v", err)
	}
	if env.Payload != expected {
		t.Fatalf("expected payload %q, got %q", expected, env.Payload)
	}
}

func TestHubPublicKeyRegistry(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	client, cleanup := newTestClient(t, hub, "alice", "Alice", "general")
	defer cleanup()

	hub.register <- client
	readWithTimeout(t, client.send, 200*time.Millisecond)

	// Simulate JOIN_ROOM payload processing as done by c.readPump():
	testKey := []byte("this-is-a-32-byte-public-key-val")
	client.mu.Lock()
	client.PublicKey = testKey
	client.mu.Unlock()

	hub.register <- client
	time.Sleep(50 * time.Millisecond)

	// Verify that hub has stored the public key
	gotKey, ok := hub.GetPublicKey("alice")
	if !ok {
		t.Fatalf("expected public key to be registered in hub")
	}
	if string(gotKey) != string(testKey) {
		t.Fatalf("expected public key %q, got %q", testKey, gotKey)
	}

	// Unregister client
	drainWithTimeout(client.send, 50*time.Millisecond)
	hub.unregister <- client
	waitForClosed(t, client.send, 200*time.Millisecond)

	// Verify that key is NOT deleted on unregister
	gotKey2, ok := hub.GetPublicKey("alice")
	if !ok {
		t.Fatalf("expected public key to persist after unregister")
	}
	if string(gotKey2) != string(testKey) {
		t.Fatalf("expected persistent key %q, got %q", testKey, gotKey2)
	}
}
