# 17 · HTTP / SSE / WebSocket 接入层 `internal/api` + `cmd/agent/main.go` DI

> 代码（**以代码为准**，行数 2025-Q1 实测）：
>
> - `router.go` (362 行) — `Server` 聚合结构 + 中间件装配 + 路由树
> - `handlers.go` (647 行) — `chat / chat/stream / chat/react-stream / sessions / ws / webhooks / index`
> - `middleware.go` (229 行) — RequestID / Metrics / 进程内 token-bucket 限流 / Recovery
> - `auth_handlers.go` (66 行) — `POST /auth/token` 颁发 JWT
> - `project_handlers.go` (144 行) — 项目生成（同步 / SSE / 状态查询）
> - `mcp_skill_handlers.go` (242 行) — MCP/Skill CRUD + `/tools` 聚合列表
> - `workspace_handlers.go` (354 行) — 工作区文件浏览/编辑（细节见 `14_workspace.md`）
> - `dynamic_tool_handlers.go` (104 行) — 运行期动态工具注册/注销/查询
> - `session_handlers.go` (62 行) — `PinMessage` / `UnpinMessage`
> - `p0_debug_handlers.go` (294 行) — P0 优化项的可观测探针端点
> - `integration_test.go` (627 行) + `integration_p0_test.go` (537 行) — httptest 端到端
> - `cmd/agent/main.go` (≈760 行) — **整个系统手动 DI 的唯一入口**
>
> 注意：本文档不复述 `doc.go` 顶端的"设计原理"——那段中间件顺序的描述与实际代码相反，详见 §3.3。

---

## 1. 模块定位

**"把所有内部子系统包装成一个 HTTP/SSE/WS 服务端，并在 `main.go` 里做一次性手动 DI。"**

职责分层：

| 层 | 文件 | 负责 |
|---|---|---|
| **DI 容器** | `cmd/agent/main.go` | 构造所有组件，按拓扑连线（cfg → redis → session → llm → rag → sandbox → mcp → store → orchestrator → planner → skill → indexer → server）、绑生命周期、注册 defer 关闭 |
| **HTTP 引擎** | `router.go` | Gin engine + 中间件 Use 顺序 + 路由表 + 晚绑定 Setter |
| **Handlers** | `*_handlers.go` | 请求反序列化 → 调用业务（多半 `orchestrator`）→ 写响应（JSON / SSE / WS frame） |

**明确不做的事**：

- 业务逻辑（在 `orchestrator`）
- 认证算法（在 `auth`）；handler 只通过中间件提取 claims
- 限流计数本身（Redis 版在 `auth/redis_ratelimit.go`，本包内仅进程内 token-bucket fallback）
- 审计落盘（在 `audit` 包）

---

## 1.5 设计哲学：6 个被代码证实的抉择

### Q1 — 为什么是 Gin

Radix-tree 路由 + 中间件生态成熟 + Prometheus/OTel 现成 adapter。
代价：`gin.Context` 是一个杂物筐（c.Set / c.Get 弱类型 + claims 注入），handler 间共享状态靠 string key 容易出 typo。

### Q2 — 为什么 chat 有 4 种端点

**真实需求**：ReAct 一轮可能 1-300 秒，前端要能渲染中间步骤；CLI 调用要简单同步。
**实际提供 4 路**：

| 路径 | 模式 | 用途 | 关键代码 |
|---|---|---|---|
| `POST /chat` | 同步 + 客户端断开 detach | 简单脚本/CLI | `handlers.go:109-169` |
| `POST /chat/stream` | SSE 单一 `message` 事件流 | token 级流式 | `handlers.go:174-234` |
| `POST /chat/react-stream` | SSE 多事件类型（intent/thinking/tool_call/tool_result/final/approval_request/done） | 前端 ReAct 可视化（默认） | `handlers.go:249-296` |
| `GET /ws` | WebSocket 双向 | 未来推送/审批 | `handlers.go:468-557` |

### Q3 — Detached Context（反直觉但务实）

`handleChat`（**仅同步端点**）的核心反直觉决策：

```go
// handlers.go:135-167
agentCtx, agentCancel := context.WithTimeout(context.Background(), 10*time.Minute)
defer agentCancel()
resultCh := make(chan chatResult, 1)
go func() {
    resp, err := s.orchestrator.ProcessMessage(agentCtx, sessionID, req.Message, ...)
    resultCh <- chatResult{resp, err}
}()
select {
case result := <-resultCh:           // 跑完了 → 返回
    c.JSON(http.StatusOK, result.resp)
case <-c.Request.Context().Done():   // 客户端断了
    // 不 cancel agentCtx，让 goroutine 继续跑完，结果存进 session
    s.logger.Info("client disconnected, agent continues in background")
}
```

理由：
- ReAct 一轮 10 分钟，HTTP 60s 默认超时会断
- 用户掉线 → 重新 `GET /sessions/:id` 仍能拿到完整结果
- LLM token 费已付，半途杀掉是浪费

⚠️ **此设计在三类端点上略有差异**：
- `POST /chat` 用 detached background ctx —— 客户端断 → 业务继续，结果落 session
- `POST /chat/stream` 仍用 `c.Request.Context()`（`handlers.go:204`）—— token 流断了无意义
- `POST /chat/react-stream` 已解耦（2026-06-04，详见 §2.5 与 09_orchestrator §4.1）：
  形参重命名 `reqCtx`，内部构造 `workCtx (background + 30min)` 跑业务；reqCtx 死时只通过
  `channelSink.droppedCtx` 静默丢弃事件，workCtx 不被传染。配合 25s SSE 心跳消除
  `server.write_timeout: 600s` 在合法长任务上的触发。
- `GET /ws` 用的是请求 ctx（`handlers.go:515,541`）

设计原则：用户离开 ≠ 任务死亡。同步与 react-stream 都贯彻此原则；`/chat/stream` 是 token 增量直传，断了无法续看，例外保留请求 ctx。

### Q4 — 晚绑定 Setter 而非构造参数

`NewServer` 只接受 `orch / sessionMgr / logger / opts`，其它依赖（MCP/Skill/Indexer/Generator/Workspace/Store/P0）通过 `Set*` 注入：

```go
apiServer := api.NewServer(orch, sessionMgr, logger, api.ServerOptions{...})
apiServer.SetIndexer(idx)
apiServer.SetSkillRegistry(skillReg)
apiServer.SetStore(pgStore)
apiServer.SetMCPGateway(mcpGateway)
apiServer.SetWorkspaceManager(wsMgr)
apiServer.SetGenerator(gen)
```

