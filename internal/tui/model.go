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
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/pkg/types"
)

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
	if rItem.name == d.activeRoom {
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
	messages  []string
	users     map[string]types.PresenceState
	userUIDs  map[string]string // display name -> UID
	width     int
	height    int
	wsClient  *WSClient
	status    string
	username  string
	encrypted bool

	// Day 12 TUI polish
	focusedPanel    PanelType
	focusedSection  SidebarSection
	selectedDMIndex int
	activeRoom      string
	dmTarget        string
	mode            ChatMode
	roomsList       list.Model
	roomHistory     map[string][]string
	dmHistory       map[string][]string
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
}

func NewChatModel(wsClient *WSClient, username string) ChatModel {
	input := textinput.New()
	input.Placeholder = "Type a message... (Tab: switch focus)"
	input.Focus()

	roomItems := []list.Item{
		roomItem{name: "general"},
		roomItem{name: "random"},
		roomItem{name: "lounge"},
	}
	rList := list.New(roomItems, roomDelegate{}, 20, 10)
	rList.SetShowStatusBar(false)
	rList.SetShowTitle(false)
	rList.SetShowHelp(false)
	rList.SetFilteringEnabled(false)

	return ChatModel{
		input:              input,
		viewport:           viewport.New(0, 0),
		messages:           []string{},
		users:              make(map[string]types.PresenceState),
		userUIDs:           make(map[string]string),
		wsClient:           wsClient,
		status:             "Connected",
		username:           username,
		encrypted:          false,
		focusedPanel:       PanelChat,
		focusedSection:     SectionRooms,
		selectedDMIndex:    0,
		activeRoom:         "general",
		mode:               ModeRoom,
		roomsList:          rList,
		roomHistory:        make(map[string][]string),
		dmHistory:          make(map[string][]string),
		unreadCounts:       make(map[string]int),
		dmUnreadCounts:     make(map[string]int),
		dmLastMessage:      make(map[string]string),
		dmPartners:         []string{},
		reconnecting:       false,
		reconnectAttempt:   0,
		reconnectCountdown: 0,
		rooms:              []string{"general"},
		lastMsgID:          make(map[string]string),
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
		m.viewport.SetContent(strings.Join(m.messages, "\n"))
		return m, nil

	case tea.KeyMsg:
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
					if selected.name != m.activeRoom {
						_ = m.wsClient.LeaveRoom(m.activeRoom)
						_ = m.wsClient.JoinRoom(selected.name, m.lastMsgID[selected.name])

						m.activeRoom = selected.name
						m.rooms = []string{selected.name}
						m.mode = ModeRoom
						m.dmTarget = ""

						m.messages = m.roomHistory[selected.name]
						if m.messages == nil {
							m.messages = []string{}
						}
						m.viewport.SetContent(strings.Join(m.messages, "\n"))
						m.viewport.GotoBottom()

						m.unreadCounts[selected.name] = 0
						m.updateRoomsListItems()

						return m, checkEncryptionCmd(m.wsClient, selected.name)
					}
				} else if m.focusedSection == SectionDMs {
					if len(m.dmPartners) > 0 {
						partner := m.dmPartners[m.selectedDMIndex]
						m.mode = ModeDM
						m.dmTarget = partner

						m.messages = m.dmHistory[partner]
						if m.messages == nil {
							m.messages = []string{}
						}
						m.viewport.SetContent(strings.Join(m.messages, "\n"))
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
				return m, cmd
			case "ctrl+c":
				return m, tea.Quit
			case "enter":
				value := strings.TrimSpace(m.input.Value())
				if value != "" && m.wsClient != nil {
					var err error
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
								m.messages = []string{}
							}
							m.viewport.SetContent(strings.Join(m.messages, "\n"))
							m.viewport.GotoBottom()

							if len(parts) == 2 {
								text := strings.TrimSpace(parts[1])
								err = m.wsClient.SendDM(uid, text)
								if err == nil {
									m.dmHistory[uid] = append(m.dmHistory[uid], fmt.Sprintf("To %s: %s", uid, text))
									m.messages = m.dmHistory[uid]
									m.viewport.SetContent(strings.Join(m.messages, "\n"))
									m.viewport.GotoBottom()
								}
							}
						}
					} else {
						if m.mode == ModeDM {
							err = m.wsClient.SendDM(m.dmTarget, value)
							if err == nil {
								m.dmHistory[m.dmTarget] = append(m.dmHistory[m.dmTarget], fmt.Sprintf("To %s: %s", m.dmTarget, value))
								m.messages = m.dmHistory[m.dmTarget]
								m.viewport.SetContent(strings.Join(m.messages, "\n"))
								m.viewport.GotoBottom()
							}
						} else {
							err = m.wsClient.Send(value)
							if err == nil {
								m.roomHistory[m.activeRoom] = append(m.roomHistory[m.activeRoom], fmt.Sprintf("%s: %s", m.username, value))
								m.messages = m.roomHistory[m.activeRoom]
								m.viewport.SetContent(strings.Join(m.messages, "\n"))
								m.viewport.GotoBottom()
							}
						}
					}

					if err != nil {
						m.messages = append(m.messages, fmt.Sprintf("send failed: %v", err))
						m.viewport.SetContent(strings.Join(m.messages, "\n"))
						m.viewport.GotoBottom()
					}
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

			var display string
			if env.Type == types.TypeDM {
				senderUID := env.FromUID
				m.addRecentDMPartner(senderUID)

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
							m.dmLastMessage[senderUID] = string(plaintext)
						}
					}
				}

				m.dmHistory[senderUID] = append(m.dmHistory[senderUID], display)

				if m.mode == ModeDM && m.dmTarget == senderUID {
					m.messages = m.dmHistory[senderUID]
					m.viewport.SetContent(strings.Join(m.messages, "\n"))
					m.viewport.GotoBottom()
				} else {
					m.dmUnreadCounts[senderUID]++
				}
			} else {
				// TypeText Room Message
				roomID := env.RoomID
				if roomID == "" {
					roomID = m.activeRoom
				}

				if len(env.EncryptedPayloads) > 0 {
					myUID := m.username
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
				if display == "" {
					display = string(msg.Data)
				}

				m.roomHistory[roomID] = append(m.roomHistory[roomID], display)
				m.lastMsgID[roomID] = env.ID

				if m.mode == ModeRoom && roomID == m.activeRoom {
					m.messages = m.roomHistory[m.activeRoom]
					m.viewport.SetContent(strings.Join(m.messages, "\n"))
					m.viewport.GotoBottom()
				} else {
					m.unreadCounts[roomID]++
					m.updateRoomsListItems()
				}
			}
		} else {
			m.messages = append(m.messages, string(msg.Data))
			m.viewport.SetContent(strings.Join(m.messages, "\n"))
			m.viewport.GotoBottom()
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

		var cmds []tea.Cmd
		for _, roomID := range m.rooms {
			cmds = append(cmds, joinRoomCmd(m.wsClient, roomID, m.lastMsgID[roomID]))
		}
		cmds = append(cmds, checkEncryptionCmd(m.wsClient, m.activeRoom))
		return m, tea.Batch(cmds...)

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
	}

	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m ChatModel) View() string {
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
