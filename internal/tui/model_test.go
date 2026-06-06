package tui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/pkg/types"
	tea "github.com/charmbracelet/bubbletea"
)

func TestChatModel_Update_Resize(t *testing.T) {
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice")

	// Test standard resizing
	msg := tea.WindowSizeMsg{Width: 80, Height: 24}
	newModel, cmd := model.Update(msg)
	if cmd != nil {
		t.Fatalf("expected nil cmd from window size update, got %v", cmd)
	}

	chatModel, ok := newModel.(ChatModel)
	if !ok {
		t.Fatalf("expected model to be ChatModel")
	}

	if chatModel.width != 80 || chatModel.height != 24 {
		t.Errorf("expected dimensions 80x24, got %dx%d", chatModel.width, chatModel.height)
	}

	// Test extremely small dimensions (corner cases/resizing down to zero) to prevent crashes
	smallResizes := []tea.WindowSizeMsg{
		{Width: 0, Height: 0},
		{Width: 1, Height: 1},
		{Width: 10, Height: 2},
		{Width: -5, Height: -5},
	}

	for _, resizeMsg := range smallResizes {
		// Verify no panic/crash occurs
		_, _ = chatModel.Update(resizeMsg)
	}
}

func TestChatModel_Update_WsMessages(t *testing.T) {
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice")

	// Test regular text message envelope
	env := &types.Envelope{
		Type:     types.TypeText,
		FromName: "Bob",
		Payload:  "Hello Alice",
	}
	envBytes, err := types.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal envelope: %v", err)
	}

	newModel, _ := model.Update(WsMessageMsg{Data: envBytes})
	chatModel := newModel.(ChatModel)

	if len(chatModel.messages) != 1 || chatModel.messages[0] != "Bob: Hello Alice" {
		t.Errorf("expected message 'Bob: Hello Alice', got %v", chatModel.messages)
	}

	// Test presence update envelope
	presenceUpdate := types.PresenceUpdate{
		UID:   "bob",
		Name:  "Bob",
		State: types.PresenceOnline,
	}
	presencePayload, _ := json.Marshal(presenceUpdate)
	presenceEnv := &types.Envelope{
		Type:    types.TypePresenceUpdate,
		Payload: string(presencePayload),
	}
	presenceBytes, _ := types.Marshal(presenceEnv)

	newModel, _ = chatModel.Update(WsMessageMsg{Data: presenceBytes})
	chatModel = newModel.(ChatModel)

	if chatModel.users["Bob"] != types.PresenceOnline {
		t.Errorf("expected Bob to be online, got state %q", chatModel.users["Bob"])
	}

	// Test disconnect message
	newModel, _ = chatModel.Update(WsDisconnectedMsg{})
	chatModel = newModel.(ChatModel)
	if chatModel.status != "Offline" {
		t.Errorf("expected status to be 'Offline', got %q", chatModel.status)
	}
}

func TestChatModel_Update_Keys(t *testing.T) {
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice")

	// Enter key (sending empty message should not fail or send)
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	chatModel := newModel.(ChatModel)
	if len(chatModel.messages) != 0 {
		t.Errorf("expected no messages sent for empty input, got %d", len(chatModel.messages))
	}

	// Ctrl+C should quit
	_, cmd := chatModel.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Errorf("expected non-nil cmd for Ctrl+C")
	}
	// Verify that the command is indeed a tea.Quit command
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg from command, got %T", msg)
	}
}

