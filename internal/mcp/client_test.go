package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// SSE was unsupported pre-2026-06; this case now only covers truly-unknown
// transports (e.g. grpc). SSE itself is covered by transport_sse_test.go and
// the AddServer transport-aware validation path.
func TestGateway_ConnectServer_UnsupportedTransport(t *testing.T) {
	cfg := &config.MCPConfig{
		Servers: []config.MCPServerConfig{
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

	// Manually wire up a gateway with this connection (wrapped in a
	// 1-slot ConnPool — production gateways always go through pools).
	gw := &Gateway{
		servers:       map[string]*ConnPool{"timeout-server": newSingletonPool(&config.MCPServerConfig{Name: "timeout-server"}, conn, logger)},
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
		servers:       map[string]*ConnPool{"close-server": newSingletonPool(&config.MCPServerConfig{Name: "close-server"}, conn, logger)},
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
		servers: map[string]*ConnPool{
			"server-a": newSingletonPool(&config.MCPServerConfig{Name: "server-a"}, conn1, logger),
			"server-b": newSingletonPool(&config.MCPServerConfig{Name: "server-b"}, conn2, logger),
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
		servers:       make(map[string]*ConnPool),
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
		servers:       map[string]*ConnPool{"args-server": newSingletonPool(&config.MCPServerConfig{Name: "args-server"}, conn, logger)},
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

// ═══════════════════════════════════════════════════════════════════════════
//  P2-7: JSON-RPC 消息帧 + 工具调用端到端测试
// ═══════════════════════════════════════════════════════════════════════════

func TestServerConnection_ConcurrentSendRequest(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("concurrent-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("concurrent-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	var wg sync.WaitGroup
	const concurrency = 10
	for i := range concurrency {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := conn.sendRequest(ctx, "tools/call", map[string]any{
				"name":      "echo",
				"arguments": map[string]any{"n": n},
			})
			assert.NoError(t, err, "goroutine %d", n)
			assert.NotNil(t, resp, "goroutine %d", n)
		}(i)
	}
	wg.Wait()
	assert.GreaterOrEqual(t, mock.handled.Load(), int64(concurrency))
}

func TestServerConnection_ReadResponses_RoutingByID(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("routing-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		time.Sleep(time.Duration(callID%3) * 10 * time.Millisecond)
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("resp-%d", callID)}},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("routing-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range 5 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := conn.sendRequest(ctx, "tools/call", map[string]any{
				"name":      "echo",
				"arguments": map[string]any{"n": n},
			})
			require.NoError(t, err)
			var result struct {
				Content []struct{ Text string } `json:"content"`
			}
			_ = json.Unmarshal(resp.Result, &result)
			if len(result.Content) > 0 {
				results[n] = result.Content[0].Text
			}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		assert.NotEmpty(t, r, "result %d should not be empty", i)
	}
}

func TestServerConnection_MalformedJSONRPC(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("malformed-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("malformed-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	// 注入 malformed 帧到 client 的 stdout（readResponses 应该跳过而不崩溃）
	mock.writeMu.Lock()
	_, _ = mock.outW.Write([]byte("this is not valid json\n"))
	mock.writeMu.Unlock()
	mock.writeLine(map[string]any{"garbage": true}) // 有效 JSON 但缺少 jsonrpc/id/method

	// 发送正常请求验证连接在 malformed 帧后仍然正常
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := conn.sendRequest(ctx, "tools/list", nil)
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestServerConnection_PendingCleanupOnCancel(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("cancel-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		// 永不响应，模拟 hung server
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("cancel-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := conn.sendRequest(ctx, "tools/call", map[string]any{"name": "echo"})
	assert.Error(t, err)

	// 验证 pending map 已清理
	conn.mu.Lock()
	pendingCount := len(conn.pending)
	conn.mu.Unlock()
	assert.Equal(t, 0, pendingCount, "pending map should be empty after cancel")
}

func TestServerConnection_ProgressNotificationRouting(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("progress-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		if token != "" {
			m.writeProgress(token, "chunk-1", 0.5)
			m.writeProgress(token, "chunk-2", 1.0)
		}
		time.Sleep(20 * time.Millisecond)
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "done"}},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("progress-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	token := "test-progress-token"
	ch := conn.subscribeProgress(token, 32)
	defer conn.unsubscribeProgress(token)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 发送带 progressToken 的请求
	_, err := conn.sendRequest(ctx, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{},
		"_meta":     map[string]any{"progressToken": token},
	})
	require.NoError(t, err)

	// 验证收到 progress 通知
	var chunks []string
	timeout := time.After(1 * time.Second)
	for {
		select {
		case prog, ok := <-ch:
			if !ok {
				goto done
			}
			chunks = append(chunks, prog.Chunk)
			if len(chunks) >= 2 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:
	assert.GreaterOrEqual(t, len(chunks), 2, "should receive at least 2 progress chunks")
}

func TestServerConnection_UnknownNotificationHandling(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("unknown-notif-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("unknown-notif-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	// 注入一条未知通知
	mock.writeLine(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/unknown",
		"params":  map[string]any{"data": "test"},
	})

	// 发送正常请求验证连接仍然正常
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := conn.sendRequest(ctx, "tools/list", nil)
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestGateway_CallTool_SuccessRoundTrip(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("roundtrip-server", logger)
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("roundtrip-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	conn.tools = []MCPTool{{Name: "echo", Description: "echo tool"}}

	gw := &Gateway{
		servers:       map[string]*ConnPool{"roundtrip-server": newSingletonPool(&config.MCPServerConfig{Name: "roundtrip-server"}, conn, logger)},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"echo": "roundtrip-server"},
		logger:        logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := gw.CallTool(ctx, "roundtrip-server", "echo", json.RawMessage(`{"msg":"hello"}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "ok")
}

func TestGateway_CallTool_ErrorResponse(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("error-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "something went wrong"}},
			"isError": true,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("error-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	conn.tools = []MCPTool{{Name: "fail_tool"}}

	gw := &Gateway{
		servers:       map[string]*ConnPool{"error-server": newSingletonPool(&config.MCPServerConfig{Name: "error-server"}, conn, logger)},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"fail_tool": "error-server"},
		logger:        logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := gw.CallTool(ctx, "error-server", "fail_tool", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "something went wrong")
}

func TestGateway_CallTool_MultipleContentTypes(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("multi-content-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "first part"},
				{"type": "text", "text": "second part"},
				{"type": "image", "data": "base64data", "mimeType": "image/png"},
			},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("multi-content-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	conn.tools = []MCPTool{{Name: "multi_tool"}}

	gw := &Gateway{
		servers:       map[string]*ConnPool{"multi-content-server": newSingletonPool(&config.MCPServerConfig{Name: "multi-content-server"}, conn, logger)},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"multi_tool": "multi-content-server"},
		logger:        logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := gw.CallTool(ctx, "multi-content-server", "multi_tool", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "first part")
}

func TestGateway_CallTool_ConcurrentDistribution(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("concurrent-gw-server", logger)
	var callCount atomic.Int64
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		callCount.Add(1)
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("concurrent-gw-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	conn.tools = []MCPTool{{Name: "echo"}}

	gw := &Gateway{
		servers:       map[string]*ConnPool{"concurrent-gw-server": newSingletonPool(&config.MCPServerConfig{Name: "concurrent-gw-server"}, conn, logger)},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"echo": "concurrent-gw-server"},
		logger:        logger,
	}

	var wg sync.WaitGroup
	const numCalls = 20
	for range numCalls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := gw.CallTool(ctx, "concurrent-gw-server", "echo", json.RawMessage(`{"x":1}`))
			assert.NoError(t, err)
			if result != nil {
				assert.False(t, result.IsError)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(numCalls), callCount.Load())
}

func TestGateway_CallTool_ComplexArguments(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("complex-args-server", logger)
	var receivedArgs map[string]any
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		receivedArgs = args
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("complex-args-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	conn.tools = []MCPTool{{Name: "complex_tool"}}

	gw := &Gateway{
		servers:       map[string]*ConnPool{"complex-args-server": newSingletonPool(&config.MCPServerConfig{Name: "complex-args-server"}, conn, logger)},
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     map[string]string{"complex_tool": "complex-args-server"},
		logger:        logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	args := json.RawMessage(`{"nested":{"key":"value"},"array":[1,2,3],"special":"hello \"world\""}`)
	result, err := gw.CallTool(ctx, "complex-args-server", "complex_tool", args)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)

	// 验证复杂参数被正确传递
	assert.NotNil(t, receivedArgs)
	nested, ok := receivedArgs["nested"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "value", nested["key"])
}

func TestServerConnection_StreamProgressEndToEnd(t *testing.T) {
	logger := zaptest.NewLogger(t)
	mock := newMockServer("stream-progress-server", logger)
	mock.onToolCall = func(m *mockServer, callID int64, name string, args map[string]any, token string) {
		if token != "" {
			for i := range 3 {
				m.writeProgress(token, fmt.Sprintf("chunk-%d", i), float64(i+1)/3.0)
				time.Sleep(10 * time.Millisecond)
			}
		}
		time.Sleep(20 * time.Millisecond)
		m.writeResult(callID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "final"}},
			"isError": false,
		})
	}
	mock.start()
	defer mock.close()

	conn := newInMemoryConnection("stream-progress-server", mock.outW, mock.stdinR, mock.stdinW, mock.outR, logger)
	defer conn.close()

	// 直接测试 progress 订阅 + sendRequest 组合
	token := "stream-test-token"
	ch := conn.subscribeProgress(token, 32)
	defer conn.unsubscribeProgress(token)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := conn.sendRequest(ctx, "tools/call", map[string]any{
		"name":      "stream_demo",
		"arguments": map[string]any{},
		"_meta":     map[string]any{"progressToken": token},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	// 收集 progress 通知
	var chunks []string
	timeout := time.After(2 * time.Second)
	for {
		select {
		case prog, ok := <-ch:
			if !ok {
				goto done
			}
			chunks = append(chunks, prog.Chunk)
			if len(chunks) >= 3 {
				goto done
			}
		case <-timeout:
			goto done
		}
	}
done:
	assert.Equal(t, 3, len(chunks), "should receive 3 progress chunks")
	assert.Equal(t, "chunk-0", chunks[0])
	assert.Equal(t, "chunk-1", chunks[1])
	assert.Equal(t, "chunk-2", chunks[2])
}