原因（`router.go:43-45` 的 doc）：
- 部分子系统初始化依赖 server 的 logger 或 router 已存在（如 P0 探针）
- 避免 main.go 顶部 `NewServer(...)` 调用变成一个 30-参数的怪物
- 单元测试可以只构造最小 Server

代价：handler 必须 nil-check（`if s.mcpGateway == nil { c.JSON(503, ...) }`）。
**这种空检在 handler 里随处可见**，不是疏忽是必然。

### Q5 — 限流：进程内 fallback vs Redis 分布式

`router.go:188-195`：

```go
if s.rdb != nil {
    redisRL := auth.NewRedisRateLimiter(s.rdb, "ratelimit:api", 10, time.Second, logger)
    s.router.Use(redisRL.GinMiddleware())
} else {
    rl := newRateLimiter(DefaultRateLimiterConfig(), logger)
    s.router.Use(rateLimiterMiddleware(rl))
}
```

- **生产**：Redis 已经是必备依赖（`main.go` 在 Redis ping 失败时 fatal），所以一定走 Redis 版
- **测试 / 极简部署**：rdb 为 nil → fallback 到 `middleware.go:140-188` 的本地 token-bucket（per-client-IP，5 分钟 GC）
- **Redis 版的"fail-open"语义**：Redis 错误时放行，而**不是**回退到内存版（`router.go:27-28` 注释明确）—— 这是用一致性换可用性

### Q6 — 为什么 `/api/v1/auth/token` 是匿名暴露 + 自我鉴权

```go
// router.go:220-222
authGroup := s.router.Group("/api/v1/auth")
authGroup.POST("/token", s.handleGenerateToken)
// 注意：authGroup 没有 .Use(AuthMiddleware) —— 鸡生蛋问题
```

token 端点必须**位于 AuthMiddleware 之外**，否则没人能拿到第一个 token。
所以 `handleGenerateToken` 内部自己鉴权（详见 §10 已知安全 bug）。

---

## 2. 依赖架构

```
┌─────────────────────── cmd/agent/main.go (DI) ───────────────────────┐
│                                                                       │
│   cfg ── redis ── sessionMgr                                          │
│      │      │       │                                                  │
│      │      │       └──┐                                              │
│      │      └─ jwtMgr  │                                              │
│      │      └─ apiKeyStore                                            │
│      │                  │                                              │
│      ├─ llmClient ──┐   │                                              │
│      ├─ ragEngine ──┤   │                                              │
│      ├─ sandboxMgr ─┤   │                                              │
│      ├─ mcpGateway ─┤   │                                              │
│      ├─ pgStore ────┤   │                                              │
│      │              ▼   ▼                                              │
│      └────────► orchestrator  ◄──── skillRegistry                      │
│                       │                                                │
│                       ▼                                                │
│            api.NewServer(orch, sess, logger, opts)                     │
│            apiServer.Set{Indexer,Skill,Store,MCP,Workspace,Gen,P0...}  │
│                       │                                                │
│                       ▼                                                │
│                 http.Server (Gin engine)                              │
└───────────────────────────────────────────────────────────────────────┘
```

**关键 main.go 行号**（DI 拓扑参考）：
- L488 `api.NewServer` 构造
- L504 `SetIndexer`（含 `WithStore` 注入 PG）
- L526-565 文件 watcher → 增量索引绑定（详 `15_indexer_repomap.md`）
- L572-574 SkillRegistry 注入到 orch + api
- L578-580 `SetStore` + 启动加载持久化动态工具（L583-602）
- L607 `SetMCPGateway`
- L613-627 Workspace + Generator 一起注入
- L631-640 Tree-sitter parser 绑 orch/rag/repomap
- L644-676 PTY manager 绑 orch
- L679-695 LSP client 绑 orch
- L697-702 包到 `http.Server`
- L728-736 启动 ListenAndServe
- L738-757 信号驱动的优雅关闭（context 超时来自 `cfg.Server.ShutdownTimeout`）

⚠️ **生命周期空白**：
- 文件 watcher 用 `context.Background()` 启动（L565）—— **优雅关闭时未被取消**，详 `15_indexer_repomap.md` §10
- ✅ ~~`handleChat` 的 detached goroutine 在 shutdown 时未 drain~~（2026-06-04 修复 — `internal/api/router.go::Server.inflight` + `Server.Drain()`，`cmd/agent/main.go` 在 `httpServer.Shutdown` 之后调用；详见 §10.2.9 与 `30_recent_improvements.md::F`）

---

## 2.5 数据流总览

