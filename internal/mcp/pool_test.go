// Package mcp — pool_test.go：**多子进程连接池 + chunked streaming** 的端到端测试。
//
// 不依赖真实 MCP 子进程：测试通过 io.Pipe 注入 mock server goroutine，
// 从 client 的 stdin 读 JSON-RPC 请求，按脚本回响应/推 progress。
//
// 覆盖点：
//
//	TestPool_StartSucceedsWithAllAlive               —— 4/4 slot 成功启动
//	TestPool_StartPartialFailureUnderMinAlive        —— 仅 1/4 成功且低于 minAlive → err
//	TestPool_StartPartialFailureAboveMinAlive        —— 3/4 成功但 ≥ minAlive → ok
//	TestPool_Pick_LeastPending                        —— 选择 inflight 最少的连接
//	TestPool_CallTool_AllRouteThroughOneConn          —— size=1 时工具调用走单连接
//	TestPool_CallTool_ConcurrentDistribute            —— size=3 并发 30 次；分布 ≥ 2 个 slot
//	TestPool_CallToolStream_ReceivesChunksThenFinal   —— 收到 N 条 progress + 1 条 IsFinal
//	TestPool_CallToolStream_ContextCancel             —— ctx 取消 → IsFinal{Err}
//	TestPool_Close_Idempotent                         —— 重复 Close 不 panic
//	TestPool_Alive_ReflectsDeadSlot                   —— slot 关闭后 Alive 递减
//	TestPool_MonitorOnce                              —— 指标结构字段完备
//
// 所有用例全部 -race + 快速（<2s total）。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// ═══════════════════════════════════════════════════════════════════════════
//  Mock infrastructure —— 用 io.Pipe 模拟 MCP 子进程 stdio
// ═══════════════════════════════════════════════════════════════════════════

// mockServer 是一个"伪 MCP server"。
//
// 结构：
//
//	client side:       [sendRequest] ───► stdin  ═══►  [mock reads reqs]
//	                  [readResponses] ◄── stdout  ═══  [mock writes resps + progress]
//
// 通过 io.Pipe 实现两条字节流；mock 在独立 goroutine 里跑 readLoop，
// 解析请求并生成响应。
//
// 行为配置：
//
//	failHandshake: 让 initialize 返回 error（触发 pool slot dial 失败）
//	onToolCall:    每次收到 tools/call 时被调用，由用例决定返回内容/推 progress
//
// 关键是所有 API（writeResponse / writeProgress / Close）都线程安全，
// 多个 mockServer 能并发跑。
type mockServer struct {
	name string

	// stdio pipes
	stdinR *io.PipeReader // mock 端读（client 写入）
	stdinW *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter

	// 行为
	failHandshake atomic.Bool
	onToolCall    func(m *mockServer, callID int64, name string, args map[string]any, progressToken string)

	// 已处理 req 计数（用于测试负载均衡分布）
	handled atomic.Int64

	// 内部
	writeMu sync.Mutex
	closed  atomic.Bool
	logger  *zap.Logger
}

// newMockServer 构造空的 mockServer（未启动 readLoop）。
func newMockServer(name string, logger *zap.Logger) *mockServer {
	stdinR, stdinW := io.Pipe()
	outR, outW := io.Pipe()
	return &mockServer{
		name:   name,
		stdinR: stdinR,
		stdinW: stdinW,
		outR:   outR,
		outW:   outW,
		logger: logger.With(zap.String("mock_server", name)),
	}
}

// start 启动 mock 的 read/dispatch goroutine。
func (m *mockServer) start() {
	go m.loop()
}

