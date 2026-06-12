package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/tui"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ─── Messages ───────────────────────────────────────────────────────────────

type streamEventMsg models.ReactStreamEvent
type streamDoneMsg struct{}
type streamStartMsg struct {
	ch <-chan models.ReactStreamEvent
}
type sessionCreatedMsg string
type sessionListMsg []string
type configMsg struct {
	config *tui.ConfigInfo
}
type errMsg struct{ err error }

// ─── Model ──────────────────────────────────────────────────────────────────

type appModel struct {
	backend   tui.Backend
	sessionID string

	// UI components
	textarea  textarea.Model
	viewport  viewport.Model
	spinner   spinner.Model
	statusBar statusBar

	// State
	chatLines []string
	streaming bool
	eventCh   <-chan models.ReactStreamEvent
	width     int
	height    int
	ready     bool
}

func newAppModel(backend tui.Backend) appModel {
	ta := textarea.New()
	ta.Placeholder = "Type your message... (Enter to send)"
	ta.Focus()
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 4096

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return appModel{
		backend:   backend,
		textarea:  ta,
		spinner:   sp,
		statusBar:newStatusBar(),
		chatLines: []string{
			titleStyle.Render("Code Agent TUI"),
			"",
			thinkingStyle.Render("Type a message to start chatting. Commands: /new, /sessions, /quit"),
			"",
		},
	}
}

func (m appModel) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		m.createSessionCmd(),
		m.fetchConfigCmd(),
	)
}

func (m *appModel) fetchConfigCmd() tea.Cmd {
	return func() tea.Msg {
		config, err := m.backend.GetConfig(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return configMsg{config}
	}
}

// ─── Update ─────────────────────────────────────────────────────────────────

func (m appModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.statusBar.width = msg.Width

		inputH := m.textarea.Height() + 2
		statusH := 1
		vpH := m.height - statusH - inputH - 1
		if vpH < 1 {
			vpH = 1
		}

		if !m.ready {
			m.viewport = viewport.New(m.width, vpH)
			m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
			m.ready = true
		} else {
			m.viewport.Width = m.width
			m.viewport.Height = vpH
		}
		m.textarea.SetWidth(m.width - 2)

	case tea.KeyMsg:
		switch {
		case msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD:
			return m, tea.Quit

		case msg.Type == tea.KeyEnter && !m.streaming:
			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				break
			}
			m.textarea.Reset()

			if strings.HasPrefix(input, "/") {
				return m.handleCommand(input)
			}

			m.chatLines = append(m.chatLines, userMsgStyle.Render("You: "+input), "")
			m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
			m.viewport.GotoBottom()
			m.streaming = true
			m.statusBar.state = "thinking"
			return m, m.startStreamCmd(input)
		}

	case streamStartMsg:
		m.eventCh = msg.ch
		return m, waitForEvent(m.eventCh)

	case streamEventMsg:
		ev := models.ReactStreamEvent(msg)
		rendered := renderEvent(ev)

		switch ev.Type {
		case "step_start":
			m.statusBar.step = ev.Step
			m.statusBar.maxSteps = ev.MaxSteps
			m.statusBar.state = "thinking"
		case "thinking":
			m.statusBar.state = "thinking"
		case "tool_call":
			m.statusBar.state = "tool_call"
		case "message":
			m.statusBar.state = "streaming"
		case "done":
			m.streaming = false
			m.statusBar.state = "idle"
		case "error":
			m.streaming = false
			m.statusBar.state = "idle"
		}

		if rendered != "" {
			m.chatLines = append(m.chatLines, rendered)
			m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
			m.viewport.GotoBottom()
		}

		if m.streaming {
			return m, waitForEvent(m.eventCh)
		}

	case streamDoneMsg:
		m.streaming = false
		m.statusBar.state = "idle"
		m.chatLines = append(m.chatLines, "")
		m.viewport.SetContent(strings.Join(m.chatLines, "\n"))

	case sessionCreatedMsg:
		m.sessionID = string(msg)
		m.statusBar.sessionID = string(msg)

	case configMsg:
		if msg.config != nil {
			m.statusBar.model = msg.config.Model
			m.statusBar.branch = msg.config.Branch
			m.statusBar.tokensMax = msg.config.MaxTokens
			m.statusBar.tokensUsed = msg.config.UsedTokens
		}

	case sessionListMsg:
		m.chatLines = append(m.chatLines, []string(msg)...)
		m.chatLines = append(m.chatLines, "")
		m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
		m.viewport.GotoBottom()

	case errMsg:
		m.streaming = false
		m.statusBar.state = "idle"
		m.chatLines = append(m.chatLines, errorMsgStyle.Render("Error: "+msg.err.Error()), "")
		m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
		m.viewport.GotoBottom()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	if !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// ─── View ───────────────────────────────────────────────────────────────────

func (m appModel) View() string {
	if !m.ready {
		return "Initializing..."
	}

	var sections []string
	sections = append(sections, m.viewport.View())
	sections = append(sections, m.statusBar.View())

	if m.streaming {
		sections = append(sections, m.spinner.View()+" Agent is working...")
	} else {
		sections = append(sections, m.textarea.View())
	}

	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// ─── Commands ───────────────────────────────────────────────────────────────

func waitForEvent(ch <-chan models.ReactStreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamDoneMsg{}
		}
		return streamEventMsg(ev)
	}
}

func (m appModel) startStreamCmd(msg string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		eventCh, err := m.backend.SendMessage(ctx, m.sessionID, msg)
		if err != nil {
			return errMsg{err}
		}
		return streamStartMsg{ch: eventCh}
	}
}

func (m appModel) createSessionCmd() tea.Cmd {
	return func() tea.Msg {
		id, err := m.backend.CreateSession(context.Background())
		if err != nil {
			return errMsg{err}
		}
		return sessionCreatedMsg(id)
	}
}

func (m appModel) handleCommand(input string) (tea.Model, tea.Cmd) {
	switch {
	case input == "/quit" || input == "/exit":
		return m, tea.Quit
	case input == "/new":
		m.chatLines = append(m.chatLines, thinkingStyle.Render("Creating new session..."), "")
		m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
		return m, m.createSessionCmd()
	case input == "/sessions":
		return m, func() tea.Msg {
			sessions, err := m.backend.ListSessions(context.Background())
			if err != nil {
				return errMsg{err}
			}
			var lines []string
			lines = append(lines, stepDividerStyle.Render("--- Sessions ---"))
			for _, s := range sessions {
				id := s.ID
				if len(id) > 8 {
					id = id[:8]
				}
				lines = append(lines, fmt.Sprintf("  %s (%d msgs) %s", id, s.MessageCount, s.LastMessage))
			}
			return sessionListMsg(lines)
		}
	default:
		m.chatLines = append(m.chatLines, errorMsgStyle.Render("Unknown command: "+input), "")
		m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
		return m, nil
	}
}