```text
═══════════════ REST 请求路径（chat 同步） ════════════════════════════

curl POST /api/v1/chat
       │
       ▼
[middleware 链 — 见 §3.3]
       │
       ▼
handleChat (handlers.go:109)
       │ 1. ShouldBindJSON → models.ChatRequest
       │ 2. sessionMgr.Get(c.Request.Context(), sid)   ← 校验 session 存在
       │ 3. agentCtx = context.WithTimeout(Background, 10*time.Minute)
       │ 4. go func{ orchestrator.ProcessMessage(agentCtx, ...) → resultCh }
       │ 5. select { resultCh: 写 200; c.Request.Context().Done(): 不 cancel agentCtx }
       ▼
gin writes JSON OR 客户端断 → 后台继续

═══════════════ SSE 流（react-stream）════════════════════════════════

curl POST /api/v1/chat/react-stream
       │
       ▼
handleChatReactStream (handlers.go:249)
       │ Set Content-Type: text/event-stream, Cache-Control: no-cache
       │ Send "session" event with sessionID
       │
       │ writeMu sync.Mutex; sendEvent := mu.Lock + sendSSEEvent + mu.Unlock
       │ go runSSEHeartbeat(c.Request.Context(), c.Writer, &writeMu, 25*time.Second)
       │   ↑ 25s 周期写业务 ping 事件 `data: {"type":"ping","ts":<ms>}\n\n`,
       │     既能防 server.write_timeout: 600s 撕扯合法长任务连接,
       │     也能触发前端 fetch ReadableStream onByte 回调重置 90s watchdog
       │     `lastEventAt`(P1 改造前用 `: ping\n\n` 注释行,某些 proxy/fetch
       │     实现不向上抛字节,导致 watchdog 误判 "网络静默超时")。
       │     前端 ChatPage.tsx 显式 drop type=ping,不入 trace UI。
       │
       │ eventCh, err := orchestrator.ProcessMessageStreamFull(c.Request.Context(), ...)
       │   ↑ 形参在 orchestrator 内重命名为 reqCtx；业务 ctx 是函数级新建的
       │     workCtx (background + 30min)，客户端断不传染业务。详见 09_orchestrator §4.1
       │
       │ for event := range eventCh {
       │     sendEvent(event)   // writeMu 与心跳串行化，禁止 SSE frame 字节交错
       │ }
       ▼
SSE 事件类型（来自 orchestrator）:
  session / intent / step_start / thinking / tool_call / tool_result /
  tool_progress / rag_context / approval_request /
  verification_warning / fix_loop_abort / message / error / done /
  llm_call_started / llm_call_progress / llm_call_completed / ping

`ping` 由 `runSSEHeartbeat` 直接写入,不经 orchestrator channel —— 仅
作为业务级心跳重置前端 watchdog,前端不渲染。

特殊事件载荷（`ReactStreamEvent.Metadata` 为 `json.RawMessage`，透传到前端）:

| 事件 | Metadata 字段 / Content | 来源 / 用途 |
|---|---|---|
| `tool_result`（edit_file/apply_diff） | `{path, diff_preview, rolled_back, lint_errors[]}` | `editResultToMetadata`（`file_tools.go`）。前端 ChatPage 用 `diff_preview` 渲染 unified diff 块（行级 +/- 着色，10 000 字符上限+Show more） |
| `verification_warning` | `{score, issues[], reasoning, retrying}` | `decideVerificationFollowup`（`verification.go`）。低分时 emit，`retrying=true` 表示 orchestrator 将自动把 critique 作为 user 反馈再跑一轮（每 task 限 1 次，仅 stream 路径） |
| `llm_call_started` / `_progress` / `_completed` | Content 是 JSON：`{attempt,messages,tools}` / `{attempt,elapsed_ms}` / `{attempt,elapsed_ms,err}` | `callLLMWithProgress`（`react_core.go`）。包住每次非流式 ChatCompletion，让 finalize 阶段长达 20+ 分钟的 LLM 调用也有 3s 周期心跳，消除 UI 假死。详见 09_orchestrator §Q4.5 |

新增 metadata 字段须同步前端 `code_agent_ui/src/types/index.ts` 中的 `ToolResultMetadata` / `VerificationWarningMetadata` / `ReactStreamEventType`。

═══════════════ WebSocket 升级 ══════════════════════════════════════

GET /api/v1/ws  (HTTP/1.1 Upgrade: websocket)
       │
       ▼
handleWebSocket (handlers.go:468)
       │ upgrader.Upgrade —— CheckOrigin 见 §6.1
       │ sessionMgr.Create(c.Request.Context(), "ws-user", "")
       │ conn.WriteJSON({type:"connected", session_id:...})
       │
       │ for {
       │     ReadMessage → 反序列化 wsMsg
       │     switch wsMsg.Type:
       │       "chat":    orchestrator.ProcessMessage → WriteJSON response
       │       "approve": orchestrator.HandleApproval → WriteJSON response
       │       default:   WriteJSON error
       │ }

═══════════════ 文件浏览/编辑 ════════════════════════════════════════

GET  /api/v1/workspaces/:id/tree          → buildFileTree (递归)
GET  /api/v1/workspaces/:id/files?path=   → workspaceMgr.ReadFile
PUT  /api/v1/workspaces/:id/files         → workspaceMgr.WriteFile
DELETE /api/v1/workspaces/:id/files       → ⚠️ 直接 os.Remove（不走 manager！见 §10）
POST /api/v1/workspaces/:id/directories   → workspaceMgr.MkdirAll
GET  /api/v1/workspaces/:id/download      → workspaceMgr.Archive(tar.gz)

═══════════════ HMAC Webhook ════════════════════════════════════════

POST /api/v1/webhooks/mcp-callback
POST /api/v1/webhooks/ci-callback
       │
       ▼
hmac.GinMiddleware()  ← X-Signature-256 / X-Timestamp 校验，见 18_auth_security
       │
       ▼
handler（仅记录、未来路由回 Temporal workflow）
```

---

## 3. 中间件栈

### 3.1 装配代码（router.go:187-203）

```go
func (s *Server) setupMiddleware(logger *zap.Logger) {
    // 1. 限流（rdb != nil → Redis，否则 in-mem）
    if s.rdb != nil {
        s.router.Use(redisRL.GinMiddleware())
    } else {
        s.router.Use(rateLimiterMiddleware(rl))
    }
    // 2-7
    s.router.Use(recoveryMiddleware(logger))
    s.router.Use(requestIDMiddleware())
    s.router.Use(tracing.GinMiddleware("code-agent"))
    s.router.Use(metricsMiddleware())
    s.router.Use(s.loggingMiddleware())
    s.router.Use(s.corsMiddleware())
}
```

### 3.2 实际中间件顺序（Gin 栈语义，外到内）

由于 Gin 的 `Use()` 是 append，调用顺序 = 执行顺序（外层先 enter，handler 后 exit 先）：

```
请求进来 ──► rateLimit ──► recovery ──► requestID ──► tracing ──► metrics ──► logging ──► CORS ──► AuthMW(可选) ──► handler
```

### 3.3 ⚠️ doc.go 的中间件顺序描述与代码相反

`doc.go:18-19` 注释写的是：

> `recovery → requestID → tracing → metrics → rateLimit → logging → CORS`

但代码（`router.go:189-203`）实际是 **限流在最外层**（先于 recovery）。

**影响**：限流中间件本身 panic（理论上不会，但 Redis EVAL 边界场景理论存在）就**没有 recovery 兜底**——会向上冒泡到 Gin 的内置 recovery（如果有）。

**结论**：注释错的，**代码是源头**。这是一个 P2 项（要么改注释要么调换顺序）。

### 3.4 各中间件职责（middleware.go）

| 中间件 | 行号 | 关键行为 |
|---|---|---|
| `requestIDMiddleware` | L66-76 | 读 `X-Request-ID` 头，无则 UUID v4 生成；c.Set("request_id") + 写回响应头 |
| `metricsMiddleware` | L80-96 | 用 `c.FullPath()`（路由模板，避免 :id 爆 cardinality）；记 status code 因此必须在 auth 之后 |
| `rateLimiterMiddleware` | L190-206 | per-client-IP token-bucket；超限 `429 RATE_LIMITED` |
| `recoveryMiddleware` | L210-229 | recover panic → 500 `INTERNAL`，把 request_id 一起返回 |
| `loggingMiddleware` | router.go:325-336 | `c.Next()` 完后记 `method/path/status/ip` |
| `corsMiddleware` | router.go:341-362 | Origin 在 `allowedOrigins` 或 same-origin 才发 Allow-Origin 头；OPTIONS → 204 |
| `tracing.GinMiddleware` | 外部包 | OTel span（spanName=路由模板） |

### 3.5 token-bucket 设计要点（middleware.go:117-138）

