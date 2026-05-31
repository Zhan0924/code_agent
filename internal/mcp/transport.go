// Package mcp - transport.go defines the wire-level transport abstraction
// shared by stdio (subprocess) and SSE (HTTP+EventSource) MCP servers.
//
// ServerConnection is now transport-agnostic: it owns the JSON-RPC request/
// response routing (pending map, progress map, reqID counter) and delegates
// byte-level send/receive to a Transport implementation. Each transport
// implements its own liveness probe (Alive) — stdio uses Signal(0) plus the
// exited atomic; SSE tracks the last received event timestamp.
package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

// Transport is the byte-level abstraction over an MCP server's wire protocol.
// Send writes a single JSON-RPC payload; Recv reads one frame. Implementations
// MUST guarantee that concurrent Send calls are serialized internally (the
// caller does not hold any lock). Recv is intended to be called from a single
// reader goroutine.
type Transport interface {
	// Send writes one JSON-RPC payload. Implementations append any framing
	// (newline for NDJSON / stdio; SSE handles framing on the receive side).
	Send(data []byte) error

	// Recv reads one JSON-RPC payload (sans framing). Returns (data, true) on
	// success, (nil, false) on EOF or terminal error. After (nil, false),
	// callers may consult Err() to disambiguate clean close vs failure.
	Recv() ([]byte, bool)

	// Err returns the terminal error after Recv returned false, or nil on a
	// clean close (EOF).
	Err() error

	// Alive reports whether the underlying transport is still healthy. For
	// stdio this checks the child PID with Signal(0) plus the reaper flag;
	// for SSE this checks whether the stream is open and traffic is recent.
	Alive() bool

	// Close terminates the transport. Implementations MUST be idempotent.
	Close() error
}

// ─── stdioTransport — subprocess stdio MCP server ───────────────────────────

type stdioTransport struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
	writeMu sync.Mutex
	exited  atomic.Bool
	closed  atomic.Bool
	logger  *zap.Logger
}

func newStdioTransport(cfg *config.MCPServerConfig, logger *zap.Logger) (*stdioTransport, error) {
	if err := ValidateCommand(cfg.Command); err != nil {
		return nil, fmt.Errorf("invalid MCP command: %w", err)
	}
	if err := ValidateArgs(cfg.Args); err != nil {
		return nil, fmt.Errorf("invalid MCP args: %w", err)
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server %s: %w", cfg.Name, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<14), 1<<22) // 4MB max line

	t := &stdioTransport{
		name:    cfg.Name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		scanner: scanner,
		logger:  logger.With(zap.String("mcp_transport", "stdio")),
	}
	return t, nil
}

func (t *stdioTransport) Send(data []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := fmt.Fprintf(t.stdin, "%s\n", data)
	return err
}

func (t *stdioTransport) Recv() ([]byte, bool) {
	if !t.scanner.Scan() {
		return nil, false
	}
	// Copy because scanner.Bytes() reuses its internal buffer on next Scan.
	b := t.scanner.Bytes()
	out := make([]byte, len(b))
	copy(out, b)
	return out, true
}

func (t *stdioTransport) Err() error { return t.scanner.Err() }

func (t *stdioTransport) Alive() bool {
	if t.closed.Load() {
		return false
	}
	if t.exited.Load() {
		return false
	}
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	err := t.cmd.Process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// ESRCH = process gone; anything else treated as "still alive" so we
	// don't tear down a working pool on a transient syscall fault.
	return !errors.Is(err, syscall.ESRCH)
}

func (t *stdioTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	if t.stdout != nil {
		_ = t.stdout.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		return t.cmd.Process.Kill()
	}
	return nil
}

// startReaper launches the cmd.Wait() reaper. Must be called AFTER the
// reader goroutine has been started (caller is responsible for sequencing).
// Sets exited when the child has been fully reaped.
func (t *stdioTransport) startReaper(readerWg *sync.WaitGroup) {
	go func() {
		readerWg.Wait()
		if t.cmd != nil {
			_ = t.cmd.Wait()
		}
		t.exited.Store(true)
	}()
}

// ─── inMemoryTransport — test-only pipe-backed transport ────────────────────

type inMemoryTransport struct {
	name    string
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	scanner *bufio.Scanner
	writeMu sync.Mutex
	closed  atomic.Bool
	logger  *zap.Logger
}

func newInMemoryTransport(name string, clientStdin io.WriteCloser, clientStdout io.ReadCloser, logger *zap.Logger) *inMemoryTransport {
	scanner := bufio.NewScanner(clientStdout)
	scanner.Buffer(make([]byte, 1<<14), 1<<22)
	return &inMemoryTransport{
		name:    name,
		stdin:   clientStdin,
		stdout:  clientStdout,
		scanner: scanner,
		logger:  logger.With(zap.String("mcp_transport", "inmem")),
	}
}

func (t *inMemoryTransport) Send(data []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err := fmt.Fprintf(t.stdin, "%s\n", data)
	return err
}

func (t *inMemoryTransport) Recv() ([]byte, bool) {
	if !t.scanner.Scan() {
		return nil, false
	}
	b := t.scanner.Bytes()
	out := make([]byte, len(b))
	copy(out, b)
	return out, true
}

func (t *inMemoryTransport) Err() error { return t.scanner.Err() }

func (t *inMemoryTransport) Alive() bool { return !t.closed.Load() }

func (t *inMemoryTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	if t.stdout != nil {
		_ = t.stdout.Close()
	}
	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// dialTransport constructs the transport matching cfg.Transport. Returns an
// error if the transport name is unrecognized.
func dialTransport(ctx context.Context, cfg *config.MCPServerConfig, httpClient *http.Client, logger *zap.Logger) (Transport, error) {
	switch cfg.Transport {
	case "", "stdio":
		return newStdioTransport(cfg, logger)
	case "sse":
		if httpClient == nil {
			return nil, errors.New("SSE transport requires a configured HTTP client")
		}
		return newSSETransport(ctx, cfg, httpClient, logger)
	default:
		return nil, fmt.Errorf("unsupported MCP transport: %q", cfg.Transport)
	}
}

// Sentinel error returned when an SSE stream is closed before any endpoint
// event has been observed. Caller treats this as a transport startup failure.
var errSSEEndpointMissing = errors.New("SSE endpoint event not received before stream close")

// keepaliveTimeout governs SSE Alive() — no traffic for this long is treated
// as a dead stream (server crashed / network partitioned).
const keepaliveTimeout = 90 * time.Second
