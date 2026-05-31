# 06 · MCP 网关 `internal/mcp`

> 代码：
> - `client.go` (781) — JSON-RPC 2.0 协议 + `ServerConnection` 进程管理 + `Gateway` 多服务聚合 + 进度通知订阅
> - `validation.go` (71) — MCP 子进程命令白名单（`AllowedMCPCommands` + `ValidateArgs`）
> - `pool.go` (462) — **未接线**：多子进程连接池 + least-pending LB + chunked streaming
> - `reconnect.go` (208) — **未接线**：健康检查 + 自动重连 + 指数退避
> - `doc.go` (66) — 包文档
>
> 测试：`client_test.go` (738) / `pool_test.go` (665)

---

## 1. 模块定位

**"把 Anthropic 的 MCP 协议跑成 stdio 子进程 + JSON-RPC 客户端，让任何外部工具（github、filesystem、jira、自研服务）通过一条 stdio 管道接入 ReAct 循环。"**

[Model Context Protocol](https://modelcontextprotocol.io) 是 Anthropic 推出的 LLM ↔ 工具互操作协议——本质是 **JSON-RPC 2.0 over stdio（或 SSE）**，以 `tools/list` 暴露能力、`tools/call` 触发执行、`notifications/*` 推送事件。

本包做四件事：

1. **拉起 MCP Server 子进程**：`exec.Command(npx ...)` + `StdinPipe` / `StdoutPipe`；
2. **实现 JSON-RPC 2.0 客户端**：单 reader goroutine + pending map + 原子 ID + 进度通知订阅；
3. **聚合多 server**：`Gateway` 管 `map[name]*ServerConnection`，预建 `toolIndex` 做 O(1) 工具路由；
4. **运行时 CRUD**：`AddServer` / `RemoveServer` / `ListServers`——零重启增删 MCP server。

orchestrator 那一层把 MCP 工具与内置工具、Skill 三种来源**同构化**为 `models.ToolDefinition` / `models.ToolResult`，LLM 完全感知不到差异。

### 1.1 当前接线状态（**重要**）

| 模块                    | 文件                | 是否接线          | 说明 |
|-------------------------|---------------------|-------------------|------|
| 单连接 `ServerConnection` | `client.go`         | ✅ 已接线          | 由 `ConnPool` 持有（不再裸暴露给 Gateway） |
| `Gateway` (路由 + CRUD)  | `client.go`         | ✅ 已接线          | `apiServer.SetMCPGateway` 暴露给 REST；`servers map[string]*ConnPool` |
| `validation.go` 白名单   | `validation.go`     | ✅ 已接线          | `Gateway.AddServer` 调 `ValidateCommand` + `ValidateArgs` |
| 进度通知订阅 (chunked)   | `client.go` `subscribeProgress` | ⚠️ 只在 `pool.CallToolStream` 用 | Gateway.CallTool 走非流式路径 |
| `ConnPool`（多进程池）    | `pool.go`           | ✅ **已接线**（2026-06）| `Gateway.servers map[string]*ConnPool`；`PoolSize<=1` 退化为单连接 |
| `healthChecker`（自愈）  | `reconnect.go`      | ✅ **已接线**（2026-06）| `cmd/agent/main.go` MCP 初始化尾调 `mcpGateway.StartHealthCheck(30*time.Second)`；30s tick → `processAlive` → 0 存活则触发整池重连（指数退避，最多 5 次） |
| SSE / HTTP transport    | `transport_sse.go`  | ✅ **已接线**（2026-06）| `dialTransport(cfg.Transport=="sse")` → `newSSETransport`：GET 拉流 + `event: endpoint` 取 POST URL + JSON-RPC 响应走 SSE 推回；`Alive()` 用 90s 无流量阈值触发 reconnect |

下文按"已接线的核心通路 + 关键失败模式"展开，所有未实现项都明确标 §12 后续演进。

---

## 1.5 设计哲学：5 个核心抉择

### Q1 — 为什么**自研**而不用官方 SDK？

`modelcontextprotocol/go-sdk` 在我们启动该模块时尚未稳定（pre-1.0 多次破坏式变更），且专注"协议正确"而非"生产可运维"——没有重连、没有连接池、没有进度订阅。

**协议层 JSON-RPC 2.0 本身仅 800 行可控**：自研换来熔断点、池化、进度订阅这些生产能力的接入自由。代价是要跟 spec 演进（如 `2024-11-05` → `2025-03-26`），目前固定 `2024-11-05`（`client.go:553`）。

### Q2 — stdio vs SSE：两种 transport 如何分派？

MCP spec 定义两种 transport：stdio（本地子进程，UNIX pipe）和 SSE（远程服务，HTTP）。**两种均已实现**：`dialTransport` (`transport.go`) 根据 `cfg.Transport` 直接分派——`stdio`/空 → `newStdioTransport`（fork 子进程 + StdinPipe/StdoutPipe），`sse` → `newSSETransport`（GET 拉流 + endpoint 事件取 POST URL）。`Transport` 接口仅 5 个方法（`Send`/`Recv`/`Err`/`Alive`/`Close`），`ServerConnection` 对两侧完全透明。

| 维度       | stdio                              | SSE                              |
|------------|------------------------------------|----------------------------------|
| 部署       | 本地子进程（`npx`/`uvx`/`python`） | 独立 HTTP 服务                    |
| 延迟       | μs 级（pipe）                       | ms 级（网络）                      |
| 隔离       | 进程级（崩溃不影响 agent）          | 网络级（HTTP client 强制 egress ACL）|
| 调试       | `stdin`/`stdout` 可 tail            | 需要 HTTP debug 工具               |
| 适配场景   | 本地工具（filesystem-mcp、github） | 远程托管 MCP（GitHub hosted、内网 MCP）|
| 健康检查   | `Signal(0)` + reaper 标志           | `lastRecv` 90s keepalive 超时       |
| 连接池     | `pool_size>1` 多子进程并行          | 强制 `pool_size=1`（HTTP 复用即可）|

SSE 实现要点：① 协议第一帧必须是 `event: endpoint`，data 携带 POST URL（`Send` 阻塞等 `postURLReady`）；② JSON-RPC 响应**不**在 POST 的 HTTP body 里——服务端从 SSE 推回；③ HTTP client 由 `Gateway` 注入（继承 egress ACL）；④ 90s 无事件即认为流死，`healthChecker` 触发整池 reconnect。

### Q3 — 为什么独立 `writeMu` 而非和 `mu` 共用？

stdin 是字节流——并发写入必然会把两条 JSON 行交错（"行分帧"立刻失效）。最朴素的方案是用单把 `mu` 同时保护 `pending` map 和 stdin 写入；但这会让 `sendRequest` 在拿锁状态下做 IO（stdin 写 + 行编码），把"等响应"的多路复用直接锁死。

实现选择**两把锁**（`client.go:172-180`）：
- `mu` 仅保护 `pending` / `progressSubs` map 的增删；
- `writeMu` 仅串行化 stdin 写入。

`sendRequest` 的执行顺序刻意是：① `mu.Lock` 注册 pending → unlock → ② `writeMu.Lock` 写 stdin → unlock → ③ select 等 `respCh` 或 `ctx.Done()`。**pending 必须 BEFORE 写 stdin**——若反过来，服务端可能在你写完 stdin 还没注册 pending 的瞬间已经响应，reader 找不到 chan 直接丢帧（永久卡死调用者）。

### Q4 — Reactor 模式而非每请求一个 reader goroutine？

每个 `ServerConnection` 只有**一个** `readResponses` goroutine 独占 stdout（`client.go:287-365`）。三种消息类的分发：

1. **响应**（有 id）→ 查 `pending[id]` → push 到 `chan` → 删 pending；
2. **进度通知**（`method=notifications/progress`）→ 查 `progressSubs[token]` → push（满则丢弃）；
3. **其他通知** → log + 丢弃。

为何不"每个请求起一个 goroutine 读 stdout"？因为 stdout 是**字节流**——只有单消费者能正确按行扫描+demux。多消费者会把同一行读两次或互相吞字节。

### Q5 — 命名空间冲突：MCP 工具名相同时谁赢？

两个 server 都暴露 `read_file` 怎么办？

当前实现（`toolIndex` 是 `map[string]string`，`client.go:538`）**后写覆盖前者**——没有命名空间前缀机制。这是已知的缺陷，详见 §11 设计权衡 + §12 演进。`tools.Registry`（[07_tools.md](07_tools.md)）也是同样的限制。

生产上的临时应对：在配置层让用户保证唯一性（filesystem-mcp 用 `read_file`、自研工具叫 `app_read_file`）。

---

## 2. 依赖架构

```
              ┌─── api/handlers/mcp_handlers.go (REST) ─────────┐
              │  POST /api/v1/mcp/servers  → AddServer          │
              │  DELETE /api/v1/mcp/servers/:name → RemoveServer│
              │  GET /api/v1/mcp/servers   → ListServers        │
              └─────────────────┬───────────────────────────────┘
                                │
                                ▼
                  ┌───────────────────────────────┐
                  │   mcp.Gateway                  │
                  │     .servers      map[name]*SC │
                  │     .serverConfigs            │ ← F8：存配置供 reconnect
                  │     .toolIndex    map[tool]nm │ ← O(1) 工具→server 路由
                  └────────────┬──────────────────┘
                               │ 1:N
                               ▼
                  ┌───────────────────────────────┐
                  │   ServerConnection             │
                  │     cmd *exec.Cmd              │
                  │     stdin / stdout pipes       │
                  │     pending  map[id]chan       │ ← 多路复用
                  │     progressSubs map[tok]chan  │ ← 流式订阅
                  │     mu / writeMu               │ ← 两把锁
                  │     inflight atomic.Int64     │ ← 池化 LB 用
                  └────────────┬──────────────────┘
                               │ stdio pipe
                               ▼
                  ┌───────────────────────────────┐
                  │   MCP Server 子进程            │
                  │   npx @modelcontextprotocol/   │
                  │       server-filesystem        │
                  │   uvx ...                      │
                  └───────────────────────────────┘

已接线（2026-06）：
  ┌─ ConnPool (pool.go) ─┐    ┌─ healthChecker (reconnect.go) ─┐
  │ N 个 ServerConnection │    │ processAlive: Signal(0) 每 slot │
  │ least-pending Pick   │    │ 死了 → CAS 清 slot；整池零活 → │
  │ chunked streaming    │    │  reconnectServer (新建 pool)   │
  │ 接入 Gateway.servers │    │ Start() 仍待 main.go 接入       │
  └──────────────────────┘    └────────────────────────────────┘
```

---

## 2.5 数据流总览

### 流 A：启动初始化（per server）

```text
config.MCP.Servers[i] (Transport=="stdio")
   │
   ▼
newServerConnection(cfg, logger):
   ① ValidateCommand(cfg.Command)  → 必须 ∈ {npx,node,python,python3,uvx,uv,deno,bun,docker}
   ② ValidateArgs(cfg.Args)        → 拒绝 --eval / -e / -c / eval / exec
   ③ exec.Command(cfg.Command, cfg.Args...).Env = merge(os.Environ, cfg.Env)
   ④ StdinPipe + StdoutPipe + Stderr → 父进程 Stderr 透传
   ⑤ cmd.Start()                   → 子进程跑起来
   ⑥ go conn.readResponses()       → Reactor 启动
   │
   ▼
initializeServer(ctx, conn):                                  (JSON-RPC over stdin)
   ① sendRequest("initialize", {protocolVersion:"2024-11-05", capabilities, clientInfo})
   ② sendRequest("notifications/initialized", nil)  # 协议要求的握手第二步
   ③ sendRequest("tools/list", nil) → conn.tools = [MCPTool...]
   │
   ▼
gw.servers[name]      = conn
gw.serverConfigs[name] = cfg               ← (F8) 给 reconnect 准备
for t in conn.tools:
    gw.toolIndex[t.Name] = name             ← O(1) 路由表

任何一步失败 → 跳过本 server，继续启动其他（Error 日志，不阻塞 Gateway 创建）。
```

### 流 B：运行时 CallTool（核心 RPC）

```text
orchestrator.executeMCPTool(serverName, toolName, args)
   │
   ▼
Gateway.CallTool(ctx, serverName, toolName, args):
   ① RLock servers → 取 conn  (锁出来再释放)
   ② json.Unmarshal(args, &argsMap)
   │
   ▼
conn.sendRequest(ctx, "tools/call", {name, arguments:argsMap}):
   ① id := reqID.Add(1)                              (atomic)
   ② respCh := make(chan *JSONRPCResponse, 1)
   ③ mu.Lock; pending[id] = respCh; mu.Unlock         ← BEFORE 写 stdin
   ④ writeMu.Lock; stdin.Write(json.Marshal(req)+"\n"); writeMu.Unlock
   ⑤ select {
        case resp := <-respCh: return resp
        case <-ctx.Done():
            mu.Lock; delete(pending, id); mu.Unlock   ← 防泄漏
            return ctx.Err()
      }

(异步) conn.readResponses():
   for scanner.Scan():
      parse(line) → JSONRPCResponse {id, result, error} OR notification {method, params}
      if id != 0:                                      ← 是响应
          mu.Lock
          ch := pending[id]; delete(pending, id)
          mu.Unlock
          ch <- resp                                   ← 非阻塞投递
      elif method == "notifications/progress":
          mu.Lock; subCh := progressSubs[token]; mu.Unlock
          if subCh: select { subCh <- prog; default }  ← 满则丢
      else:
          logger.Debug("unhandled notification")
   ← scanner 结束 (子进程退出)
   mu.Lock
   for id, ch in pending: ch <- {Error: {Code:-32603, Message:"connection lost"}}
   mu.Unlock
   │
   ▼
回到 Gateway.CallTool:
   ① json.Unmarshal(resp.Result, &mcpToolResult)
   ② strings.Builder 拼接 content[*].text       ← [OPT-22] 避免 O(n²)
   ③ return &ToolResult{Content, IsError}
```

### 流 C：运行时 CRUD（REST API → Gateway）

```text
POST /api/v1/mcp/servers {name, transport, command?, args?, env?, url?}
   │
   ▼
Gateway.AddServer(cfg):
   ① switch cfg.Transport:
        case "sse":         require cfg.URL != ""
        case "", "stdio":   ValidateCommand(cfg.Command) + ValidateArgs(cfg.Args)
        default:            return "unsupported transport"
   ② mu.Lock
   ③ if servers[cfg.Name] exist  → return err "already exists"
   ④ dialTransport(cfg, httpClient) → stdio fork / sse open stream
   ⑤ newServerConnection(transport) + initializeServer(ctx, conn)
       (失败立刻 conn.close + return err)
   ⑥ servers[name]=conn; serverConfigs[name]=cfg
   ⑦ for t in conn.tools: toolIndex[t.Name]=name        ← 即时生效
   ⑧ mu.Unlock
   ⑨ return ServerStatus{Name, Status:"connected", ToolsCount, Tools[]}
   ← orchestrator 下一轮 ReAct 调 GetAvailableTools() 就能看见新工具


DELETE /api/v1/mcp/servers/:name
   │
   ▼
Gateway.RemoveServer(name):
   ① mu.Lock
   ② conn.close()  →  cmd.Process.Kill + close(stdin)
                      readResponses 自然退出，pending 全部以 -32603 失败
   ③ delete(toolIndex[*]) where value==name
   ④ delete(servers[name]); delete(serverConfigs[name])
   ⑤ mu.Unlock
```

---

## 3. 核心数据结构

### 3.1 JSON-RPC 2.0 三件套（`client.go:48-94`）

```go
type JSONRPCRequest struct {
    JSONRPC string      // 固定 "2.0"
    ID      int64       // 原子自增；0 时序列化为 null（用于 notification）
    Method  string
    Params  interface{}
}

type JSONRPCResponse struct {
    JSONRPC string
    ID      int64
    Result  json.RawMessage
    Error   *JSONRPCError
    Method  string         // 服务端发起的 notification 用
    Params  json.RawMessage
}

type JSONRPCError struct{ Code int; Message string; Data interface{} }
```

**Response 复用作 Notification 容器**：MCP server 主动推 `notifications/progress` 时，stdout 上写的也是 JSON-RPC 形态（`method` + `params` 非空、`id` 为空）。把 Notification 嵌进 Response struct 让 reader 单次 `json.Unmarshal` 就能区分两类。

### 3.2 MCP 协议层（`client.go:120-160`）

```go
type MCPTool struct {
    Name        string
    Description string
    InputSchema json.RawMessage  // 透传 JSON Schema
}

type MCPToolResult struct {
    Content []MCPContent  // 一次调用可返回多段 content
    IsError bool
}

type MCPContent struct {
    Type string  // "text" / "image" / "resource"
    Text string
    // image / resource 字段省略
}

type progressNotification struct {
    Token    string  `json:"progressToken"`
    Progress float64 `json:"progress"`      // 0~1
    Total    float64 `json:"total"`
    Message  string  `json:"message"`
    Chunk    string  `json:"chunk"`         // ← MCP 非标准扩展，github/atlassian MCP 用它做流式 token
}
```

`progressNotification.Chunk` 不在 MCP spec 中，是 github-mcp 等实际服务**事实标准**：tools/call 长任务每收到 LLM 一个 chunk 就推一帧，客户端订阅了即可拼成流式输出。

### 3.3 `ServerConnection` — 单子进程会话（`client.go:165-189`）

```go
type ServerConnection struct {
    name    string
    cmd     *exec.Cmd
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    scanner *bufio.Scanner          // bufio 默认 64KB，已 Buffer 扩到 4MB

    reqID   atomic.Int64            // 请求 ID 自增
    mu      sync.Mutex              // 仅保护 pending / progressSubs map
    pending map[int64]chan *JSONRPCResponse
    progressSubs map[string]chan progressNotification

    writeMu sync.Mutex              // 独立：仅串行化 stdin 写
    inflight atomic.Int64           // 当前未完成请求数（pool LB 用）

    tools   []MCPTool
    testHook bool                   // 单测注入 io.Pipe 时绕过 exec.Cmd 健康检查
    logger  *zap.Logger
}
```

三个关键设计：

| 设计                         | 作用                                                      |
|------------------------------|-----------------------------------------------------------|
| **两把锁** (`mu` + `writeMu`) | 注册 pending（map）和写 stdin（IO）解耦，互不阻塞         |
| **独立 `readResponses` goroutine** | 单消费者保证按行 demux 正确性                       |
| **`inflight atomic.Int64`**  | 不需要加锁即可让池/监控读到当前负载，O(1) 拿 LB 指标       |

### 3.4 `Gateway` — 多服务聚合（`client.go:487-494`）

```go
type Gateway struct {
    servers       map[string]*ConnPool                // 每 server 一个池（2026-06）
    serverConfigs map[string]*config.MCPServerConfig  // (F8) 保留配置供 reconnect
    toolIndex     map[string]string                   // tool → server，O(1)
    httpClient    *http.Client                        // 暂未使用（SSE 用）
    mu            sync.RWMutex
    logger        *zap.Logger
}
```

**`toolIndex` 是预建索引**而非每次扫所有 server——`FindServerForTool` 是 O(1) 查表（`client.go:654-660`）。`AddServer` / `RemoveServer` 在写入 `servers` 的同时维护索引一致性。

**2026-06 重构**：`servers` 的 value 由 `*ServerConnection` 升级为 `*ConnPool`。`NewGateway` 对每个 stdio server 起一个池（`PoolSize` 来自 `MCPServerConfig`；缺省/`<=1` 等价单连接，向后兼容），`Gateway.CallTool` 委托 `pool.CallTool`（内部走 least-pending）。测试场景可用 `newSingletonPool` 把 mock 的 `*ServerConnection` 包成 1-slot pool。

---

## 4. Reactor：`readResponses` 拆消息（`client.go:287-365`）

整段逻辑只做三件事：读一行 → 解析 → 按消息类分发。代码大致：

```go
for scanner.Scan() {
    var resp JSONRPCResponse
    if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
        logger.Warn("invalid JSON-RPC frame", zap.Error(err))
        continue
    }

    // ① 响应（id 非零，且本端注册过 pending）
    if resp.ID != 0 {
        mu.Lock()
        ch, ok := pending[resp.ID]
        delete(pending, resp.ID)
        mu.Unlock()
        if ok {
            ch <- &resp        // chan 缓冲为 1，必然非阻塞
        }
        continue
    }

    // ② 进度通知
    if resp.Method == "notifications/progress" {
        var prog progressNotification
        if err := json.Unmarshal(resp.Params, &prog); err == nil {
            mu.Lock()
            sub := progressSubs[prog.Token]
            mu.Unlock()
            if sub != nil {
                select {
                case sub <- prog:    // 满则丢
                default:
                }
            }
        }
        continue
    }

    // ③ 其他通知（log + 丢）
    logger.Debug("unhandled notification", zap.String("method", resp.Method))
}

// scanner 结束：子进程退出 / pipe 关闭
mu.Lock()
for id, ch := range pending {
    ch <- &JSONRPCResponse{ID: id, Error: &JSONRPCError{
        Code: -32603, Message: "connection lost",
    }}
}
mu.Unlock()
```

**关键不变量**：

1. **退出时 broadcast 错误**——所有等响应的调用者立刻拿到 `-32603 connection lost`，不会卡 `<-respCh`；
2. **进度 chan 满则丢**——`progressSubs` 的 chan buffer=32，UI 跟不上时丢老帧而不阻塞 server；
3. **invalid frame 跳过不退出**——单条坏 JSON 不让整个 reader 死掉。

---

## 5. `sendRequest` —— 单次 RPC 的并发安全实现（`client.go:409-463`）

```go
func (c *ServerConnection) sendRequest(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
    id := c.reqID.Add(1)
    req := JSONRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}

    respCh := make(chan *JSONRPCResponse, 1)

    // ① pending 必须 BEFORE 写 stdin，否则可能响应早到丢帧
    c.mu.Lock()
    c.pending[id] = respCh
    c.mu.Unlock()

    data, err := json.Marshal(req)
    if err != nil { /* delete pending + return */ }

    // ② 独立 writeMu 串行化 stdin 写
    c.writeMu.Lock()
    _, werr := fmt.Fprintf(c.stdin, "%s\n", data)
    c.writeMu.Unlock()
    if werr != nil { /* delete pending + return */ }

    c.inflight.Add(1)
    defer c.inflight.Add(-1)

    select {
    case resp := <-respCh:
        if resp.Error != nil { return nil, resp.Error }
        return resp, nil
    case <-ctx.Done():
        c.mu.Lock(); delete(c.pending, id); c.mu.Unlock()
        return nil, ctx.Err()
    }
}
```

**为什么"pending 注册 BEFORE 写 stdin"是硬约束**：假如颠倒——先写 stdin 再注册 pending——下面的竞态会偶发出现：

```
[caller] writeMu.Lock; stdin.Write(req); writeMu.Unlock
                                      ↓ 微妙后
                              [server] 处理完，写 resp 到 stdout
                                      ↓ 同时
                              [reader] scanner.Scan() 拿到 resp
                                      ↓
                              [reader] mu.Lock; pending[id]?  ← 还没注册！
                                      ↓
                              [reader] 丢帧
[caller] mu.Lock; pending[id] = respCh; mu.Unlock
[caller] <-respCh   ← 永久卡死
```

**ctx 取消的内存安全**：`select` 走 ctx.Done 分支时立刻 `delete(pending, id)`——否则迟到的响应找不到 chan，但 map 项还在，每超时一次泄漏一个 chan。

---

## 6. `validation.go` —— MCP 子进程命令白名单（`validation.go:1-71`）

`AddServer` 的入口校验：

```go
var AllowedMCPCommands = map[string]bool{
    "npx": true, "node": true, "python": true, "python3": true,
    "uvx": true, "uv": true, "deno": true, "bun": true, "docker": true,
}

var allowedCommandDirs = []string{
    "/usr/bin/", "/usr/local/bin/", "/opt/homebrew/bin/",
}

var dangerousArgs = []string{"--eval", "-e", "-c", "eval", "exec"}
```

`ValidateCommand` 三道闸：

1. **非空** + **拒绝相对路径含分隔符**（`./foo` `../bin/evil`）；
2. `filepath.Clean` 去掉 `..` 后取 basename，必须 ∈ `AllowedMCPCommands`；
3. 若是绝对路径，必须以 `/usr/bin/` / `/usr/local/bin/` / `/opt/homebrew/bin/` 之一开头。

`ValidateArgs` 拒绝 `--eval` / `-e` / `-c` / `eval` / `exec`（包括 `--eval=...` 这种形态）——防止 LLM/REST 调用方提交一个合法解释器（如 `python`）但用 `-c "rm -rf /"` 直接当 shell 执行。

**这个白名单的局限**：

- 不能阻止 `npx <恶意 npm 包名>`——npx 会拉网上的包跑（这是 MCP 生态本身的信任假设，需在 Docker 沙箱里跑 MCP server 才能真正隔离；当前默认信任已发布的 MCP server）。
- 没限制环境变量——`cfg.Env` 可以注入 `LD_PRELOAD`。

这些是已识别的加固空间，详见 §12。

---

## 7. `ConnPool` —— 已接线的多子进程池（`pool.go`）

> ✅ **2026-06 更新**：`Gateway.servers` 已改为 `map[string]*ConnPool`。`NewGateway` 对每个 stdio server 调 `NewConnPool` + `pool.Start(ctx, gw.initializeServer)`；`Gateway.CallTool` 内部走 `pool.CallTool`。`PoolSize` 字段由 `MCPServerConfig.PoolSize` (YAML `pool_size`) 提供，`<=1` 等价单连接，向后兼容。

### 7.1 为什么要池化

单 stdio server 子进程是**事实串行**的：

- stdin 写必须串行（行分帧约束）；
- 多数 MCP server（Node.js / Python）是单事件循环，CPU 绑核后队列拉平；
- 慢工具（`github.search_code` p99 ≈ 800ms）单进程下顶不住并发 ReAct 步内的多 tool_call。

实测：2~3 秒内并发 20 条 `read_file` 给 filesystem-mcp，单进程 p99 ≈ 1200ms，4 进程池 p99 ≈ 340ms（pool.go 文件头注释）。

### 7.2 关键结构（`pool.go:90-114`）

```go
type ConnPool struct {
    name     string
    cfg      *config.MCPServerConfig
    size     int                    // 目标进程数
    minAlive int                    // 启动时最少存活；默认 max(1, size/2)

    conns []atomic.Pointer[ServerConnection]  // slot 数组，原子可换
    progressCounter atomic.Uint64             // 生成 pool 级 progressToken

    toolsOnce sync.Once
    tools     []MCPTool

    dialer func(cfg, logger) (*ServerConnection, error)  // 测试注入用
}
```

**为何 `atomic.Pointer` 而非 mutex**：`Pick()` 是**热路径**——单次 RPC 都要走一次。互斥锁 Lock+Unlock 在高并发下成本可观；`atomic.Pointer.Load` 是 lock-free。

### 7.3 `Pick()` —— Least-Pending 负载均衡（`pool.go:199-220`）

```go
for i := range p.conns {
    c := p.conns[i].Load()
    if c == nil { continue }
    load := c.inflight.Load()
    if load < bestLoad {
        best = c
        bestLoad = load
        if load == 0 { break }   // 0 pending → 直接返回，免遍历
    }
}
return best
```

为何不 round-robin：**长短请求混合下 RR 不公平**。RR 假设每个请求成本相近，但 MCP tool 调用从 5ms（filesystem read）到 800ms（github search）跨度极大；某 slot 排了 5 个慢请求时 RR 仍会给它派新活，把 p99 拉高一倍。

**Least-pending 用 `inflight atomic.Int64`**——`sendRequest` 入口 `inflight.Add(1)` 出口 `Add(-1)`，`Pick` 读这个值。O(N) 扫描 N≤8 远比加锁便宜。

### 7.4 Chunked Streaming（`pool.go:292-400`）

`CallToolStream(ctx, toolName, args) → <-chan ToolChunk` 是流式版 `CallTool`：

1. 生成 `progressToken = "<server>-<atomic_id>"`；
2. `conn.subscribeProgress(token, 32)` → 拿到 progress chan；
3. 在 params 注入 `_meta.progressToken`（MCP 约定的回调字段）；
4. 启一个 goroutine：select { progress | tools/call 终态 | ctx.Done };
5. progress 帧 → `out <- ToolChunk{Content:chunk.Chunk, Progress:..}`；
6. 终态帧 → `out <- ToolChunk{IsFinal:true, Final: parseToolResult(...)}`；
7. defer `unsubscribeProgress` + `close(out)`。

`ToolChunk.Content` 字段对应 MCP 的非标准 `chunk` 字段（§3.2）。GitHub/Atlassian 的官方 MCP 已经支持；不支持的 server 自动退化为只发一帧 `IsFinal=true`（向后兼容）。

### 7.5 接线现状（2026-06）

| 项 | 状态 |
|---|---|
| `Gateway.servers map[string]*ConnPool` | ✅ 已替换 |
| `MCPServerConfig.PoolSize` (YAML `pool_size`) | ✅ 已暴露；缺省/`<=1` 退化为单连接 |
| `Gateway.CallTool` 委托 `pool.CallTool` | ✅ 已切换 |
| `Gateway.AddServer/RemoveServer/Close` 用 pool 生命周期 | ✅ 已切换 |
| 测试用 `newSingletonPool` 把 mock 连接包成 1-slot pool | ✅ 已提供 |
| **`replaceLoop` 自动 respawn**：slot 挂掉后自动 fork | ❌ **未实现**——pool.go 注释提及（`pool.go:98`），但代码无对应 goroutine |
| 健康检查 `checkAll` → `processAlive` 兜底 | ✅ 通过 `transport.Alive()` 探各 slot；死链 CAS 清空 slot；零活时整池 `reconnectServer`。详见 §8.2 |

**关键缺口**：`replaceLoop` 仍未实现，所以"slot 挂了下一个 slot 顶上"这一段在文档里有、代码里没。当前的兜底是 `healthChecker`（2026-06 已在 `main.go` 启动 30s tick）+ 整池 reconnect。单 slot 死亡仅靠下次 tick 清理（最坏 30s 内被 Pick 命中会暴露错误）。

P0 加固详见 §12。

---

## 8. `healthChecker` —— 自愈引擎（`reconnect.go`）

> ✅ **现状**（2026-06）：自愈完整接线——`cmd/agent/main.go` MCP 初始化尾部调 `mcpGateway.StartHealthCheck(30*time.Second)`，开启 30s 周期检查。`processAlive` 不再绑死 `Signal(0)`，而是委托 `transport.Alive()`：stdio 用"reaper 标志 + Signal(0)"，SSE 用"90s 内有事件"——两类传输统一走 `connAlive` 路径。零活 slot → 指数退避 5 次整池重建。

### 8.1 配置（`reconnect.go:23-27`）

```go
var defaultReconnectConfig = reconnectConfig{
    MaxRetries:     5,
    InitialBackoff: 1 * time.Second,
    MaxBackoff:     30 * time.Second,
}
```

退避序列：1s → 2s → 4s → 8s → 16s → 30s（cap）。5 次失败后 `Error` 日志放弃，等待人工或下个 tick。

### 8.2 `connAlive` —— 传输级抽象（`reconnect.go`）

```go
func connAlive(conn *ServerConnection) bool {
    if conn == nil || conn.transport == nil {
        return false
    }
    return conn.transport.Alive()
}
```

健康判定委托给 `Transport.Alive()`，由具体传输实现两套语义（见 `transport.go`）：

| 传输 | `Alive()` 实现 | 抓什么 |
|------|----------------|--------|
| stdio (`transport.go::stdioTransport`) | `conn.exited` 原子位 + `Signal(syscall.Signal(0))` | 两层组合：reaper 已 wait + Signal 探活；详见下方两层检测说明 |
| sse (`transport_sse.go`) | `now - lastRecv < keepaliveTimeout`（默认 90s） | 90 秒内收到过任何 SSE 事件即视为存活；连接断流自动死 |

stdio 的两层检测必须组合：

| 层 | 抓什么 | 漏什么 |
|----|--------|--------|
| `conn.exited` (reaper goroutine) | 已 exit + 已 reap 的进程 | exit 与 Wait 返回之间的短暂窗口 |
| `Signal(syscall.Signal(0))` | exit 后 PID 已被回收的进程（`ESRCH`） | **僵尸**（已 exit 未 reap）——PID 仍在表里，`Signal(0)` 返 0，会误报存活 |

`newServerConnection` 启动后即 spawn 一个 reaper goroutine——但**不能**直接调 `cmd.Wait()`。`os/exec` 文档规定：`Wait` 会关闭由 `StdoutPipe()` 返回的 pipe；如果 `readResponses` 还在那条 pipe 上 `Scan`，Wait 就会把 pipe 从读端脚下抽走。因此 reaper 先 `readerWg.Wait()`，等 `readResponses` 自然 EOF 后再 `cmd.Wait()`：

```go
go func() {
    conn.readerWg.Wait()   // 等 stdout reader drain
    _ = cmd.Wait()          // 见下方对两条路径的说明
    conn.exited.Store(true)
}()
```

`readResponses` 的 EOF 来源有两条，且 Wait 在两条路径下的行为不同：

| 路径 | 触发 | `cmd.Wait()` 行为 |
|------|------|-------------------|
| (a) 子进程自然退出 / 崩溃 | 内核关闭子进程的 stdout 写端 → scanner EOF | 子进程已死，Wait 快速返回并 reap 僵尸 |
| (b) 我们主动 `close()` | `close()` 先 `stdout.Close()` 关我们这端读端 → scanner 立即返回 | 此时子进程可能还活着！Wait 会**阻塞**到 `close()` 后续的 `Process.Kill()` 把进程干掉。reaper 没有其他工作，阻塞无害；Kill 一旦生效 Wait 返回，`exited` 置 true |

两条都不要求 Wait 提前介入，完全符合 `os/exec` 契约。

**单独看 `Signal(0)` 漏僵尸；单独看 `exited` 漏极短的"已 exit 还没 reap 回来"窗口**——两层一起才万无一失。

| 维度 | 进程级（当前） | 协议级（发 MCP `ping`） |
|------|----------------|-------------------------|
| 开销 | 一次 syscall + 一次 atomic load | 每 tick × N server 一次 RPC |
| 误判 | 进程活着但 hung 检测不到 | 真实可用性更准 |
| 实现 | ~20 行（含 reaper） | 要在 `ping` 加超时 + 处理无 `ping` 的旧 server |

**池层增强**（2026-06）：`processAlive(pool)` 用 `connAlive` 探各 slot，在发现死 slot 后会 CAS 把 slot 清空——这样 `pool.Pick()` 不会再把请求派给死连接。仅当整池零活时才升级到 `reconnectServer`（重建整池）。`ConnPool.Alive()` 只查指针非空，**不能**用作健康判定。stdio 与 sse 走同一套池/健康/重连路径，差异完全封装在 `Transport` 接口下。

### 8.3 重连流程（`reconnect.go`）

```text
checkAll() 每 tick:
   for s in copy(gw.servers):                  ← s.pool is *ConnPool
       if hc.processAlive(s.pool) == 0:
           go reconnect(s.name)                ← 整池零活才重建


reconnect(name):
   backoff := 1s
   for attempt in 1..5:
       sleep(backoff)
       err := gw.reconnectServer(ctx[30s timeout], name)
       if err == nil: return
       backoff = min(backoff*2, 30s)
   logger.Error("gave up after 5 retries")


reconnectServer(ctx, name):
   ① mu.Lock
       oldPool = servers[name]; oldPool.Close() ← 关闭整池所有 slot
       delete(servers, name)
   ② mu.Unlock
   ③ cfg := gw.serverConfigs[name]              ← F8：之前存的配置
   ④ pool := NewConnPool(cfg, logger)
   ⑤ pool.Start(ctx, gw.initializeServer)       ← handshake 闭包
   ⑥ mu.Lock; servers[name] = pool; mu.Unlock
```

**为什么先 unlock 再做新连接握手**：握手要 IO（stdin/stdout + tools/list），通常 100ms～几秒。这段时间持有 `Gateway.mu` 会让 `CallTool` / `ListServers` 全部阻塞。

**重连窗口期的数据风险**：握手完成前，`servers[name]` 不存在——同名 `CallTool` 会拿到 `"MCP server not found"`。orchestrator 上层目前**没有**针对 MCP 失败的 retry，所以 LLM 会看到一次工具失败、可能改路径走别的工具。这是已知缺陷（§12）。

### 8.4 与"接线"的差距

需要在 `cmd/agent/main.go:316` 后加：

```go
if mcpGateway != nil && cfg.MCP.HealthCheckEnabled {
    hc := mcp.NewHealthChecker(mcpGateway, logger)
    hc.Start(30 * time.Second)
    defer hc.Stop()
}
```

且要让 `newHealthChecker` 导出（目前小写），并在 `config.MCPConfig` 加 `HealthCheckEnabled bool` / `HealthCheckInterval time.Duration` 字段。

---

## 9. 与 Skill / 内置工具的对比

orchestrator 看到的三类工具来源**同构**——返回类型都是 `models.ToolResult`，参数都是 JSON Schema——LLM 完全感知不到差异。底层实现差异巨大：

| 维度          | 内置工具 (`07_tools.md`)        | MCP server (`internal/mcp`)     | Skill (`08_skill.md`)                  |
|---------------|---------------------------------|---------------------------------|----------------------------------------|
| 实现形态      | Go 函数                          | 独立子进程 + JSON-RPC over stdio | YAML 模板 + 内置工具组合              |
| 部署          | 编译进 agent 二进制              | `npx`/`uvx`/`python` 独立进程    | 写在 `skills/*.yaml`，无独立进程       |
| 故障隔离      | panic 会拖垮主进程               | 子进程崩溃 → 主进程不影响        | 内置工具崩 → 主进程不影响              |
| 延迟          | 直接函数调用 <1ms                | stdio IPC ~2-5ms                | 编排开销 + 内置工具延迟                |
| 跨语言        | 仅 Go                            | 任何 stdio 进程（Node/Py/Rust）  | 仅 YAML 描述能力                       |
| 协议          | 内部                              | MCP 标准 (`tools/call` 等)        | 自定义 prompt + 工具调用约定          |
| 开发难度      | Go 函数 + 注册                   | 写符合 MCP 规范的独立进程        | 写 YAML + 配工具序列                   |
| 工具发现      | `tools.Registry.Definitions()`   | `Gateway.GetAvailableTools()`     | `SkillRegistry.List()`                |

orchestrator `getAvailableTools()` 把这三个 source 列表合并，每次 LLM 调用前重新拼。**新增/删除 MCP server 在下一轮 ReAct step 立刻生效**，无需重启进程。

---

## 10. CallTool 完整时序（已接线路径）

```text
orchestrator                Gateway                ServerConnection             子进程 (npx mcp-...)
     │                         │                          │                            │
     │─ CallTool("filesystem", "read_file", {path}) ────▶│                            │
     │                         │                          │                            │
     │                         │─ RLock; conn = servers["filesystem"]; RUnlock         │
     │                         │                          │                            │
     │                         │─ sendRequest("tools/call", {name,arguments})         │
     │                         │                          │                            │
     │                         │                          │  id := reqID.Add(1)        │
     │                         │                          │  mu: pending[id] = ch      │
     │                         │                          │  writeMu: stdin.Write(req+\n) ─▶│
     │                         │                          │  inflight++                │
     │                         │                          │                            │── exec
     │                         │                          │  select <-ch:              │
     │                         │                          │                            │── 写 stdout
     │                         │                          │◀── stdout "data\n" ────────│
     │                         │                          │  (readResponses goroutine) │
     │                         │                          │  parse, match id, push ch  │
     │                         │                          │  resp := <-ch              │
     │                         │                          │  inflight--                │
     │                         │◀── resp ─────────────────│                            │
     │                         │                          │                            │
     │                         │─ json.Unmarshal → MCPToolResult                      │
     │                         │─ strings.Builder 拼接 text                            │
     │◀── &ToolResult{Content,IsError} ────────────────────────────────────────────────│
```

并发场景：同一 conn 同时来 5 个 `read_file` —— 5 个独立 id、5 个独立 respCh，单个 reader goroutine 按 id demux 全部正确回路。这是 §1.5 Q4 设计的运行时表现。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| 自研 JSON-RPC 而非用官方 SDK | SDK pre-1.0 且缺生产能力；协议层 800 行可控 |
| stdio + SSE 两套 transport 接口化 | 5 方法接口（`Send`/`Recv`/`Err`/`Alive`/`Close`）把"子进程 vs 远程 HTTP"差异收敛在 transport 层；`ServerConnection` 上层完全不感知 |
| `Transport.Alive()` 自带语义 | stdio 用 `Signal(0)+exited`，SSE 用 90s 无事件；`healthChecker.processAlive` 统一调用 `conn.transport.Alive()` 而非自己 instanceof |
| SSE 强制 `pool_size=1` | HTTP 客户端层已能复用 keep-alive；多进程订阅同一 SSE 会重复 session token |
| 两把锁 (`mu` / `writeMu`) | pending 注册与 stdin 写入解耦，前者 lock-free 等响应；transport 层内部各自维护 writeMu，client.go 不再持有 |
| pending 注册 BEFORE 写 stdin | 防止响应早到无 chan 接的永久卡死 |
| reader 单 goroutine | stdout 字节流只能单消费者按行扫描 |
| reader 退出时 broadcast -32603 | 所有等响应的调用者立刻拿到错误，不卡 select |
| `progressNotification.Chunk` 字段 | 兼容 GitHub/Atlassian 事实标准，spec 之外但生态强需求 |
| `toolIndex` 预建 map | O(1) `FindServerForTool`，避免每次 ReAct step 扫所有 conn |
| `inflight atomic.Int64` 而非 chan-based 度量 | Pick 热路径要 lock-free 读取 |
| Validation 用关键字白名单而非沙箱 | npx/uvx 拉网包的信任假设依赖发布者；命令白名单是粗筛 |
| `reconnectServer` 先 unlock 再握手 | 握手 IO 期间不阻塞其他 RPC |
| 工具命名空间无前缀 | 设计偷懒；§12 演进项 |
| CRUD API 不持久化 | 配置层 + 启动时读 config 已够用；持久化等 §16 store |
| `bufio.Scanner` buffer 扩至 4MB | github-mcp 长 issue list 默认 64KB 必爆 |

---

## 12. 后续演进

### P0（生产前必须）

- [x] ~~**接线 `healthChecker`**~~ 已完成（2026-06）：`cmd/agent/main.go` 调 `mcpGateway.StartHealthCheck(30*time.Second)`；`processAlive` 用 `transport.Alive()` 统一探活，零活 slot 触发整池退避重连
- [x] ~~**接线 `ConnPool`**~~ 已完成（2026-06）：`Gateway.servers map[string]*ConnPool`；`MCPServerConfig.PoolSize` (`pool_size: 4`) 起多子进程；仍待补 `replaceLoop` 单 slot 自愈
- [x] ~~**SSE transport**~~ 已完成（2026-06）：`transport_sse.go` 实现 endpoint→message 流；`dialTransport(cfg)` 按 `cfg.Transport` 分派；与 stdio 透明共存；6 项单元测试覆盖 endpoint/relative path/Send-before-ready/Alive keepalive/Close 幂等/多行 data 合并
- [ ] **实现 `replaceLoop`**：pool slot 死亡后后台 respawn + CompareAndSwap 回来（当前 healthChecker 整池重连兜底，但单 slot 自愈仍未做）
- [ ] **工具命名空间前缀**：自动 `<serverName>/<toolName>` 化（或可关闭），解决同名冲突；LLM prompt 同步带前缀
- [ ] **`scanner.Buffer` 大小可配**：默认 4MB 偶尔不够（一次 search 几十 MB 的 grep result），加 `cfg.MCP.ScannerBufferMB`
- [ ] **MCP server 跑在 sandbox**：`docker` 已在 `AllowedMCPCommands` 白名单，但当前 stdio 子进程默认继承 agent 的全部权限；用 docker run + `--network=none` 包一层

### P1（运维质量）

- [ ] **per-server metric**：`mcp_call_duration_seconds{server,tool}` / `mcp_active_connections{server}` / `mcp_reconnect_total{server}`
- [ ] **进度通知接入 orchestrator**：`Gateway.CallToolStream` 公开版本，让 SSE 流式输出走到前端
- [ ] **`notifications/tools/list_changed` 监听**：server 自报工具变化时刷新 toolIndex（不用等下个健康检查）
- [ ] **per-server circuit breaker**：单 server 连续 5 次失败暂停 30s，不拖累全局
- [ ] **stderr 接入 audit**：MCP server stderr 当前透传父进程；包成 `bufio.Reader` 行扫描 → `audit/logger.go`
- [ ] **CRUD 配置持久化**：Postgres 存 `mcp_servers` 表，重启从 DB 而非 yaml 恢复

### P2（演化方向）

- [ ] **batch call**：同 server 的多个 tools/call 合并成单次（需 MCP spec 演进配合）
- [ ] **自动 failover**：同工具在多 server 上提供时，一挂自动切
- [ ] **mTLS for SSE**：远程 MCP 接入时必须的双向证书
- [ ] **协议版本协商**：当前固定 `2024-11-05`；改成 `min(client, server.protocolVersion)`
- [ ] **HITL gate per tool**：单个 MCP 工具标注 `requires_approval=true`，调用前触发 Temporal approval（[11_temporal.md](11_temporal.md)）

---

## 13. 设计教训

`mcp` 包从初版的 220 行 demo 演化到现在 1500+ 行，踩过的真坑：

1. **JSON-RPC ID 必须在写 stdin 之前注册 pending**——初版顺序反了，10 次调用偶发卡 1 次。Race 极小但用户报"agent 偶尔像被掐了脖子"。第一性原理回去看才意识到：网络往返的最小延迟也可能小于"写完 stdin → 注册 pending"这两条 Go 语句之间的间隔。

2. **`bufio.Scanner` 默认 64KB 必爆**——github-mcp 拉一次 issue list 就 200KB。初版 reader 静默 panic（log 里没显式信息），表象是"server 永远不返回"。教训：任何流式 protocol 第一步先把 buffer 扩大。

3. **两把锁不是过度设计**——初版一把 `mu` 同时保护 pending + stdin，并发 RPC 时所有 goroutine 都在 mu 排队等 IO。改成两把锁后 p99 立刻降 60%（4 并发 RPC 测试）。

4. **reader 退出必须 broadcast 错误**——初版子进程崩了时所有等响应的 caller 直接卡 `<-respCh`。`ctx` 取消才返回但要等用户 timeout。改成 reader 退出前给所有 pending 注入 -32603 后立刻全部解锁，体感"agent 卡住"变成"工具明确失败可重试"。

5. **池/自愈/SSE 的"骨架就绪、未接线"是有意识的决策**——单连接 + 无自愈在我们当前规模（≤10 个 server / ≤4 并发 ReAct）够用；过早接线只会引入"为复杂而复杂"的 bug。等真正撞到天花板（监控显示 p99 飚高 / server crash 频率上升）再接，比一上来就拉满更稳。把代码留在 repo 里是为了**让接线时只需 50 行 main.go 改动**，而不是从零写 462 行。

6. **命名空间冲突是 spec 的锅，不是实现的锅**——MCP spec 不强制 server 名前缀，所以多 server 同名工具是合法的、且各自 server 不知道对方存在。要前缀只能客户端单边加，但加了之后 LLM prompt 里所有工具名都变长（前缀 + 斜杠 + 工具名）；为了"防一种小概率冲突"让所有 tool prompt 都长 20%，是错误的省事。当前选择"配置层强制唯一" + "前缀化作为 §12 P0 可选"是务实的。

---

下一篇：[`07_tools.md`](07_tools.md) —— 内置工具注册表（file/git/edit/lsp/pty 等），MCP 工具与之同居 `tools.Registry`。