```go
type tokenBucket struct {
    tokens     float64       // 当前可用 token
    maxTokens  float64       // 桶容量 = BurstSize (默认 20)
    refillRate float64       // 填充速率 = RequestsPerSecond (默认 10/s)
    lastRefill time.Time
}

func (b *tokenBucket) allow() bool {
    elapsed := time.Since(b.lastRefill).Seconds()
    b.tokens += elapsed * b.refillRate
    if b.tokens > b.maxTokens { b.tokens = b.maxTokens }
    b.lastRefill = now
    if b.tokens >= 1 { b.tokens--; return true }
    return false
}
```

- **Burst 20**：允许短时 20 个突发，之后稳态 10 qps
- **per-IP**：`rl.buckets[clientIP]`（`middleware.go:158`）
- **后台 GC**：5 分钟没访问的 IP 桶清掉（避免 map 无限增长）
- **不是分布式版**：N 个副本各管各的桶，实际上限 N × 10 qps

---

## 4. 路由表（router.go:206-322）

### 4.1 完整路由清单

| 方法 | 路径 | 鉴权 | 角色要求 | Handler |
|---|---|---|---|---|
| GET | `/healthz` | 匿名 | — | `handleHealthz` |
| GET | `/readyz` | 匿名 | — | `handleReadyz`（探 Redis + 可选 PG） |
| GET | `/metrics` | 匿名 | — | `promhttp.Handler()` |
| POST | `/api/v1/auth/token` | **自我鉴权** | X-Admin-Secret 非空 OR admin JWT | `handleGenerateToken` |
| POST | `/api/v1/chat` | 可选 | any | `handleChat`（detached） |
| POST | `/api/v1/chat/stream` | 可选 | any | `handleChatStream`（SSE） |
| POST | `/api/v1/chat/react-stream` | 可选 | any | `handleChatReactStream`（SSE） |
| POST | `/api/v1/chat/:session_id/interrupt` | 可选 | any | `handleInterrupt` |
| POST | `/api/v1/sessions` | 可选 | any | `handleCreateSession`（自动建 workspace） |
| GET | `/api/v1/sessions/:id` | 可选 | any | `handleGetSession` |
| GET | `/api/v1/sessions/:id/workspace` | 可选 | any | `handleGetSessionWorkspace` |
| DELETE | `/api/v1/sessions/:id` | 可选 | any | `handleDeleteSession` |
| POST | `/api/v1/sessions/:id/messages/:msg_id/pin` | 可选 | any | `handlePinMessage` |
| POST | `/api/v1/sessions/:id/messages/:msg_id/unpin` | 可选 | any | `handleUnpinMessage` |
| POST | `/api/v1/tasks/:id/approve` | 可选 | **admin\|dev** | `handleApproval` |
| POST | `/api/v1/index` | 可选 | **admin\|dev** | `handleIndexRepository` |
| POST | `/api/v1/projects/generate` | 可选 | **admin\|dev** | `handleGenerateProject` |
| POST | `/api/v1/projects/generate/stream` | 可选 | **admin\|dev** | `handleGenerateProjectSSE` |
| GET | `/api/v1/projects/:id/status` | 可选 | **admin\|dev** | `handleGetProjectStatus` |
| POST | `/api/v1/mcp/servers` | 可选 | **admin\|dev** | `handleAddMCPServer` |
| DELETE | `/api/v1/mcp/servers/:name` | 可选 | **admin\|dev** | `handleRemoveMCPServer` |
| GET | `/api/v1/mcp/servers` | 可选 | **admin\|dev** | `handleListMCPServers` |
| POST | `/api/v1/skills` | 可选 | **admin\|dev** | `handleAddSkill` |
| DELETE | `/api/v1/skills/:name` | 可选 | **admin\|dev** | `handleRemoveSkill` |
| GET | `/api/v1/skills` | 可选 | **admin\|dev** | `handleListSkills` |
| GET/PUT/DELETE | `/api/v1/workspaces/:id/...` | 可选 | any | workspace_handlers.go |
| GET | `/api/v1/tools` | 可选 | any | `handleListTools`（统一 builtin+MCP+Skill） |
| POST/GET/DELETE | `/api/v1/tools/dynamic/*` | 可选 | **admin\|dev** | dynamic_tool_handlers.go |
| GET/POST | `/api/v1/debug/p0[/*]` | **匿名！** | — | p0_debug_handlers.go |
| GET | `/api/v1/ws` | 可选 | any | `handleWebSocket` |
| POST | `/api/v1/webhooks/mcp-callback` | **HMAC** | — | `handleWebhookMCPCallback` |
| POST | `/api/v1/webhooks/ci-callback` | **HMAC** | — | `handleWebhookCICallback` |

### 4.2 "可选 / any" 的含义

`v1 := s.router.Group("/api/v1")` 之后只在 `s.authEnabled && s.jwtMgr != nil` 才挂 `auth.AuthMiddleware`。

- **authEnabled = true**（生产）：所有 `/api/v1/*` 需要 Bearer JWT 或 X-API-Key；`/auth/token` 走 `authGroup` 独立无鉴权
- **authEnabled = false**（开发/CI）：完全匿名通过

### 4.3 ⚠️ `/api/v1/debug/p0` 没有任何鉴权

```go
// router.go:303-309
debugGroup := v1.Group("/debug/p0")
debugGroup.GET("", s.handleP0Aggregate)
debugGroup.GET("/schema", s.handleP0SchemaSnapshot)
debugGroup.GET("/spec-cache", s.handleP0SpecCacheGet)
debugGroup.POST("/spec-cache", s.handleP0SpecCachePut)
debugGroup.GET("/spec-cache/query", s.handleP0SpecCacheQuery)
```

debugGroup **没有** `RequireRole`。
- 走 AuthMiddleware（authEnabled=true 时）即放行
- authEnabled=false 时**完全公开**
- `handleP0SchemaSnapshot` 会返回**所有工具 schema**——含可能敏感的工具元数据

文件顶部注释明确这是"内部测试 / Prometheus 抓取 / 开发自检"，**生产建议通过网关限制**。
**这是一个 P1 风险**——未来如果生产开 authEnabled=false，debug 接口会暴露 schema。

---

## 5. Handler 行为细节

### 5.1 `handleChat`（同步 + detached，handlers.go:109）

```
1. ShouldBindJSON → ChatRequest
2. 校验 sessionID != ""
3. sessionMgr.Get(req ctx) 校验 session 存在 → 404 if not found
4. agentCtx := WithTimeout(Background, 10*time.Minute)
5. go orchestrator.ProcessMessage(agentCtx, ...) → resultCh
6. select:
   - <-resultCh:                 c.JSON(200, response)
   - <-c.Request.Context().Done(): 不 cancel agentCtx；只 log，让 goroutine 跑完
```

**注意**：`OutputFormat` 字段透传到 orchestrator（控制 markdown/plain）。

