package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agent/code_agent/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	mode := flag.String("mode", "remote", "Backend mode: remote (SSE to running server)")
	addr := flag.String("addr", "localhost:18080", "Remote agent address")
	sessionID := flag.String("session", "", "Resume existing session by ID")
	flag.Parse()

	var backend tui.Backend

	switch *mode {
	case "remote":
		backend = NewRemoteBackend(*addr)
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s (currently only 'remote' is supported)\n", *mode)
		os.Exit(1)
	}
	defer backend.Close()

	model := newAppModel(backend)
	if *sessionID != "" {
		model.sessionID = *sessionID
		model.statusBar.sessionID = *sessionID
	}

	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}
}