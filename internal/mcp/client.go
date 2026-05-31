// Package mcp implements the Model Context Protocol (MCP) client and tool registry.
// It enables dynamic discovery and invocation of external tools via JSON-RPC 2.0.
//
// ============================================================================
//
//	设 计 原 理（核心要点）
//
// ============================================================================
//
// 【MCP 是什么】
//
//	Model Context Protocol 是 Anthropic 发起的开放协议（类似 "LSP for LLM"），
//	规范了 AI Agent 与外部工具/数据源之间的通信契约：
//	  - 传输层：stdio（子进程）或 HTTP SSE（全双工）
//	  - 消息层：JSON-RPC 2.0
//	  - 语义层：tools/list, tools/call, resources/*, prompts/*
//	好处：一份协议，所有兼容 MCP 的工具（github-mcp、jira-mcp、postgres-mcp）
//	都能被 Agent 即插即用，无需为每种集成写胶水代码。
//
// 【为什么要自己实现而非用官方 SDK】
//
//	官方 TS SDK 阻塞模型不适合 Go 高并发；且我们需要深度控制断线重连、熔断、
//	以及和内部 Skill Registry (internal/skill) 的无缝融合。
//
// 【双向通信模型】
//
//	                                   stdin
//	┌───────────────────┐   request   ──────▶   ┌─────────────────────┐
//	│  MCP Client       │                       │ MCP Server 子进程    │
//	│  (本进程)         │   response            │ (github-mcp 等)      │
//	│                   │   ◀──────  stdout     │                     │
//	│  pending map[id]  │                       │                     │
//	└────────▲──────────┘   notifications       └─────────────────────┘
//	         └────────────  (server-initiated)
//
//	关键点：MCP 是 **双向** 协议——Server 可主动推送 notifications/*。
//	因此我们用独立 goroutine 负责 stdout-reader，按 id 匹配 pending map，
//	无 id 的消息则分发给 notification handler。
//
// 【请求-响应 的并发安全匹配】
//   - reqID 用 atomic.Int64 自增，保证多 goroutine 并发请求时 id 唯一；
//   - pending map[int64]chan *Response，发送前占位，响应到达时 close(ch)；
//   - 这是经典的 "reactor 模式"——单读 goroutine + N 等待 goroutine。
//
// 【握手流程（Initialize）】
//
//	Client → initialize {protocolVersion, capabilities}
//	Server → result    {serverInfo, capabilities}
//	Client → notifications/initialized
//	Client → tools/list
//	Server → result    {tools: [...]}  —— 这些 tools 会注册进 Skill Registry
//	                                     最终以 OpenAI function_call schema
//	                                     形式提交给 LLM。
//
// ============================================================================
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ─── JSON-RPC 2.0 Types ──────────────────────────────────────────────────────

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
//
// 注意：MCP server 也会主动下发 "notifications/*" 消息（无 id），
// 我们用 Method 字段区分：Method != "" && ID == 0 → 通知。
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	// Method / Params 仅对服务端推送的通知有效（例如 notifications/progress）。
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// progressNotification 对应 MCP 规范中的 notifications/progress payload。
//
//	{
//	  "progressToken": "<token>",
//	  "progress":       0.42,
//	  "total":          1.0,
//	  "message":        "... (optional)",
//	  "chunk":          "... (自定义扩展，携带增量 content)"
//	}
//
// 其中 "chunk" 字段不是 MCP 正式规范，但业内 server（github/atlassian 等）
// 广泛约定，用于把 LLM-like streaming 输出通过 progress 管道回传。
type progressNotification struct {
	ProgressToken string  `json:"progressToken"`
	Progress      float64 `json:"progress"`
	Total         float64 `json:"total"`
	Message       string  `json:"message,omitempty"`
	Chunk         string  `json:"chunk,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *JSONRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// ─── MCP Tool Types ──────────────────────────────────────────────────────────

// MCPTool describes a tool exposed by an MCP server.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPToolResult represents the result of a tool call.
type MCPToolResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// MCPContent represents a content item in an MCP response.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ─── Server Connection ───────────────────────────────────────────────────────

// ServerConnection manages the lifecycle of a single MCP server process.
//
// 每个 ServerConnection 代表"我方 Agent ↔ 一个 MCP Server 子进程"的一条信道。
// 字段说明：
//   - name    : 逻辑名（来自 YAML 配置），便于日志定位与 Registry 去重
//   - cmd     : 子进程句柄，Close 时 kill
//   - stdin   : 请求写入端（json 每行一条，遵循 JSON-RPC 2.0 换行分隔协议）
//   - stdout  : 响应读取端；scanner 按行切分
//   - reqID   : 原子自增的请求 ID 生成器（int64 即便每秒百万请求也够用 29 万年）
//   - mu      : 保护 pending map 并发读写
//   - pending : id → chan<-Response，实现"异步请求 → 同步等待"的唤醒机制
//   - progress: progressToken → chan<-*progressNotification（chunked streaming）
//   - inflight: 当前 pending 请求数；连接池做 "least-pending" LB 的信号量
//   - tools   : 握手时从 server 拿到的工具元数据快照
type ServerConnection struct {
	name      string
	transport Transport    // wire-level byte plumbing (stdio / sse / inmem)
	reqID     atomic.Int64 // 请求 ID 原子自增器
	inflight  atomic.Int64 // 当前 pending 请求数（Pool LB）
	// mu 只保护 pending/progress map。**不要**用它保护 transport.Send，
	// 否则"持锁写 IO"会与 readResponses 的锁竞争形成死锁：
	// sender 持 mu 写 → mock 端阻塞 → reader 想取 mu 找 pending → 阻塞。
	// transport.Send 实现自己负责写串行化（写锁内置在 transport 里）。
	mu       sync.Mutex
	pending  map[int64]chan *JSONRPCResponse // id → 等待方
	progress map[string]chan *progressNotification
	tools    []MCPTool // 通过 tools/list 获得的工具列表
	logger   *zap.Logger
	readerWg sync.WaitGroup // 跟踪 readResponses goroutine 生命周期

	// testHook 让 mock server 在进程外用 io.Pipe 注入响应流而无需真正 fork 进程。
	// 生产路径下永远为 false；仅单元测试使用。
	testHook bool
}

// newServerConnection starts an MCP server (stdio subprocess or SSE HTTP
// stream depending on cfg.Transport) and establishes JSON-RPC plumbing.
func newServerConnection(cfg *config.MCPServerConfig, httpClient *http.Client, logger *zap.Logger) (*ServerConnection, error) {
	tr, err := dialTransport(context.Background(), cfg, httpClient, logger.With(zap.String("mcp_server", cfg.Name)))
	if err != nil {
		return nil, err
	}

	conn := &ServerConnection{
		name:      cfg.Name,
		transport: tr,
		pending:   make(map[int64]chan *JSONRPCResponse),
		progress:  make(map[string]chan *progressNotification),
		logger:    logger.With(zap.String("mcp_server", cfg.Name)),
	}

	// Start response reader goroutine
	conn.readerWg.Add(1)
	go conn.readResponses()

	// For stdio: reaper goroutine waits for child exit and flips exited flag.
	// Per os/exec contract, cmd.Wait() must follow all reads from StdoutPipe,
	// so we sequence reaper-after-reader via readerWg.Wait inside startReaper.
	if st, ok := tr.(*stdioTransport); ok {
		st.startReaper(&conn.readerWg)
	}

	return conn, nil
}

// newInMemoryConnection 构造一个**不 fork 子进程**、仅用 io.Pipe 对接的
// ServerConnection。供单元测试向 readResponses 喂模拟 JSON-RPC 帧。
//
//	writerToClient —— 测试侧用它写"server → client"方向的字节流（包括响应和 progress 通知）。
//	readerFromClient —— 测试侧用它读"client → server"方向的请求字节流。
//
// 返回的 ServerConnection 已经启动 readResponses goroutine，直接 sendRequest 即可。
func newInMemoryConnection(name string, writerToClient *io.PipeWriter, readerFromClient *io.PipeReader,
	clientStdin io.WriteCloser, clientStdout io.ReadCloser, logger *zap.Logger) *ServerConnection {
	_ = writerToClient
	_ = readerFromClient
	tr := newInMemoryTransport(name, clientStdin, clientStdout, logger.With(zap.String("mcp_server", name)))
	conn := &ServerConnection{
		name:      name,
		transport: tr,
		pending:   make(map[int64]chan *JSONRPCResponse),
		progress:  make(map[string]chan *progressNotification),
		logger:    logger.With(zap.String("mcp_server", name)),
		testHook:  true,
	}
	conn.readerWg.Add(1)
	go conn.readResponses()
	return conn
}

// readResponses continuously reads JSON-RPC responses from the server's stdout.
//
// 这是典型的 **Reactor 模式**：一个专属 goroutine 独占 stdout 的读取权，
// 无论同时有多少个 sendRequest 在等待结果，都由此 goroutine 负责按 id 路由：
//
//	┌─ goroutine A ─ sendRequest(id=1) ─┐              stdout line
//	│                                    │  ┌──────────────────────┐
//	├─ goroutine B ─ sendRequest(id=2) ─┼─▶│  pending[id] → ch    │
//	│                                    │  └────────┬─────────────┘
//	├─ goroutine C ─ sendRequest(id=3) ─┘           │ close(ch)
//	                                                 ▼
//	                                        唤醒对应 goroutine
//
// 同时处理 3 类入帧：
//  1. 带 id 的 response/error ——> 按 pending[id] 路由
//  2. method=="notifications/progress" 的推送 ——> 按 progressToken 路由（chunked streaming）
//  3. 其他通知（log/messages 等）——> 丢弃，debug 级别日志记录
func (sc *ServerConnection) readResponses() {
	defer sc.readerWg.Done()
	for {
		line, ok := sc.transport.Recv()
		if !ok {
			break
		}
		if len(line) == 0 {
			continue
		}

		// 每行一条 JSON-RPC 消息（Line-delimited JSON，即 NDJSON）。
		var resp JSONRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			sc.logger.Warn("failed to parse MCP response", zap.Error(err))
			continue
		}

		// Case 1: 带 id 的响应 → 路由到 pending 槽
		if resp.ID != 0 && resp.Method == "" {
			sc.mu.Lock()
			ch, ok := sc.pending[resp.ID]
			if ok {
				delete(sc.pending, resp.ID) // 防止 pending 泄漏
			}
			sc.mu.Unlock()

			if ok {
				// channel 长度 1，非阻塞送达。
				ch <- &resp
			}
			continue
		}

		// Case 2: server → client 通知
		if resp.Method == "notifications/progress" {
			var prog progressNotification
			if err := json.Unmarshal(resp.Params, &prog); err != nil {
				sc.logger.Debug("bad progress payload", zap.Error(err))
				continue
			}
			sc.mu.Lock()
			ch, ok := sc.progress[prog.ProgressToken]
			sc.mu.Unlock()
			if !ok {
				// 订阅方已退出或从未订阅；直接丢弃。
				continue
			}
			// 非阻塞投递：如果订阅方慢，为防止 read loop 被顶死，
			// 用 select+default 丢弃最老一帧（log debug 保留痕迹）。
			select {
			case ch <- &prog:
			default:
				sc.logger.Debug("progress channel full, dropping chunk",
					zap.String("token", prog.ProgressToken))
			}
			continue
		}

		// Case 3: 其他通知（忽略）
		sc.logger.Debug("unhandled mcp notification", zap.String("method", resp.Method))
	}

	// Check for transport errors after loop exits
	if err := sc.transport.Err(); err != nil {
		sc.logger.Error("MCP transport read error", zap.Error(err))
		// Notify all pending requests with error
		sc.mu.Lock()
		for id, ch := range sc.pending {
			resp := &JSONRPCResponse{
				ID: id,
				Error: &JSONRPCError{
					Code:    -32603,
					Message: fmt.Sprintf("scanner error: %v", err),
				},
			}
			ch <- resp
			delete(sc.pending, id)
		}
		sc.mu.Unlock()
	}
}

// Inflight 返回此连接当前 pending 的请求数。
// 连接池用它做"最少 pending"的负载均衡决策。
func (sc *ServerConnection) Inflight() int64 { return sc.inflight.Load() }

// Name 返回逻辑 server 名（便于日志）。
func (sc *ServerConnection) Name() string { return sc.name }

// subscribeProgress 注册 progressToken → 通道，调用方必须 defer unsubscribe。
// buf 控制通道缓冲；建议 32，足以吸收短时背压。
func (sc *ServerConnection) subscribeProgress(token string, buf int) <-chan *progressNotification {
	ch := make(chan *progressNotification, buf)
	sc.mu.Lock()
	sc.progress[token] = ch
	sc.mu.Unlock()
	return ch
}

// unsubscribeProgress 解绑并 close channel，确保下游 goroutine 能感知结束。
func (sc *ServerConnection) unsubscribeProgress(token string) {
	sc.mu.Lock()
	ch, ok := sc.progress[token]
	if ok {
		delete(sc.progress, token)
	}
	sc.mu.Unlock()
	if ok {
		close(ch)
	}
}

// sendRequest sends a JSON-RPC request and waits for the response.
//
// 典型的"同步封装异步"模式。调用方看到的是一次阻塞调用，
// 内部实现为：
//
//	(1) reqID.Add(1)                原子自增，拿到唯一 id
//	(2) pending[id] = respCh        占位等待槽（唤醒机制）
//	(3) Fprintf(stdin, "...\n")     写请求到子进程 stdin
//	(4) select <-respCh / <-ctx     阻塞等待 readResponses 唤醒，或上下文取消
//
// 超时/取消时必须清理 pending[id]，否则长期运行会内存泄漏。
// inflight 计数器在进入 / 退出时自增自减，连接池据此决定 LB 选谁。
func (sc *ServerConnection) sendRequest(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
	sc.inflight.Add(1)
	defer sc.inflight.Add(-1)

	// (1) 取唯一请求 ID；atomic 保证即便 1000 goroutine 并发调用也不会冲突。
	id := sc.reqID.Add(1)

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// (2) 预先注册等待通道。注意必须在写 stdin 之前注册，否则响应可能
	//     先于注册到达 → readResponses 找不到槽位丢弃消息 → 永远阻塞。
	respCh := make(chan *JSONRPCResponse, 1)
	sc.mu.Lock()
	sc.pending[id] = respCh
	sc.mu.Unlock()

	// (3) 写传输层；transport.Send 内部已 serialize 写操作。
	err = sc.transport.Send(data)

	if err != nil {
		// 失败时主动清理 pending 防泄漏
		sc.mu.Lock()
		delete(sc.pending, id)
		sc.mu.Unlock()
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// (4) 阻塞等待，两种退出路径：
	//       a) 收到响应 → 检查 Error 字段，返回给上层
	//       b) ctx.Done → 上层超时/取消，必须清理 pending 防泄漏
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	case <-ctx.Done():
		sc.mu.Lock()
		delete(sc.pending, id) // 幂等：即便 readResponses 已 delete 过也无害
		sc.mu.Unlock()
		return nil, ctx.Err()
	}
}

// close shuts down the underlying transport (stdio subprocess / SSE stream /
// in-memory pipe). The reader goroutine returns when the transport EOFs.
func (sc *ServerConnection) close() error {
	err := sc.transport.Close()
	sc.readerWg.Wait()
	return err
}

// ─── Gateway (Registry + Client) ─────────────────────────────────────────────

// Gateway manages connections to multiple MCP servers and provides
// a unified tool registry for the LLM.
//
// Each configured server is backed by a ConnPool (one process per slot, sized
// via MCPServerConfig.PoolSize). When PoolSize <= 1 the pool degenerates to
// a single connection — same behavior as before this refactor, no config
// migration required. Setting PoolSize > 1 unlocks parallel tools/call
// distribution across forked subprocesses, useful for chatty servers like
// filesystem-mcp under bursty ReAct loads.
type Gateway struct {
	servers       map[string]*ConnPool
	serverConfigs map[string]*config.MCPServerConfig // (F8) stored for reconnection
	toolIndex     map[string]string                  // toolName -> serverName for O(1) lookup
	httpClient    *http.Client
	healthCheck   *healthChecker
	mu            sync.RWMutex
	logger        *zap.Logger
}

// StartHealthCheck launches the periodic health checker. Idempotent: calling
// it twice replaces the previous checker. interval <= 0 disables the checker
// (a no-op).
func (gw *Gateway) StartHealthCheck(interval time.Duration) {
	if interval <= 0 {
		return
	}
	gw.mu.Lock()
	if gw.healthCheck != nil {
		gw.healthCheck.Stop()
	}
	hc := newHealthChecker(gw, gw.logger)
	gw.healthCheck = hc
	gw.mu.Unlock()
	hc.Start(interval)
}

// NewGateway creates a new MCP gateway and connects to configured servers.
func NewGateway(cfg *config.MCPConfig, httpClient *http.Client, logger *zap.Logger) (*Gateway, error) {
	gw := &Gateway{
		servers:       make(map[string]*ConnPool),
		serverConfigs: make(map[string]*config.MCPServerConfig),
		toolIndex:     make(map[string]string),
		httpClient:    httpClient,
		logger:        logger.With(zap.String("component", "mcp")),
	}

	for i := range cfg.Servers {
		serverCfg := &cfg.Servers[i]
		// dialTransport 内部按 cfg.Transport 分派 stdio / sse；
		// 未识别的传输由 dialTransport 报错，这里不必再做白名单。

		pool := NewConnPool(serverCfg, gw.httpClient, gw.logger)
		if err := pool.Start(context.Background(), gw.initializeServer); err != nil {
			gw.logger.Error("failed to start MCP server pool",
				zap.String("server", serverCfg.Name),
				zap.Error(err),
			)
			continue
		}

		gw.servers[serverCfg.Name] = pool
		gw.serverConfigs[serverCfg.Name] = serverCfg
		for _, t := range pool.Tools() {
			gw.toolIndex[t.Name] = serverCfg.Name
		}
		gw.logger.Info("MCP server pool started",
			zap.String("server", serverCfg.Name),
			zap.Int("pool_size", pool.Size()),
			zap.Int("alive", pool.Alive()),
			zap.Int("tools", len(pool.Tools())),
		)
	}

	return gw, nil
}

// newSingletonPool wraps an already-constructed ServerConnection in a 1-slot
// ConnPool. Test-only convenience used by tests that hand-construct the
// gateway with mock connections. The handshake step is bypassed (conn.tools
// is taken as-is from the caller).
func newSingletonPool(cfg *config.MCPServerConfig, conn *ServerConnection, logger *zap.Logger) *ConnPool {
	if cfg == nil {
		cfg = &config.MCPServerConfig{Name: "test", PoolSize: 1}
	}
	p := NewConnPool(cfg, nil, logger)
	p.conns[0].Store(conn)
	p.tools = conn.tools
	return p
}

// initializeServer performs the MCP initialization handshake and tool discovery.
func (gw *Gateway) initializeServer(ctx context.Context, conn *ServerConnection) error {
	// Send initialize request
	_, err := conn.sendRequest(ctx, "initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]interface{}{
			"name":    "code-agent",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	// Send initialized notification (no response expected, but we send it as a request for simplicity)
	_, _ = conn.sendRequest(ctx, "notifications/initialized", nil)

	// Discover available tools
	resp, err := conn.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return fmt.Errorf("tool discovery failed: %w", err)
	}

	var toolsResult struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &toolsResult); err != nil {
		return fmt.Errorf("failed to parse tools: %w", err)
	}

	conn.tools = toolsResult.Tools
	return nil
}

// GetAvailableTools returns all tools from all connected MCP servers
// in a format suitable for LLM function calling.
func (gw *Gateway) GetAvailableTools() []models.ToolDefinition {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	var tools []models.ToolDefinition
	for serverName, pool := range gw.servers {
		for _, t := range pool.Tools() {
			tools = append(tools, models.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
				Source:      fmt.Sprintf("mcp:%s", serverName),
			})
		}
	}
	return tools
}

// CallTool invokes a tool on the appropriate MCP server. Internally delegates
// to ConnPool.CallTool which picks the least-loaded subprocess via
// least-pending + atomic CAS — see pool.go for the load balancing strategy.
func (gw *Gateway) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (*models.ToolResult, error) {
	gw.mu.RLock()
	pool, ok := gw.servers[serverName]
	gw.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("MCP server not found: %s", serverName)
	}

	// Pre-validate JSON args here so the caller gets a structured error before
	// the pool dispatch (matches the historical Gateway.CallTool contract).
	if len(args) > 0 {
		var probe interface{}
		if err := json.Unmarshal(args, &probe); err != nil {
			return nil, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	}
	return pool.CallTool(ctx, toolName, args)
}

// FindServerForTool locates which MCP server provides the given tool.
// Uses pre-built index for O(1) lookup instead of linear scan.
func (gw *Gateway) FindServerForTool(toolName string) (string, bool) {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	serverName, ok := gw.toolIndex[toolName]
	return serverName, ok
}

// ─── Dynamic Server Management ───────────────────────────────────────────────

// ServerStatus describes the runtime state of a connected MCP server.
type ServerStatus struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"` // "connected" | "error"
	ToolsCount int      `json:"tools_count"`
	Tools      []string `json:"tools"`
}

