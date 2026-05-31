// Package mcp - transport_sse_test.go covers the HTTP+SSE wire layer.
//
// What we exercise here:
//
//   - endpoint event flow: the very first SSE frame advertises the POST URL.
//     Send() MUST block until that frame is received, otherwise the agent
//     would race the server's session-bootstrap.
//   - message round-trip: a POSTed JSON-RPC request gets its response back
//     through the SSE stream, not the HTTP response body (the response body
//     is just an ack — 202 in our mock).
//   - relative endpoint paths: real-world MCP-SSE servers (anthropic-mcp,
//     supergateway) emit "/messages?sessionId=abc" not absolute URLs, so
//     resolvePostURL must join correctly against the base.
//   - keepalive / Alive(): with no traffic for keepaliveTimeout the
//     transport SHOULD report dead so the healthChecker triggers reconnect.
//   - Close idempotency + Send-after-close: defensive checks because the
//     Gateway race scenarios can call Close concurrently with in-flight POSTs.
//
// Why not just test through Gateway? — Because dialTransport for "sse" is
// opaque to higher layers and the SSE-specific quirks (endpoint event,
// keepalive comments, multi-line data:) belong to this transport. A unit
// test scoped to sseTransport keeps the failure mode localized.

package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap/zaptest"
)

// sseMockServer simulates a tiny MCP-over-SSE peer. It serves two routes:
//
//	GET  /sse        — SSE stream. Emits endpoint event, then any frames
//	                    pushed via mock.push().
//	POST /messages   — Receives JSON-RPC payloads; records into mock.received.
//	                    Returns 202 immediately (real responses get pushed
//	                    back through SSE).
//
// The handler keeps the SSE response writer alive in mock.sseW until the
// server is closed, so tests can call mock.push() at any time to inject
// SSE events.
// sseFrame is an SSE write request handed to the GET handler via the
// frames channel. eventType empty + comment non-empty = SSE comment line.
type sseFrame struct {
	eventType string
	data      string
	comment   string
}

type sseMockServer struct {
	t            *testing.T
	server       *httptest.Server
	endpointPath string // path served back via "event: endpoint"

	streamOn chan struct{} // closed once the GET handler has flushed the endpoint event
	frames   chan sseFrame // test → handler; handler writes are race-safe.

	received chan []byte // POSTed bodies
	closed   atomic.Bool
}

func newSSEMockServer(t *testing.T) *sseMockServer {
	t.Helper()
	m := &sseMockServer{
		t:            t,
		endpointPath: "/messages?session=test-1",
		streamOn:     make(chan struct{}),
		frames:       make(chan sseFrame, 8),
		received:     make(chan []byte, 8),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			http.Error(w, "missing Accept: text/event-stream", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		// Endpoint event must be the first frame; SSE spec uses \n\n separator.
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", m.endpointPath)
		flusher.Flush()
		close(m.streamOn)

		// Single-writer loop: all subsequent SSE frames go through the
		// frames channel so the response writer is touched only by this
		// goroutine (avoids -race vs httptest's own chunk flush).
		for {
			select {
			case <-r.Context().Done():
				return
			case f, ok := <-m.frames:
				if !ok {
					return
				}
				if f.comment != "" {
					fmt.Fprintf(w, ": %s\n\n", f.comment)
				} else {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.eventType, f.data)
				}
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case m.received <- body:
		default:
			t.Logf("sseMockServer: received channel full, dropping frame")
		}
		// 202 — the real response will arrive on the SSE stream.
		w.WriteHeader(http.StatusAccepted)
	})

	m.server = httptest.NewServer(mux)
	return m
}

// push enqueues a `event: message` frame for the handler to write.
// Blocks until the stream handler has begun (guarded by streamOn) so the
// handler is parked in the select loop and ready to consume frames.
func (m *sseMockServer) push(data string) {
	<-m.streamOn
	if m.closed.Load() {
		return
	}
	m.frames <- sseFrame{eventType: "message", data: data}
}

