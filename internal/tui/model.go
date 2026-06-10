package tui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/pkg/types"
	"github.com/gorilla/websocket"
)

type AIResponseMsg struct {
	Response string
}

type AISetupState int

const (
	AISetupNone AISetupState = iota
	AISetupProvider
	AISetupKey
)

type DeliveryStatus int

const (
	StatusNone DeliveryStatus = iota
	StatusSending
	StatusSent
	StatusDelivered
	StatusRead
)

type ChatMessage struct {
	ID      string
	Content string
}

type WsMessageMsg struct {
	Data []byte
}

type WsDisconnectedMsg struct{}

type WsReconnectingMsg struct {
	Attempt int
	Delay   time.Duration
}

type WsSessionExpiredMsg struct{}

type WsConnectedMsg struct{}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

type checkEncryptionMsg struct {
	encrypted bool
}

func checkEncryptionCmd(ws *WSClient, roomID string) tea.Cmd {
	return func() tea.Msg {
		keys, err := ws.FetchRoomKeys(ws.serverURL, roomID)
		if err != nil || len(keys) == 0 {
			return checkEncryptionMsg{encrypted: false}
		}
		return checkEncryptionMsg{encrypted: true}
	}
}

func joinRoomCmd(ws *WSClient, roomID, lastMsgID string) tea.Cmd {
	return func() tea.Msg {
		_ = ws.JoinRoom(roomID, lastMsgID)
		return nil
	}
}

type PanelType int

const (
	PanelSidebar PanelType = iota
	PanelChat
)

type SidebarSection int

const (
	SectionRooms SidebarSection = iota
	SectionDMs
)

type ChatMode int

const (
	ModeRoom ChatMode = iota
	ModeDM
)

type roomItem struct {
	id     string
	name   string
	unread int
}

func (i roomItem) Title() string       { return i.name }
func (i roomItem) Description() string { return "" }
func (i roomItem) FilterValue() string { return i.name }

type roomDelegate struct {
	activeRoom     string
	focusedSection SidebarSection
	focusedPanel   PanelType
}

func (d roomDelegate) Height() int                               { return 1 }
func (d roomDelegate) Spacing() int                              { return 0 }
func (d roomDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd { return nil }
func (d roomDelegate) Render(w io.Writer, htmlList list.Model, index int, item list.Item) {
	rItem, ok := item.(roomItem)
	if !ok {
		return
	}

	activeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
	normalStyle := lipgloss.NewStyle()
	selectedStyle := lipgloss.NewStyle().Background(lipgloss.Color("8")).Foreground(lipgloss.Color("15"))

	var style lipgloss.Style
	if rItem.id == d.activeRoom {
		style = activeStyle
	} else {
		style = normalStyle
	}

	content := "#" + rItem.name
	if rItem.unread > 0 {
		content = fmt.Sprintf("%s (%d)", content, rItem.unread)
	}

	if index == htmlList.Index() && d.focusedPanel == PanelSidebar && d.focusedSection == SectionRooms {
		fmt.Fprintf(w, "%s", selectedStyle.Render("> "+content))
	} else {
		fmt.Fprintf(w, "  %s", style.Render(content))
	}
}

type ChatModel struct {
	input     textinput.Model
	viewport  viewport.Model
	messages  []ChatMessage
	users     map[string]types.PresenceState
	userUIDs  map[string]string // display name -> UID
	width     int
	height    int
	wsClient  *WSClient
	status    string
	username  string
	uid       string
	encrypted bool

	// Day 12 TUI polish
	focusedPanel    PanelType
	focusedSection  SidebarSection
	selectedDMIndex int
	activeRoom      string
	dmTarget        string
	mode            ChatMode
	roomsList       list.Model
	roomHistory     map[string][]ChatMessage
	dmHistory       map[string][]ChatMessage
	unreadCounts    map[string]int
	dmUnreadCounts  map[string]int
	dmLastMessage   map[string]string
	dmPartners      []string

	// Day 13 Reconnection state
	reconnecting       bool
	reconnectAttempt   int
	reconnectCountdown int
	rooms              []string
	lastMsgID          map[string]string
	seenMsgIDs         map[string]bool
	oldestMsgID        map[string]string
	historyAvailable   map[string]bool

	// Day 22 AI state
	aiEnabled        bool
	aiSetupState     AISetupState
	aiSetupProvider  string
	showAIWarning    bool
	aiPendingPrompt  string
	aiLoading        bool
	aiSpinner        spinner.Model
	aiContext        []string
	aiContextCleared bool

	// Day 23 Delivery Receipts state
	deliveryStatus map[string]DeliveryStatus
	visibleMsgIDs  map[string]bool
}

func NewChatModel(wsClient *WSClient, username string, uid string, initialItems []list.Item, initialRooms []string) ChatModel {
	input := textinput.New()
	input.Placeholder = "Type a message... (Tab: switch focus)"
	input.Focus()

	if len(initialRooms) == 0 {
		initialRooms = []string{"general"}
	}
	if len(initialItems) == 0 {
		initialItems = []list.Item{
			roomItem{id: "general", name: "general"},
			roomItem{id: "random", name: "random"},
			roomItem{id: "lounge", name: "lounge"},
		}
	}

	rList := list.New(initialItems, roomDelegate{}, 20, 10)
	rList.SetShowStatusBar(false)
	rList.SetShowTitle(false)
	rList.SetShowHelp(false)
	rList.SetFilteringEnabled(false)

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))

	return ChatModel{
		input:              input,
		viewport:           viewport.New(0, 0),
		messages:           []ChatMessage{},
		users:              make(map[string]types.PresenceState),
		userUIDs:           make(map[string]string),
		wsClient:           wsClient,
		status:             "Connected",
		username:           username,
		uid:                uid,
		encrypted:          false,
		focusedPanel:       PanelChat,
		focusedSection:     SectionRooms,
		selectedDMIndex:    0,
		activeRoom:         initialRooms[0],
		mode:               ModeRoom,
		roomsList:          rList,
		roomHistory:        make(map[string][]ChatMessage),
		dmHistory:          make(map[string][]ChatMessage),
		unreadCounts:       make(map[string]int),
		dmUnreadCounts:     make(map[string]int),
		dmLastMessage:      make(map[string]string),
		dmPartners:         []string{},
		reconnecting:       false,
		reconnectAttempt:   0,
		reconnectCountdown: 0,
		rooms:              initialRooms,
		lastMsgID:          make(map[string]string),
		seenMsgIDs:         make(map[string]bool),
		oldestMsgID:        make(map[string]string),
		historyAvailable:   make(map[string]bool),
		aiEnabled:          true,
		aiSetupState:       AISetupNone,
		aiSpinner:          s,
		aiContext:          []string{},
		deliveryStatus:     make(map[string]DeliveryStatus),
		visibleMsgIDs:      make(map[string]bool),
	}
}