// AddServer connects a new MCP server at runtime. The server's tools become
// immediately available to the LLM on the next ReAct step.
//
// Validation is transport-aware: stdio configs go through the command/args
// whitelist (defense against shell injection via runtime-provided args);
// SSE configs require a URL but skip the command checks — host-level
// safety is the egress ACL's job at dial time.
func (gw *Gateway) AddServer(cfg *config.MCPServerConfig) (*ServerStatus, error) {
	switch cfg.Transport {
	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("invalid MCP server: sse transport requires a URL")
		}
	case "", "stdio":
		if err := ValidateCommand(cfg.Command); err != nil {
			return nil, fmt.Errorf("invalid MCP server command: %w", err)
		}
		if err := ValidateArgs(cfg.Args); err != nil {
			return nil, fmt.Errorf("invalid MCP server args: %w", err)
		}
	default:
		return nil, fmt.Errorf("invalid MCP server: unsupported transport %q", cfg.Transport)
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()

	// Check for duplicate
	if _, exists := gw.servers[cfg.Name]; exists {
		return nil, fmt.Errorf("MCP server '%s' already exists", cfg.Name)
	}

	pool := NewConnPool(cfg, gw.httpClient, gw.logger)
	if err := pool.Start(context.Background(), gw.initializeServer); err != nil {
		return nil, fmt.Errorf("failed to start MCP server %s: %w", cfg.Name, err)
	}

	gw.servers[cfg.Name] = pool
	gw.serverConfigs[cfg.Name] = cfg
	for _, t := range pool.Tools() {
		gw.toolIndex[t.Name] = cfg.Name
	}

	toolNames := make([]string, len(pool.Tools()))
	for i, t := range pool.Tools() {
		toolNames[i] = t.Name
	}

	gw.logger.Info("MCP server pool added at runtime",
		zap.String("server", cfg.Name),
		zap.Int("pool_size", pool.Size()),
		zap.Int("tools", len(pool.Tools())))

	return &ServerStatus{
		Name:       cfg.Name,
		Status:     "connected",
		ToolsCount: len(pool.Tools()),
		Tools:      toolNames,
	}, nil
}