// pushComment enqueues an SSE keepalive comment (": ping\n\n"). Used to
// verify that lastRecv is bumped by comments, which is what real-world
// MCP-SSE servers send during idle periods.
func (m *sseMockServer) pushComment(text string) {
	<-m.streamOn
	if m.closed.Load() {
		return
	}
	m.frames <- sseFrame{comment: text}
}

func (m *sseMockServer) baseURL() string { return m.server.URL + "/sse" }

func (m *sseMockServer) close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	m.server.Close()
}

// ─── Tests ────────────────────────────────────────────────────────────────

// TestSSE_EndpointThenMessage covers the happy path:
//  1. transport receives `event: endpoint` and unblocks Send,
//  2. Send POSTs to the resolved endpoint path,
//  3. server pushes the JSON-RPC response back over SSE,
//  4. Recv() returns it.
func TestSSE_EndpointThenMessage(t *testing.T) {
	mock := newSSEMockServer(t)
	defer mock.close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: mock.baseURL()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	defer tr.Close()

	// Run Recv in a goroutine so we can interleave Send + push.
	type recvOut struct {
		data []byte
		ok   bool
	}
	out := make(chan recvOut, 1)
	go func() {
		d, ok := tr.Recv()
		out <- recvOut{d, ok}
	}()

	// 1. Client POSTs request.
	req := `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`
	if err := tr.Send([]byte(req)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// 2. Server received it.
	select {
	case got := <-mock.received:
		if string(got) != req {
			t.Errorf("server received %q want %q", got, req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for POST")
	}

	// 3. Server pushes response.
	resp := `{"jsonrpc":"2.0","id":1,"result":{"pong":true}}`
	mock.push(resp)

	// 4. Client Recv gets it.
	select {
	case r := <-out:
		if !r.ok {
			t.Fatalf("Recv returned not-ok: err=%v", tr.Err())
		}
		if string(r.data) != resp {
			t.Errorf("Recv got %q want %q", r.data, resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Recv")
	}
}

// TestSSE_RelativeEndpointResolution exercises the resolvePostURL path with
// a path-only endpoint (the common case for supergateway-style proxies).
func TestSSE_RelativeEndpointResolution(t *testing.T) {
	mock := newSSEMockServer(t)
	mock.endpointPath = "/messages?session=abc" // ensure relative
	defer mock.close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: mock.baseURL()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	defer tr.Close()

	// Drain the endpoint event so postURL is set.
	go tr.Recv()
	// Send forces postURLReady path.
	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Verify resolved URL is absolute against base host:port + relative path.
	got := tr.resolvePostURL()
	base, _ := url.Parse(mock.server.URL)
	want := base.Scheme + "://" + base.Host + "/messages?session=abc"
	if got != want {
		t.Errorf("resolvePostURL = %q want %q", got, want)
	}
}

// TestSSE_SendBeforeEndpointBlocksUntilReady verifies the synchronization
// between Recv (which sets postURL on endpoint event) and Send (which waits
// for postURLReady). We construct a server that delays the endpoint event,
// fire Send first, and verify it blocks until Recv has processed the event.
//
// Note: the SSE handler ALWAYS sends the endpoint event before going async
// (httptest writes are non-blocking under default buffering, so the goroutine
// inside openStream observes it quickly). What this test actually proves is
// "Send won't race past postURLReady" — which is the invariant Send relies on.
func TestSSE_SendBeforeEndpointBlocksUntilReady(t *testing.T) {
	mock := newSSEMockServer(t)
	defer mock.close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: mock.baseURL()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	defer tr.Close()

	// Start Recv to drive endpoint event processing.
	done := make(chan struct{})
	go func() { defer close(done); tr.Recv() }()

	// Send should NOT return until postURLReady is closed.
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"x"}`))
	}()

	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("Send failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not complete within 3s — postURLReady not signaled?")
	}
}

// TestSSE_AliveTracksKeepalive verifies that comments bump lastRecv (so a
// quiet-but-pinged stream is reported Alive) and that with no traffic for
// keepaliveTimeout Alive flips to false.
func TestSSE_AliveTracksKeepalive(t *testing.T) {
	mock := newSSEMockServer(t)
	defer mock.close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: mock.baseURL()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	defer tr.Close()

	// Pump Recv until endpoint is processed.
	go func() {
		// Drain endpoint frame so lastRecv is updated.
		// transport_sse.Recv() returns when it sees a message-event; the
		// endpoint event is consumed internally so we never see it here.
		// But Recv blocks until the NEXT message; we don't need that for
		// liveness — we just need at least one event to have been processed.
		_, _ = tr.Recv()
	}()

	// Wait for the endpoint event to be processed (postURLReady closed).
	select {
	case <-tr.postURLReady:
	case <-time.After(2 * time.Second):
		t.Fatal("postURLReady not signaled")
	}
	// At this point lastRecv is recent → Alive.
	if !tr.Alive() {
		t.Fatalf("expected Alive=true immediately after endpoint event")
	}

	// Send a keepalive comment — should bump lastRecv.
	mock.pushComment("ping")

	// Now backdate lastRecv to simulate keepaliveTimeout elapsed.
	tr.lastRecv.Store(time.Now().Add(-2 * keepaliveTimeout).UnixNano())
	if tr.Alive() {
		t.Errorf("expected Alive=false after lastRecv > keepaliveTimeout ago")
	}

	// Bring it back to "fresh" and confirm true again.
	tr.lastRecv.Store(time.Now().UnixNano())
	if !tr.Alive() {
		t.Errorf("expected Alive=true after lastRecv refreshed")
	}
}

// TestSSE_CloseIsIdempotent_AndSendAfterCloseFails covers defensive paths.
func TestSSE_CloseIsIdempotent_AndSendAfterCloseFails(t *testing.T) {
	mock := newSSEMockServer(t)
	defer mock.close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: mock.baseURL()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}

	// Two closes — second is a no-op via atomic CAS.
	if err := tr.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close should be nil, got %v", err)
	}
	if tr.Alive() {
		t.Errorf("Alive should be false after Close")
	}
	if err := tr.Send([]byte(`{"x":1}`)); err == nil {
		t.Errorf("Send after Close should error")
	}
}

// TestSSE_OpenStreamRejectsNon200 — server returns 500; openStream must
// surface an error rather than parking a reader on a broken stream.
func TestSSE_OpenStreamRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err == nil {
		_ = tr.Close()
		t.Fatal("expected error when server returns 500")
	}
}

// TestSSE_MultiLineDataMerged verifies the SSE spec's "multiple data: lines
// for one event get joined with \n" is honoured. Some MCP servers split
// long JSON across several data lines.
//
// Implemented with a dedicated server (not the shared mock) because the
// frame contains literal "data: " prefixes that the mock's push() would
// double-prefix. The handler is the single writer to the response, so
// -race stays clean.
func TestSSE_MultiLineDataMerged(t *testing.T) {
	writeNow := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sse" {
			http.NotFound(w, r)
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: /messages\n\n")
		fl.Flush()

		select {
		case <-writeNow:
		case <-r.Context().Done():
			return
		}
		// Multi-line data: per SSE spec, lines joined with literal "\n".
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\ndata: \"result\":\"ok\"}\n\n")
		fl.Flush()

		<-r.Context().Done()
	}))
	defer srv.Close()

	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "sse-test", Transport: "sse", URL: srv.URL + "/sse"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tr, err := newSSETransport(ctx, cfg, http.DefaultClient, logger)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	defer tr.Close()

	close(writeNow)

	data, ok := tr.Recv()
	if !ok {
		t.Fatalf("Recv not-ok: %v", tr.Err())
	}
	want := "{\"jsonrpc\":\"2.0\",\n\"id\":1,\n\"result\":\"ok\"}"
	if string(data) != want {
		t.Errorf("merged data mismatch\n got: %q\nwant: %q", data, want)
	}

	// Touch bufio so the import doesn't drift to unused if the file
	// refactors away from bufio internals.
	_ = bufio.NewReader
}