func (m *ChatModel) addRecentDMPartner(uid string) {
	found := false
	for _, p := range m.dmPartners {
		if p == uid {
			found = true
			break
		}
	}
	if !found {
		m.dmPartners = append(m.dmPartners, uid)
	}
}

func (m *ChatModel) updateRoomsListItems() {
	items := m.roomsList.Items()
	newItems := make([]list.Item, len(items))
	for i, item := range items {
		rItem := item.(roomItem)
		rItem.unread = m.unreadCounts[rItem.name]
		newItems[i] = rItem
	}
	m.roomsList.SetItems(newItems)
}

func (m ChatModel) Init() tea.Cmd {
	m.wsClient.roomIDMu.RLock()
	roomID := m.wsClient.roomID
	m.wsClient.roomIDMu.RUnlock()
	if roomID == "" {
		roomID = "general"
	}
	return tea.Batch(
		textinput.Blink,
		checkEncryptionCmd(m.wsClient, roomID),
	)
}

func (m ChatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	if m.showAIWarning {
		if msg, ok := msg.(tea.KeyMsg); ok {
			switch strings.ToLower(msg.String()) {
			case "y":
				m.showAIWarning = false
				cfg, err := LoadConfig(DefaultConfigPath())
				if err == nil {
					cfg.AIWarned = true
					_ = SaveConfig(cfg, DefaultConfigPath())
				}
				return m.triggerAIQuery(m.aiPendingPrompt)
			case "n", "esc":
				m.showAIWarning = false
				m.aiPendingPrompt = ""
				m.messages = append(m.messages, ChatMessage{Content: "System: AI query cancelled."})
				m.viewport.SetContent(m.getRenderedViewportContent())
				m.viewport.GotoBottom()
				return m, nil
			}
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		sidebarWidth := 20
		availableWidth := msg.Width
		if msg.Width > sidebarWidth+1 {
			availableWidth = msg.Width - sidebarWidth - 1
		}
		viewportHeight := msg.Height - 3
		if viewportHeight < 1 {
			viewportHeight = 1
		}
		m.viewport.Width = availableWidth
		m.viewport.Height = viewportHeight
		inputWidth := availableWidth - 2
		if inputWidth < 1 {
			inputWidth = 1
		}
		m.input.Width = inputWidth
		m.viewport.SetContent(m.getRenderedViewportContent())
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "esc" {
			if m.aiSetupState != AISetupNone {
				m.aiSetupState = AISetupNone
				m.input.SetValue("")
				m.input.Prompt = "> "
				m.input.EchoMode = textinput.EchoNormal
				m.messages = append(m.messages, ChatMessage{Content: "System: AI setup cancelled."})
				m.viewport.SetContent(m.getRenderedViewportContent())
				m.viewport.GotoBottom()
				return m, nil
			}
		}

		// Toggle focus with Tab
		if msg.String() == "tab" {
			if m.focusedPanel == PanelSidebar {
				m.focusedPanel = PanelChat
				m.input.Focus()
			} else {
				m.focusedPanel = PanelSidebar
				m.input.Blur()
			}
			return m, nil
		}

		// Ctrl+D Shortcut
		if msg.Type == tea.KeyCtrlD {
			m.focusedPanel = PanelSidebar
			m.focusedSection = SectionDMs
			if len(m.dmPartners) > 0 {
				m.selectedDMIndex = 0
			}
			m.input.Blur()
			return m, nil
		}

		if m.focusedPanel == PanelSidebar {
			switch msg.String() {
			case "up":
				if m.focusedSection == SectionDMs {
					if m.selectedDMIndex > 0 {
						m.selectedDMIndex--
					} else {
						m.focusedSection = SectionRooms
						m.roomsList.Select(len(m.roomsList.Items()) - 1)
					}
				} else {
					m.roomsList.CursorUp()
				}
				return m, nil

			case "down":
				if m.focusedSection == SectionRooms {
					if m.roomsList.Index() < len(m.roomsList.Items())-1 {
						m.roomsList.CursorDown()
					} else {
						if len(m.dmPartners) > 0 {
							m.focusedSection = SectionDMs
							m.selectedDMIndex = 0
						}
					}
				} else {
					if m.selectedDMIndex < len(m.dmPartners)-1 {
						m.selectedDMIndex++
					}
				}
				return m, nil

			case "enter":
				if m.focusedSection == SectionRooms {
					selected := m.roomsList.SelectedItem().(roomItem)
					if selected.id != m.activeRoom {
						_ = m.wsClient.LeaveRoom(m.activeRoom)
						_ = m.wsClient.JoinRoom(selected.id, m.lastMsgID[selected.id])

						m.activeRoom = selected.id
						m.mode = ModeRoom
						m.dmTarget = ""

						m.messages = m.roomHistory[selected.id]
						if m.messages == nil {
							m.messages = []ChatMessage{}
						}
						m.viewport.SetContent(m.getRenderedViewportContent())
						m.viewport.GotoBottom()

						m.unreadCounts[selected.id] = 0
						m.updateRoomsListItems()

						return m, checkEncryptionCmd(m.wsClient, selected.id)
					}
				} else if m.focusedSection == SectionDMs {
					if len(m.dmPartners) > 0 {
						partner := m.dmPartners[m.selectedDMIndex]
						m.mode = ModeDM
						m.dmTarget = partner

						m.messages = m.dmHistory[partner]
						if m.messages == nil {
							m.messages = []ChatMessage{}
						}
						m.viewport.SetContent(m.getRenderedViewportContent())
						m.viewport.GotoBottom()

						m.dmUnreadCounts[partner] = 0
					}
				}
				return m, nil

			case "ctrl+c", "q":
				return m, tea.Quit
			}
			return m, nil
		} else {
			// PanelChat focused
			switch msg.String() {
			case "up", "down", "pgup", "pgdown":
				m.viewport, cmd = m.viewport.Update(msg)
				visCmds := m.updateVisibility()
				return m, tea.Batch(append(visCmds, cmd)...)
			case "ctrl+c":
				return m, tea.Quit
			case "ctrl+l":
				if m.mode == ModeRoom && m.historyAvailable[m.activeRoom] {
					oldestID := m.oldestMsgID[m.activeRoom]
					return m, joinRoomCmd(m.wsClient, m.activeRoom, oldestID)
				}
				return m, nil
			case "enter":
				if m.aiSetupState == AISetupProvider {
					val := strings.ToLower(strings.TrimSpace(m.input.Value()))
					if val != "gemini" && val != "anthropic" && val != "openai" {
						m.messages = append(m.messages, ChatMessage{Content: "System: Invalid provider. Choose [gemini/anthropic/openai]."})
						m.input.SetValue("")
						return m, m.updateViewportAndVisibility()
					}
					m.aiSetupProvider = val
					m.aiSetupState = AISetupKey
					m.input.SetValue("")
					m.input.Prompt = "API Key: "
					m.input.EchoMode = textinput.EchoPassword
					return m, nil
				} else if m.aiSetupState == AISetupKey {
					key := strings.TrimSpace(m.input.Value())
					cfg, err := LoadConfig(DefaultConfigPath())
					if err == nil {
						cfg.AIProvider = m.aiSetupProvider
						cfg.AIAPIKey = key
						_ = SaveConfig(cfg, DefaultConfigPath())
					}
					m.aiSetupState = AISetupNone
					m.input.SetValue("")
					m.input.Prompt = "> "
					m.input.EchoMode = textinput.EchoNormal
					m.messages = append(m.messages, ChatMessage{Content: "System: Key stored locally. Never sent to Sanctum server."})
					return m, m.updateViewportAndVisibility()
				}

				value := strings.TrimSpace(m.input.Value())
				if value != "" && m.wsClient != nil {
					// 1. Check if it is an AI slash command
					if strings.HasPrefix(value, "/ai ") || value == "/ai" || value == "/ai setup" || value == "/ai off" || value == "/ai on" || value == "/ai clear" {
						cmdStr := strings.TrimSpace(value)
						if cmdStr == "/ai setup" {
							m.aiSetupState = AISetupProvider
							m.input.SetValue("")
							m.input.Prompt = "Provider? [gemini/anthropic/openai]: "
							m.input.EchoMode = textinput.EchoNormal
							return m, nil
						} else if cmdStr == "/ai off" {
							m.aiEnabled = false
							m.messages = append(m.messages, ChatMessage{Content: "System: AI assistant disabled"})
							m.input.SetValue("")
							return m, m.updateViewportAndVisibility()
						} else if cmdStr == "/ai on" {
							m.aiEnabled = true
							m.messages = append(m.messages, ChatMessage{Content: "System: AI assistant enabled"})
							m.input.SetValue("")
							return m, m.updateViewportAndVisibility()
						} else if cmdStr == "/ai clear" {
							m.aiContext = []string{}
							m.aiContextCleared = true
							m.messages = append(m.messages, ChatMessage{Content: "System: AI context cleared"})
							m.input.SetValue("")
							return m, m.updateViewportAndVisibility()
						}
					}

					// 2. Check if it is an @ai query
					if strings.HasPrefix(value, "@ai ") || value == "@ai" {
						if !m.aiEnabled {
							m.messages = append(m.messages, ChatMessage{Content: "System: AI assistant is disabled. Use /ai on to enable."})
							m.input.SetValue("")
							return m, m.updateViewportAndVisibility()
						}
						cfg, err := LoadConfig(DefaultConfigPath())
						if err != nil || cfg.AIAPIKey == "" {
							m.messages = append(m.messages, ChatMessage{Content: "System: AI provider not configured. Use /ai setup to configure."})
							m.input.SetValue("")
							return m, m.updateViewportAndVisibility()
						}
						prompt := strings.TrimSpace(strings.TrimPrefix(value, "@ai"))
						if prompt == "" {
							m.input.SetValue("")
							return m, nil
						}

						// First, send the prompt as a normal message over WebSocket so it is visible in history
						var errSend error
						var msgID string
						if m.mode == ModeDM {
							msgID, errSend = m.wsClient.SendDM(m.dmTarget, value)
							if errSend == nil {
								m.deliveryStatus[msgID] = StatusSending
								m.dmHistory[m.dmTarget] = append(m.dmHistory[m.dmTarget], ChatMessage{ID: msgID, Content: fmt.Sprintf("To %s: %s", m.dmTarget, value)})
								m.messages = m.dmHistory[m.dmTarget]
							}
						} else {
							msgID, errSend = m.wsClient.Send(value)
							if errSend == nil {
								m.deliveryStatus[msgID] = StatusSending
								m.roomHistory[m.activeRoom] = append(m.roomHistory[m.activeRoom], ChatMessage{ID: msgID, Content: fmt.Sprintf("%s: %s", m.username, value)})
								m.messages = m.roomHistory[m.activeRoom]
							}
						}
						if errSend != nil {
							m.messages = append(m.messages, ChatMessage{Content: "send failed: " + userError(errSend)})
						}
						viewportCmd := m.updateViewportAndVisibility()

						// If warned is false, trigger warning
						if !cfg.AIWarned {
							m.showAIWarning = true
							m.aiPendingPrompt = prompt
							m.input.SetValue("")
							return m, viewportCmd
						}

						// Otherwise trigger query
						m.input.SetValue("")
						aiModel, aiCmd := m.triggerAIQuery(prompt)
						return aiModel, tea.Batch(viewportCmd, aiCmd)
					}

					var err error
					var msgID string
					if strings.HasPrefix(value, "/dm ") {
						parts := strings.SplitN(strings.TrimPrefix(value, "/dm "), " ", 2)
						if len(parts) >= 1 {
							targetName := strings.TrimSpace(parts[0])
							uid := targetName
							if matchedUID, ok := m.userUIDs[targetName]; ok {
								uid = matchedUID
							}

							m.dmTarget = uid
							m.mode = ModeDM
							m.addRecentDMPartner(uid)

							m.messages = m.dmHistory[uid]
							if m.messages == nil {
								m.messages = []ChatMessage{}
							}
							viewportCmd := m.updateViewportAndVisibility()

							if len(parts) == 2 {
								text := strings.TrimSpace(parts[1])
								msgID, err = m.wsClient.SendDM(uid, text)
								if err == nil {
									m.deliveryStatus[msgID] = StatusSending
									m.dmHistory[uid] = append(m.dmHistory[uid], ChatMessage{ID: msgID, Content: fmt.Sprintf("To %s: %s", uid, text)})
									m.messages = m.dmHistory[uid]
									viewportCmd = m.updateViewportAndVisibility()
								}
							}
							m.input.SetValue("")
							m.input.CursorEnd()
							return m, viewportCmd
						}
					} else if strings.HasPrefix(value, "/create ") {
						roomName := strings.TrimSpace(strings.TrimPrefix(value, "/create "))
						if roomName != "" {
							env := &types.Envelope{
								Type:    types.TypeCreateRoom,
								Payload: roomName,
							}
							data, err := types.Marshal(env)
							if err == nil {
								_ = m.wsClient.conn.WriteMessage(websocket.TextMessage, data)
							}
						}
					} else if strings.HasPrefix(value, "/invite ") {
						targetName := strings.TrimSpace(strings.TrimPrefix(value, "/invite "))
						if targetName != "" {
							targetUID := targetName
							if matchedUID, ok := m.userUIDs[targetName]; ok {
								targetUID = matchedUID
							}
							env := &types.Envelope{
								Type:   types.TypeInvite,
								RoomID: m.activeRoom,
								ToUID:  targetUID,
							}
							data, err := types.Marshal(env)
							if err == nil {
								_ = m.wsClient.conn.WriteMessage(websocket.TextMessage, data)
								m.messages = append(m.messages, ChatMessage{Content: fmt.Sprintf("System: Sent invitation to %s", targetName)})
								viewportCmd := m.updateViewportAndVisibility()
								m.input.SetValue("")
								m.input.CursorEnd()
								return m, viewportCmd
							}
						}
					} else {
						if m.mode == ModeDM {
							msgID, err = m.wsClient.SendDM(m.dmTarget, value)
							if err == nil {
								m.deliveryStatus[msgID] = StatusSending
								m.dmHistory[m.dmTarget] = append(m.dmHistory[m.dmTarget], ChatMessage{ID: msgID, Content: fmt.Sprintf("To %s: %s", m.dmTarget, value)})
								m.messages = m.dmHistory[m.dmTarget]
							}
						} else {
							msgID, err = m.wsClient.Send(value)
							if err == nil {
								m.deliveryStatus[msgID] = StatusSending
								m.roomHistory[m.activeRoom] = append(m.roomHistory[m.activeRoom], ChatMessage{ID: msgID, Content: fmt.Sprintf("%s: %s", m.username, value)})
								m.messages = m.roomHistory[m.activeRoom]
							}
						}
					}

					if err != nil {
						m.messages = append(m.messages, ChatMessage{Content: "send failed: " + userError(err)})
					}
					viewportCmd := m.updateViewportAndVisibility()
					m.input.SetValue("")
					m.input.CursorEnd()
					return m, viewportCmd
				}
				m.input.SetValue("")
				m.input.CursorEnd()
				return m, nil
			}

			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}

	case WsMessageMsg:
		if env, err := types.Unmarshal(msg.Data); err == nil {
			if env.Type == types.TypeAIMessage {
				display := m.decryptAndFormatEnvelope(env)
				if env.RoomID != "" {
					roomID := env.RoomID
					m.roomHistory[roomID] = append(m.roomHistory[roomID], display)
					if m.mode == ModeRoom && roomID == m.activeRoom {
						m.messages = m.roomHistory[m.activeRoom]
						return m, m.updateViewportAndVisibility()
					}
				} else {
					senderUID := env.FromUID
					m.addRecentDMPartner(senderUID)
					m.dmHistory[senderUID] = append(m.dmHistory[senderUID], display)
					if m.mode == ModeDM && m.dmTarget == senderUID {
						m.messages = m.dmHistory[senderUID]
						return m, m.updateViewportAndVisibility()
					}
				}
				return m, nil
			}

			if env.Type == types.TypePresenceUpdate {
				var update types.PresenceUpdate
				if err := json.Unmarshal([]byte(env.Payload), &update); err == nil {
					name := update.Name
					if name == "" {
						name = update.UID
					}
					if name != "" {
						m.users[name] = update.State
						m.userUIDs[name] = update.UID
					}
				}
				m.wsClient.roomIDMu.RLock()
				roomID := m.wsClient.roomID
				m.wsClient.roomIDMu.RUnlock()
				if roomID == "" {
					roomID = "general"
				}
				return m, checkEncryptionCmd(m.wsClient, roomID)
			}

			if env.Type == types.TypeHistoryBatch {
				var history []types.Envelope
				if err := json.Unmarshal([]byte(env.Payload), &history); err == nil && len(history) > 0 && history[0].Type != "" {
					roomID := env.RoomID
					if roomID == "" {
						roomID = m.activeRoom
					}

					var decodedHistory []ChatMessage
					for _, hEnv := range history {
						if hEnv.ID != "" {
							if m.seenMsgIDs[hEnv.ID] {
								continue
							}
							m.seenMsgIDs[hEnv.ID] = true
						}
						display := m.decryptAndFormatEnvelope(&hEnv)
						decodedHistory = append(decodedHistory, display)
					}

					if len(decodedHistory) > 0 {
						m.roomHistory[roomID] = append(decodedHistory, m.roomHistory[roomID]...)
						m.oldestMsgID[roomID] = history[0].ID
					}

					m.historyAvailable[roomID] = len(history) == 50

					if roomID == m.activeRoom {
						m.messages = m.roomHistory[m.activeRoom]
						return m, m.updateViewportAndVisibility()
					}
					return m, nil
				} else {
					var updates []types.PresenceUpdate
					if err := json.Unmarshal([]byte(env.Payload), &updates); err == nil {
						for _, update := range updates {
							name := update.Name
							if name == "" {
								name = update.UID
							}
							if name != "" {
								m.users[name] = update.State
								m.userUIDs[name] = update.UID
							}
						}
					}
					return m, nil
				}
			}

			if env.Type == types.TypeAck {
				msgID := env.ID
				if msgID != "" {
					m.deliveryStatus[msgID] = StatusSent
				}
				return m, m.updateViewportAndVisibility()
			}
			if env.Type == types.TypeDeliveredAck {
				msgID := env.ID
				if msgID != "" {
					m.deliveryStatus[msgID] = StatusDelivered
				}
				return m, m.updateViewportAndVisibility()
			}
			if env.Type == types.TypeReadAck {
				msgID := env.ID
				if msgID != "" {
					m.deliveryStatus[msgID] = StatusRead
				}
				return m, m.updateViewportAndVisibility()
			}

			if env.Type == types.TypeError {
				var errData struct {
					Code string `json:"code"`
				}
				msgText := env.Payload
				if err := json.Unmarshal([]byte(env.Payload), &errData); err == nil && errData.Code != "" {
					if errData.Code == "not_authorized" {
						msgText = "Unauthorized: You do not have permission to join/invite in this room."
					} else {
						msgText = errData.Code
					}
				}
				m.messages = append(m.messages, ChatMessage{Content: lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true).Render("Error: " + msgText)})
				return m, m.updateViewportAndVisibility()
			}

			if env.Type == types.TypeCreateRoom {
				newRoomID := env.RoomID
				newRoomName := env.Payload

				found := false
				for _, rID := range m.rooms {
					if rID == newRoomID {
						found = true
						break
					}
				}
				if !found {
					m.rooms = append(m.rooms, newRoomID)
					m.roomsList.InsertItem(len(m.roomsList.Items()), roomItem{id: newRoomID, name: newRoomName})
					m.roomHistory[newRoomID] = []ChatMessage{}
					m.lastMsgID[newRoomID] = ""
				}

				m.activeRoom = newRoomID
				m.mode = ModeRoom
				m.dmTarget = ""
				m.messages = m.roomHistory[newRoomID]
				m.unreadCounts[newRoomID] = 0
				m.updateRoomsListItems()
				return m, tea.Batch(
					m.updateViewportAndVisibility(),
					checkEncryptionCmd(m.wsClient, newRoomID),
				)
			}

			if env.Type == types.TypeInvite {
				roomID := env.RoomID
				roomName := env.Payload

				found := false
				for _, rID := range m.rooms {
					if rID == roomID {
						found = true
						break
					}
				}
				if !found {
					m.rooms = append(m.rooms, roomID)
					m.roomsList.InsertItem(len(m.roomsList.Items()), roomItem{id: roomID, name: roomName})
					m.roomHistory[roomID] = []ChatMessage{}
					m.lastMsgID[roomID] = ""
					m.updateRoomsListItems()
				}

				m.messages = append(m.messages, ChatMessage{Content: lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Italic(true).Render(fmt.Sprintf("System: You have been invited to room #%s", roomName))})
				return m, m.updateViewportAndVisibility()
			}

			// Deduplicate live text / DM messages
			if env.Type == types.TypeText || env.Type == types.TypeDM {
				if env.ID != "" {
					if m.seenMsgIDs[env.ID] {
						return m, nil
					}
					m.seenMsgIDs[env.ID] = true
				}

				// Immediately send back TypeDeliveredAck{MsgID} if it's from someone else
				if env.ID != "" && env.FromUID != m.uid && env.FromUID != m.username {
					delAckEnv := &types.Envelope{
						Type:   types.TypeDeliveredAck,
						ID:     env.ID,
						RoomID: env.RoomID,
						ToUID:  env.FromUID,
					}
					_ = m.wsClient.SendEnvelope(delAckEnv)
				}
			}

			var display ChatMessage
			if env.Type == types.TypeDM {
				senderUID := env.FromUID
				m.addRecentDMPartner(senderUID)
				display = m.decryptAndFormatEnvelope(env)
				m.dmHistory[senderUID] = append(m.dmHistory[senderUID], display)

				// Keep track of last DM message preview
				parts := strings.SplitN(display.Content, ": ", 2)
				if len(parts) == 2 {
					m.dmLastMessage[senderUID] = parts[1]
				} else {
					m.dmLastMessage[senderUID] = display.Content
				}

				if m.mode == ModeDM && m.dmTarget == senderUID {
					m.messages = m.dmHistory[senderUID]
					return m, m.updateViewportAndVisibility()
				} else {
					m.dmUnreadCounts[senderUID]++
				}
			} else {
				// TypeText Room Message
				roomID := env.RoomID
				if roomID == "" {
					roomID = m.activeRoom
				}
				display = m.decryptAndFormatEnvelope(env)

				m.roomHistory[roomID] = append(m.roomHistory[roomID], display)
				m.lastMsgID[roomID] = env.ID

				if m.mode == ModeRoom && roomID == m.activeRoom {
					m.messages = m.roomHistory[m.activeRoom]
					return m, m.updateViewportAndVisibility()
				} else {
					m.unreadCounts[roomID]++
					m.updateRoomsListItems()
				}
			}
		} else {
			m.messages = append(m.messages, ChatMessage{Content: string(msg.Data)})
			return m, m.updateViewportAndVisibility()
		}
		return m, nil

	case WsDisconnectedMsg:
		m.status = "Offline"
		if !m.reconnecting {
			m.reconnecting = true
			m.reconnectAttempt = 0
			m.reconnectCountdown = 0
			go m.wsClient.reconnectLoop(context.Background())
		}
		return m, nil

	case WsReconnectingMsg:
		m.reconnecting = true
		m.reconnectAttempt = msg.Attempt
		m.reconnectCountdown = int(msg.Delay.Seconds())
		m.status = fmt.Sprintf("Reconnecting... (attempt %d)", msg.Attempt)
		return m, tickCmd()

	case WsSessionExpiredMsg:
		m.status = "Session expired. Please log in again."
		m.reconnecting = false
		m.reconnectAttempt = 0
		m.reconnectCountdown = 0
		return m, nil

	case WsConnectedMsg:
		m.status = "Connected"
		m.reconnecting = false
		m.reconnectAttempt = 0
		m.reconnectCountdown = 0

		return m, tea.Batch(
			joinRoomCmd(m.wsClient, m.activeRoom, m.lastMsgID[m.activeRoom]),
			checkEncryptionCmd(m.wsClient, m.activeRoom),
		)

	case tickMsg:
		if m.reconnecting && m.reconnectCountdown > 0 {
			m.reconnectCountdown--
			if m.reconnectCountdown > 0 {
				return m, tickCmd()
			}
		}
		return m, nil

	case checkEncryptionMsg:
		m.encrypted = msg.encrypted
		return m, nil

	case AIResponseMsg:
		m.aiLoading = false
		resp := msg.Response

		purpleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
		mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		formattedMsg := fmt.Sprintf("%s %s AI: %s", purpleStyle.Render("🤖"), mutedStyle.Render("[AI - unencrypted]"), resp)

		if m.mode == ModeDM {
			m.dmHistory[m.dmTarget] = append(m.dmHistory[m.dmTarget], ChatMessage{Content: formattedMsg})
			m.messages = m.dmHistory[m.dmTarget]
		} else {
			m.roomHistory[m.activeRoom] = append(m.roomHistory[m.activeRoom], ChatMessage{Content: formattedMsg})
			m.messages = m.roomHistory[m.activeRoom]
		}

		return m, m.updateViewportAndVisibility()

		// Send to WebSocket server
		if m.wsClient != nil {
			if m.mode == ModeDM {
				env := &types.Envelope{
					Type:     types.TypeAIMessage,
					ToUID:    m.dmTarget,
					Payload:  resp,
					FromName: "AI",
				}
				data, err := types.Marshal(env)
				if err == nil {
					_ = m.wsClient.conn.WriteMessage(websocket.TextMessage, data)
				}
			} else {
				env := &types.Envelope{
					Type:     types.TypeAIMessage,
					RoomID:   m.activeRoom,
					Payload:  resp,
					FromName: "AI",
				}
				data, err := types.Marshal(env)
				if err == nil {
					_ = m.wsClient.conn.WriteMessage(websocket.TextMessage, data)
				}
			}
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.aiSpinner, cmd = m.aiSpinner.Update(msg)
		if m.aiLoading {
			m.viewport.SetContent(m.getRenderedViewportContent())
			return m, cmd
		}
		return m, nil
	}

	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m ChatModel) View() string {
	if m.showAIWarning {
		return m.renderAIWarning()
	}

	statusBar := renderStatusBar(
		m.status,
		m.reconnecting,
		m.reconnectAttempt,
		m.reconnectCountdown,
		m.encrypted || m.mode == ModeDM,
		m.activeRoom,
		m.mode,
		m.dmTarget,
		m.width,
	)

	var roomsHeader, dmsHeader string
	focusedHeaderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	blurredHeaderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	if m.focusedPanel == PanelSidebar {
		roomsHeader = focusedHeaderStyle.Render("[Rooms]")
		dmsHeader = focusedHeaderStyle.Render("[DMs]")
	} else {
		roomsHeader = blurredHeaderStyle.Render("[Rooms]")
		dmsHeader = blurredHeaderStyle.Render("[DMs]")
	}

	m.roomsList.SetDelegate(roomDelegate{
		activeRoom:     m.activeRoom,
		focusedSection: m.focusedSection,
		focusedPanel:   m.focusedPanel,
	})
	roomsView := m.roomsList.View()

	dmsView := renderDMList(m)

	sidebarContent := fmt.Sprintf("%s\n%s\n\n%s\n%s", roomsHeader, roomsView, dmsHeader, dmsView)
	sidebar := lipgloss.NewStyle().Width(20).MaxWidth(20).Render(sidebarContent)

	chatArea := lipgloss.JoinVertical(lipgloss.Left,
		m.viewport.View(),
		m.input.View(),
	)

	mainArea := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, chatArea)
	return fmt.Sprintf("%s\n%s", mainArea, statusBar)
}

