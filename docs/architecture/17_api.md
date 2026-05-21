# 17 · HTTP/WS 接入层 `internal/api` + `cmd/agent/main.go`

> 代码：
> - `internal/api/router.go` (273) — Server 结构 + 路由注册 + 中间件装配
> - `internal/api/handlers.go` (565) — 核心业务 handler（chat / sessions / WS / approval / webhooks / index）
> - `internal/api/middleware.go` (186) — RequestID / Metrics / RateLimit / Recovery
> - `internal/api/auth_handlers.go` (65) — `POST /auth/token` 颁发
> - `internal/api/project_handlers.go` (144) — 项目生成（同步 / SSE / 状态查询）
> - `internal/api/mcp_skill_handlers.go` (203) — MCP 服务器 + Skill CRUD
> - `internal/api/workspace_handlers.go` (354) — 工作区文件浏览/编辑（已在 14_workspace 详述）
> - `internal/api/integration_test.go` (626) — 端到端集成测试
> - `cmd/agent/main.go` (361) — **整个系统的依赖注入（DI）入口**

---

## 1. 模块定位

**"把所有内部子系统包装成一个 HTTP / WebSocket / SSE 服务端，并在 `main.go` 里做一次性手动 DI。"**

职责分为三层：

| 层 | 负责什么 |
|---|---|
| **`cmd/agent/main.go`** | 构造所有组件（cfg → redis → session → llm → rag → sandbox → mcp → store → orchestrator → planner → skill → indexer → server），按**正确拓扑**连线 |
| **`api.Server`** | Gin 引擎 + 中间件栈 + 路由表 + handler 方法集合 |
| **Handlers** | 参数解析 → 调用业务（多半是 `orchestrator`）→ 序列化响应（JSON / SSE / WS） |

**不做**的事：

- 业务逻辑（那是 orchestrator 的活）；
- 认证原语（JWT 验证在 `internal/auth`，见 18_auth_security）；
- 审计 / 限流算法本身（只在中间件里调用）。

---

## 1.5 设计哲学：API 层的 5 个抉择

### Q1 — Gin vs net/http vs echo vs chi？

| 维度 | net/http | Gin | echo | chi |
|---|---|---|---|---|
| 性能 | ⭐⭐⭐⭐ | ⭐⭐⭐⭐⭐（radix tree 路由） | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ |
| 中间件生态 | 少 | 丰富 | 丰富 | 中 |
| 学习曲线 | 陡 | 平 | 平 | 平 |
| 二进制大小 | 小 | 中 | 中 | 小 |
| 路径参数 | 需自己 parse | 原生 | 原生 | 原生 |

**选 Gin 的理由**：
- 中间件（recovery / cors / limit）生态现成
- Prometheus / OpenTelemetry 均有 Gin adapter
- 前端团队熟悉（Go 社区事实标准之一）

**放弃 net/http 的理由**：简单路由写起来啰嗦 10×，中间件要手写。

### Q2 — 同步 vs 异步 vs SSE vs WebSocket 四种 chat 模式

**真实需求**：ReAct 可能跑 1-300 秒，用户能看到中间步骤。

**选项**：

| 模式 | 端点 | 适用场景 | 实现 |
|---|---|---|---|
| 同步 | `POST /chat` | 简单问答 / 脚本调用 | 阻塞直到完成 |
| SSE 流 | `POST /chat/stream` | token 级流式渲染 | `text/event-stream` |
| ReAct SSE | `POST /chat/react-stream` | 显示每步思考 | 事件类型化 |
| WebSocket | `GET /ws` | 双向实时（未来推通知） | gorilla/websocket |

**决策**：**四种都提供**。前端默认走 `react-stream`（最符合 Cursor 体验），
CLI / 集成脚本走同步 API。

### Q3 — Detached context 的反直觉选择

**问题**：客户端 60s 超时断开时，LLM 还在生成——后台任务该继续还是杀掉？

**选项**：
- (A) 用 `c.Request.Context()`：客户端断 → context cancel → LLM 停
- (B) 用 `context.Background()`：客户端断 → 后台继续，结果存 session

