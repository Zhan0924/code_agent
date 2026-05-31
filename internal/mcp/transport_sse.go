// Package mcp - transport_sse.go implements the HTTP+SSE transport for MCP
// servers per the 2024-11-05 spec.
//
// Wire model: client opens GET <baseURL> as an SSE stream. The first event
// received is `event: endpoint` whose data payload is the relative POST URL
// (with a session token) that the client must use for all subsequent
// JSON-RPC requests. JSON-RPC responses are pushed by the server over the
// SSE stream as `event: message` (or default) frames whose data payload is
// the response JSON.
//
// This implementation owns ONE long-lived SSE GET and reuses the underlying
// HTTP client (which already enforces the egress ACL) for POSTs. Reconnect
// is not automatic — the orchestrator-level healthChecker observes Alive()
// going false and triggers Gateway.reconnectServer, which dials a fresh
// transport.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

type sseTransport struct {
	name       string
	baseURL    string
	httpClient *http.Client
	logger     *zap.Logger

	// SSE stream (server → client)
	sseResp   *http.Response
	sseReader *bufio.Reader

	// POST endpoint URL (received via "endpoint" event)
	postURL      string
	postURLReady chan struct{}
	postURLOnce  sync.Once

	// Track last received SSE event for keepalive-based Alive().
	lastRecv atomic.Int64

	closed atomic.Bool
	mu     sync.Mutex
	err    error
}

func newSSETransport(ctx context.Context, cfg *config.MCPServerConfig, httpClient *http.Client, logger *zap.Logger) (*sseTransport, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("SSE MCP server %q requires a URL", cfg.Name)
	}

	t := &sseTransport{
		name:         cfg.Name,
		baseURL:      cfg.URL,
		httpClient:   httpClient,
		logger:       logger.With(zap.String("mcp_transport", "sse"), zap.String("server", cfg.Name)),
		postURLReady: make(chan struct{}),
	}
	t.lastRecv.Store(time.Now().UnixNano())

	if err := t.openStream(ctx); err != nil {
		return nil, fmt.Errorf("open SSE stream: %w", err)
	}
	return t, nil
}

// openStream issues the GET <baseURL> with Accept: text/event-stream and
// holds the response body open as a bufio.Reader. The actual SSE parsing
// happens inline in Recv().
func (t *sseTransport) openStream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return fmt.Errorf("SSE GET returned %d: %s", resp.StatusCode, string(body))
	}

	t.sseResp = resp
	t.sseReader = bufio.NewReaderSize(resp.Body, 1<<22) // 4MB buffer
	return nil
}

// Send POSTs one JSON-RPC payload to the endpoint URL advertised by the
// server via the initial `endpoint` event. Blocks up to 5s waiting for the
// endpoint URL on the first send; subsequent sends return immediately.
func (t *sseTransport) Send(data []byte) error {
	if t.closed.Load() {
		return fmt.Errorf("SSE transport %q closed", t.name)
	}

	select {
	case <-t.postURLReady:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("SSE endpoint event not received within 5s")
	}

	postURL := t.resolvePostURL()
	req, err := http.NewRequest(http.MethodPost, postURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE POST to %s: %w", postURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SSE POST returned %d: %s", resp.StatusCode, string(body))
	}
	// 200 / 202 / 204 all acceptable — actual JSON-RPC response will arrive
	// asynchronously via the SSE stream.
	return nil
}

// resolvePostURL joins the relative endpoint path emitted by the server
// with the base URL to handle servers that emit paths like "/messages?...".
func (t *sseTransport) resolvePostURL() string {
	t.mu.Lock()
	rel := t.postURL
	t.mu.Unlock()
	if strings.HasPrefix(rel, "http://") || strings.HasPrefix(rel, "https://") {
		return rel
	}
	// Strip query/fragment from baseURL, then concat.
	base := t.baseURL
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	// If base ends with "/sse" or similar, take the host root.
	if u := strings.LastIndex(base, "/"); u > strings.Index(base, "://")+2 {
		base = base[:u]
	}
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return base + rel
}

// Recv parses the SSE event stream and returns the next `message` event
// payload. Endpoint events are consumed internally to set t.postURL.
func (t *sseTransport) Recv() ([]byte, bool) {
	var (
		eventType = ""
		dataBuf   []byte
	)
	for {
		if t.closed.Load() {
			return nil, false
		}
		line, err := t.sseReader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				t.mu.Lock()
				t.err = err
				t.mu.Unlock()
				t.logger.Debug("SSE stream read error", zap.Error(err))
			}
			return nil, false
		}
		line = bytes.TrimRight(line, "\r\n")

		// Empty line = end of event
		if len(line) == 0 {
			if len(dataBuf) == 0 {
				eventType = ""
				continue
			}
			t.lastRecv.Store(time.Now().UnixNano())
			switch eventType {
			case "endpoint":
				t.mu.Lock()
				t.postURL = string(dataBuf)
				t.mu.Unlock()
				t.postURLOnce.Do(func() { close(t.postURLReady) })
				t.logger.Debug("received SSE endpoint", zap.String("post_url", string(dataBuf)))
			case "", "message":
				out := dataBuf
				return out, true
			default:
				t.logger.Debug("ignoring unknown SSE event", zap.String("event", eventType))
			}
			eventType = ""
			dataBuf = nil
			continue
		}

		// Comment line (heartbeat)
		if line[0] == ':' {
			t.lastRecv.Store(time.Now().UnixNano())
			continue
		}

		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			payload := line[len("data:"):]
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, payload...)
		case bytes.HasPrefix(line, []byte("id:")), bytes.HasPrefix(line, []byte("retry:")):
			// ignore — MCP doesn't use these
		default:
			t.logger.Debug("unknown SSE field", zap.ByteString("line", line))
		}
	}
}

func (t *sseTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Alive returns true if the SSE stream is open AND has emitted SOME traffic
// within keepaliveTimeout. A long quiet stream is indistinguishable from a
// silently-dropped connection, so we treat it as dead and let the gateway
// trigger reconnect.
func (t *sseTransport) Alive() bool {
	if t.closed.Load() {
		return false
	}
	last := t.lastRecv.Load()
	if last == 0 {
		return true // brand new, no traffic yet
	}
	elapsed := time.Now().UnixNano() - last
	return elapsed < int64(keepaliveTimeout)
}

func (t *sseTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Signal any blocked Send() waiting on postURLReady.
	t.postURLOnce.Do(func() { close(t.postURLReady) })
	if t.sseResp != nil {
		return t.sseResp.Body.Close()
	}
	return nil
}