func renderDMList(m ChatModel) string {
	if len(m.dmPartners) == 0 {
		return "  (none)"
	}

	dmLines := []string{}
	for idx, partner := range m.dmPartners {
		selected := (idx == m.selectedDMIndex && m.focusedPanel == PanelSidebar && m.focusedSection == SectionDMs)

		style := lipgloss.NewStyle()
		if m.mode == ModeDM && m.dmTarget == partner {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
		}

		displayName := partner
		for name, id := range m.userUIDs {
			if id == partner {
				displayName = name
				break
			}
		}

		preview := ""
		if msg, ok := m.dmLastMessage[partner]; ok {
			if len(msg) > 15 {
				preview = msg[:12] + "..."
			} else {
				preview = msg
			}
		}

		unread := ""
		if count := m.dmUnreadCounts[partner]; count > 0 {
			unread = fmt.Sprintf(" (%d)", count)
		}

		line := fmt.Sprintf("%s%s", displayName, unread)
		if preview != "" {
			line = fmt.Sprintf("%s: %s", line, preview)
		}

		if selected {
			dmLines = append(dmLines, lipgloss.NewStyle().Background(lipgloss.Color("8")).Foreground(lipgloss.Color("15")).Render("> "+line))
		} else {
			dmLines = append(dmLines, "  "+style.Render(line))
		}
	}

	return strings.Join(dmLines, "\n")
}

