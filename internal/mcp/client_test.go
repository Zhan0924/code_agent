package mcp

import (
	"testing"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
)

func TestNewGateway_EmptyServers(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, err := NewGateway(cfg, zap.NewNop())
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
	gw, _ := NewGateway(cfg, zap.NewNop())

	_, ok := gw.FindServerForTool("nonexistent-tool")
	if ok {
		t.Error("expected tool not found")
	}
}

func TestGateway_GetAvailableTools_Empty(t *testing.T) {
	cfg := &config.MCPConfig{Servers: nil}
	gw, _ := NewGateway(cfg, zap.NewNop())

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