**决策**：(B) — 反直觉但务实。原因：
- 用户重新刷新后 `GET /sessions/:id` 能拿到完整结果
- LLM 调用费用已经付了，中途断掉浪费
- **注意**：这违反了 Go 标准"context 传递"惯例。需要额外的 shutdown
  机制 drain 这些孤儿 goroutine（P1 待办）

### Q4 — 错误响应格式

**选项**：
- (A) 裸 string: `{"error": "session not found"}`
- (B) 结构化: `{"error": {"code": "NOT_FOUND", "message": "..."}}`
- (C) RFC 7807 Problem Details

**现状**：**(A) 和部分 (B) 并存**（历史遗留），应朝 (B) 统一。

**正确姿势**（未来）：
```go
type ErrorResponse struct {
    Error      string `json:"error"`       // 人读
    Code       string `json:"code"`        // 机读
    RequestID  string `json:"request_id"`  // 对齐日志
    Details    any    `json:"details,omitempty"`
}
```

### Q5 — 中间件顺序的原则

```
recovery → requestID → tracing → metrics → rateLimit → logging → CORS
                                                 ↓
                                        认证组 auth.AuthMiddleware
                                                 ↓
                                        RBAC 组 RequireRole(admin)
                                                 ↓
                                               handler
```

**关键原则**：
- **recovery 最外层**：panic 必须被兜底翻成 500，否则 worker goroutine 死
- **requestID 尽早**：整个 request 生命周期共用一个 ID
- **metrics 在 auth 之后**：`status` label 要反映认证结果（401 是有效指标）
- **rateLimit 在 auth 之前**：阻拦恶意匿名流量，不消耗 auth 的 Redis 预算
- **logging 最后**：看到所有中间件处理后的最终 status

---

## 2. 依赖架构全景（含 main.go DI 拓扑）

```
                     cmd/agent/main.go
                            │
      ┌─────────────────────┼─────────────────────┐
      ▼                     ▼                     ▼
   config.Load        redis.NewClient      zap.NewProduction
      │                     │
      │        ┌────────────┼────────────┐
      │        ▼            ▼            ▼
      │    session     store.Store   auth.JWT +
      │   .Manager    (PG optional)   RedisRevocation
      │        │
      │        ▼
      ▼   llm.Client (primary + fallback router)
   rag.Engine  ─  Qdrant + Embedder + Reranker   (可选)
   sandbox.Manager (Docker)                      (可选)
   mcp.Gateway   (stdio / sse 多 server)          (可选)
   skill.Registry
   tools.Registry
   indexer.Indexer
   generator.Generator (项目脚手架生成)
      │
      ▼
  orchestrator.NewOrchestrator(llm, rag, sandbox, mcp,
                               session, store, audit, wsMgr, ...)
      │
      ▼
  api.NewServer(cfg, orch, sessionMgr, ...)
  server.SetMCPGateway(gw)
  server.SetSkillRegistry(sr)
  server.SetIndexer(idx)
  server.SetGenerator(gen)
      │
      ▼
  http.Server.ListenAndServe(addr, server.Handler())
```

**关键拓扑约束**：

- **Redis 必需**（session + revocation）；
- **Qdrant / Docker / MCP / Postgres** 全部**可选**，失败只 `Warn` 不 `Fatal` —— "降级可用" 是设计目标；
- **晚绑定**：MCP / Skill / Indexer / Generator 在 Server 构造后用 Setter 注入（防循环依赖）；
- `defer` 链正确：qdrantStore / sandboxMgr / mcpGateway / redis / logger 逆序关闭。

---

## 2.5 数据流总览