func renderStatusBar(status string, reconnecting bool, attempt, countdown int, encrypted bool, room string, mode ChatMode, dmTarget string, width int) string {
	base := lipgloss.NewStyle().Width(width).Background(lipgloss.Color("236")).Foreground(lipgloss.Color("7"))

	label := "Status: " + status
	var statusColor lipgloss.Color
	switch {
	case reconnecting:
		statusColor = lipgloss.Color("3")
		label = fmt.Sprintf("Reconnecting... (attempt %d) | Retrying in %ds", attempt, countdown)
	case status == "Connected":
		statusColor = lipgloss.Color("2")
	case status == "Offline":
		statusColor = lipgloss.Color("1")
	default:
		statusColor = lipgloss.Color("3")
	}
	statusStr := lipgloss.NewStyle().Foreground(statusColor).Render(label)

	contextStr := ""
	if mode == ModeDM {
		contextStr = fmt.Sprintf(" | DM: @%s", dmTarget)
	} else {
		contextStr = fmt.Sprintf(" | Room: #%s", room)
	}

	encStr := " | ⚠️ unencrypted"
	if encrypted {
		encStr = " | 🔒"
	}

	return base.Render(" " + statusStr + contextStr + encStr)
}

func (m *ChatModel) decryptAndFormatEnvelope(env *types.Envelope) ChatMessage {
	if env.Type == types.TypeAIMessage {
		purpleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
		mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		return ChatMessage{
			ID:      env.ID,
			Content: fmt.Sprintf("%s %s AI: %s", purpleStyle.Render("🤖"), mutedStyle.Render("[AI - unencrypted]"), env.Payload),
		}
	}

	var display string
	if env.Type == types.TypeDM {
		senderUID := env.FromUID
		pubKeyBytes, err := m.wsClient.FetchPublicKey(m.wsClient.serverURL, senderUID)
		if err != nil {
			mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			display = fmt.Sprintf("DM from %s: %s", senderUID, mutedStyle.Render("[decryption failed: missing public key]"))
		} else {
			var senderPubKey [32]byte
			copy(senderPubKey[:], pubKeyBytes)

			ciphertext, err1 := base64.StdEncoding.DecodeString(env.Payload)
			nonce, err2 := base64.StdEncoding.DecodeString(env.Nonce)

			if err1 != nil || err2 != nil {
				mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				display = fmt.Sprintf("DM from %s: %s", senderUID, mutedStyle.Render("[decryption failed: invalid encoding]"))
			} else {
				plaintext, ok := crypto.DecryptDM(ciphertext, nonce, &senderPubKey, m.wsClient.PrivateKey)
				if !ok {
					mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					display = fmt.Sprintf("DM from %s: %s", senderUID, mutedStyle.Render("[decryption failed]"))
				} else {
					display = fmt.Sprintf("DM from %s: %s", senderUID, string(plaintext))
				}
			}
		}
	} else {
		// TypeText Room Message
		if len(env.EncryptedPayloads) > 0 {
			myUID := m.uid
			if myUID == "" {
				myUID = m.username
			}
			ep, found := env.EncryptedPayloads[myUID]
			if !found {
				display = "[message not for you]"
			} else {
				senderUID := env.FromUID
				pubKeyBytes, err := m.wsClient.FetchPublicKey(m.wsClient.serverURL, senderUID)
				if err != nil {
					mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					display = mutedStyle.Render("[decryption failed: missing public key]")
				} else {
					var senderPubKey [32]byte
					copy(senderPubKey[:], pubKeyBytes)

					ciphertext, err1 := base64.StdEncoding.DecodeString(ep.Ciphertext)
					nonce, err2 := base64.StdEncoding.DecodeString(ep.Nonce)

					if err1 != nil || err2 != nil {
						mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
						display = mutedStyle.Render("[decryption failed: invalid encoding]")
					} else {
						plaintext, ok := crypto.DecryptDM(ciphertext, nonce, &senderPubKey, m.wsClient.PrivateKey)
						if !ok {
							mutedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
							display = mutedStyle.Render("[decryption failed]")
						} else {
							display = string(plaintext)
						}
					}
				}
			}
		} else {
			display = env.Payload
		}

		if env.FromName != "" {
			display = env.FromName + ": " + display
		} else if env.FromUID != "" {
			display = env.FromUID + ": " + display
		}
	}
	return ChatMessage{
		ID:      env.ID,
		Content: display,
	}
}

