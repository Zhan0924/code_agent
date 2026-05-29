package pty

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type localSession struct {
	id          SessionID
	name        string
	workspaceID string
	cmd         *exec.Cmd
	ptmx        *os.File
	lines       chan string
	readErr     chan error
	mu          sync.Mutex
	lastActive  time.Time
	createdAt   time.Time
	closed      bool
	cfg         ManagerConfig
	logger      *zap.Logger
}

func (m *manager) createLocalSession(ctx context.Context, workspaceID, name string) (ShellSession, error) {
	wsPath := fmt.Sprintf("%s/%s", m.cfg.WorkspaceBase, workspaceID)

	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, m.cfg.Shell)
	cmd.Dir = wsPath
	cmd.Env = minimalEnv(wsPath)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})

	now := time.Now()
	sess := &localSession{
		id:          SessionID(uuid.New().String()),
		name:        name,
		workspaceID: workspaceID,
		cmd:         cmd,
		ptmx:        ptmx,
		lines:       make(chan string, 1000),
		readErr:     make(chan error, 1),
		lastActive:  now,
		createdAt:   now,
		cfg:         m.cfg,
		logger:      m.logger.With(zap.String("session_id", name)),
	}

	// Start persistent reader goroutine
	go sess.readLoop()

	// Give shell time to initialize, drain any initial prompt output
	time.Sleep(100 * time.Millisecond)
	sess.drainPending()

	return sess, nil
}

// readLoop continuously reads lines from PTY and sends them to the lines channel.
// This goroutine owns the bufio.Reader exclusively — no other goroutine may read from it.
func (s *localSession) readLoop() {
	reader := bufio.NewReader(s.ptmx)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			s.readErr <- err
			return
		}
		s.lines <- strings.TrimRight(line, "\r\n")
	}
}

// drainPending discards any buffered lines in the channel.
func (s *localSession) drainPending() {
	for {
		select {
		case <-s.lines:
		default:
			return
		}
	}
}

func (s *localSession) ID() SessionID { return s.id }

func (s *localSession) Execute(ctx context.Context, command string) (*ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session is closed")
	}

	start := time.Now()
	s.lastActive = start

	marker := fmt.Sprintf("__PTY_DONE_%s__", uuid.New().String()[:8])
	fullCmd := fmt.Sprintf("%s; __ec=$?; echo \"%s $__ec\"\n", command, marker)

	if _, err := io.WriteString(s.ptmx, fullCmd); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}

	var output strings.Builder
	timeout := s.cfg.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	truncated := false
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return &ExecResult{
				Output:    stripANSI(output.String()),
				ExitCode:  -1,
				Duration:  time.Since(start),
				Truncated: true,
			}, nil
		case <-ctx.Done():
			return &ExecResult{
				Output:    stripANSI(output.String()),
				ExitCode:  -1,
				Duration:  time.Since(start),
				Truncated: true,
			}, nil
		case err := <-s.readErr:
			if output.Len() > 0 {
				return &ExecResult{
					Output:   stripANSI(output.String()),
					ExitCode: -1,
					Duration: time.Since(start),
				}, nil
			}
			return nil, fmt.Errorf("read output: %w", err)
		case line := <-s.lines:
			if strings.HasPrefix(line, marker) {
				exitCode := 0
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					fmt.Sscanf(parts[1], "%d", &exitCode)
				}
				return &ExecResult{
					Output:    stripANSI(output.String()),
					ExitCode:  exitCode,
					Duration:  time.Since(start),
					Truncated: truncated,
				}, nil
			}

			// Skip lines containing the marker (echoed command)
			if strings.Contains(line, marker) {
				continue
			}

			if output.Len()+len(line) > s.cfg.OutputLimit {
				truncated = true
				continue
			}
			output.WriteString(line + "\n")
		}
	}
}

func (s *localSession) Resize(rows, cols uint16) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

func (s *localSession) IsAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.cmd.Process == nil {
		return false
	}
	return s.cmd.ProcessState == nil
}

func (s *localSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	s.ptmx.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
		s.cmd.Wait()
	}

	s.logger.Info("local PTY session closed")
	return nil
}

func minimalEnv(workDir string) []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + workDir,
		"TERM=xterm-256color",
		"LANG=en_US.UTF-8",
		"SHELL=/bin/bash",
	}
}