```text
┌──────────────────┐
│  Client (前端)   │
│  POST /api/v1/   │
│  chat/react-stream
└────────┬─────────┘
         │ HTTP Request
         ▼
┌─────────────────────────────────────────────────────────────┐
│                    Gin Middleware Chain                        │
│  recovery → requestID → tracing → metrics → rateLimit       │
│       → CORS → auth.AuthMiddleware → RequireRole            │
└──────────────────────────┬──────────────────────────────────┘
                           │ (ctx with userID, traceID, claims)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Handler: handleReactStream                                    │
│  ① 解析 ChatRequest (sessionID, message, workspaceID)       │
│  ② detached context (写超时不中断 ReAct 长任务)              │
│  ③ 设置 SSE headers: text/event-stream, no-cache            │
│  ④ 创建 flusher + eventChan                                 │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ orchestrator.ProcessMessageStreamFull(ctx, req, eventChan)   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │ ReAct Loop (每步产生事件):                            │   │
│  │  thinking → tool_call → tool_result → content        │   │
│  │  每个事件 → eventChan <- ReactStreamEvent            │   │
│  └──────────────────────────────────────────────────────┘   │
└──────────────────────────┬──────────────────────────────────┘
                           │ (events via chan)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ SSE Writer goroutine:                                        │
│  for event := range eventChan:                              │
│    fmt.Fprintf(w, "event: %s\ndata: %s\n\n", type, json)   │
│    flusher.Flush()                                          │
│  最终 event: done                                            │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌──────────────────┐
│  Client (前端)   │
│  EventSource     │
│  逐事件渲染 UI   │
└──────────────────┘


HITL 审批回调:
┌──────────────────────────────────────────────────────────────┐
│ POST /api/v1/tasks/:id/approve                               │
│  → handler → orchestrator.HandleApproval(taskID, approved)  │
│  → 唤醒被挂起的 ReAct goroutine (channel signal)            │
└──────────────────────────────────────────────────────────────┘
```

---

## 3. ★ Server 结构与中间件栈

### 3.1 `Server` (router.go:24)

```go
type Server struct {
    cfg          *config.Config
    router       *gin.Engine
    logger       *zap.Logger
    orchestrator *orchestrator.Orchestrator
    sessionMgr   *session.Manager
    jwtMgr       *auth.JWTManager
    apiKeys      *auth.ApiKeyStore
    authEnabled  bool
    hmac         *security.HMACGuard
    mcp          *mcp.Gateway       // SetMCPGateway 晚绑
    skills       *skill.Registry    // SetSkillRegistry 晚绑
    indexer      Indexer            // SetIndexer 晚绑
    generator    ProjectGenerator   // SetGenerator 晚绑
    toolReg      *tools.Registry    // 合并来自 orchestrator / mcp / skill 的工具视图
}

type Indexer interface {                 // 解耦：router 不知道具体 indexer 类型
    IndexRepository(ctx, repoPath, projectName) (*IndexStats, error)
}
```

**晚绑定的 Setter 模式** 的好处：`api.Server` 的构造函数只需要核心依赖（orch + sessionMgr + auth），其他特性包（MCP / Skill / Indexer / Generator）在 main.go 初始化完后再 `SetX()` 注入。避免循环依赖、也方便单元测试（不需要的 feature 直接不 Set）。

### 3.2 中间件栈（router.go:125，**严格按序**）

```
recoveryMiddleware        捕获 panic，转 500 + 日志
    │
requestIDMiddleware       注入 X-Request-ID，贯穿整条日志链
    │
tracing.GinMiddleware     OpenTelemetry span 绑定 HTTP 生命周期
    │
metricsMiddleware         Prometheus：method/path/status/duration
    │
rateLimiterMiddleware     Token Bucket 限流（按 client IP）
    │
loggingMiddleware         结构化访问日志（method/path/status/latency/reqID）
    │
corsMiddleware            开发 CORS（通配，生产应收紧）
```

**顺序很关键**：

1. **recovery 永远第一**：即使 requestID 还没注入，也能从 panic 中恢复；
2. **requestID 紧随其后**：后续所有 middleware 日志都能带上它；
3. **tracing 先于业务**：让 metric / log 都能挂到 span；
4. **rateLimit 放在 cors / logging 之前**：被限流的请求不浪费日志资源；
5. **cors 最后**：只影响响应头，顺序位置可以最后。

### 3.3 路由表（router.go:138）

**公开端点（无 auth）**：

| Method | Path | 说明 |
|---|---|---|
| GET | `/healthz` | 存活探测 |
| GET | `/readyz` | 就绪探测（会 ping Redis / PG） |
| GET | `/metrics` | Prometheus scrape |
| POST | `/api/v1/auth/token` | 颁发 JWT |