// RemoveServer disconnects and removes an MCP server at runtime.
func (gw *Gateway) RemoveServer(name string) error {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	pool, ok := gw.servers[name]
	if !ok {
		return fmt.Errorf("MCP server '%s' not found", name)
	}

	if err := pool.Close(); err != nil {
		gw.logger.Warn("error closing MCP server pool", zap.String("server", name), zap.Error(err))
	}

	// Remove tool index entries for this server
	for _, t := range pool.Tools() {
		delete(gw.toolIndex, t.Name)
	}

	delete(gw.servers, name)
	delete(gw.serverConfigs, name)

	gw.logger.Info("MCP server pool removed", zap.String("server", name))
	return nil
}

// ListServers returns the status of all connected MCP servers.
func (gw *Gateway) ListServers() []ServerStatus {
	gw.mu.RLock()
	defer gw.mu.RUnlock()

	var result []ServerStatus
	for name, pool := range gw.servers {
		tools := pool.Tools()
		toolNames := make([]string, len(tools))
		for i, t := range tools {
			toolNames[i] = t.Name
		}
		result = append(result, ServerStatus{
			Name:       name,
			Status:     "connected",
			ToolsCount: len(tools),
			Tools:      toolNames,
		})
	}
	return result
}

// Close shuts down all MCP server connections.
func (gw *Gateway) Close() error {
	gw.mu.Lock()
	hc := gw.healthCheck
	gw.healthCheck = nil
	gw.mu.Unlock()
	if hc != nil {
		hc.Stop()
	}

	gw.mu.Lock()
	defer gw.mu.Unlock()
	for name, pool := range gw.servers {
		if err := pool.Close(); err != nil {
			gw.logger.Warn("failed to close MCP server pool", zap.String("server", name), zap.Error(err))
		}
	}
	gw.servers = make(map[string]*ConnPool)
	return nil
}
