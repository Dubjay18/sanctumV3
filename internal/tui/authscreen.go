package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type ScreenType int

const (
	ModeLogin ScreenType = iota
	ModeRegister
)

type AuthSuccessMsg struct {
	IDToken      string
	RefreshToken string
	UID          string
	DisplayName  string
}

type authResultMsg struct {
	idToken      string
	refreshToken string
	uid          string
	displayName  string
	err          error
}

type AuthModel struct {
	emailInput       textinput.Model
	passwordInput    textinput.Model
	displayNameInput textinput.Model
	mode             ScreenType
	focusedIdx       int
	err              error
	loading          bool
	width            int
	height           int
}

func NewAuthModel() AuthModel {
	email := textinput.New()
	email.Placeholder = "email@example.com"
	email.Focus()
	email.CharLimit = 64
	email.Width = 32

	password := textinput.New()
	password.Placeholder = "••••••••"
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '•'
	password.CharLimit = 64
	password.Width = 32

	displayName := textinput.New()
	displayName.Placeholder = "username"
	displayName.CharLimit = 32
	displayName.Width = 32

	return AuthModel{
		emailInput:       email,
		passwordInput:    password,
		displayNameInput: displayName,
		mode:             ModeLogin,
		focusedIdx:       0,
	}
}

func (m AuthModel) Init() tea.Cmd {
	return textinput.Blink
}

func submitAuthCmd(isRegister bool, email, password, displayName string) tea.Cmd {
	return func() tea.Msg {
		var idToken, refreshToken, uid string
		var err error
		if isRegister {
			idToken, refreshToken, uid, err = Register(email, password, displayName)
		} else {
			idToken, refreshToken, uid, err = SignIn(email, password)
			if err == nil {
				displayName = decodeDisplayName(idToken)
			}
		}
		return authResultMsg{
			idToken:      idToken,
			refreshToken: refreshToken,
			uid:          uid,
			displayName:  displayName,
			err:          err,
		}
	}
}

func decodeDisplayName(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(payloadBytes, &claims)
	return claims.Name
}

func (m AuthModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "tab", "down":
			m.focusedIdx++
			maxIdx := 2 // email, password, submit
			if m.mode == ModeRegister {
				maxIdx = 3 // email, password, display name, submit
			}
			if m.focusedIdx > maxIdx {
				m.focusedIdx = 0
			}
			m.updateFocus()
			return m, nil

		case "shift+tab", "up":
			m.focusedIdx--
			maxIdx := 2
			if m.mode == ModeRegister {
				maxIdx = 3
			}
			if m.focusedIdx < 0 {
				m.focusedIdx = maxIdx
			}
			m.updateFocus()
			return m, nil

		case "ctrl+t": // Toggle Mode Shortcut
			m.toggleMode()
			return m, nil

		case "enter":
			maxIdx := 2
			if m.mode == ModeRegister {
				maxIdx = 3
			}
			// Submit if on the submit button (last index) or if enter is pressed generally
			if m.focusedIdx == maxIdx || msg.String() == "enter" {
				email := strings.TrimSpace(m.emailInput.Value())
				password := m.passwordInput.Value()
				displayName := strings.TrimSpace(m.displayNameInput.Value())

				if email == "" || password == "" {
					m.err = fmt.Errorf("email and password are required")
					return m, nil
				}
				if m.mode == ModeRegister && displayName == "" {
					m.err = fmt.Errorf("username (display name) is required")
					return m, nil
				}

				m.loading = true
				m.err = nil
				return m, submitAuthCmd(m.mode == ModeRegister, email, password, displayName)
			}
		}

	case authResultMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}

		// Save Configuration to ~/.sanctum/config.json
		cfgPath := DefaultConfigPath()
		cfg, _ := LoadConfig(cfgPath)
		cfg.IDToken = msg.idToken
		cfg.RefreshToken = msg.refreshToken
		cfg.UID = msg.uid
		cfg.DisplayName = msg.displayName
		_ = SaveConfig(cfg, cfgPath)

		// Transition to chat by bubbling success msg
		return m, func() tea.Msg {
			return AuthSuccessMsg{
				IDToken:      msg.idToken,
				RefreshToken: msg.refreshToken,
				UID:          msg.uid,
				DisplayName:  msg.displayName,
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}

	// Update the inputs based on focus
	if m.focusedIdx == 0 {
		m.emailInput, cmd = m.emailInput.Update(msg)
	} else if m.focusedIdx == 1 {
		m.passwordInput, cmd = m.passwordInput.Update(msg)
	} else if m.focusedIdx == 2 && m.mode == ModeRegister {
		m.displayNameInput, cmd = m.displayNameInput.Update(msg)
	}

	return m, cmd
}