**需认证端点（`v1` 带 `AuthMiddleware`）**：

```
POST   /api/v1/chat
POST   /api/v1/chat/stream             (SSE 流式 - 纯文本)
POST   /api/v1/chat/react-stream       (SSE 流式 - ReAct 全事件)
POST   /api/v1/sessions
GET    /api/v1/sessions/:id
GET    /api/v1/sessions/:id/workspace
DELETE /api/v1/sessions/:id
GET    /api/v1/ws                      (WebSocket)
GET    /api/v1/tools                   (聚合所有工具列表)
```

**需 admin/dev 角色（RBAC）**：

```
POST   /api/v1/tasks/:id/approve       (HITL 授权)
POST   /api/v1/index                   (仓库索引)
POST   /api/v1/projects/generate       (项目脚手架)
POST   /api/v1/projects/generate/stream
GET    /api/v1/projects/:id/status
POST   /api/v1/mcp/servers             (新增 MCP server)
DELETE /api/v1/mcp/servers/:name
GET    /api/v1/mcp/servers
POST   /api/v1/skills                  (新增 skill)
DELETE /api/v1/skills/:name
GET    /api/v1/skills
```

**Workspace 组**（见 14_workspace）：

```
GET    /api/v1/workspaces
GET    /api/v1/workspaces/:id/tree
GET    /api/v1/workspaces/:id/files
PUT    /api/v1/workspaces/:id/files
DELETE /api/v1/workspaces/:id/files
POST   /api/v1/workspaces/:id/directories
GET    /api/v1/workspaces/:id/download
```

**Webhook 组（HMAC 保护）**：

```
POST   /api/v1/webhooks/mcp-callback    (MCP 外部回调)
POST   /api/v1/webhooks/ci-callback     (CI/CD 触发)
```

HMAC 而非 JWT：这些来自**外部机器调用**，用共享 secret + HMAC-SHA256 更合适。

---

## 4. ★ 三种 Chat 端点：同步 / SSE / ReAct-SSE

### 4.1 `POST /chat`（同步）

- 一次请求一次响应；
- 适合短任务、curl 调试、LLM 主备切换、无前端场景；
- handler 内：`orchestrator.ProcessMessage(ctx, sessionID, msg)` → `c.JSON(200, resp)`。

### 4.2 `POST /chat/stream`（SSE · 纯文本增量）

- 只流式返回 LLM 的 token delta；
- 适合"聊天气泡打字机"效果的简单场景；
- 每个 SSE event type=`token`，data 是文本片段。

### 4.3 `POST /chat/react-stream`（SSE · **ReAct 全事件流**，★）

**最核心的端点**。handler (handlers.go:198) 的逻辑：

```
1. 解析 req，若无 sessionID 则 sessionMgr.Create()
2. 设 SSE headers: text/event-stream + no-cache + keep-alive + chunked
3. 先推一个 type="session" 事件，前端拿到 session ID
4. eventCh := orchestrator.ProcessMessageStreamFull(ctx, sessionID, msg)
5. for event := range eventCh:
      sendSSEEvent(c, event)             # type 可能是：
                                         # - thought   LLM 思考 token 增量
                                         # - tool_call 即将调用工具
                                         # - tool_result 工具结果
                                         # - approval_needed  敏感操作挂起
                                         # - final     最终回答
                                         # - error
```

**为什么不用 WebSocket？**

| 场景 | 选择 |
|---|---|
| **服务端推流为主 + 单向** | **SSE** ✅（简单、自动重连、HTTP 友好） |
| 双向高频消息 | WebSocket |

Agent 的流式对话 **本质是服务端推**，SSE 完美契合。前端只需 `EventSource(url)` 即可。

### 4.4 `GET /ws`（WebSocket，备选）

handler (handlers.go:386) 提供 WS 端点，适合未来扩展"实时协作 / 多端同步"场景。当前实现和 SSE 端点并行，前端可选其一。

---

## 5. HITL 授权回调：`POST /tasks/:id/approve`

handler (handlers.go:335) 流程：

