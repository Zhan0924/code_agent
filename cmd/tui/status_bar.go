package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type statusBar struct {
	sessionID string
	step      int
	maxSteps  int
	state     string // "idle", "thinking", "tool_call", "streaming"
	width     int
}

func newStatusBar() statusBar {
	return statusBar{state: "idle"}
}

func (s statusBar) View() string {
	left := fmt.Sprintf(" Session: %s", truncateID(s.sessionID))

	var stateIcon string
	switch s.state {
	case "idle":
		stateIcon = "Ready"
	case "thinking":
		stateIcon = "Thinking..."
	case "tool_call":
		stateIcon = "Executing tool..."
	case "streaming":
		stateIcon = "Responding..."
	default:
		stateIcon = s.state
	}
	center := stateIcon

	var right string
	if s.maxSteps > 0 {
		right = fmt.Sprintf("Step %d/%d ", s.step, s.maxSteps)
	} else {
		right = "Ready "
	}

	leftW := lipgloss.Width(left)
	centerW := lipgloss.Width(center)
	rightW := lipgloss.Width(right)
	gap := s.width - leftW - centerW - rightW
	if gap < 0 {
		gap = 0
	}
	leftGap := gap / 2
	rightGap := gap - leftGap

	bar := left +
		strings.Repeat(" ", leftGap) +
		center +
		strings.Repeat(" ", rightGap) +
		right

	return statusBarStyle.Width(s.width).Render(bar)
}

func truncateID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}