// loop 以行为单位解析 client 请求并 dispatch。
func (m *mockServer) loop() {
	sc := bufio.NewScanner(m.stdinR)
	sc.Buffer(make([]byte, 1<<14), 1<<22)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			m.logger.Debug("bad req", zap.Error(err))
			continue
		}
		m.handled.Add(1)
		switch req.Method {
		case "initialize":
			if m.failHandshake.Load() {
				m.writeError(req.ID, -32000, "simulated init failure")
				continue
			}
			m.writeResult(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]any{"name": m.name, "version": "1.0"},
				"capabilities":    map[string]any{},
			})
		case "notifications/initialized":
			// 规范上是通知，这里 server 被当作 request 收到（gateway
			// 的 handshake 用 sendRequest 发），直接回空 result 即可。
			m.writeResult(req.ID, map[string]any{})
		case "tools/list":
			m.writeResult(req.ID, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "echo",
						"description": "echo the input",
						"inputSchema": map[string]any{"type": "object"},
					},
					{
						"name":        "stream_demo",
						"description": "demo streaming tool",
						"inputSchema": map[string]any{"type": "object"},
					},
				},
			})
		case "tools/call":
			// 解析 name + args + _meta.progressToken
			var params struct {
				Name      string                 `json:"name"`
				Arguments map[string]any         `json:"arguments"`
				Meta      map[string]interface{} `json:"_meta"`
			}
			_ = json.Unmarshal([]byte(toJSON(req.Params)), &params)
			var token string
			if params.Meta != nil {
				if v, ok := params.Meta["progressToken"].(string); ok {
					token = v
				}
			}
			if m.onToolCall != nil {
				m.onToolCall(m, req.ID, params.Name, params.Arguments, token)
			} else {
				// 默认 echo
				m.writeResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
					"isError": false,
				})
			}
		default:
			m.writeError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

// writeLine 给 client 写一条 NDJSON；线程安全。
func (m *mockServer) writeLine(v any) {
	if m.closed.Load() {
		return
	}
	b, _ := json.Marshal(v)
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_, _ = m.outW.Write(b)
	_, _ = m.outW.Write([]byte{'\n'})
}

func (m *mockServer) writeResult(id int64, result any) {
	b, _ := json.Marshal(result)
	m.writeLine(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(b)})
}

func (m *mockServer) writeError(id int64, code int, msg string) {
	m.writeLine(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
}

// writeProgress 推一帧 notifications/progress。
func (m *mockServer) writeProgress(token string, chunk string, progress float64) {
	m.writeLine(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/progress",
		"params": map[string]any{
			"progressToken": token,
			"progress":      progress,
			"total":         1.0,
			"chunk":         chunk,
			"message":       "streaming",
		},
	})
}

// close 关闭所有 pipe；幂等。
func (m *mockServer) close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	_ = m.stdinR.Close()
	_ = m.stdinW.Close()
	_ = m.outR.Close()
	_ = m.outW.Close()
}

// toJSON helper: interface{} → string
func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ─── dialer 注入 ─────────────────────────────────────────────────────────────

// newMockDialer 返回一个 dialer：每次被调用时创建 mockServer + 返回对应
// ServerConnection。返回 servers 切片供用例断言行为。
//
// failSlots 指定哪些 slot 索引在握手阶段失败（模拟进程挂/无法启动）。
// onToolCall 是 server-side tools/call 的统一 handler。
func newMockDialer(logger *zap.Logger,
	onToolCall func(m *mockServer, callID int64, name string, args map[string]any, token string),
	failSlots map[int]bool,
) (func(cfg *config.MCPServerConfig, logger *zap.Logger) (*ServerConnection, error), *mockRegistry) {

	reg := &mockRegistry{}
	var idx atomic.Int32

	return func(cfg *config.MCPServerConfig, logger *zap.Logger) (*ServerConnection, error) {
		slot := int(idx.Add(1) - 1)
		m := newMockServer(fmt.Sprintf("%s#%d", cfg.Name, slot), logger)
		if failSlots[slot] {
			m.failHandshake.Store(true)
		}
		m.onToolCall = onToolCall
		m.start()

		reg.add(m)
		// 把 mock 的 pipes 伪装成 stdin/stdout 交给 ServerConnection
		conn := newInMemoryConnection(
			cfg.Name,
			m.outW,
			m.stdinR,
			m.stdinW, // client stdin = mock stdinW
			m.outR,   // client stdout = mock outR
			logger,
		)
		return conn, nil
	}, reg
}

// mockRegistry 持有所有 dial 出来的 mockServer，便于用例访问。
type mockRegistry struct {
	mu      sync.Mutex
	servers []*mockServer
}