```
1. 解析 body: {"approved": bool, "user": string}
2. 调 orchestrator.HandleApproval(taskID, approved, userID)
   - 内部：更新 approvals 表 status=approved/rejected
   - 唤醒挂起的 reactLoop 继续执行（或终止）
3. 写审计日志 (audit.Log)
4. 返回 {"status": "resumed" | "aborted"}
```

**RBAC 保护**：`auth.RequireRole(RoleAdmin, RoleDev)` 中间件 —— 只有 admin / dev 能授权高危操作。

---

## 6. 中间件实现要点（middleware.go）

### 6.1 Token Bucket 限流（L59-L165）

```go
type tokenBucket struct {
    tokens, maxTokens, refillRate float64
    lastRefill time.Time
}

allow():  // 懒加款 / lazy refill
    elapsed := now - lastRefill
    tokens += elapsed * refillRate
    if tokens > maxTokens: tokens = maxTokens
    lastRefill = now
    if tokens >= 1: tokens--; return true
    return false
```

**三个关键设计**：

1. **Per-IP 隔离**：`map[string]*tokenBucket`（key = `c.ClientIP()`）；
2. **懒加 refill**（不用 goroutine 定时器）：下一次 `allow()` 时根据时间差补齐。省大量后台 ticker；
3. **后台清理**（cleanupLoop）：每 5 min 清理超过窗口未用的 bucket，防止 IP 爆炸内存。

**默认参数**：10 rps、burst 20、5 min 清理窗口。生产下可从 config 注入。

### 6.2 `requestIDMiddleware` (L23)

```
X-Request-ID ← 客户端传入的优先，否则 uuid.New()
Set to context + response header
```

所有日志用 `c.GetString("request_id")` 取同一 ID → 一次请求贯穿所有子系统日志可追踪。

### 6.3 `recoveryMiddleware` (L167)

**不要用 gin 默认的 `gin.Recovery()`**：它不 log 到 zap，也不带 requestID。自己写一个：

```go
defer func() {
    if r := recover(); r != nil {
        logger.Error("panic recovered",
            zap.Any("error", r),
            zap.String("request_id", c.GetString("request_id")),
            zap.ByteString("stack", debug.Stack()),
        )
        c.AbortWithStatusJSON(500, ...)
    }
}()
```

### 6.4 `metricsMiddleware`

```go
defer func() {
    duration := time.Since(start).Seconds()
    metrics.HTTPRequestDuration.WithLabelValues(
        c.Request.Method, c.FullPath(), strconv.Itoa(c.Writer.Status()),
    ).Observe(duration)
}()
```

注意用 `c.FullPath()`（路由模板如 `/api/v1/sessions/:id`），而非 `c.Request.URL.Path`（含动态参数）—— 前者 label cardinality 可控，后者会爆表（每个 UUID 都是一条 metric）。

---

## 7. Webhook + HMAC

```go
webhooks := v1.Group("/webhooks")
webhooks.Use(s.hmac.GinMiddleware())    // 见 security/hmac.go
```

**HMAC 中间件**在 18 篇详讲，这里只说用法：
- 请求必须带 `X-Signature: sha256=<hex>`；
- 服务端用共享 secret 重算，常量时间比对；
- 防止重放：要求 `X-Timestamp` 在 ±5min 内 + 可选 nonce 缓存。

**两个 webhook**：

- `/webhooks/mcp-callback` —— 异步 MCP 服务器的"任务完成"回调；
- `/webhooks/ci-callback` —— CI/CD 构建完成触发后续 Agent 分析。

---

## 8. ★ `cmd/agent/main.go` 依赖注入全景（361 行）

### 8.1 启动八阶段

```
阶段 1: Logger (zap.Production)
阶段 2: Config.Load + Validate
阶段 3: Redis.NewClient + Ping      (必需)
阶段 4: Session.Manager
阶段 5: LLM.NewClient (primary + fallback router)
阶段 6: 可选组件（失败 Warn 不 Fatal）:
        ├─ Qdrant + RAG.Engine
        ├─ Docker.Sandbox
        ├─ MCP.Gateway
        └─ PostgreSQL Store
阶段 7: Orchestrator.NewOrchestrator(所有组件)
阶段 8: api.NewServer + SetX() 晚绑 + ListenAndServe
```

