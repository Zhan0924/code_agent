# 06 · MCP 网关 `internal/mcp`

> 代码：
> - `client.go` (486) — JSON-RPC 2.0 协议 + `ServerConnection` 进程管理 + `Gateway` 多服务聚合
> - `reconnect.go` (208) — 健康检查与自动重连 (`healthChecker`)
>
> 背景文档：`docs/mcp_skill_design.md` 详细设计
>
> 测试：`client_test.go` (58)

---

## 1. 模块定位

**"给 Agent 一条插上任何外部工具的电源线。"**

[Model Context Protocol](https://modelcontextprotocol.io) 是 Anthropic 推出的 LLM ↔ 工具互操作协议，本质是"**JSON-RPC 2.0 over stdio / SSE**"。Agent 系统接入 MCP Server 之后：

- 可以调用 GitHub MCP 来查 issue / open PR；
- 可以挂 Database MCP 直接读线上库；
- 可以挂 Jira / Slack / 自研 CRUD 服务…

本包干的四件事：

1. **把 MCP Server 当子进程启动**（`exec.Command` + stdin/stdout 管道）；
2. **实现 JSON-RPC 客户端**：同步 request/response + 并发 pending map + 异步 reader goroutine；
3. **汇聚多个服务**：`Gateway` 管一组 `ServerConnection`，按 `tools/list` 结果建立 "工具名 → 服务" 的路由表；
4. **运行时 CRUD**：`AddServer` / `RemoveServer` / `ListServers` —— 零重启新增/删除能力。

加上 `reconnect.go` 的健康检查与自愈：子进程崩溃 → 自动 respawn，工具照用。

---

## 1.5 设计哲学：MCP 客户端的 4 个抉择

### Q1 — 为什么**自研**而不用官方 SDK？

**官方 SDK**（`modelcontextprotocol/go-sdk`）现状：
- API 不稳定（2024 初期 pre-1.0，多次 break change）
- 专注"协议正确"而非"生产需求"——没有重连、没有熔断、没有 PoolSize
- stdio 子进程生命周期管理简陋（进程崩溃不自动重启）

**我们的需求**：
- stdio server 挂了 → 自动重启（10s 内恢复）
- 并发 tool call → 多个 stdio 子进程池分担
- 部分 MCP server 慢 → 单 server 熔断，不拖累其他

**决策**：自研。协议层 JSON-RPC 2.0 本身简单（几百行），可控性远胜
SDK 抽象。

### Q2 — stdio vs SSE vs HTTP？

MCP 协议定义了两种传输：stdio（子进程）和 HTTP SSE。

| 维度 | stdio | SSE |
|---|---|---|
| 部署 | 本地子进程 | 独立进程 / 远程服务 |
| 延迟 | μs 级（UNIX pipe） | ms 级（网络） |
| 隔离 | 进程级（崩了不影响 agent） | 网络级（隔离更强） |
| 部署复杂度 | 低（npm install 即可） | 高（要 HTTP server） |
| 调试 | stdin/stdout 可 tail | 需要 HTTP debug 工具 |
| 场景 | 本地工具（github-cli） | 远程服务（内网 DB） |

**支持：两者皆支持**。`MCPServerConfig.Transport` 字段切换。

### Q3 — PoolSize 的意义？

**痛点**：某些 MCP server（如 github-mcp）单连接是单线程的——上一个
tool call 没返回，下一个就得等。

**决策**：`MCPServerConfig.PoolSize >= 2` 时，起 N 个子进程池，round-robin
分发 call。并发度提升 N 倍。对不敏感的 server（本地 filesystem-mcp）
保持 PoolSize=1 避免开销。

### Q4 — tool 命名空间冲突

**问题**：2 个 MCP server 都注册了 `get_file` 工具名——谁赢？

**决策**：在 tool name 前加 server 前缀：`github/get_file` vs
`filesystem/get_file`。orchestrator 看到的是全名；LLM 传 tool_call 也
要带前缀。

**好处**：跨 server 没有歧义，LLM 的 system prompt 可以明确声明。
**代价**：增加 prompt 长度（前缀 token）。实测 1 MCP server 20 个工具
前缀 token 约占 prompt 总长 < 1%，可接受。

---

## 2. 依赖架构

```
     ┌─── api/mcp_skill_handlers.go (REST CRUD) ────┐
     └──────┬────────────────────────────────────────┘
            │ AddServer / RemoveServer / ListServers
            ▼
     ┌──────────────────────────────┐
     │     mcp.Gateway              │  维护 map[name]*ServerConnection
     │       .toolRouter{tool→srv}  │  维护 map[tool]name
     │       .healthChecker         │
     └─────┬──────────────┬─────────┘
           │              │ calls
           │              ▼
           │      ┌──────────────────┐   tools/list
           │      │ ServerConnection │ ◄──────────── periodic
           │      │  cmd *exec.Cmd   │
           │      │  stdin/stdout    │
           │      │  pending map     │
           │      │  reader goroutine│
           │      └────────┬─────────┘
           │               │ stdio
           │               ▼
           │      ┌──────────────────┐
           │      │ MCP Server 进程  │
           │      │ (github-mcp,     │
           │      │  db-mcp, …)      │
           │      └──────────────────┘
           │
           ▼ 聚合后暴露给
     ┌──────────────────────────────┐
     │ orchestrator.getAvailable    │
     │ Tools()                      │ ← 每次 LLM 调用动态组装
     └──────────────────────────────┘
```

---

## 2.5 数据流总览

下图展示 MCP Gateway 的三条主数据流：**启动初始化**、**运行时 CallTool**、**健康检查与自愈**。

```text
═══════════════ 启动初始化 (per MCP Server) ═══════════════

┌──────────────────────┐
│ config.MCP.Servers[] │
│ {name, command, args}│
└──────────┬───────────┘
           │
           ▼
┌──────────────────────────────────────────────────────────┐
│ exec.Command(command, args...)                            │
│ → stdin pipe (writer)                                    │
│ → stdout pipe (reader goroutine: scanner → pending map)  │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼  JSON-RPC: initialize
┌──────────────────────────────────────────────────────────┐
│ sendRequest("initialize", {capabilities})                │
│ → 写 stdin → 读 stdout → 解析 response                  │
│ → 获取 server capabilities + protocol version           │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼  JSON-RPC: tools/list
┌──────────────────────────────────────────────────────────┐
│ sendRequest("tools/list", {})                            │
│ → 解析 tool definitions                                  │
│ → 写入 toolMap[toolName] = serverConn                    │
│ → 注册到 tools.Registry (作为 Provider)                  │
└──────────────────────────────────────────────────────────┘


═══════════════ 运行时 CallTool ═══════════════

┌─────────────────────┐
│ orchestrator.       │
│ executeTool("mcp_*")│
└──────────┬──────────┘
           │
           ▼
┌──────────────────────────────────────────────────────────┐
│ Gateway.CallTool(serverName, toolName, args)              │
│  ① toolMap[toolName] → 定位 ServerConnection             │
│  ② 构造 JSONRPCRequest{method:"tools/call", params}      │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼
┌──────────────────────────────────────────────────────────┐
│ ServerConnection.sendRequest(req)                         │
│  ① nextID++ → req.ID                                    │
│  ② pending[id] = make(chan *JSONRPCResponse, 1)          │
│  ③ json.Encode(req) → 写入 stdin pipe                   │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼ (异步)
┌──────────────────────────────────────────────────────────┐
│ reader goroutine (持续运行):                              │
│  scanner.Scan() → json.Decode(line)                      │
│  → pending[resp.ID] <- resp                              │
│  → delete(pending, resp.ID)                              │
└──────────────────────────┬───────────────────────────────┘
                           │
                           ▼ (chan recv with timeout)
┌──────────────────────────────────────────────────────────┐
│ result := <-pending[id]                                   │
│ → 解析 result.Content                                    │
│ → 返回 *ToolResult{Content, IsError}                     │
└──────────────────────────────────────────────────────────┘


═══════════════ 健康检查与自愈 ═══════════════

┌──────────────────┐    ┌─────────────────────────────────┐
│ healthChecker    │    │ 定时 ping (每 30s)               │
│ goroutine        │──▶ │ sendRequest("ping", {})          │
└──────────────────┘    └────────────────┬────────────────┘
                                         │
                        ┌────────────────┴────────────────┐
                        ▼                                 ▼
               ┌──────────────┐                 ┌──────────────┐
               │ ping 成功    │                 │ ping 失败    │
               │ → 继续       │                 │ / 进程退出   │
               └──────────────┘                 └──────┬───────┘
                                                       │
                                                       ▼
                                        ┌──────────────────────────┐
                                        │ kill 旧进程               │
                                        │ exponential backoff 重启  │
                                        │ (1s → 2s → 4s → max 30s)│
                                        │ 重走 initialize 握手      │
                                        │ 重建 toolMap             │
                                        └──────────────────────────┘
```

---

## 3. 核心数据结构

### 3.1 JSON-RPC 2.0 消息

```go
type JSONRPCRequest struct {
    JSONRPC string      // 固定 "2.0"
    ID      int64       // 递增，用于匹配响应
    Method  string      // "initialize", "tools/list", "tools/call", …
    Params  interface{} // 方法相关 payload
}

type JSONRPCResponse struct {
    JSONRPC string
    ID      int64
    Result  json.RawMessage
    Error   *JSONRPCError
}

type JSONRPCError struct { Code int; Message, Data string }
func (e *JSONRPCError) Error() string { ... }    // 实现 error 接口
```

> JSON-RPC ID 刻意用 `int64` 自增而非字符串 UUID —— 匹配更快，内存占用小。

### 3.2 MCP 协议层

```go
type MCPTool struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    InputSchema json.RawMessage `json:"inputSchema"` // JSON Schema
}

type MCPToolResult struct {
    Content []MCPContent `json:"content"`
    IsError bool         `json:"isError"`
}

type MCPContent struct {
    Type string // "text" / "image" / "resource"
    Text string
    // ... blob / url 等
}
```

这些直接对应 MCP spec 的 `tools/list` 返回和 `tools/call` 的结果。

### 3.3 `ServerConnection` — 单个子进程

```go
type ServerConnection struct {
    name    string
    cmd     *exec.Cmd
    stdin   io.WriteCloser
    stdout  io.ReadCloser
    scanner *bufio.Scanner              // 按行读 JSON-RPC

    reqID   atomic.Int64                // 请求 ID 生成器
    pending map[int64]chan *JSONRPCResponse  // 等待中的请求
    mu      sync.Mutex                   // 保护 pending + stdin 写

    tools   []MCPTool                    // 该 server 发布的工具
    logger  *zap.Logger
}
```

三个关键并发机制：

| 机制                         | 作用                                          |
|------------------------------|-----------------------------------------------|
| `pending map + chan`         | 多路复用：并发发送 N 个请求，按 ID 匹配响应     |
| 独立 `readResponses` goroutine | stdin 写阻塞时，reader 不会被拖住              |
| `mu` 同时保护 pending 和 stdin | stdin 写必须串行化（否则两条 JSON 会被交错）   |

### 3.4 `Gateway` — 多服务聚合

```go
type Gateway struct {
    mu       sync.RWMutex
    servers  map[string]*ServerConnection   // serverName → conn
    toolMap  map[string]string              // toolName   → serverName  (路由表)
    configs  map[string]*config.MCPServerConfig  // 保留配置，用于 reconnect
    hc       *healthChecker
    logger   *zap.Logger
}
```

**路由表**是双向导航的桥梁：

- Orchestrator 只知道 `toolName`（LLM 返回的）→ `FindServerForTool(toolName)` → `serverName`；
- 再 `servers[serverName].sendRequest("tools/call", …)`。

---

## 4. 启动与初始化流程

```
NewGateway(cfg, logger):
  for srvCfg in cfg.Servers:
     ├── conn = newServerConnection(srvCfg)
     │      - exec.Command(cmd, args...)
     │      - env: merge cfg.Env
     │      - stdin/stdout pipe
     │      - cmd.Start()       ← 子进程跑起来了
     │      - go conn.readResponses()
     │
     ├── initializeServer(ctx, conn)        ← 见 §4.1
     │
     ├── servers[srvCfg.Name] = conn
     └── for tool in conn.tools:
             toolMap[tool.Name] = srvCfg.Name
  go gw.hc.Start(healthCheckInterval)       ← 启动健康检查
```

### 4.1 `initializeServer` 的握手三步

标准 MCP 协议握手：

```
1. POST initialize
   {
     "protocolVersion": "2024-11-05",
     "capabilities": { "tools": {} },
     "clientInfo": { "name": "code-agent", "version": "1.0" }
   }
   → 拿到 server 的 capabilities/serverInfo

2. NOTIFICATION initialized
   (不需要响应；告诉 server "客户端准备好了")

3. POST tools/list
   → 拿到 conn.tools = [MCPTool{...}, ...]
```

这三步一次失败，`newServerConnection` 返回错误，`Gateway` 会跳过该 server 继续启动其他的 —— **单服务故障不阻塞启动**。

---

## 5. `sendRequest` —— 核心 RPC

```go
sendRequest(ctx, method, params):
  id := reqID.Add(1)
  req := JSONRPCRequest{ JSONRPC:"2.0", ID:id, Method, Params }

  # 1. 注册响应 chan
  respCh := make(chan *JSONRPCResponse, 1)
  mu.Lock(); pending[id] = respCh; mu.Unlock()

  # 2. 写入 stdin (必须串行)
  mu.Lock(); fmt.Fprintf(stdin, "%s\n", data); mu.Unlock()

  # 3. 等待响应 or ctx 取消
  select {
    case resp := <-respCh:    return resp, nil
    case <-ctx.Done():        delete(pending, id); return nil, ctx.Err()
  }
```

**关键设计**：

- 用 `bufio.Scanner` 以 `\n` 分帧 —— MCP over stdio 的事实惯例；
- 同一个 `ServerConnection` 可以**并发**有多个 in-flight 请求（不同 ID 各自有 chan）；
- ctx 取消后**删 pending 记录**，防止内存泄漏（即使响应迟到了，也找不到 chan 被 GC）。

`readResponses` goroutine 则是纯 demux：

```go
for scanner.Scan():
  parse line → resp
  ch := pending[resp.ID]
  delete(pending, resp.ID)
  ch <- resp
```

---

## 6. 工具调用 `CallTool`

```go
Gateway.CallTool(ctx, serverName, toolName, args json.RawMessage) (*models.ToolResult, error)
```

```
1. conn := gw.servers[serverName]              RLock
2. resp, err := conn.sendRequest(ctx, "tools/call", {
     "name": toolName,
     "arguments": args   // 已经是 JSON RawMessage，直接塞
   })
3. 解析 resp.Result 到 MCPToolResult
4. 转换 → models.ToolResult:
     Content 拼接为一段 text（MCP 可能返回多个 content blob）
     IsError = mcpResult.IsError
     Duration = 本次调用耗时
```

`models.ToolResult` 是 orchestrator 那一层统一的工具结果类型 —— MCP、内置工具、Skill 三种来源**同构**。

---

## 7. 工具聚合 `GetAvailableTools`

```go
GetAvailableTools() []models.ToolDefinition:
  mu.RLock
  out := []models.ToolDefinition{}
  for _, conn := range servers:
    for _, tool := range conn.tools:
      out = append(out, models.ToolDefinition{
        Name:        tool.Name,
        Description: tool.Description,
        Parameters:  tool.InputSchema,   // 透传 JSON Schema
      })
  return out
```

这个列表会被 orchestrator 每次发 LLM 请求前和 **内置工具 + Skills** 合并，塞进 `function_call` 参数 —— 所以"新加一个 MCP Server"的下一轮 chat 就生效了，**不用重启**。

---

## 8. 运行时 CRUD API

### 8.1 `AddServer(cfg)` → `*ServerStatus`

```
1. 校验：name 不能重复
2. newServerConnection(cfg) + initializeServer
3. mu.Lock → servers[name] = conn; toolMap[tool] = name (for each tool)
4. 保存 configs[name] = cfg     (给 reconnect 用)
5. 返回 ServerStatus{ Name, Connected:true, ToolCount, Tools }
```

失败要**回滚**：杀掉已启动子进程，避免 orphan。

### 8.2 `RemoveServer(name)`

```
1. mu.Lock
2. conn.close()
     - cmd.Process.Signal(SIGTERM)
     - 等 2s
     - 还没死就 SIGKILL
     - 关 stdin/stdout (readResponses goroutine 自然退出)
3. delete(servers, name); delete(configs, name)
4. 清理 toolMap：删所有 value == name 的条目
```

### 8.3 `ListServers()`

```go
type ServerStatus struct {
    Name        string
    Connected   bool      // 子进程活着 + 握手完成
    ToolCount   int
    Tools       []string  // 工具名列表
    LastError   string    // 最近一次错误（便于排查）
}
```

前端 `code_agent_ui/src/pages/MCPPage.tsx` 用这个渲染"已连接的 MCP 服务器列表 + 启停按钮"。

---

## 9. `healthChecker` —— 自愈引擎 (`reconnect.go`)

### 9.1 配置

```go
var defaultReconnectConfig = reconnectConfig{
    MaxAttempts:       5,
    InitialBackoff:    1 * time.Second,
    MaxBackoff:        60 * time.Second,
    BackoffMultiplier: 2.0,
}
```

### 9.2 周期性检查

```go
healthChecker.Start(interval):
  go loop {
     select {
       case <-tick:  hc.checkAll()
       case <-stop:  return
     }
  }

checkAll():
  gw.mu.RLock → 拷贝 servers 列表
  for name, conn in copied:
    if !hc.isAlive(conn):
        go hc.reconnect(name)         ← 并发重连，不阻塞其他检查
```

### 9.3 `isAlive`

简单却有效：

```go
isAlive(conn):
  ctx, cancel := context.WithTimeout(ctx, 2s)
  _, err := conn.sendRequest(ctx, "ping", nil)   ← MCP 有 ping 方法
  return err == nil
```

> MCP spec 定义了 `ping` 方法专门用于保活，本代码直接用 —— 不用自己定心跳协议。

### 9.4 指数退避重连

```go
reconnect(serverName):
  cfg := gw.configs[serverName]   ← 之前保存的配置
  for attempt in 1..MaxAttempts:
      sleep(min(Initial * Mul^(attempt-1), MaxBackoff))
      err := gw.reconnectServer(ctx, serverName)
      if err == nil: return
      logger.Warn("reconnect failed", attempt=...)

  logger.Error("max reconnect attempts exhausted")
  → 放弃；工具仍在 toolMap 里，但 CallTool 会失败
  → 用户看 ListServers 发现 Connected=false 便知道要手动处理
```

### 9.5 `reconnectServer` 实际工作

```
1. gw.mu.Lock
2. old := servers[name]; old.close()           ← 清掉僵尸
3. new := newServerConnection(cfg)
4. initializeServer(ctx, new)
5. servers[name] = new
6. 重建 toolMap 的对应条目（工具集可能变化）
7. gw.mu.Unlock
```

**⚠️ 注意点**：重连窗口期（几百 ms）调用 `CallTool` 会读到 old closed conn 并失败。orchestrator 的重试机制会兜一下；更进一步可以用 **读写锁的 upgrade** 或 **原子指针替换**（进一步优化方向）。

---

## 10. 与 Skill 的关系（对比）

`07_tools.md` / `08_skill.md` 会细讲；这里说清楚差异：

| 维度           | MCP Server                       | Skill (`internal/skill`)              |
|----------------|----------------------------------|---------------------------------------|
| 实现           | **子进程** + JSON-RPC over stdio  | 内存中的 Go 函数 / HTTP webhook        |
| 部署           | 独立程序（npx / python / go）     | 仅在 Gateway 进程内                    |
| 生命周期       | Gateway 拉起+维护                 | 完全在 Gateway 内存                    |
| 故障隔离       | 子进程崩溃不影响主进程            | 崩溃（panic）会影响主进程             |
| 协议           | MCP 标准                          | 自定义 HTTP POST（轻量）              |
| 延迟           | IPC 2-5ms                         | 函数调用 <1ms / HTTP 5-20ms           |
| 开发难度       | 要写个符合 MCP 规范的独立进程      | 直接写 Go 函数或简单 HTTP handler      |

Orchestrator 层对三种来源（内置/ MCP / Skill）**统一抽象**，LLM 完全感知不到差异。

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| stdio 优先于 SSE | stdio 零配置、调试友好、无网络依赖；SSE 适合远程服务（规划中） |
| `bufio.Scanner` 按行读 | MCP 约定一行一个 JSON 帧；Scanner 默认 64KB 行上限 → 需要 `scanner.Buffer()` 扩容（TODO） |
| `pending map + chan` 做多路复用 | 支持单 server 并发调用；ID 匹配响应是 JSON-RPC 经典范式 |
| 子进程崩溃**不**自动抛异常 | 健康检查 + 重连兜底；主流程永远拿到的是 `ErrNotConnected`，可 fallback |
| `reconnectServer` 保存原始 cfg 而非 handle | cfg 不变意味着重启恢复出的 server 行为一致；handle 可能已被 GC |
| `tools/list` 只在握手时拉一次 | MCP 标准有 `notifications/tools/list_changed`，待实现；现在重连刷新 |
| CRUD API 直连 Gateway 而非走 DB | 运行时状态天然在内存；若要持久化重启恢复，在 `16_store.md` 的 config persistence 里加 |
| `healthChecker` 用单例 goroutine + 并发 reconnect | 检查频率低（30s/次）不值得 worker pool；reconnect 并发以防"一个挂服务拖延所有" |
| JSON-RPC ID 用 int64 atomic 而非 UUID | 短、快、天然有序；单进程不会冲突 |

---

## 12. 后续演进

- [ ] **SSE transport**：MCP spec 定义的另一种通道，支持远程托管的 MCP 服务（GitHub 官方有 hosted 版本）；
- [ ] **`scanner.Buffer()` 扩容**：某些 MCP Server 一次返回很长的 `tools/list`（几十 KB），默认 64 KB 会被 scanner 截断；
- [ ] **`notifications/tools/list_changed` 支持**：工具热更新，不用等健康检查周期；
- [ ] **subprocess sandbox**：MCP Server 子进程默认继承 Agent 的全部权限 —— 生产应把它关进 Docker（和 `sandbox` 模块复用）；
- [ ] **原子指针 swap**：`reconnectServer` 过程中对 `servers[name]` 用 `atomic.Pointer`，彻底消除重连窗口的竞争；
- [ ] **持久化配置**：Gateway 启动时从 `store.Postgres` 读取已注册 servers，避免重启后需要重新 AddServer；
- [ ] **权限模型**：对单个 tool 标注所需权限（读/写/敏感），调用前查 HITL 策略（见 `11_temporal.md` 的 await 机制）；
- [ ] **tool 返回流式**：MCP spec 支持增量返回，`CallTool` 目前只等完成；要改成 `CallToolStream`。

---

## 13. 实现剖析与改进方向

### 一次 CallTool 调用的完整时序（stdio transport）

```text
orchestrator              mcp.Gateway          stdioServer (子进程)
      │                        │                     │
      │─ CallTool("github/create_issue", args) ──▶  │
      │                        │                     │
      │                        │─ acquire pool slot  │
      │                        │─ nextRequestID()    │
      │                        │─ encode JSON-RPC:    │
      │                        │   {"jsonrpc":"2.0", │
      │                        │    "id":42,         │
      │                        │    "method":"tools/call",│
      │                        │    "params":{...}}   │
      │                        │─ write to stdin ───▶│
      │                        │                     │── 处理请求（调 API）
      │                        │                     │
      │                        │  registerPending(42)│
      │                        │  等待 response chan │
      │                        │                     │
      │                        │◀── stdout "data\n" ─│
      │                        │  scanner.Scan()     │
      │                        │  parse JSON-RPC     │
      │                        │  match id=42        │
      │                        │  pendingChan[42] <- │
      │                        │                     │
      │◀── ToolResult ─────────│                     │
      │                        │─ release pool slot  │
```

**关键并发模型**：
- **一条读 goroutine** 独占 stdout，分发给 pendingChan
- 多个调用方 goroutine 并发调 `CallTool`，各自等自己的 response channel
- `PoolSize >= 2` 时起 N 个子进程，`Gateway` 做 round-robin

### 健康检查与重连（`reconnect.go`）

```text
每 30s tick:
  ├─ 发 "ping" 风格的 tools/list（轻量 RPC）
  ├─ 超时 5s → 认定 unhealthy
  │   ├─ kill 子进程
  │   ├─ 清空 pending requests（返回 error 给调用方）
  │   ├─ 指数退避：wait 1s, 2s, 4s, ... max 60s
  │   └─ Exec 新子进程（重新握手 initialize）
  └─ pong 返回 → 维持连接
```

**恢复时间**：典型 stdio server 重启 10-30s。期间所有 `CallTool` 排队
或直接 fail。

### JSON-RPC 帧的坑

MCP 规定 **一行一帧**（换行分隔 JSON）。我们用 `bufio.Scanner` 读取：

```go
scanner := bufio.NewScanner(stdout)
scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)  // ← 必须扩容
for scanner.Scan() {
    line := scanner.Bytes()
    handleFrame(line)
}
```

**踩过的坑**：默认 `bufio.Scanner` 行上限是 **64 KiB**，github-mcp 返回长
issue list 很容易超。必须用 `scanner.Buffer()` 显式扩大。

### 利弊评估

**优势（Pros）**
- ✅ 自研协议层仅几百行，可控性强
- ✅ 自动重连让 stdio server 的崩溃对 orchestrator 透明
- ✅ PoolSize 多进程对慢 server 有效（吞吐 × N）
- ✅ 命名空间前缀 `github/xxx` 避免 tool 冲突
- ✅ stdio / SSE 双支持，本地工具和远程服务都能接

**代价（Cons）**
- ⚠️ 每次 CallTool 是 RPC 往返（stdio 虽然 μs 级但还是 round-trip）
- ⚠️ 无 batch：10 个独立 tool call 就是 10 次 round-trip
- ⚠️ tool 结果不能流式（当前 blocking 等 full response）
- ⚠️ stdio server 的错误日志去 stderr，和 RPC 混在一起，不好排查
- ⚠️ 没有 per-server 熔断——某 server 慢会拖累它自己的 pool，其他 server
  不受影响（这点反而是好事）

### 可改进点

**P0**
1. 添加 per-server metric（`mcp_call_duration{server,tool}`）便于找慢点
2. Scanner buffer 上限从 16 MiB 改为可配置（某些 MCP 返回大 attachment）

**P1**
3. 支持 tool 结果流式（MCP spec 里已定义 notification 机制）
4. Server health dashboard：`/api/v1/mcp/servers` 返回每 server 最近 30s 成功率
5. stdio stderr 收到错误日志时记 audit log（现在只丢到 debug log）

**P2**
6. batch call：同一 tool 的多次连续调用合并（tricky，需要 tool 本身支持）
7. 自动故障转移：同一 tool 如果在多个 server 有，一个挂了切另一个
8. SSE transport 完善（目前 stdio 更成熟）

---

下一篇：`07_tools.md` —— 内置工具注册表（file/git/exec 工具的声明与调度）。