### 5.2 `handleChatStream`（OPT-1，handlers.go:174）

```
1. sessionID 为空 → 自动 sessionMgr.Create()
2. SSE 头：Content-Type: text/event-stream, Cache-Control: no-cache, Connection: keep-alive, Transfer-Encoding: chunked
3. SSEvent("session", sessionID)
4. orchestrator.ProcessMessageStream(c.Request.Context(), ...) → streamCh
5. for chunk := range streamCh: SSEvent("message", json.Marshal(chunk))
6. SSEvent("done", {})
```

**用的是 `c.Request.Context()`**：客户端断 → orchestrator 也停。

### 5.3 `handleChatReactStream`（handlers.go:249）

与 `handleChatStream` 同结构，但调 `ProcessMessageStreamFull`，事件类型由 orchestrator 决定。

### 5.3.1 Replay / Resume：`stream/status` + `stream/resume` + 合成 `done` 兜底

短端点 `GET /api/v1/chat/react-stream/status` / `GET /api/v1/chat/react-stream/resume` 复用 `streamReplayFollow` 通用流程：从 `StreamCache.Replay` 拿历史快照、`Status` 判任务是否还在跑、`Follow` XREAD BLOCK 续追新事件。前端断线、tab 重开、SessionStart 重连都走这条路径恢复 SSE。

**问题**：在"task 已 `MarkDone` 但 `done` 事件尚未或永远不会进 Stream"的竞态/截断窗口里，朴素 Replay 会把没有 `done` 的 history 直接吐给客户端然后退出 ——SSE reader 永远等不到终态，UI 卡 spinner。

**对策**（`handlers.go: streamReplayFollow`，2026-06-07）：

| 触发点 | 条件 | 动作 | 合成事件 `reason` |
|---|---|---|---|
| Replay 后 | history 末尾不是 `done`/`error` 且 `Status` 报 not running | 主动 emit 一条合成 `done` | `synthesized_after_replay` |
| Follow 退出后 | Follow 通道关闭时 ctx 仍存活、整段会话从未见过终态 | 主动 emit 一条合成 `done` | `synthesized_after_follow` |

合成 `done` 的 `Data` 是 `{"reason": "synthesized_after_..."}`，让审计/排查能区分真假终态。客户端对 `done` 幂等，已有 `done` 时不会重复合成（用 `hasTerminal` 守门）。

测试覆盖在 `internal/api/stream_replay_followup_test.go`：
- `TestStreamReplayFollow_NoTerminalInHistory_SynthesizesDone` — 校验合成路径
- `TestStreamReplayFollow_TerminalInHistory_NoSyntheticDone` — 校验幂等不重复

为了让 `streamReplayFollow` 可测，handler 把 StreamCache 抽象成局部 `streamCacheReplayer` 接口，测试可以直接用 miniredis-backed 真实 StreamCache 验证，不依赖完整 Orchestrator 构造。

**status endpoint 返回字段(2026-06-07 起增加 `last_event_at_ms`)**:

```
GET /api/v1/chat/react-stream/status?session_id=<id>
200 {
  "running":          true|false,                    // 是否仍在跑
  "event_count":      <int>,                         // Redis Stream 总条数
  "last_event_at_ms": <unix_millis>,                 // 最末条事件写入毫秒戳;0 表示流空
  "task_id":          "<uuid>"                       // 仅 running=true 返回
}
```

`last_event_at_ms` 是 `StreamCache.LastEventAt` 解析 Redis Stream 末条 ID 的前段毫秒戳返回(Stream ID 格式 `1717843200000-0`),不需要服务端 TIME 调用。前端可在 watchdog 触发前据此判断"后端是否真活着":若 `running=true` 但 `last_event_at_ms` 已停滞数分钟,后端八成卡在 finalize LLM 调用上,UI 可给出更友好提示而非粗暴报错。

### 5.3.2 前端 UI 解锁不依赖 reader 自然退出(2026-06-07）

后端的 `done`/`error` 即使按时到达 Redis Stream，前端 SSE reader 仍可能因为浏览器 fetch 缓冲、Vite dev proxy、HTTP keep-alive 等中间层卡在 `body.getReader().read()` 长达数十秒。期间 `consumeReactStream` 不会自然结束，外层 `finally` 块里 `setLoading(false)` / `reconcileFinalMessage` 永远不跑，UI spinner 永转——观感与"后端没回报告"完全相同。

**约定**（`code_agent_ui/src/pages/ChatPage.tsx`）：

| 改动点 | 规则 |
|---|---|
| `consumeReactStream` | 收到 `done`/`error` 事件时立即调上层 `onTerminal` 回调；不等 reader 退出 |
| 两条 `runWithSilentWatchdog` 调用点 | 在 `onTerminal` 里调 `controller.abort()`，强制 reader 抛 `AbortError` 走 catch+finally |
| `ReActTrace` `finalMessage` | 反向 for 取**最后**一条 `type:"message"` 而非 `find`(首条)——fallback resume 启动前会插入"🔄 网络静默超时…"告警，若 finalMessage 取首条，真 finalize 报告永远被遮蔽 |
| `ReActTrace` loading indicator | 关闭条件叠加 `!finalMessage`：`loading && !isDone && !finalMessage`——即使 done event 丢失，只要 `reconcileFinalMessage` 已从 PG 补出 final message，spinner 也立即关掉 |
| 告警/重连提示 step type | 一律 `type:"thinking"`，不再使用 `type:"message"`——避免污染 finalMessage 检测 |
| `useEffect` resume 失败兜底 | 一次 resume 静默 90s 后自动二次 resume，对称于 `runReactStream` 的双层 fallback |
| F1 — PG polling 兜底 | watchdog 触发 + fallback resume 也超时后,启 `pollSessionUntilFinal`:每 5s 调 `GET /api/v1/sessions/:id`,2 分钟内拉到 `timestamp > entry.createdAt` 的 assistant message 即推到 UI(silent recovery),否则才报错"已尝试 2 分钟 PG 兜底,请刷新页面" |
| F3 — reconcile 幂等 | `reconcileFinalMessage` 三重 guard:① `!Number.isFinite(createdAt)`/<=0 拒绝 ② `reconciledEntryIdsRef`(以 createdAt 为 key 的 Set)同 entry 只 reconcile 一次 ③ `messagesToEntries` 给历史 entry 派生 `createdAt = msg.timestamp`,防 0-createdAt 击穿 |
| F2 — error step 软化 | 同 entry 已有 `type=message` 时,所有 `type=error` step 标记 `softHidden=true`,UI 淡灰显示并加 "(已自动恢复)" 后缀,不再红字粘屏 |

### 5.4 `handleInterrupt`（handlers.go:407）

