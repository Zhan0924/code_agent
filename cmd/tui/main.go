package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent/code_agent/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	mode := flag.String("mode", "remote", "Backend mode: remote (SSE to running server)")
	addr := flag.String("addr", "localhost:18080", "Remote agent address")
	sessionID := flag.String("session", "", "Resume existing session by ID")
	workDir := flag.String("dir", "", "Working directory (defaults to current directory)")
	flag.Parse()

	// Determine working directory
	var workPath string
	if *workDir != "" {
		absPath, err := filepath.Abs(*workDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving working directory: %v\n", err)
			os.Exit(1)
		}
		workPath = absPath
	} else {
		absPath, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting current directory: %v\n", err)
			os.Exit(1)
		}
		workPath = absPath
	}

	// Check if directory exists
	if _, err := os.Stat(workPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Directory does not exist: %s\n", workPath)
		os.Exit(1)
	}

	var backend tui.Backend

	switch *mode {
	case "remote":
		backend = NewRemoteBackend(*addr, workPath)
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

	// Set working directory in status bar
	model.statusBar.workDir = workPath

	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}
}