func TestWSClient_FetchPublicKey(t *testing.T) {
	// Start a local HTTP test server simulating the keys registry
	testUID := "user-1"
	testKeyBytes := []byte("a-mock-32-byte-public-key-bytes-")
	testKeyBase64 := base64.StdEncoding.EncodeToString(testKeyBytes)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys/user-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"uid":"user-1","public_key":"` + testKeyBase64 + `"}`))
	}))
	defer ts.Close()

	wsClient := &WSClient{
		keyring: make(map[string][]byte),
	}

	// Fetch public key
	pubKey, err := wsClient.FetchPublicKey(ts.URL, testUID)
	if err != nil {
		t.Fatalf("failed to fetch public key: %v", err)
	}

	if string(pubKey) != string(testKeyBytes) {
		t.Errorf("expected public key %s, got %s", testKeyBytes, pubKey)
	}

	// Verify caching
	cachedKey, ok := wsClient.keyring[testUID]
	if !ok {
		t.Errorf("expected key to be cached in keyring")
	}
	if string(cachedKey) != string(testKeyBytes) {
		t.Errorf("cached key does not match")
	}
}

func TestTUI_E2EDM(t *testing.T) {
	// 1. Generate keys
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()

	// 2. Bob's TUI model
	bobWS := &WSClient{
		PublicKey:  pubBob,
		PrivateKey: privBob,
		keyring:    make(map[string][]byte),
	}
	bobWS.keyring["alice"] = pubAlice[:]

	bobModel := NewChatModel(bobWS, "Bob")
	bobModel.mode = ModeDM
	bobModel.dmTarget = "alice"

	// 3. Alice encrypts a DM to Bob
	plaintext := []byte("hello bob, this is encrypted!")
	ciphertext, nonce, err := crypto.EncryptDM(plaintext, privAlice, pubBob)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	// Confirm that ciphertext is different from plaintext
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatalf("ciphertext should not match plaintext")
	}

	// 4. Create the incoming DM Envelope (as if received from WebSocket)
	env := &types.Envelope{
		Type:    types.TypeDM,
		FromUID: "alice",
		ToUID:   "bob",
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
	}

	data, err := types.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// 5. Inject into Bob's TUI update loop
	newModel, _ := bobModel.Update(WsMessageMsg{Data: data})
	chatModel := newModel.(ChatModel)

	// Bob should have decrypted it successfully!
	if len(chatModel.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(chatModel.messages))
	}

	expectedMsg := "DM from alice: hello bob, this is encrypted!"
	if chatModel.messages[0] != expectedMsg {
		t.Errorf("expected %q, got %q", expectedMsg, chatModel.messages[0])
	}

	// 6. Test decryption failure (e.g. tampered payload)
	env.Payload = base64.StdEncoding.EncodeToString([]byte("tampered-bytes"))
	data2, _ := types.Marshal(env)

	newModel2, _ := chatModel.Update(WsMessageMsg{Data: data2})
	chatModel2 := newModel2.(ChatModel)

	if len(chatModel2.messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(chatModel2.messages))
	}

	// The decryption error should be displayed (and contain "[decryption failed]")
	if !strings.Contains(chatModel2.messages[1], "[decryption failed]") {
		t.Errorf("expected message to indicate decryption failure, got: %q", chatModel2.messages[1])
	}
}

func TestTUI_GroupEncryptionAndPolish(t *testing.T) {
	// 1. Setup keys
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()

	// 2. Setup Bob's TUI model
	bobWS := &WSClient{
		PublicKey:  pubBob,
		PrivateKey: privBob,
		keyring:    make(map[string][]byte),
	}
	bobWS.keyring["alice"] = pubAlice[:]

	bobModel := NewChatModel(bobWS, "Bob")

	// 3. Test Tab key focuses panel toggle
	if bobModel.focusedPanel != PanelChat {
		t.Fatalf("expected focusedPanel to start as PanelChat")
	}

	newModel, _ := bobModel.Update(tea.KeyMsg{Type: tea.KeyTab})
	bobModel = newModel.(ChatModel)
	if bobModel.focusedPanel != PanelSidebar {
		t.Errorf("expected Tab to switch focus to PanelSidebar")
	}

	newModel, _ = bobModel.Update(tea.KeyMsg{Type: tea.KeyTab})
	bobModel = newModel.(ChatModel)
	if bobModel.focusedPanel != PanelChat {
		t.Errorf("expected Tab to switch focus back to PanelChat")
	}

	// 4. Test Ctrl+D focuses DM section in sidebar
	newModel, _ = bobModel.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	bobModel = newModel.(ChatModel)
	if bobModel.focusedPanel != PanelSidebar || bobModel.focusedSection != SectionDMs {
		t.Errorf("expected Ctrl+D to focus sidebar DMs section")
	}

	// 5. Test group decryption (Bob is recipient)
	bobModel.mode = ModeRoom
	bobModel.activeRoom = "general"

	// Alice sends group message encrypted for Bob
	recipients := map[string]*[32]byte{
		"Bob": pubBob,
	}
	encryptedPayloads, err := crypto.EncryptGroup([]byte("secret group content"), privAlice, recipients)
	if err != nil {
		t.Fatalf("failed to encrypt group message: %v", err)
	}

	groupEnv := &types.Envelope{
		Type:              types.TypeText,
		FromUID:           "alice",
		FromName:          "Alice",
		RoomID:            "general",
		EncryptedPayloads: encryptedPayloads,
	}

	groupEnvBytes, _ := types.Marshal(groupEnv)
	newModel, _ = bobModel.Update(WsMessageMsg{Data: groupEnvBytes})
	bobModel = newModel.(ChatModel)

	if len(bobModel.messages) != 1 {
		t.Fatalf("expected 1 message in general room history, got %d", len(bobModel.messages))
	}
	if !strings.Contains(bobModel.messages[0], "secret group content") {
		t.Errorf("expected Bob to decrypt group message, got: %q", bobModel.messages[0])
	}

	// 6. Test group decryption (Bob is NOT a recipient)
	// Alice sends group message encrypted ONLY for Charlie
	pubCharlie, _, _ := crypto.GenerateKeypair()
	recipientsCharlieOnly := map[string]*[32]byte{
		"Charlie": pubCharlie,
	}
	encryptedPayloads2, _ := crypto.EncryptGroup([]byte("secret charlie content"), privAlice, recipientsCharlieOnly)

	groupEnv2 := &types.Envelope{
		Type:              types.TypeText,
		FromUID:           "alice",
		FromName:          "Alice",
		RoomID:            "general",
		EncryptedPayloads: encryptedPayloads2,
	}

	groupEnvBytes2, _ := types.Marshal(groupEnv2)
	newModel, _ = bobModel.Update(WsMessageMsg{Data: groupEnvBytes2})
	bobModel = newModel.(ChatModel)

	if len(bobModel.messages) != 2 {
		t.Fatalf("expected 2 messages in general room history, got %d", len(bobModel.messages))
	}
	if !strings.Contains(bobModel.messages[1], "[message not for you]") {
		t.Errorf("expected Bob to receive 'not for you' indicator, got: %q", bobModel.messages[1])
	}

	// 7. Test unread badge increments on receiving message in background room
	bgEnv := &types.Envelope{
		Type:     types.TypeText,
		FromUID:  "alice",
		FromName: "Alice",
		RoomID:   "random",
		Payload:  "unencrypted random content",
	}
	bgEnvBytes, _ := types.Marshal(bgEnv)
	newModel, _ = bobModel.Update(WsMessageMsg{Data: bgEnvBytes})
	bobModel = newModel.(ChatModel)

	if bobModel.unreadCounts["random"] != 1 {
		t.Errorf("expected unread counts for random room to be 1, got %d", bobModel.unreadCounts["random"])
	}
}