```
POST /api/v1/chat/:session_id/interrupt
Body: { "type": "cancel|new_message", "new_message": "..." }
↓
orchestrator.InterruptSession(sessionID, InterruptSignal{...})
→ 200 if 有 active task, 404 otherwise
```

详见 `09_orchestrator.md` 的中断机制。

### 5.5 `handleCreateSession`（handlers.go:300）

```
POST /api/v1/sessions
Body: { "user_id": "...", "project_id": "..." }
↓
1. sessionMgr.Create()
2. 若 workspaceMgr != nil → workspaceMgr.CreateForSession(sess.ID, sess.ID, "session-" + sess.ID[:8])
   (失败仅 Warn 不致命，session 仍正常返回)
3. 返回 { session_id, workspace_id, created_at }
```

### 5.6 `handleReadyz`（handlers.go:76）

```
1. sessionMgr.Ping(req ctx) → checks["redis"]
2. pgHealthPing != nil → checks["postgres"]
3. allReady ? 200 ready : 503 not_ready
```

**注意**：Qdrant、Temporal、LLM API、MCP 服务器**都未纳入 readyz**。
仅检 Redis + (可选) PG。完整健康检查应通过 `/metrics` + 外部黑盒探针。

### 5.7 `handleWebhookMCPCallback / handleWebhookCICallback`（handlers.go:564, 587）

HMAC 中间件已校验通过才能进 handler。**handler 当前仅 log + 200**——
真正的"路由回 Temporal workflow"是 TODO（`handlers.go:582` 注释）。

### 5.8 `handleIndexRepository`（handlers.go:611, OPT-21）

```
POST /api/v1/index
Body: { "repo_path": "/abs/path", "project_name": "..." }
↓
若 indexer 已注入 → go indexer.IndexRepositoryAny(WithTimeout(Background, 30min), ...)
返回 202 Accepted（不等索引完成）
```

**注意 30 分钟超时是 background ctx**——HTTP 请求结束不影响。

### 5.9 `handleListTools`（mcp_skill_handlers.go:176）—— 防 nil 防重统一出口

```
1. tools = make([]toolInfo, 0, 16)   // 保证 JSON 序列化是 [] 不是 null
2. 若 orchestrator != nil: 走 GetAvailableTools() 一次性拿全（含 builtin + MCP + skill）
3. 若 orch == nil 但 mcpGateway != nil: 单独走 MCP（兜底分支）
4. 若 skillRegistry != nil: 走 Snapshot 路径，把 ETag 写到 X-Tools-Etag 响应头
5. seen map 去重
```

**X-Tools-Etag**：暴露 skill registry 的 ETag，前端/Anthropic prompt cache 可据此精确判断需不需要重传 tool schema（P0-1 优化的 HTTP 暴露）。

### 5.10 `handleGenerateToken`（auth_handlers.go:22）

```
POST /api/v1/auth/token  (在 authGroup，不走 AuthMiddleware)
Body: { user_id, tenant_id, role, email }
↓
1. jwtMgr == nil → 503
2. 读 X-Admin-Secret 头 + 读 c.Get("claims")
3. if adminSecret == "" && (claims == nil || claims.Role != admin):
       → 403 "token generation requires admin privileges"
4. 校验 role ∈ {admin, dev, readonly, service}
5. jwtMgr.GenerateToken(...) → 200 { token, user_id, role }
```

⚠️ **§10.1 标记的 P0 安全 bug 入口在第 3 步**——条件只检查"非空"。

---

## 6. WebSocket 升级

### 6.1 Origin 白名单（handlers.go:440-465）

```go
var allowedOrigins = map[string]bool{
    "http://localhost:3000":  true,
    "http://localhost:5173":  true,
    "http://localhost:8080":  true,
    "https://localhost:3000": true,
}

CheckOrigin: func(r *http.Request) bool {
    origin := r.Header.Get("Origin")
    if origin == "" { return true }                     // curl 等非浏览器放行
    if allowedOrigins[origin] { return true }           // 配置白名单
    if origin == "http://"+r.Host || origin == "https://"+r.Host { return true }  // 同源
    return false
}
```

**注意**：`allowedOrigins` 是**包级硬编码 map**——CORS 的 corsMiddleware 也复用这同一个 map（router.go:346）。
**生产部署改前端域名时必须改源码**——P1，应该从 config 读。

### 6.2 消息协议（handlers.go:491-556）

```jsonc
// 客户端 → 服务端
{ "type": "chat",    "message": "用户输入" }
{ "type": "approve", "data": { ApprovalResponse JSON } }

// 服务端 → 客户端
{ "type": "connected",        "session_id": "..." }
{ "type": "response",         "task_id": "...", "message": "...", "state": "..." }
{ "type": "approval_required","approval": {...} }
{ "type": "approval_response","task_id": "...", ... }
{ "type": "error",            "error": "..." }
```

⚠️ **WS 连接所有 chat 都用 `c.Request.Context()`**（handlers.go:515）——
客户端断 → orchestrator 任务也终止。**与同步 `POST /chat` 的 detached 设计不一致**，
但这里合理（WS 断开就没人接收推送了）。

### 6.3 心跳

代码里**没有显式 ping/pong 心跳**——`doc.go` 注释提到的 60s 心跳是设计构想，未实现。
gorilla/websocket 默认 read/write 无超时，长闲连接靠 TCP keepalive。
**P2 项**：应该实现显式 heartbeat。

---

## 7. 工作区端点 vs orchestrator 工作区工具的差异

### 7.1 两条路径

| 路径 | 调用方 | 鉴权 | tenant 隔离 |
|---|---|---|---|
| `/api/v1/workspaces/:id/files` | 前端文件浏览器 | `resolveWorkspace`（id 解析） | **不严格** |
| `read_file / write_file / patch_file ...` 工具 | orchestrator ReAct 循环内 | sessionID → workspace | **严格**（见 `file_tools.go:935-1005`） |

### 7.2 `resolveWorkspace` 的"default" 兜底（workspace_handlers.go:259-289）

```go
if id == "default" || id == "active" {
    workspaces := s.workspaceMgr.ListWorkspaces()
    if len(workspaces) > 0 {
        return workspaces[0]   // ⚠️ 多租户场景下会返回别人的 workspace
    }
    // 不存在则自动建一个名为 "default" 的 workspace
}
```

⚠️ **跨租户泄漏风险**：在 authEnabled=true 的多租户环境下，用户 A 调
`/api/v1/workspaces/default/files?path=secret.txt`，
可能拿到用户 B 创建的 workspace 文件。

**对比 orchestrator 的修复**：`file_tools.go:935-971` 的 `ResolveSessionWorkspace`
**拒绝** `ListWorkspaces()[0]` fallback，明确返回 nil。
**API 层这里没修**——P0 安全风险（前端 UI 调用 `default` 是常态，等于完全失能 tenant 隔离）。