func (r *mockRegistry) add(m *mockServer) {
	r.mu.Lock()
	r.servers = append(r.servers, m)
	r.mu.Unlock()
}
func (r *mockRegistry) all() []*mockServer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*mockServer, len(r.servers))
	copy(out, r.servers)
	return out
}

// newPoolWithMocks 构造 + Start 一个注入 mock dialer 的 ConnPool。
// 返回 pool, mock registry, cleanup。
func newPoolWithMocks(t *testing.T, name string, size int,
	onToolCall func(m *mockServer, id int64, tname string, args map[string]any, token string),
	failSlots map[int]bool,
) (*ConnPool, *mockRegistry, func()) {
	t.Helper()
	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{
		Name:      name,
		Transport: "stdio",
		PoolSize:  size,
	}
	dialer, reg := newMockDialer(logger, onToolCall, failSlots)
	pool := NewConnPool(cfg, logger)
	pool.dialer = dialer

	// 握手函数用 Gateway.initializeServer 的同构实现
	handshake := func(ctx context.Context, conn *ServerConnection) error {
		if _, err := conn.sendRequest(ctx, "initialize", map[string]any{
			"protocolVersion": "2024-11-05",
		}); err != nil {
			return err
		}
		_, _ = conn.sendRequest(ctx, "notifications/initialized", nil)
		resp, err := conn.sendRequest(ctx, "tools/list", nil)
		if err != nil {
			return err
		}
		var wr struct {
			Tools []MCPTool `json:"tools"`
		}
		_ = json.Unmarshal(resp.Result, &wr)
		conn.tools = wr.Tools
		return nil
	}
	err := pool.Start(context.Background(), handshake)
	cleanup := func() {
		_ = pool.Close()
		for _, m := range reg.all() {
			m.close()
		}
	}
	if err != nil {
		cleanup()
		t.Fatalf("pool.Start: %v", err)
	}
	return pool, reg, cleanup
}

// ═══════════════════════════════════════════════════════════════════════════
//  Tests
// ═══════════════════════════════════════════════════════════════════════════

// TestPool_StartSucceedsWithAllAlive 基线：4/4 slot 全部握手成功。
func TestPool_StartSucceedsWithAllAlive(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "ok", 4, nil, nil)
	defer cleanup()

	if got := pool.Alive(); got != 4 {
		t.Fatalf("Alive=%d want 4", got)
	}
	if got := pool.Size(); got != 4 {
		t.Errorf("Size=%d want 4", got)
	}
	if len(pool.Tools()) == 0 {
		t.Errorf("Tools should be populated from handshake")
	}
}

// TestPool_StartPartialFailureUnderMinAlive 4 个 slot 中 3 个失败 → 只剩 1，
// 低于 minAlive=2，Start 必须返回错误（且 Close 之前确保不泄漏进程）。
func TestPool_StartPartialFailureUnderMinAlive(t *testing.T) {
	logger := zaptest.NewLogger(t)
	cfg := &config.MCPServerConfig{Name: "bad", Transport: "stdio", PoolSize: 4}
	dialer, reg := newMockDialer(logger, nil, map[int]bool{0: true, 1: true, 2: true}) // 0/1/2 fail
	pool := NewConnPool(cfg, logger)
	pool.dialer = dialer
	defer func() {
		_ = pool.Close()
		for _, m := range reg.all() {
			m.close()
		}
	}()

	handshake := func(ctx context.Context, conn *ServerConnection) error {
		_, err := conn.sendRequest(ctx, "initialize", nil)
		return err
	}
	err := pool.Start(context.Background(), handshake)
	if err == nil {
		t.Fatalf("expected error when only 1/4 alive (min=2)")
	}
	if !strings.Contains(err.Error(), "alive") {
		t.Errorf("error should mention alive count: %v", err)
	}
}

// TestPool_StartPartialFailureAboveMinAlive 4 个 slot 中 1 个失败 →
// 3 alive ≥ minAlive=2，Start 成功；Alive()==3。
func TestPool_StartPartialFailureAboveMinAlive(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "partial", 4, nil, map[int]bool{2: true})
	defer cleanup()
	if got := pool.Alive(); got != 3 {
		t.Fatalf("Alive=%d want 3", got)
	}
}