### 8.2 关键注入点

#### Embedding provider 自动选择（main.go:~90）

```
provider := cfg.RAG.EmbeddingProvider
if provider == "":
    if cfg.RAG.EmbeddingAPIKey && cfg.RAG.EmbeddingBaseURL: provider = "openai"
    elif cfg.LLM.Primary.APIKey: provider = "openai"   # 复用 LLM 凭据
    else: provider = "local"   # Hash-based fallback
```

**"开箱即用"**：没配 embedding 也能跑（local hash 模式 / 质量低但能工作）。

#### 可选组件的"软失败"

```go
qdrantStore, err := rag.NewQdrantStore(...)
if err != nil {
    logger.Warn("Qdrant not available, RAG disabled", zap.Error(err))
} else {
    // 构造 RAG engine
    defer qdrantStore.Close()
}
```

**同样策略**用于 sandbox / mcp / pg。效果：**用户只启 Redis + Agent 就能最小运行**，其他服务慢慢接入。

#### Postgres 必需的"打补丁"写法（main.go:~160）

```go
if cfg.Postgres.DSN != "" {
    pgCfg := &store.PostgresConfig{
        Host: "localhost", Port: 5432, ...  // 实际用不到，因为走 DSN
        MaxOpenConns: cfg.Postgres.MaxOpenConns,
    }
    // ... 构造 Store
}
```

当前 config 同时支持 "DSN 字符串" 和 "分字段"，main.go 里有兼容逻辑。**待清理**：统一只走 `NewStoreFromDSN`。

### 8.3 Graceful Shutdown

```go
srv := &http.Server{Addr: ..., Handler: server.Handler()}
go srv.ListenAndServe()

sig := make(chan os.Signal, 1)
signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
<-sig

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
srv.Shutdown(ctx)  // 等待 in-flight 请求完成
// defer 链自动关闭 redis / qdrant / sandbox / mcp / pg / logger
```

**30s 宽限期** 是关键：ReAct 循环可能还在 LLM 调用中，粗暴 kill 会让用户看到半截回答。

---

## 9. 集成测试（integration_test.go 626 行）

按端点组覆盖：

- `TestHealthz / TestReadyz` — 两个探针
- `TestAuthToken` — JWT 颁发 + 过期验证
- `TestChatFlow` — 创建 session → chat → 获取对话
- `TestChatReactStream` — SSE 流解析、事件顺序断言
- `TestApprovalFlow` — HITL：触发敏感操作 → 收到 approval_needed → POST approve → 恢复执行
- `TestWebhookHMAC` — 签名对 / 错两种
- `TestRateLimit` — burst + 拒绝码 429

**测试 SSE 的技巧**：

```go
resp, _ := http.Post(...)
reader := bufio.NewReader(resp.Body)
for {
    line, _ := reader.ReadString('\n')
    if strings.HasPrefix(line, "data: ") {
        parseEvent(line[6:])
    }
}
```

---

## 10. 前端 UI (`code_agent_ui/`)