func (m *AuthModel) updateFocus() {
	m.emailInput.Blur()
	m.passwordInput.Blur()
	m.displayNameInput.Blur()

	if m.focusedIdx == 0 {
		m.emailInput.Focus()
	} else if m.focusedIdx == 1 {
		m.passwordInput.Focus()
	} else if m.focusedIdx == 2 && m.mode == ModeRegister {
		m.displayNameInput.Focus()
	}
}

func (m *AuthModel) toggleMode() {
	m.err = nil
	m.focusedIdx = 0
	if m.mode == ModeLogin {
		m.mode = ModeRegister
	} else {
		m.mode = ModeLogin
	}
	m.updateFocus()
}

func (m AuthModel) View() string {
	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("45")).
		Bold(true).
		MarginBottom(1)

	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("63")).
		Padding(1, 3).
		Width(46)

	labelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("244")).
		Bold(true)

	activeLabelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("81")).
		Bold(true)

	buttonStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("0")).
		Background(lipgloss.Color("244")).
		Padding(0, 3).
		Bold(true).
		MarginTop(1)

	activeButtonStyle := buttonStyle.
		Background(lipgloss.Color("81"))

	errorStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("197")).
		Bold(true).
		MarginTop(1)

	linkStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("63")).
		Underline(true)

	var formBuilder strings.Builder

	// Title
	formBuilder.WriteString(titleStyle.Render("🔐 SANCTUM CHAT"))
	formBuilder.WriteString("\n")

	// Email Field
	emailLabel := labelStyle
	if m.focusedIdx == 0 {
		emailLabel = activeLabelStyle
	}
	formBuilder.WriteString(emailLabel.Render("Email Address:"))
	formBuilder.WriteString("\n")
	formBuilder.WriteString(m.emailInput.View())
	formBuilder.WriteString("\n\n")

	// Password Field
	passwordLabel := labelStyle
	if m.focusedIdx == 1 {
		passwordLabel = activeLabelStyle
	}
	formBuilder.WriteString(passwordLabel.Render("Password:"))
	formBuilder.WriteString("\n")
	formBuilder.WriteString(m.passwordInput.View())
	formBuilder.WriteString("\n\n")

	// Display Name Field (Register Mode only)
	var submitIdx int
	if m.mode == ModeRegister {
		displayNameLabel := labelStyle
		if m.focusedIdx == 2 {
			displayNameLabel = activeLabelStyle
		}
		formBuilder.WriteString(displayNameLabel.Render("Username (Display Name):"))
		formBuilder.WriteString("\n")
		formBuilder.WriteString(m.displayNameInput.View())
		formBuilder.WriteString("\n\n")
		submitIdx = 3
	} else {
		submitIdx = 2
	}

	// Submit Button
	submitText := "[ SIGN IN ]"
	if m.mode == ModeRegister {
		submitText = "[ REGISTER ]"
	}
	var button string
	if m.focusedIdx == submitIdx {
		button = activeButtonStyle.Render(submitText)
	} else {
		button = buttonStyle.Render(submitText)
	}
	formBuilder.WriteString(button)
	formBuilder.WriteString("\n\n")

	// Navigation tips and toggling
	formBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("Ctrl+T to toggle mode  •  Tab to navigate"))
	formBuilder.WriteString("\n")
	if m.mode == ModeLogin {
		formBuilder.WriteString("Need an account? " + linkStyle.Render("Press Ctrl+T to Register"))
	} else {
		formBuilder.WriteString("Already registered? " + linkStyle.Render("Press Ctrl+T to Sign In"))
	}

	// Loading / Errors
	if m.loading {
		formBuilder.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("Connecting to Firebase... Please wait..."))
	}
	if m.err != nil {
		formBuilder.WriteString("\n" + errorStyle.Render("Error: "+m.err.Error()))
	}

	// Center the box in the terminal window
	box := borderStyle.Render(formBuilder.String())
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(
			m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			box,
		)
	}
	return box
}