// TestPool_Pick_LeastPending 让 conn-0 挂 5 个 pending、conn-1 挂 1 个，
// Pick 必须返回 conn-1。
func TestPool_Pick_LeastPending(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "lb", 2, func(m *mockServer, id int64, _ string, _ map[string]any, _ string) {
		// 故意拖延回复：后续 goroutine 监测 inflight 差异
		go func(mm *mockServer, cid int64) {
			time.Sleep(50 * time.Millisecond)
			mm.writeResult(cid, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
			})
		}(m, id)
	}, nil)
	defer cleanup()

	c0 := pool.conns[0].Load()
	c1 := pool.conns[1].Load()
	// 手动注入 inflight —— 模拟"conn-0 正忙 5 个请求"。
	c0.inflight.Store(5)
	c1.inflight.Store(1)

	got := pool.Pick()
	if got != c1 {
		t.Fatalf("Pick should return least-pending (c1 with 1), got %v", got)
	}

	// 翻转情况：c1 更忙
	c0.inflight.Store(0)
	c1.inflight.Store(7)
	got = pool.Pick()
	if got != c0 {
		t.Fatalf("Pick should return c0 now, got %v", got)
	}
}

// TestPool_CallTool_AllRouteThroughOneConn size=1 时所有流量走同一连接。
func TestPool_CallTool_AllRouteThroughOneConn(t *testing.T) {
	pool, reg, cleanup := newPoolWithMocks(t, "one", 1, func(m *mockServer, id int64, _ string, _ map[string]any, _ string) {
		m.writeResult(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "hi"}},
		})
	}, nil)
	defer cleanup()

	for i := 0; i < 5; i++ {
		res, err := pool.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
		if err != nil || res == nil || res.Content != "hi" {
			t.Fatalf("iter %d: res=%+v err=%v", i, res, err)
		}
	}
	if servers := reg.all(); len(servers) != 1 {
		t.Errorf("expect 1 mock server, got %d", len(servers))
	}
}

// TestPool_CallTool_ConcurrentDistribute size=3，发 30 次并发请求，
// 每个 server 应都处理过一些（least-pending 对等负载下接近均分）。
func TestPool_CallTool_ConcurrentDistribute(t *testing.T) {
	pool, reg, cleanup := newPoolWithMocks(t, "dist", 3, func(m *mockServer, id int64, _ string, _ map[string]any, _ string) {
		// 加一点延迟让 inflight 有差异，LB 决策才有意义
		time.Sleep(5 * time.Millisecond)
		m.writeResult(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": m.name}},
		})
	}, nil)
	defer cleanup()

	var wg sync.WaitGroup
	const N = 30
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pool.CallTool(context.Background(), "echo", json.RawMessage(`{}`)); err != nil {
				t.Errorf("call err: %v", err)
			}
		}()
	}
	wg.Wait()

	// 统计每个 mock server 的 handled 次数 —— 减去 3 次 handshake 开销
	// （initialize, notifications/initialized, tools/list）
	busyServers := 0
	for _, m := range reg.all() {
		// handshake 阶段每台处理 3 个请求
		if m.handled.Load() > 3 {
			busyServers++
		}
	}
	if busyServers < 2 {
		t.Fatalf("expect load distributed across ≥2 conns, only %d busy; per-conn handled=%v",
			busyServers, handleCounts(reg))
	}
}

func handleCounts(r *mockRegistry) []int64 {
	xs := r.all()
	out := make([]int64, len(xs))
	for i, m := range xs {
		out[i] = m.handled.Load()
	}
	return out
}

// TestPool_CallToolStream_ReceivesChunksThenFinal 服务端推 3 条 chunk
// 再回 result；客户端应收到"chunk,chunk,chunk,final"顺序。
func TestPool_CallToolStream_ReceivesChunksThenFinal(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "stream", 1, func(m *mockServer, id int64, _ string, _ map[string]any, token string) {
		go func() {
			m.writeProgress(token, "hello ", 0.25)
			time.Sleep(5 * time.Millisecond)
			m.writeProgress(token, "world ", 0.50)
			time.Sleep(5 * time.Millisecond)
			m.writeProgress(token, "!", 0.75)
			time.Sleep(5 * time.Millisecond)
			m.writeResult(id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "hello world !"}},
				"isError": false,
			})
		}()
	}, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := pool.CallToolStream(ctx, "stream_demo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallToolStream: %v", err)
	}

	var (
		chunks  []string
		final   *ToolChunk
		timeout = time.After(2 * time.Second)
	)