### 7.3 `handleDeleteWorkspaceFile` 绕过 manager（workspace_handlers.go:192-208）

```go
absPath := filepath.Join(ws.RootDir, filepath.Clean(relPath))
if !strings.HasPrefix(absPath, ws.RootDir+string(filepath.Separator)) {
    c.JSON(403, "path traversal not allowed")
    return
}
os.Remove(absPath)   // ⚠️ 直接调系统调用，绕过 workspaceMgr 的 safePath 三层防护
```

只做了 `HasPrefix` 一层检查，**未 EvalSymlinks**。
如果 workspace 内有 symlink 指向外部，`os.Remove` 会跟着链接删外面的文件。
**与 14_workspace.md §3 描述的三层防御不一致**——P1。

---

## 8. P0 调试探针端点（p0_debug_handlers.go）

| 端点 | 作用 |
|---|---|
| GET /debug/p0 | 聚合 schema/spec_cache/warm_pool/embedding_cache 四项指标 |
| GET /debug/p0/schema | 返回 skill.Registry.Snapshot，**支持 If-None-Match → 304** |
| GET /debug/p0/spec-cache | 返回 SpecCache 的 hits/misses/bypass/hit_rate |
| POST /debug/p0/spec-cache | **测试用**：注入一条 cache 记录（仅幂等白名单工具） |
| GET /debug/p0/spec-cache/query | 查 (session, tool, args) → hit/content |

### 8.1 探针注入方式（p0_debug_handlers.go:62）

```go
server.SetP0Probes(&P0Probes{
    SpecCache:  specCache,
    WarmPool:   warmPool,
    EmbedCache: api.EmbedCacheAdapterFunc(func() (uint64, uint64) {
        st := cache.Stats()
        return st.Hits, st.Misses
    }),
})
```

`EmbedCacheAdapterFunc` 是 `func() (uint64, uint64)` 的命名类型，
实现 `EmbedCacheProbe.EmbedStats`——避免 api 包反向依赖 rag 包。

### 8.2 ETag 协议（P0-1 优化的 HTTP 暴露）

```go
// p0_debug_handlers.go:144-147
if inm := c.GetHeader("If-None-Match"); inm == snap.ETag {
    c.Status(http.StatusNotModified)
    return
}
```

同 generation 内 schema 字节完全相同，客户端拿到 ETag 后下次 If-None-Match → 304。
这是 P0-1（"skill registry generation+ETag"）在 HTTP 层的体现。

---

## 9. 集成测试

### 9.1 整体方法

`integration_test.go` (627 行) 用 `httptest.NewServer` + `miniredis` + `zap.Observer`
跑端到端 HTTP 测试。**不 mock 单个 handler**，全链路验证。

### 9.2 P0 集成测试（integration_p0_test.go, 537 行）

专门测试 P0 优化项在 HTTP 服务运行时**真的生效**：
- Schema snapshot 跨请求字节相同（ETag 304）
- SpecCache 跨请求复用（Put 后 Query 命中）
- WarmPool 计数器正确（Created/Acquired/Recycled/Fallback）

**为什么需要端口级测试**：单元测试在 isolation 里证明 cache 命中，但 HTTP 服务里
注入路径出错就会让生产里走的是另一个实例。集成测试堵住这个漏洞。

---

## 10. 实现剖析与改进方向

### 10.1 当前优势

- ✅ 4 路 chat 端点覆盖 CLI/前端/双向所有场景
- ✅ Detached context 在同步端点上正确实施
- ✅ 晚绑定 Setter 解耦了构造顺序
- ✅ 限流自适应：rdb 有就分布式、没有就本地 token-bucket
- ✅ P0 探针端点支持端口级集成测试
- ✅ `/tools` ETag 暴露给前端做 prompt-cache 决策
- ✅ JSON 序列化保证 `[]` 不是 `null`（防 JS 端 `forEach` 崩）

### 10.2 已知风险与 bug

#### P0（生产风险）

1. **`handleGenerateToken` 的 X-Admin-Secret 空值绕过**
   `auth_handlers.go:31`：`if adminSecret == "" && ...` ——
   **只要客户端发任意非空字符串作为 `X-Admin-Secret` 头，就跳过 admin role 检查**。
   攻击者可以 `curl -H "X-Admin-Secret: anything"` 拿到任意角色 token。
   **修复**：要么删除 X-Admin-Secret 路径，要么从环境变量读真实 secret 并 `subtle.ConstantTimeCompare`。

2. **`resolveWorkspace` 的 `default/active` 跨租户泄漏**
   `workspace_handlers.go:272-278`：用 `default` 或 `active` 当 id，返回 `ListWorkspaces()[0]`——
   多租户下前端常用 `default` 显示"当前工作区"，会拿到第一个被创建的（很可能是别人的）。
   **修复**：从 JWT claims 提取 tenantID，过滤 ListWorkspaces 后返回。

3. **`handleDeleteWorkspaceFile` 绕过 manager 三层 safePath**
   `workspace_handlers.go:199`：仅 `HasPrefix` 校验，未 `EvalSymlinks`。symlink 攻击可删 workspace 外文件。

4. **`/api/v1/debug/p0/*` 无任何鉴权**
   生产 authEnabled=false 时全网开放，schema snapshot 会泄漏工具元数据。
   `handleP0SpecCachePut` 还允许写 cache（虽然有 IsIdempotentTool 限制）。

#### P1（设计缺陷）

5. **`doc.go` 描述的中间件顺序与代码相反**
   `doc.go:18` 写"recovery → requestID → ... → rateLimit"，代码是"rateLimit → recovery → ..."。
   修注释或调代码二选一。

6. **`allowedOrigins` 硬编码**
   `handlers.go:440`：localhost:3000/5173/8080 写死，部署到其他域名要改代码重编。
   应该从 config 读。

7. **WS 心跳未实现**
   `doc.go` 提的 60s ping/pong 心跳代码中不存在，长闲连接靠 TCP keepalive 不可靠。

8. **文件 watcher 用 Background ctx 启动**
   `main.go:565`：shutdown 时未 cancel，goroutine 泄漏。

9. ✅ ~~**`handleChat` 的孤儿 goroutine 也用 Background**~~
   2026-06-04 修复：`internal/api/router.go::Server.inflight` (`sync.WaitGroup`) + `Server.Drain(ctx)`，`cmd/agent/main.go` 在 `httpServer.Shutdown` 之后调用,bounded by `shutdownCtx`。`handleChat` 的 detached goroutine 通过 `trackInflight` 自动登记。

