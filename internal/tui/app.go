package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

type AppState int

const (
	AppStateAuth AppState = iota
	AppStateChat
)

type ProgramHolder struct {
	P *tea.Program
}

type AppModel struct {
	state       AppState
	auth        AuthModel
	chat        ChatModel
	wsClient    *WSClient
	wsURL       string
	pubKey      *[32]byte
	privKey     *[32]byte
	username    string
	initialized bool
	pHolder     *ProgramHolder
	width       int
	height      int
}

func NewAppModel(wsURL string, pubKey, privKey *[32]byte, pHolder *ProgramHolder) AppModel {
	return AppModel{
		state:    AppStateAuth,
		auth:     NewAuthModel(),
		wsURL:    wsURL,
		pubKey:   pubKey,
		privKey:  privKey,
		pHolder:  pHolder,
	}
}

func (m AppModel) Init() tea.Cmd {
	// 1. Check if we have a valid, non-expired session on disk
	cfg, err := LoadConfig(DefaultConfigPath())
	if err == nil && cfg.IDToken != "" && !IsTokenExpired(cfg.IDToken) {
		m.username = cfg.DisplayName
		// Dispatch success message immediately to auto-login
		return func() tea.Msg {
			return AuthSuccessMsg{
				IDToken:      cfg.IDToken,
				RefreshToken: cfg.RefreshToken,
				UID:          cfg.UID,
				DisplayName:  cfg.DisplayName,
			}
		}
	}

	return m.auth.Init()
}

func (m AppModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case AuthSuccessMsg:
		// 1. Establish the WS connection
		m.username = msg.DisplayName
		wsClient, err := Connect(m.wsURL)
		if err != nil {
			m.auth.err = fmt.Errorf("websocket connection failed: %v", err)
			m.state = AppStateAuth
			m.auth.loading = false
			return m, nil
		}

		// Inject keypair and program handler
		m.wsClient = wsClient
		m.wsClient.PublicKey = m.pubKey
		m.wsClient.PrivateKey = m.privKey
		if m.pHolder != nil {
			m.wsClient.Program = m.pHolder.P
		}

		// Join general room
		_ = m.wsClient.JoinRoom("general", "")

		// Initialize ChatModel
		m.chat = NewChatModel(m.wsClient, m.username)
		if m.width > 0 && m.height > 0 {
			// Pre-size chat components if size was received earlier
			m.chat.width = m.width
			m.chat.height = m.height
			m.chat.viewport.Width = m.width - 21
			m.chat.viewport.Height = m.height - 3
			m.chat.input.Width = m.width - 23
		}

		// Start WebSocket listener loop
		go m.wsClient.Listen()

		m.state = AppStateChat
		m.initialized = true

		// Trigger ChatModel's Init
		return m, m.chat.Init()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		var authModel tea.Model
		authModel, cmd = m.auth.Update(msg)
		m.auth = authModel.(AuthModel)
		cmds = append(cmds, cmd)

		if m.initialized {
			var chatModel tea.Model
			chatModel, cmd = m.chat.Update(msg)
			m.chat = chatModel.(ChatModel)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}

	if m.state == AppStateAuth {
		var authModel tea.Model
		authModel, cmd = m.auth.Update(msg)
		m.auth = authModel.(AuthModel)
		return m, cmd
	}

	var chatModel tea.Model
	chatModel, cmd = m.chat.Update(msg)
	m.chat = chatModel.(ChatModel)
	return m, cmd
}

func (m AppModel) View() string {
	if m.state == AppStateAuth {
		return m.auth.View()
	}
	return m.chat.View()
}
