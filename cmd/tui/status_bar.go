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
	workDir    string // Current working directory
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
	// Left section: Working directory (project name)
	projectName := "unknown"
	if s.workDir != "" {
		// Show only the last directory name
		parts := strings.Split(s.workDir, "/")
		if len(parts) > 0 {
			projectName = parts[len(parts)-1]
		}
	}
	left := statusBarProjectStyle.Render(fmt.Sprintf("📁 %s", projectName))

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

	// Right section: Session + Branch + Step
	var rightParts []string
	
	// Show session ID
	if s.sessionID != "" {
		rightParts = append(rightParts, statusBarStyle.Render(fmt.Sprintf("● %s", truncateID(s.sessionID))))
	}
	
	if s.branch != "" && s.branch != "unknown" {
		rightParts = append(rightParts, statusBarBranchStyle.Render(fmt.Sprintf("📦 %s", s.branch)))
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