- React + Vite + TypeScript；
- `src/api/client.ts` 封装了所有 REST + SSE；
- 主要页面：ChatPage / WorkspacePage / HealthPage / MCPPage / SkillsPage / ToolsPage / DashboardPage；
- SSE 消费：用 EventSource 订阅 `/chat/react-stream`，按 event.type 渲染"思考气泡 / 工具卡 / 授权对话框"。

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **Gin** 而非 net/http 原生 / echo / chi | 路由树 radix 极快；中间件模型成熟；社区广；生态对接 prometheus/pprof 简单 |
| **Server 结构体 + 方法 handler**（非独立函数） | 方便拿共享依赖（orch / logger / jwt）；避免全局变量 |
| **晚绑定 Setter**（`SetMCPGateway` 等） | 避免循环依赖；构造时只要核心三件套；可选特性 lazy-wire |
| **三个 chat 端点并存**（sync / stream / react-stream） | 照顾不同客户端（curl / 简易 UI / 完整 UI）；每种数据量和延迟需求不同 |
| **SSE 而非 WebSocket** 做流式 | 服务端推场景 SSE 更简单；自动重连；HTTP/1.1 & HTTP/2 友好；防火墙穿透强 |
| 中间件**栈序固定**（recovery → reqID → tracing → metrics → rate → log → cors） | 任一乱序都会导致：日志缺 reqID / panic 没 span / rate limit 之后才记日志浪费等问题 |
| **Per-IP 懒加 Token Bucket** | 无后台定时 refill 线程；O(1) allow() 复杂度；stale bucket 周期清理防 OOM |
| `c.FullPath()` 而非 `URL.Path` 做 metric label | label cardinality 可控（1000 条路由 vs 无穷 UUID） |
| Webhook 用 **HMAC** 而非 JWT | 机器到机器；共享 secret 简单；无 token 生命周期问题 |
| **RBAC 分组路由** (`approvalGroup.Use(RequireRole(...))`) | 声明式；新增端点只需选组；无需在 handler 里手写角色判断 |
| **所有可选组件软失败** | 开发 / 单机只启 Redis 就能跑；降级可用 |
| **30s Graceful Shutdown** | ReAct 循环可能正在 LLM 调用中途；粗暴 kill 破坏用户体验 |
| Embedding provider **自动选择**（no config 也能跑） | 新手开箱即用；生产再显式配置 |
| main.go **一次性手动 DI**（非 Wire / Fx） | 依赖项 < 20 个，手动写更清晰；新同事两分钟看懂；代价是文件略长 |

---

## 12. 后续演进

- [ ] **OpenAPI 自动生成**：当前有 `api/openapi.yaml` 手写，改成从代码注解生成（oapi-codegen / swag）；
- [ ] **WebSocket 升级**：当前 handleWebSocket 占位，完整实现实时消息推送 / 多端同步；
- [ ] **HTTP/2 + gRPC 路径**：给 Agent 间调用用更高效的二进制协议；
- [ ] **Rate Limit 从 config 读**：当前硬编码 10 rps；
- [ ] **Rate Limit per-token / per-user**：不仅按 IP，认证用户有独立配额；
- [ ] **CORS 收紧**：生产环境按 allow-list 配置 origin；
- [ ] **distributed tracing 全链路**：tracing 中间件已注入，但下游 LLM / Qdrant / Docker 调用还未全覆盖；
- [ ] **请求体大小限制**：防 DoS（`MaxMultipartMemory` 已用，JSON body 还需）；
- [ ] **Wire / uber-go/fx** 重构 DI：依赖项超过 30 时手动 DI 会吃力；
- [ ] **健康探针细化**：`/readyz` 分 hard deps（Redis）/ soft deps（Qdrant）的不同权重；
- [ ] **SSE 心跳**：长连接闲置时定期推 `: ping` 防 LB 断；
- [ ] **WebSocket 子协议**（如 `graphql-ws`）标准化；
- [ ] **K8s 探针对接 Prometheus**：alert 规则直接绑到 Server metrics；
- [ ] **集成测试用 testcontainers**：目前部分组件 mock，升级到真实 Redis / PG container；
- [ ] **Go 1.22 新路由语法**：考虑 stdlib mux（`{id}` 原生支持）替代 Gin。

---

## 13. 实现剖析与改进方向

### 一次 `POST /chat/react-stream` 的完整路径