loop:
	for {
		select {
		case <-timeout:
			t.Fatalf("timeout; chunks=%v final=%+v", chunks, final)
		case c, ok := <-ch:
			if !ok {
				break loop
			}
			if c.IsFinal {
				fc := c
				final = &fc
			} else {
				chunks = append(chunks, c.Content)
			}
		}
	}

	if len(chunks) < 3 {
		t.Errorf("want ≥3 chunks, got %v", chunks)
	}
	if final == nil || final.Err != nil {
		t.Fatalf("final missing or errored: %+v", final)
	}
	if final.Final == nil || !strings.Contains(final.Final.Content, "hello world") {
		t.Errorf("final content missing: %+v", final.Final)
	}
}

// TestPool_CallToolStream_ContextCancel 取消 ctx → 拿到带 Err 的 Final 帧
// 然后通道关闭。
func TestPool_CallToolStream_ContextCancel(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "cancel", 1, func(m *mockServer, id int64, _ string, _ map[string]any, token string) {
		// server 慢吞吞地推，永远不发最终 result
		go func() {
			for i := 0; i < 100; i++ {
				m.writeProgress(token, fmt.Sprintf("chunk-%d", i), float64(i)/100)
				time.Sleep(20 * time.Millisecond)
			}
			_ = id // 永不 writeResult
		}()
	}, nil)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	ch, err := pool.CallToolStream(ctx, "stream_demo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallToolStream: %v", err)
	}

	var saw = false
	deadline := time.After(1 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				if !saw {
					t.Errorf("channel closed without IsFinal frame")
				}
				return
			}
			if c.IsFinal {
				if c.Err == nil {
					t.Errorf("expected Err on ctx-cancelled final, got %+v", c)
				}
				saw = true
			}
		case <-deadline:
			t.Fatalf("stream did not close after ctx cancel")
		}
	}
}

// TestPool_Close_Idempotent 多次 Close 不 panic、不泄漏。
func TestPool_Close_Idempotent(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "cls", 2, nil, nil)
	defer cleanup()
	if err := pool.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if pool.Alive() != 0 {
		t.Errorf("alive should be 0 after close, got %d", pool.Alive())
	}
}

// TestPool_Alive_ReflectsDeadSlot 手动把一个 slot 换成 nil（模拟进程挂掉），
// Alive() 应该感知到。
func TestPool_Alive_ReflectsDeadSlot(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "dead", 3, nil, nil)
	defer cleanup()

	if pool.Alive() != 3 {
		t.Fatalf("Alive=%d want 3", pool.Alive())
	}
	// 模拟进程挂掉
	if c := pool.conns[1].Swap(nil); c != nil {
		_ = c.close()
	}
	if pool.Alive() != 2 {
		t.Fatalf("after swapping slot 1 to nil, Alive=%d want 2", pool.Alive())
	}
	// Pick 仍能返回剩下的活连接
	if got := pool.Pick(); got == nil {
		t.Error("Pick should still return an alive conn")
	}
}

// TestPool_MonitorOnce 指标快照字段完备；死槽 Inflight=-1。
func TestPool_MonitorOnce(t *testing.T) {
	pool, _, cleanup := newPoolWithMocks(t, "mon", 3, nil, nil)
	defer cleanup()

	// 杀一个
	if c := pool.conns[2].Swap(nil); c != nil {
		_ = c.close()
	}

	m := pool.MonitorOnce()
	if m.Name != "mon" || m.Size != 3 || m.Alive != 2 {
		t.Errorf("metrics basics wrong: %+v", m)
	}
	if len(m.Inflight) != 3 {
		t.Fatalf("inflight len want 3, got %d", len(m.Inflight))
	}
	if m.Inflight[2] != -1 {
		t.Errorf("dead slot should have inflight=-1, got %v", m.Inflight[2])
	}
	if time.Since(m.Snapshot) > time.Second {
		t.Errorf("snapshot timestamp too old: %v", m.Snapshot)
	}
}
