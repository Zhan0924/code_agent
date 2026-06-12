package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type statusBar struct {
	sessionID  string
	step       int
	maxSteps   int
	state      string // "idle", "thinking", "tool_call", "streaming"
	width      int
	model      string
	branch     string
	tokensUsed int
	tokensMax  int
}

func newStatusBar() statusBar {
	return statusBar{
		state:  "idle",
		model:  "unknown",
		branch: "unknown",
	}
}

func truncateID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func (s statusBar) View() string {
	// Left section: Session ID with icon
	sessionText := "No Session"
	if s.sessionID != "" {
		sessionText = truncateID(s.sessionID)
	}
	left := statusBarStyle.Render(fmt.Sprintf(" ● %s", sessionText))

	// Center section: State with icon
	var stateDisplay string
	switch s.state {
	case "idle":
		stateDisplay = statusBarIdleStyle.Render("◯ Ready")
	case "thinking":
		stateDisplay = statusBarActiveStyle.Render("◉ Thinking...")
	case "tool_call":
		stateDisplay = statusBarToolStyle.Render("⚙ Executing...")
	case "streaming":
		stateDisplay = statusBarActiveStyle.Render("◉ Responding...")
	default:
		stateDisplay = statusBarStyle.Render(s.state)
	}

	// Right section: Model + Branch + Tokens
	var rightParts []string
	
	if s.model != "" && s.model != "unknown" {
		rightParts = append(rightParts, statusBarModelStyle.Render(fmt.Sprintf("🤖 %s", s.model)))
	}
	
	if s.branch != "" && s.branch != "unknown" {
		rightParts = append(rightParts, statusBarBranchStyle.Render(fmt.Sprintf("📦 %s", s.branch)))
	}
	
	if s.tokensMax > 0 {
		tokenPercent := 0
		if s.tokensUsed > 0 {
			tokenPercent = (s.tokensUsed * 100) / s.tokensMax
		}
		tokenColor := successColor
		if tokenPercent > 80 {
			tokenColor = errorColor
		} else if tokenPercent > 50 {
			tokenColor = lipgloss.Color("#F59E0B")
		}
		tokenStyle := lipgloss.NewStyle().Foreground(tokenColor).Padding(0, 1)
		rightParts = append(rightParts, tokenStyle.Render(fmt.Sprintf("📊 %d/%d", s.tokensUsed, s.tokensMax)))
	}
	
	if s.maxSteps > 0 {
		stepText := fmt.Sprintf("Step %d/%d", s.step, s.maxSteps)
		rightParts = append(rightParts, statusBarStepStyle.Render(stepText))
	}
	
	right := strings.Join(rightParts, " ")

	// Calculate widths
	leftW := lipgloss.Width(left)
	centerW := lipgloss.Width(stateDisplay)
	rightW := lipgloss.Width(right)
	
	// Distribute space
	gap := s.width - leftW - centerW - rightW
	if gap < 0 {
		gap = 0
	}
	
	leftGap := gap / 3
	rightGap := gap - leftGap
	if rightGap < 0 {
		rightGap = 0
	}

	// Build status bar
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		left,
		strings.Repeat(" ", leftGap),
		stateDisplay,
		strings.Repeat(" ", rightGap),
		right,
	)
}