package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func TestNewGateway_EmptyServers(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, err := NewGateway(cfg, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("expected no error for empty MCP config: %v", err)
	}
	tools := gw.GetAvailableTools()
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestGateway_FindServerForTool_NotFound(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, _ := NewGateway(cfg, nil, zap.NewNop())

	_, ok := gw.FindServerForTool("nonexistent-tool")
	if ok {
		t.Error("expected tool not found")
	}
}

func TestGateway_GetAvailableTools_Empty(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, _ := NewGateway(cfg, nil, zap.NewNop())

	tools := gw.GetAvailableTools()
	if tools == nil {
		// nil is acceptable for empty
	}
	if len(tools) != 0 {
		t.Errorf("expected empty tools list, got %d", len(tools))
	}
}

func TestJSONRPCError_Error(t *testing.T) {
	e := &JSONRPCError{Code: -32601, Message: "Method not found"}
	msg := e.Error()
	if msg != "JSON-RPC error -32601: Method not found" {
		t.Errorf("unexpected error message: %s", msg)
	}
}

func TestMCPContent_Fields(t *testing.T) {
	c := MCPContent{Type: "text", Text: "hello"}
	if c.Type != "text" {
		t.Errorf("expected text, got %s", c.Type)
	}
}

func TestGateway_ConnectServer_StdioTransport(t *testing.T) {
	cfg := &config.MCPConfig{
		Servers: []config.MCPServerConfig{
			{
				Name:      "nonexistent-binary",
				Transport: "stdio",
				Command:   "this-binary-does-not-exist-anywhere-12345",
			},
		},
	}
	gw, err := NewGateway(cfg, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("NewGateway should not return error even if server fails to connect: %v", err)
	}
	if len(gw.servers) != 0 {
		t.Errorf("expected 0 connected servers when binary is missing, got %d", len(gw.servers))
	}
	tools := gw.GetAvailableTools()
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestGateway_ConnectServer_UnsupportedTransport(t *testing.T) {
	cfg := &config.MCPConfig{
		Servers: []config.MCPServerConfig{
			{
				Name:      "sse-server",
				Transport: "sse",
				URL:       "http://localhost:9999/mcp",
			},
			{
				Name:      "grpc-server",
				Transport: "grpc",
			},
		},
	}
	gw, err := NewGateway(cfg, nil, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("NewGateway should not error for unsupported transports: %v", err)
	}
	if len(gw.servers) != 0 {
		t.Errorf("expected 0 connected servers for unsupported transports, got %d", len(gw.servers))
	}
}

func TestGateway_CallTool_ServerNotFound(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, _ := NewGateway(cfg, nil, zap.NewNop())

	result, err := gw.CallTool(context.Background(), "nonexistent-server", "some-tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when calling tool on nonexistent server")
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func TestGateway_CallTool_Timeout(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("timeout-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		// Never respond — simulates a hung server
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("timeout-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)

	// Manually wire up a gateway with this connection
	gw := &Gateway{
		servers:       map[string]*ServerConnection{"timeout-server": conn},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"slow-tool": "timeout-server"},
		logger:        logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := gw.CallTool(ctx, "timeout-server", "slow-tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool should not return raw error on timeout, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected a ToolResult with IsError=true")
	}
	if !result.IsError {
		t.Errorf("expected IsError=true on timeout, got false; content=%s", result.Content)
	}
}

func TestGateway_Close(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("close-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("close-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)

	gw := &Gateway{
		servers:       map[string]*ServerConnection{"close-server": conn},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"tool-a": "close-server"},
		logger:        logger,
	}

	err := gw.Close()
	if err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if len(gw.servers) != 0 {
		t.Errorf("expected servers map to be empty after Close, got %d", len(gw.servers))
	}

	// Calling Close again should be safe
	err = gw.Close()
	if err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

func TestGateway_GetToolDefinitions(t *testing.T) {
	logger := zaptest.NewLogger(t)

	// Create two mock servers with different tools
	mock1 := newMockServer("server-a", logger)
	mock1.start()
	defer mock1.close()
	conn1 := newInMemoryConnection("server-a", mock1.outW, mock1.stdinR, mock1.stdinW, mock1.outR, logger)
	conn1.tools = []MCPTool{
		{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "write_file", Description: "Write a file", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	mock2 := newMockServer("server-b", logger)
	mock2.start()
	defer mock2.close()
	conn2 := newInMemoryConnection("server-b", mock2.outW, mock2.stdinR, mock2.stdinW, mock2.outR, logger)
	conn2.tools = []MCPTool{
		{Name: "search", Description: "Search code", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}

	gw := &Gateway{
		servers: map[string]*ServerConnection{
			"server-a": conn1,
			"server-b": conn2,
		},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex: map[string]string{
			"read_file":  "server-a",
			"write_file": "server-a",
			"search":     "server-b",
		},
		logger: logger,
	}

	tools := gw.GetAvailableTools()
	if len(tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(tools))
	}

	toolNames := make(map[string]bool)
	for _, td := range tools {
		toolNames[td.Name] = true
		if td.Source == "" {
			t.Errorf("tool %s has empty Source", td.Name)
		}
	}
	for _, name := range []string{"read_file", "write_file", "search"} {
		if !toolNames[name] {
			t.Errorf("expected tool %s in definitions", name)
		}
	}
}

func TestGateway_FindServerForTool_WithServers(t *testing.T) {
	gw := &Gateway{
		servers:       make(map[string]*ServerConnection),
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex: map[string]string{
			"git_status": "git-server",
			"db_query":   "postgres-server",
		},
		logger: zap.NewNop(),
	}

	server, ok := gw.FindServerForTool("git_status")
	if !ok || server != "git-server" {
		t.Errorf("expected git-server, got %s (ok=%v)", server, ok)
	}

	server, ok = gw.FindServerForTool("db_query")
	if !ok || server != "postgres-server" {
		t.Errorf("expected postgres-server, got %s (ok=%v)", server, ok)
	}

	_, ok = gw.FindServerForTool("nonexistent")
	if ok {
		t.Error("expected not found for nonexistent tool")
	}
}

func TestGateway_CallTool_InvalidArgs(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("args-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("args-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)

	gw := &Gateway{
		servers:       map[string]*ServerConnection{"args-server": conn},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"tool": "args-server"},
		logger:        logger,
	}

	// Invalid JSON args
	_, err := gw.CallTool(context.Background(), "args-server", "tool", json.RawMessage(`{invalid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON args")
	}
}
