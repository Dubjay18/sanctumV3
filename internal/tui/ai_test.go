package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dubjay/sanctum/pkg/types"
	tea "github.com/charmbracelet/bubbletea"
)

func TestBuildAIContext(t *testing.T) {
	messages := []string{
		"User1: Hello",
		"User2: Hi there",
		"User1: How's it going?",
	}
	prompt := "What is the weather?"
	got := BuildAIContext(messages, prompt)
	want := "Context from Sanctum chat:\nUser1: Hello\nUser2: Hi there\nUser1: How's it going?\n\nUser question: What is the weather?"

	if got != want {
		t.Errorf("expected:\n%s\ngot:\n%s", want, got)
	}

	// Test slice truncation to last 20
	largeMessages := make([]string, 30)
	for i := 0; i < 30; i++ {
		largeMessages[i] = "msg"
	}
	gotLarge := BuildAIContext(largeMessages, "prompt")
	if strings.Count(gotLarge, "msg") != 20 {
		t.Errorf("expected exactly 20 messages in context, got %d", strings.Count(gotLarge, "msg"))
	}
}

func TestQueryGemini(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		if r.Header.Get("x-goog-api-key") != "mock-key" {
			t.Errorf("expected x-goog-api-key header, got %s", r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"candidates": [
				{
					"content": {
						"parts": [
							{"text": "Hello from Gemini"}
						]
					}
				}
			]
		}`))
	}))
	defer ts.Close()

	_, err := QueryGemini("", "prompt")
	if err == nil {
		t.Errorf("expected error with empty apiKey")
	}
}

func TestAISetupStateMachine(t *testing.T) {
	// Set up model
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice", "Alice", nil, nil)

	// Step 1: Input "/ai setup"
	model.input.SetValue("/ai setup")
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m := newModel.(ChatModel)

	if m.aiSetupState != AISetupProvider {
		t.Fatalf("expected state AISetupProvider, got %v", m.aiSetupState)
	}
	if !strings.Contains(m.input.Prompt, "Provider?") {
		t.Errorf("expected prompt to ask for provider, got %q", m.input.Prompt)
	}

	// Step 2: Input invalid provider
	m.input.SetValue("invalid")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)
	if m.aiSetupState != AISetupProvider {
		t.Fatalf("expected state to remain AISetupProvider, got %v", m.aiSetupState)
	}

	// Step 3: Input valid provider "gemini"
	m.input.SetValue("gemini")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)
	if m.aiSetupState != AISetupKey {
		t.Fatalf("expected state AISetupKey, got %v", m.aiSetupState)
	}
	if m.aiSetupProvider != "gemini" {
		t.Errorf("expected provider 'gemini', got %s", m.aiSetupProvider)
	}
	if !strings.Contains(m.input.Prompt, "API Key:") {
		t.Errorf("expected prompt to ask for API Key, got %q", m.input.Prompt)
	}

	// Step 4: Input API Key
	m.input.SetValue("my-gemini-key")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)
	if m.aiSetupState != AISetupNone {
		t.Fatalf("expected state AISetupNone, got %v", m.aiSetupState)
	}

	// Check if config saved correctly
	cfg, err := LoadConfig(DefaultConfigPath())
	if err == nil {
		if cfg.AIProvider != "gemini" || cfg.AIAPIKey != "my-gemini-key" {
			t.Errorf("config not updated: provider=%s, key=%s", cfg.AIProvider, cfg.AIAPIKey)
		}
	}
}

func TestAICommandsAndWarning(t *testing.T) {
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice", "Alice", nil, nil)

	// Test /ai off
	model.input.SetValue("/ai off")
	newModel, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m := newModel.(ChatModel)
	if m.aiEnabled {
		t.Errorf("expected aiEnabled to be false after /ai off")
	}

	// Test /ai on
	m.input.SetValue("/ai on")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)
	if !m.aiEnabled {
		t.Errorf("expected aiEnabled to be true after /ai on")
	}

	// Test /ai clear
	m.input.SetValue("/ai clear")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)
	if len(m.aiContext) != 0 || !m.aiContextCleared {
		t.Errorf("expected aiContext to be cleared")
	}

	// Set configuration values for trigger test
	cfgPath := DefaultConfigPath()
	dir := filepath.Dir(cfgPath)
	_ = os.MkdirAll(dir, 0700)
	cfg := Config{
		AIProvider: "gemini",
		AIAPIKey:   "dummy-key",
		AIWarned:   false,
	}
	_ = SaveConfig(cfg, cfgPath)

	// Test @ai trigger first time (shows warning)
	m.input.SetValue("@ai hello")
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(ChatModel)

	if !m.showAIWarning {
		t.Fatalf("expected showAIWarning to be true")
	}
	if m.aiPendingPrompt != "hello" {
		t.Errorf("expected pending prompt 'hello', got %s", m.aiPendingPrompt)
	}

	// Test warning rejection "N"
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = newModel.(ChatModel)
	if m.showAIWarning {
		t.Errorf("expected warning to close after N")
	}

	// Trigger warning again
	m.showAIWarning = true
	m.aiPendingPrompt = "hello"

	// Test warning acceptance "Y"
	// Mock wsClient Program
	m.wsClient.Program = tea.NewProgram(m)
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = newModel.(ChatModel)
	if m.showAIWarning {
		t.Errorf("expected warning to close after Y")
	}
	if cmd == nil {
		t.Errorf("expected spinner command to be returned")
	}

	// Clean up config
	_ = os.Remove(cfgPath)
}

func TestAIMessageFormatting(t *testing.T) {
	wsClient := &WSClient{}
	model := NewChatModel(wsClient, "Alice", "Alice", nil, nil)

	env := &types.Envelope{
		Type:    types.TypeAIMessage,
		Payload: "AI response",
	}
	display := model.decryptAndFormatEnvelope(env)
 
	if !strings.Contains(display.Content, "🤖") {
		t.Errorf("expected 🤖 prefix in display, got %q", display.Content)
	}
	if !strings.Contains(display.Content, "[AI - unencrypted]") {
		t.Errorf("expected [AI - unencrypted] label in display, got %q", display.Content)
	}
}