func (m *ChatModel) getRenderedViewportContent() string {
	var rendered []string
	for _, msg := range m.messages {
		rendered = append(rendered, m.formatChatMessage(msg))
	}

	var content string
	if m.mode == ModeRoom && m.historyAvailable[m.activeRoom] {
		content = "   [Load more... (Press Ctrl+L)]\n" + strings.Join(rendered, "\n")
	} else {
		content = strings.Join(rendered, "\n")
	}
	if m.aiLoading {
		content += "\n🤖 " + m.aiSpinner.View() + " Thinking..."
	}
	return content
}

func (m *ChatModel) formatChatMessage(msg ChatMessage) string {
	if msg.ID == "" {
		return msg.Content
	}
	status, ok := m.deliveryStatus[msg.ID]
	if !ok || status == StatusNone {
		return msg.Content
	}

	var icon string
	switch status {
	case StatusSending:
		icon = " ⏳"
	case StatusSent:
		icon = " ✓"
	case StatusDelivered:
		icon = " ✓✓"
	case StatusRead:
		icon = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("✓✓")
	}
	return msg.Content + icon
}

func (m *ChatModel) updateVisibility() []tea.Cmd {
	if len(m.messages) == 0 {
		return nil
	}

	headerOffset := 0
	if m.mode == ModeRoom && m.historyAvailable[m.activeRoom] {
		headerOffset = 1
	}

	var cmds []tea.Cmd
	yStart := m.viewport.YOffset
	yEnd := m.viewport.YOffset + m.viewport.Height

	for i, msg := range m.messages {
		msgLine := i + headerOffset
		if msgLine >= yStart && msgLine < yEnd {
			if msg.ID != "" {
				if !m.visibleMsgIDs[msg.ID] {
					m.visibleMsgIDs[msg.ID] = true
					_, sentByUs := m.deliveryStatus[msg.ID]
					if !sentByUs {
						env := &types.Envelope{
							Type:   types.TypeReadAck,
							ID:     msg.ID,
							RoomID: m.activeRoom,
						}
						if m.mode == ModeDM {
							env.ToUID = m.dmTarget
							env.RoomID = ""
						}
						cmds = append(cmds, func() tea.Msg {
							_ = m.wsClient.SendEnvelope(env)
							return nil
						})
					}
				}
			}
		}
	}
	return cmds
}

