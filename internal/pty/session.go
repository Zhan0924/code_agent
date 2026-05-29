// Package pty provides persistent PTY terminal sessions that maintain shell
// state (cwd, env vars, aliases) across multiple tool invocations within a
// workspace. Supports Docker-backed (production) and local (dev/test) backends.
package pty

import (
	"context"
	"time"
)

// SessionID uniquely identifies a PTY session.
type SessionID string

// ExecResult holds the output of a command execution in a PTY session.
type ExecResult struct {
	Output    string
	ExitCode  int
	Duration  time.Duration
	Truncated bool
}

// SessionInfo provides metadata about an active session.
type SessionInfo struct {
	ID          SessionID
	Name        string
	WorkspaceID string
	CreatedAt   time.Time
	LastActive  time.Time
	IsAlive     bool
}

// ShellSession represents a single persistent PTY session.
type ShellSession interface {
	ID() SessionID
	Execute(ctx context.Context, command string) (*ExecResult, error)
	Resize(rows, cols uint16) error
	IsAlive() bool
	Close() error
}