```text
client                  Gin           Middleware chain        Handler      Orchestrator
  │                      │                  │                     │             │
  │── HTTP POST ────────▶│                  │                     │             │
  │                      │─ recovery ───────▶                     │             │
  │                      │─ requestID ──────▶ (set X-Request-ID)  │             │
  │                      │─ tracing ────────▶ (start span)        │             │
  │                      │─ metrics ────────▶ (start timer)       │             │
  │                      │─ rateLimit ──────▶ (token bucket)      │             │
  │                      │─ logging ────────▶                     │             │
  │                      │─ CORS ───────────▶                     │             │
  │                      │                  │── handler ─────────▶│             │
  │                      │                  │                     │             │
  │                      │                  │           ShouldBindJSON(&req)    │
  │                      │                  │           c.Header("Content-Type","text/event-stream")
  │                      │                  │           c.Flush()               │
  │                      │                  │                     │             │
  │                      │                  │           streamCh, errCh = orch.ProcessMessageStreamFull(
  │                      │                  │             agentCtx,  ← Background, NOT c.Request.Context
  │                      │                  │             sessionID, message)   │
  │                      │                  │                     │             │
  │                      │                  │             go func() {           │── intent
  │                      │                  │               for evt := range streamCh {
  │                      │                  │                 c.SSEvent(evt.Type, evt.Data)
  │                      │                  │                 c.Writer.Flush()  │── tool_call
  │                      │                  │               }                   │── tool_result
  │                      │                  │             }()                   │── final
  │                      │                  │                     │             │── done
  │◀── SSE stream ───────│                  │                     │             │
  │                      │                  │           wait for channel close  │
  │                      │                  │           return                  │
  │                      │◀─────────────────│── (all middlewares unwind)──────  │
```

**关键点**：
1. **context detach**：`agentCtx = context.Background()`，客户端断开不会杀
   任务。see [17 §Q3](17_api.md#q3)
2. **SSE 非 Gin 原生**：用 `c.SSEvent() + c.Writer.Flush()` 组合。
3. **middleware 在 handler 返回后才"解栈"**：tracing 的 span end 在这时发生。
4. **错误路径**：streamCh 关闭前 errCh 可能收到 err，得 select 两个 channel。

### Handler / Middleware 代码量

```
internal/api/
├── router.go            283 行 — 路由注册 + 中间件装配 + Server 构造
├── handlers.go          600 行 — 核心 chat / session / auth handler
├── middleware.go        186 行 — requestID / metrics / rateLimit / CORS / recovery
├── workspace_handlers.go 354 行 — 工作区 CRUD
├── project_handlers.go  144 行 — 项目生成
├── mcp_skill_handlers.go 206 行 — MCP/Skill 管理
├── auth_handlers.go     65 行  — JWT token 颁发
├── p0_debug_handlers.go 300 行 — 调试端点
└── integration_test.go  626 行 — 集成测试
```

### 利弊评估

**优势（Pros）**
- ✅ Gin 中间件栈成熟，性能好（radix tree 路由）
- ✅ 四种 chat 模式覆盖所有集成场景
- ✅ SSE + WS 双协议支持不同客户端
- ✅ 统一 setupMiddleware 保证顺序不会乱
- ✅ 集成测试覆盖完整 HTTP 流（使用 miniredis + httptest）

**代价（Cons）**
- ⚠️ `agentCtx = Background()` 反惯例，shutdown 时孤儿 goroutine 无法 drain
- ⚠️ 错误响应 schema 不统一（部分用 `{error: ...}`，部分 `{error, code}`）
- ⚠️ 没有 body size 限制（`http.MaxBytesReader` 未包装）
- ⚠️ rateLimit 是进程内的（见 middleware.go），N 副本 = N×RPS 实际限制
- ⚠️ CORS allowlist 硬编码在 `corsMiddleware`，不从配置读
- ⚠️ debug endpoints (`/api/v1/debug/p0`) 无独立鉴权

### 可改进点

**P0**
1. 切换 `rateLimit` 到 `auth/redis_ratelimit.go`（类已实现但未接线）
2. 统一错误 schema：`{error, code, request_id, details}`
3. 所有 handler 加 `http.MaxBytesReader(c.Request.Body, 10MB)`
4. debug endpoints 加 admin-only 鉴权

**P1**
5. Shutdown drain：trackDetachedAgentCtx 并 main.go 等它们结束
6. CORS allowlist 读配置：`api.cors.allowed_origins`
7. 错误映射：`internal/errors` 的 `Code` 自动映射 HTTP status

**P2**
8. 切到 stdlib mux + middleware 手写（Go 1.22 新语法够用）
9. WebSocket 心跳 + deadline（目前连接泄漏风险）
10. SSE 的 keepalive 定时发 comment 防代理超时

---

下一篇：`18_auth_security.md` —— JWT / API Key / Rate Limit / HMAC / Egress Policy / Audit Logger 合订本。