func (m *ChatModel) updateViewportAndVisibility() tea.Cmd {
	m.viewport.SetContent(m.getRenderedViewportContent())
	m.viewport.GotoBottom()
	cmds := m.updateVisibility()
	return tea.Batch(cmds...)
}

func (m ChatModel) renderAIWarning() string {
	width := m.width
	height := m.height

	banner := lipgloss.NewStyle().
		Foreground(lipgloss.Color("11")). // Yellow
		Bold(true).
		Render("⚠️  AI Context Disclosure")

	provider := "the AI provider"
	cfg, err := LoadConfig(DefaultConfigPath())
	if err == nil && cfg.AIProvider != "" {
		provider = cfg.AIProvider
	}

	lines := []string{
		banner,
		"",
		fmt.Sprintf("The last 20 messages will be sent as plaintext to %s.", provider),
		"This bypasses Sanctum's encryption for this query only.",
		"",
		lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("[Y] Proceed") + "   " +
			lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("[N] Cancel"),
	}

	dialogBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("11")). // Yellow border
		Padding(1, 3).
		Align(lipgloss.Center).
		Render(strings.Join(lines, "\n"))

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, dialogBox)
}

func (m *ChatModel) triggerAIQuery(prompt string) (tea.Model, tea.Cmd) {
	cfg, err := LoadConfig(DefaultConfigPath())
	if err != nil || cfg.AIAPIKey == "" {
		m.messages = append(m.messages, ChatMessage{Content: "System: AI provider not configured. Use /ai setup to configure."})
		m.viewport.SetContent(m.getRenderedViewportContent())
		m.viewport.GotoBottom()
		return *m, nil
	}

	m.aiLoading = true
	m.aiSpinner = spinner.New()
	m.aiSpinner.Spinner = spinner.Dot
	m.aiSpinner.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))

	if !m.aiContextCleared {
		start := len(m.messages) - 20
		if start < 0 {
			start = 0
		}
		m.aiContext = nil
		for _, msg := range m.messages[start:] {
			m.aiContext = append(m.aiContext, msg.Content)
		}
	}

	contextualPrompt := BuildAIContext(m.aiContext, prompt)

	provider := cfg.AIProvider
	apiKey := cfg.AIAPIKey
	program := m.wsClient.Program

	go func() {
		var resp string
		var err error
		switch provider {
		case "gemini":
			resp, err = QueryGemini(apiKey, contextualPrompt)
		case "anthropic":
			resp, err = QueryAnthropic(apiKey, contextualPrompt)
		case "openai":
			resp, err = QueryOpenAI(apiKey, contextualPrompt)
		default:
			err = fmt.Errorf("unknown provider: %s", provider)
		}
		if err != nil {
			resp = fmt.Sprintf("Error calling AI: %v", err)
		}
		if program != nil {
			program.Send(AIResponseMsg{Response: resp})
		}
	}()

	return *m, m.aiSpinner.Tick
}

