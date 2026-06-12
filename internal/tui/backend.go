// Package tui defines the shared interface between the TUI frontend and
// the agent backend (local in-process or remote HTTP/SSE).
package tui

import (
	"context"

	"github.com/agent/code_agent/internal/models"
)

// Backend abstracts the agent communication layer for the TUI.
// Implementations: LocalBackend (in-process orchestrator) and RemoteBackend (SSE client).
type Backend interface {
	// SendMessage sends a user message and returns a channel of streaming events.
	// The channel is closed when the response is complete.
	SendMessage(ctx context.Context, sessionID, message string) (<-chan models.ReactStreamEvent, error)

	// CreateSession creates a new chat session and returns its ID.
	CreateSession(ctx context.Context) (string, error)

	// ListSessions returns a list of existing session summaries.
	ListSessions(ctx context.Context) ([]SessionSummary, error)

	// Close releases any resources held by the backend.
	Close() error
}

// SessionSummary is a minimal view of a session for TUI display.
type SessionSummary struct {
	ID             string
	MessageCount   int
	LastMessage    string
	LastUpdateTime string
}