10. **`/readyz` 检查覆盖不全**
    仅 Redis + PG。Qdrant、Temporal、LLM、MCP 都没纳入——
    K8s readiness probe 通过不代表服务真能干活。

11. **错误响应格式不统一**
    部分 `{"error": "string"}`、部分 `{"error": "...", "code": "INTERNAL"}`，
    `request_id` 仅 recovery 中间件返回，其他地方不带。
    应该统一成 RFC 7807 Problem Details 或自定义 `ErrorResponse`。

#### P2（小优化）

12. WS 升级握手没有显式 client identity（curl 等无 Origin 直接放行）
13. SSE handler 没有显式 flush 间隔，依赖 Gin 默认行为
14. `/metrics` 没有鉴权（Prometheus 内网抓取惯例，但生产暴露公网会泄露指标）

### 10.3 优先级修复路线

```
本周必修（P0）:
  - auth_handlers.go:31 X-Admin-Secret 校验改 ConstantTimeCompare
  - workspace_handlers.go:272 ListWorkspaces[0] 改为 tenantID 过滤
  - workspace_handlers.go:199 改走 workspaceMgr.Delete 走三层 safePath
  - debug/p0 group 加 RequireRole(admin)

本月修（P1）:
  - 中间件 Use 顺序调成 recovery → ratelimit → ...
  - allowedOrigins 从 config 读
  - WS 实现 ping/pong heartbeat（30s 间隔）
  - main.go watcher.Start 传 shutdown ctx
  - ✅ ~~Detached goroutine pool + Shutdown 时 drain~~（2026-06-04 完成，见 §10.2.9）
  - /readyz 增加 Qdrant/Temporal/MCP

明季修（P2）:
  - 统一 ErrorResponse 格式
  - /metrics 加 basic auth（可选）
```

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **Gin** 而非 net/http | 中间件生态 + radix tree 路由 + Prometheus/OTel adapter |
| **4 路 chat 端点** | 同步/流式/ReAct/双向各覆盖一种场景 |
| **`POST /chat` detached context** | 客户端断不浪费 LLM token；结果落 session |
| **`POST /chat/stream` 用请求 ctx** | 客户端断了就没人看，继续跑也无意义 |
| **晚绑定 Setter** | 避免 NewServer 30 参数 + 测试可造最小 server |
| **rdb 有则 Redis 限流否则进程内** | 测试不强依赖 Redis；生产一致走 Redis |
| **Redis 限流 fail-open** | 一致性 < 可用性 |
| **`/api/v1/auth/token` 自我鉴权** | 鸡生蛋；必须无 AuthMW |
| **`allowedOrigins` 集中 map** | CORS + WS 共享同一份 |
| **`handleListTools` 强制 `[]` 不 `null`** | JS 端 `.forEach` 兼容 |
| **`X-Tools-Etag` 响应头** | 前端 prompt cache 精确 bust |
| **P0 探针端点** | 端口级证明 P0 优化在 HTTP 服务里真的生效，单元测试堵不住注入漏洞 |
| **HMAC 中间件而非 JWT** for webhooks | 外部系统只有共享 secret，没有 JWT 颁发能力 |

---

## 12. 后续演进

- [ ] **P0 4 个严重 bug 全修**（X-Admin-Secret / default workspace / 删文件绕 safePath / debug endpoint 鉴权）
- [ ] **请求级 detached goroutine pool**：可观测 + shutdown drain + 限制并发上限
- [ ] **统一 ErrorResponse**：`{error, code, request_id, details}` 全 handler 走同一辅助函数
- [ ] **`allowedOrigins` 从 config 读** + WS heartbeat
- [ ] **`/readyz` 全量探针**：Qdrant、Temporal、LLM ping、MCP 主要服务器
- [ ] **`/api/v1/debug/p0` 加 admin role 守卫**
- [ ] **OpenAPI/Swagger 生成**：当前完全手写表格，下版本用 swag 生成
- [ ] **WS 多 channel**：当前单一 conn 通过 type 字段路由消息；未来如果消息类型多了应该升级到多 subprotocol
- [ ] **SSE 端点 backpressure**：当前 `streamCh` 无缓冲，慢客户端会阻塞 orchestrator
- [ ] **Prometheus `/metrics` basic auth**：默认匿名是 K8s 内网惯例，外网暴露需要保护

---

## 13. 设计教训

1. **手动 DI 在 Go 项目里仍是首选**：Wire/dig/fx 等框架增加心智负担。`main.go` 760 行虽长但**顺序明确**，新人按行号读就能理解依赖图。**复杂度 < 代码量**。

2. **晚绑定 Setter 解耦构造顺序**：当依赖图存在循环或多阶段初始化时，setter 比"构造参数大数组"更优雅。代价是 handler 必须 nil-check——这是合理的可观测性税。

3. **同步与流式端点的 ctx 策略要不对称**：同步端点用 detached 让用户重试可恢复；流式端点用请求 ctx 因为客户端断了流就没意义。**不能一刀切**。

4. **限流应该最外层**：当前代码把限流放第一个 Use 是对的，但 doc 注释写错。
   **教训**：注释滞后于重构是常态，**代码是事实源**。

5. **`X-Admin-Secret` 这种"用值是否为空判断"的鉴权是经典错误**：必须用 `subtle.ConstantTimeCompare` 比对真实 secret，否则任何非空值都通过。这是 OWASP A07-2021 Identification and Authentication Failures 的教科书案例。

6. **`default` / `active` 这种"友好别名"是租户隔离的大杀手**：前端用着方便，后端永远要先 tenant 过滤再返回。**这种 API 设计应该被 lint 阻拦**。

7. **HTTP 接口的 `null` vs `[]`**：默认 nil slice 会 marshal 成 `null`，前端 `.forEach(undefined)` 立即崩。**统一在 handler 内 `make([]X, 0, cap)` 预分配是简单可靠的 fix**。

8. **集成测试比单元测试更能堵注入漏洞**：P0 探针端点 + integration_p0_test 的设计目标就是"证明这个对象真的是 HTTP 服务里跑的那个对象，不是测试里另起的"。这种"端口级"验证应该成为关键优化的标配。

9. **`doc.go` 描述与代码不符不是细节问题**：它会**主动误导**新维护者。要么删除，要么放进 `docs/rfc/` 并标注 "design only"。**真相只能有一份**。

10. **`allowedOrigins`、`require_approval_commands`、`api_keys.last_used`** 这类硬编码 / 死字段，单看每个都"小事"，加起来就是技术债复利。重构时一并清理。

---

下一篇：[`18_auth_security.md`](18_auth_security.md) —— JWT/APIKey/RBAC + HMAC webhook + CIDR egress ACL + 敏感词过滤的完整安全栈。
