# Code Agent 架构技术文档 · 00 总览

> 本系列文档逐模块拆解 `code_agent/` 后端实现。每一篇聚焦一个 Go 包，覆盖：
> **定位 → 关键类型与函数 → 依赖关系 → 数据流 → 设计权衡 → 踩坑与演进**。
>
> **推荐阅读顺序**：
> ```
> 00 总览 (本篇)
> 01 配置 → 02 模型 → 03 LLM → 04 RAG → 05 Sandbox → 06 MCP →
> 07 Tools → 08 Skill → 09 Orchestrator → 10 Planner → 11 Temporal →
> 12 Session → 13 Context → 14 Workspace → 15 Indexer/Repomap →
> 16 Store → 17 API → 18 Auth/Security → 19 Observability → 20 Deploy
>
> ──────────── P1 进阶模块（2026 Q1-Q2 引入）────────────
> 21 Agentloop（失败追踪 / step-back / 自动回归）
> 22 Multiagent（Code/Test/Review 三角色协作）
> 23 ToolLearn（工具调用学习 + 成功率统计）
> 24 Tree-sitter（多语言 AST 解析 / CGO + 正则双实现）
> 25 Memory（向量化长期记忆 + 压缩召回）
> 26 PTY（持久化 shell 会话）
> 27 LSP（goto_definition / find_references 工具）
> 28 Generator（项目骨架生成器）
>
> ──────────── 回顾与改进 ────────────
> 29 Conclusion（全景回顾 + 演进路线图）
> 30 Recent Improvements（近期 P0/P1 修复 & 缺陷汇总）
> ```
>
> 💡 实操测试文档请看 `docs/API_TEST_GUIDE.md`（所有端点的 curl + 回归用例）。

---

## 1. 项目定位

`code_agent` 是一个 **Go 实现的生产级代码智能 Agent 后端**，具备：

- **"思考 → 检索 → 执行 → 求助"** 完整闭环（ReAct + 多代理协作）；
- 直接操作本地 workspace（读/写/编辑/Git/测试/持久 shell）；
- 对 LLM 侧以 OpenAI-compatible API 对接，支持主备熔断；
- 以 Docker 容器做瞬态沙箱执行脚本/测试，并提供持久化 PTY 会话；
- 以 Qdrant + AST 切分做代码级 RAG，AST 由 tree-sitter（CGO）或正则降级提供；
- 以 MCP (Model Context Protocol) 即插即用外部工具；
- 以 LSP（占位接口，待补完）提供 `goto_definition / find_references / rename_symbol`；
- 以 Temporal 提供可恢复的 HITL 审批工作流；
- 以**项目生成器**（Blueprint→Scaffold→Implementation→Validation→Polish 五段流水线）从需求描述产出可运行项目骨架；
- 以**长期记忆**（向量化 + 压缩 + 召回）跨会话保留上下文；
- 对外提供 REST + WebSocket + SSE 三种交互通道，前端 `code_agent_ui` 基于 React。

一句话：**"Cline/Claude Code 的开源 Go 后端，天然为企业内网/多租户设计。"**

---

## 2. 顶层分层视图

```
┌─────────────────────── 入口 ──────────────────────────────┐
│ cmd/agent/main.go     configs/*.yaml                     │
└───────────┬──────────────────────────────────────────────┘
            ▼
┌──────────────── 接入层 (internal/api) ────────────────────┐
│ Gin Router · Middleware · Auth · WebSocket · SSE         │
│ /api/v1/chat · /api/v1/projects/generate · /api/v1/tools │
└───────────┬──────────────────────────────────────────────┘
            ▼
┌──────────── 编排层 (internal/orchestrator) ───────────────┐
│ ReAct 主循环 · Planner Bridge · Edit Engine              │
│ File/Git Tools · Failure Tracker · Message Pruner       │
│ P1 桥接: PTY/LSP/Tree-sitter/Memory/Multiagent           │
└─┬────────┬──────────┬──────────┬───────────┬────────────┘
  ▼        ▼          ▼          ▼           ▼
┌────┐ ┌──────┐ ┌────────┐ ┌─────────┐ ┌──────────┐
│LLM │ │ RAG  │ │Sandbox │ │   MCP   │ │Workspace │
└────┘ └──────┘ └────────┘ └─────────┘ └──────────┘
  │        │          │          │           │
  ▼        ▼          ▼          ▼           ▼
OpenAI   Qdrant   Docker API  External    本地文件树
Router  Embedding  cgroups    MCP Server    + Git
 +CB   +Reranker  + no-net    (stdio/SSE)
        +BM25
            │
            ▼
┌─── 代码智能能力（Code Intelligence）───────────────────────┐
│ tree-sitter (AST) · LSP client · symbol/repomap          │
│ → goto_definition / find_references / hover / rename     │
└──────────────────────────────────────────────────────────┘

┌─── Agent 进阶（Agent Capabilities）───────────────────────┐
│ agentloop (failure tracker / step-back / auto-test)      │
│ multiagent (Code / Test / Review 角色协作)               │
│ toollearn (工具调用学习 / 成功率统计)                    │
└──────────────────────────────────────────────────────────┘

┌─── 持久化与长期上下文（Persistence）──────────────────────┐
│ session (Redis hot/cold) · memory (向量化召回)          │
│ pty (持久 shell 会话) · store (Postgres)                │
│ temporal workflow · tool registry                        │
└──────────────────────────────────────────────────────────┘

┌─── 产品层（Product Surface）──────────────────────────────┐
│ generator (项目骨架生成器：Blueprint→Scaffold→Impl→...)  │
│ skill (技能模板) · indexer/repomap (仓库导览)            │
└──────────────────────────────────────────────────────────┘

┌─── 横切关注（Cross-cutting）──────────────────────────────┐
│ Metrics (Prometheus) · Tracing (OTel) · Audit Log       │
│ Auth (JWT + API Key) · HMAC Webhook · Egress ACL        │
└──────────────────────────────────────────────────────────┘
```

---

## 3. 请求在系统内的典型流转

### 3.1 一次 chat / ReAct 流程

以 **"帮我找到 `session.Manager.SaveMessage` 并加错误处理"** 为例：

1. **API 层**（`internal/api/handlers.go#handleChatReactStream`）
   - 收到 POST `/api/v1/chat/react-stream`，升级为 SSE。
   - 调用 `orchestrator.RunReactStream(ctx, req, events <-chan)`.

2. **Orchestrator ReAct 主循环**（`internal/orchestrator/orchestrator.go`）
   - 拉取 session messages（Redis hot 区）；
   - 召回 long-term memory（vector search，若启用，见 [25](25_memory.md)）；
   - 构建 system prompt + repomap + rag 上下文；
   - 调用 `llm.Client.ChatCompletion` with `tools = ToolRegistry.Definitions()`；
   - LLM 返回 `tool_calls`，逐个执行；
   - 失败累计触发 step-back（见 [21](21_agentloop.md)）。

3. **搜索工具**（`internal/orchestrator/file_tools.go#search_code`）
   - 委托给 `rag.Engine.Retrieve`：BM25 + Dense 双路 → 重排 → top-K chunks；
   - chunks 由 tree-sitter AST-aware 切分（见 [24](24_treesitter.md)）。

4. **跳转工具**（`internal/orchestrator/lsp_tools.go#goto_definition`）
   - 调 `lsp.Client.GotoDefinition`（当前占位返回 not implemented）；
   - 失败时回退（规划中）到 tree-sitter symbol table。

5. **编辑工具**（`internal/orchestrator/edit_engine.go#ApplyEdit`）
   - 读原文 → 校验 `old_text` 唯一匹配 → 写 `.bak` → 写新内容；
   - 运行 `go vet`（LintChecker）；失败则自动回滚。

6. **Shell 工具**（`internal/orchestrator/pty_tools.go#shell_exec`）
   - 走 `pty.SessionManager` 持久会话（见 [26](26_pty.md)）：`cd /pkg && go test` 跨调用保留 cwd；
   - 入口先过 `validateWorkspaceCommand` 黑名单（与 file_tools 共用），违禁命令直接拒绝。

7. **HITL 拦截**（高危场景）
   - `temporal/workflows.go#RunTaskWorkflow` 调用 `workflow.Await(signal)`；
   - 前端收到 SSE `approval_request`，用户点批准后发 `Signal`。

8. **返回**
   - 每一步通过 SSE `ReactStreamEvent` 推给前端，流式渲染。

### 3.2 一次项目生成流程

`POST /api/v1/projects/generate` 触发 `internal/generator` 五段流水线（详见 [28](28_generator.md)）：

```text
Blueprint (LLM JSON 蓝图：files[] + deps[])
    ↓
Scaffold (确定性模板：go.mod / Dockerfile / Makefile)
    ↓
Implementation (拓扑排序后逐文件 LLM 生成 + 依赖上下文拼接)
    ↓
Validation (每 5 文件一轮 sandbox 编译；失败 attemptAutoFix 一轮)
    ↓
Polish (README.md + TreeString 导航)
```

生成是异步的（fire-and-forget goroutine + 30 分钟硬超时），前端通过 `/api/v1/projects/:id/status` 或 `/generate/stream` (SSE) 观察进度。

---

## 3.5 设计哲学（系统级）

这 10 条是跨所有模块的共同约束，任何模块设计都要回答它们：

### P1 — 可降级 > 零故障

**问题**：下游依赖（Qdrant / Docker / MCP / Postgres / Temporal / LSP / tree-sitter CGO）任何一个挂了，整个 Agent 要不要停服？

**决策**：**Redis 必需**（session 无法降级），其余**全部可选**。构造失败只 `Warn` + 设 nil，handler 必须做 nil 检查。

**推导**：
- 开发机器 / 受限测试环境要能跑最小 Agent；
- 演示日 Qdrant 挂了，聊天仍然可用（只是没检索）；
- Docker socket 没挂上去，文件编辑仍能工作（只是不能执行代码）；
- CGO 不可用 → tree-sitter 走正则降级（见 [24 §5](24_treesitter.md#5)）；
- LSP 未启用 → 4 个语义工具在工具菜单消失，LLM 自然不调（见 [27 §8.1](27_lsp.md#81-注入)）。

**反例**：如果 Qdrant 必需 → 任何一次 Qdrant 部署抖动，整个聊天 API 502。

### P2 — 显式 DI > 魔法框架

**问题**：N 个子系统相互依赖，如何装配？

**决策**：手写 `main.go` 600+ 行拉齐。新人一眼能看出"谁依赖谁"；装配失败立即能定位；没有框架 bug 可怨。代价是 main.go 长—— 通过分组 + 注释保持可读。

### P3 — 缓存与作用域一致

**问题**：任何缓存的 key 设计必须回答"**什么写入会使我失效**"。

**踩过的坑**：
- Intent cache 按 sessionID → 第一条消息污染同 session 后续消息（P0 #12）
- Speculative cache 按 sessionID → 共享 workspace 的两个 session 互相看不到写入（P0 #14）

**原则**：缓存 scope 必须 ≥ 失效触发器 scope。workspace 的写失效 → 按 workspace 缓存；message 分类失效 → 按 (session, message) 缓存。

### P4 — Deadline 必须 end-to-end

**问题**：LLM 调用 60s，Docker 沙箱 120s，PTY Execute 120s，generator 30min ——如何防止单次请求吊死？

**决策**：每一层都显式 `context.WithTimeout`。不继承上层的 deadline 是错（上游断开 → 下游仍跑 10 分钟），不设 deadline 也是错（永久阻塞）。

**特例**：`generator.Generate` 用 `context.Background()` 而非 HTTP 请求 ctx——HTTP 断开不应取消已经跑了 10 分钟的生成（见 [28 §7.1](28_generator.md#71-异步语义)）；改用独立 30 分钟硬超时。

### P5 — 安全是深度而不是一道墙

同一个威胁要被**多层防御**独立拦截。以 SSRF 为例：
- L1: Go egress validator 在 URL 层拒绝（快）
- L2: Dialer.Control 在解析后 IP 层拒绝（挡 DNS rebinding）
- L3: Docker / PTY 容器 `NetworkMode=none` 阻断
- L4: iptables OUTPUT DROP（即便 L1-3 全绕过，内核层兜底）

任一层失效，其他层仍能拦住。

### P6 — 稳定的 prompt > 聪明的 prompt

**问题**：LLM 延迟 40% 来自 prompt 处理。KV cache 能省这部分——只要 prefix 字节级稳定。

**决策**：system prompt / tool schema / project rules **排序稳定、不带时间戳**。工具定义必须按名字 sort，map 遍历零容忍。

**量化**：相同 session 多轮对话，prompt 前 90% 字节稳定 → 延迟降 35-40%。

### P7 — 工具结果可信但输出有界

**问题**：LLM 的 tool 返回能返回无限大小。怎么防"`cat /dev/zero` 塞爆上下文"？

**决策**：
- 每个工具结果截断到 maxLen（文本工具 30k 字符，test 工具 15k，PTY 1MB）
- 截断策略保头尾（错误通常在尾部）
- sandbox 流式输出限 1 MiB，PTY 命令 stdout 限 1 MB
- generator 依赖文件上下文每文件截断 3000 字符（见 [28 §4.3](28_generator.md#43-依赖上下文拼接)）

### P8 — 幂等读可缓存，写必须穿透

所有 `read_file / list_dir / grep / git_status / rag_search / goto_definition / hover_info` 归入幂等白名单，可缓存；任何 `edit_file / run_* / rename_symbol / shell_exec` 后**整个 scope** 缓存失效（见 P3）。

### P9 — 多租户硬隔离

`workspace_id` 是租户边界。绝不允许任何 fallback 路径把租户 A 的请求路到租户 B 的 workspace（见 P0 #15）。宁可返回"没有 workspace"也不要静默用别人的。PTY 会话也以 `workspace_id` 为索引（`MaxSessionsPerWorkspace=3`），长期记忆同理。

### P10 — 每次修复都加测试

每个 P0 都有对应的单测/集成测试，避免同样的 bug 重新回来。测试应**测语义而不是代码形状**——测"两个并发 edit 不丢失"（TestEditEngine_ConcurrentEditsSerialized），不是"lockPath 被调用了"。新模块（tree-sitter / pty / memory / generator）每个都有独立 test 文件。

---

## 4. 关键非功能特性

| 维度 | 落地模块 | 说明 |
|---|---|---|
| 高可用（本地） | `llm/client.go` + gobreaker | 进程内熔断 + 指数退避 + 主备降级 |
| 高可用（分布式） | `llm/shared_breaker.go` | **跨副本熔断**（Redis 聚合失败计数）— [30 § C.1](30_recent_improvements.md#c1) |
| 分布式限流 | `auth/redis_ratelimit.go` | Redis INCR fixed-window，替代单节点 token-bucket — [30 § C.2](30_recent_improvements.md#c2) |
| 沙箱加固 | `sandbox/manager.go` buildHostConfig | `PidsLimit + no-new-privileges + CapDrop=ALL + ReadonlyRootfs + Tmpfs(noexec)` — [05 §7.4](05_sandbox.md) / [30 § A.4](30_recent_improvements.md#a4) |
| SSRF 防御 | `security/egress.go` EgressTransport + Dialer.Control | URL 层 + 解析后 IP 层两级拦截 — [18 § 6.4](18_auth_security.md#64) / [30 § A.3](30_recent_improvements.md#a3) |
| Webhook 防重放 | `security/hmac.go` | **强制** timestamp 窗口 + 未来时间戳拒绝 — [30 § A.1](30_recent_improvements.md#a1) |
| API Key 安全 | `auth/jwt.go` SHA-256 + 常量时间比对 | 绝不保留 plaintext — [30 § A.2](30_recent_improvements.md#a2) |
| 多租户 | `rag/qdrant_store.go` Payload filter + `pty/manager.go` 按 workspaceID 索引 | Qdrant 硬过滤；workspace 不跨 session fallback — [30 § A.6](30_recent_improvements.md#a6) |
| 可观测 | `metrics/` + `tracing/otel.go` + `audit/logger.go` | Prometheus + OTel tracing + 审计 |
| 可恢复 | `temporal/workflows.go` + main.go `startTemporalWorker` | Workflow 真实注册（非 stub）— [30 § C.3](30_recent_improvements.md#c3) |
| BM25 稀疏召回 | `rag/bm25.go` | 真实 Robertson-Sparck Jones + camelCase 拆分 — [04 §6.4](04_rag.md) / [30 § D.1](30_recent_improvements.md#d1) |
| Prompt 稳定性 | `tools/registry.go` 排序 + `context/prompt_builder.go` | 工具定义排序保证 KV cache 命中 |
| 并发安全编辑 | `orchestrator/edit_engine.go` per-path mutex | 多并发 ApplyEdit 串行化，rollback 不互撞 — [30 § B.1](30_recent_improvements.md#b1) |
| **AST 双实现** | `treesitter/parser_cgo.go` + `parser_fallback.go` | build tag 切换 CGO/正则——不强依赖 libgcc — [24 §1.5](24_treesitter.md#15-核心设计问题) |
| **持久 shell** | `pty/manager.go` + `session_local.go` / `session_docker.go` | 多次 shell_exec 共享 cwd/env；idle 5min reaper 回收 — [26](26_pty.md) |
| **长期记忆** | `memory/manager.go` | 跨会话向量化召回；压缩窗口外消息 — [25](25_memory.md) |
| **多代理协作** | `multiagent/sub_agent.go` | Code/Test/Review 三角色 + 工具白名单 + 冲突检测 — [22](22_multiagent.md) |
| **工具学习** | `toollearn/registry.go` | 调用统计、成功率反馈、自动 deprecate — [23](23_toollearn.md) |
| **失败追踪** | `orchestrator/failure_tracker.go` | 连续 N 次同类失败触发 step-back / 切换策略 — [21](21_agentloop.md) |
| **项目生成器** | `generator/generator.go` | Blueprint→Scaffold→Impl→Validation→Polish 五段 — [28](28_generator.md) |

---

## 5. 代码包全景（目录索引）

| 包路径 | 文档位置 | 一行摘要 |
|---|---|---|
| `cmd/agent`        | 本篇 §6                    | main 入口、依赖装配 |
| `internal/config`  | `01_config.md`             | YAML + env 覆盖 + schema 校验 |
| `internal/models`  | `02_models.md`             | 跨模块共享的领域类型 |
| `internal/llm`     | `03_llm.md`                | OpenAI 兼容客户端 + 主备路由 + 熔断 |
| `internal/rag`     | `04_rag.md`                | AST 解析 + Qdrant + BM25 + Rerank |
| `internal/sandbox` | `05_sandbox.md`            | Docker 瞬态容器执行 |
| `internal/mcp`     | `06_mcp.md`                | JSON-RPC 2.0 client + 自动重连 |
| `internal/tools`   | `07_tools.md`              | 并发安全工具注册表 |
| `internal/skill`   | `08_skill.md`              | 技能（模板化 prompt）注册表 |
| `internal/orchestrator` | `09_orchestrator.md`  | ReAct 主循环 + 文件/Git/编辑工具 + P1 桥接 |
| `internal/planner` | `10_planner.md`            | DAG 计划生成 + 拓扑并发执行 |
| `internal/temporal`| `11_temporal.md`           | Workflow + HITL Signal |
| `internal/session` | `12_session.md`            | Redis hot/cold 分层会话 |
| `internal/context` | `13_context.md`            | Token pruner + prompt builder |
| `internal/workspace`| `14_workspace.md`         | 本地 workspace 文件管理 |
| `internal/indexer` + `internal/repomap` | `15_indexer_repomap.md` | 仓库索引/符号图生成 |
| `internal/store`   | `16_store.md`              | Postgres 持久化 |
| `internal/api`     | `17_api.md`                | HTTP/WebSocket/SSE 全部入口 |
| `internal/auth` + `internal/security` | `18_auth_security.md` | JWT/API Key/HMAC/限流/出口白名单 |
| `internal/metrics` + `internal/tracing` + `internal/audit` | `19_observability.md` | Prometheus + OTel + 审计 |
| 部署 (`configs/`, `docker-compose.yml`, `deployments/k8s`, `Dockerfile*`) | `20_deploy.md` | 部署与运行 |
| **`internal/agentloop`** | `21_agentloop.md` | **失败追踪 / step-back / trajectory memory（被 orchestrator.reactLoopCore 消费）** |
| **`internal/multiagent`** | `22_multiagent.md` | **Code/Test/Review 角色协作 + 工具白名单** |
| **`internal/toollearn`** | `23_toollearn.md` | **工具调用学习 + 成功率统计 + auto-deprecate** |
| **`internal/treesitter`** | `24_treesitter.md` | **多语言 AST 解析（CGO/正则双实现）** |
| **`internal/memory`** | `25_memory.md` | **跨会话长期记忆：向量化 + 压缩 + 召回** |
| **`internal/pty`** | `26_pty.md` | **持久化 shell 会话（local/docker 双后端）** |
| **`internal/lsp`** | `27_lsp.md` | **LSP 客户端（接口齐全，wire protocol 占位）** |
| **`internal/generator`** | `28_generator.md` | **项目骨架生成器（Blueprint→Scaffold→Impl→...）** |
| — | `29_conclusion.md` | 全景回顾 + 演进路线图 |
| — | `30_recent_improvements.md` | **近期 P0/P1 修复 & 缺陷汇总**（2026-05 周期） |
| — | `ARCHITECTURE_DIAGRAM.md` | 全局 Mermaid / ASCII 数据流图 |
| — | `../API_TEST_GUIDE.md` | 按端点的 curl 测试 + 回归用例 |

---

## 6. `cmd/agent/main.go`：依赖装配清单

启动顺序（简化，行号近似当前 master）：

```go
1.  config.Load(path)                       // 配置 + 校验
2.  zap.NewProduction()                     // Logger
3.  tracing.NewProvider(cfg.Tracing)        // OTel
4.  redis.NewClient(...)                    // Redis (mandatory)
5.  store.New(cfg.Postgres)                 // Postgres
6.  llm.NewClient(cfg.LLM)                  // LLM 主备
7.  rag.NewEngine(cfg.RAG, llm)             // Embedder + Qdrant + Reranker
8.  sandbox.NewManager(cfg.Sandbox)         // Docker
9.  mcp.NewGateway(cfg.MCP)                 // MCP servers
10. skill.NewRegistry()                     // Skill registry
11. tools.NewRegistry()                     // 顶层工具索引
12. session.NewManager(redis, cfg)          // Session

// ────── Memory（长期记忆，§25）──────
13. memory.NewManager(cfg.Memory, ragEngine, llmClient)
    orch.SetMemoryManager(mem)

// ────── Workspace + Generator ──────
14. workspace.NewManager(cfg.Workspace)     // Workspace FS
15. generator.NewGenerator(llm, sandbox, ws, logger)
    apiServer.SetGenerator(gen)             // 项目生成器，§28

16. temporal.NewClient(cfg.Temporal)        // Workflow client

17. orchestrator.New(llm, rag, sandbox, …)  // ReAct 核心

// ────── P1 进阶能力（§21-§27）──────
18. if cfg.TreeSitter.Enabled:
      tsParser := treesitter.NewCGOParser(logger)   // CGO 或正则降级
      orch.SetTreeSitterParser(tsParser)
      ragEngine.SetTreeSitterParser(adapter)
      repomapGen.SetTreeSitterParser(adapter)
19. if cfg.PTY.Enabled:
      ptyMgr := pty.NewManager(cfg.PTY)              // local/docker 后端
      orch.SetPTYManager(ptyMgr)
      defer ptyMgr.Close()
20. if cfg.LSP.Enabled:                              // 默认 false（占位实现）
      lspClient := lsp.NewClient(cfg.LSP)
      orch.SetLSPClient(lspClient)
      defer lspClient.ShutdownAll()
21. if cfg.MultiAgent.Enabled:
      maMgr := multiagent.NewManager(llm, tools)     // Code/Test/Review
      orch.SetMultiAgentManager(maMgr)
22. if cfg.ToolLearn.Enabled:
      toolLearn := toollearn.NewRegistry(store)      // 成功率反馈
      orch.SetToolLearnRegistry(toolLearn)

// ────── 接入层 + 运行 ──────
23. api.NewServer(orch, session, …)          // Gin
24. http.ListenAndServe(cfg.Addr)            // 起
```

每个依赖的生命周期 & shutdown hook 都在 `main` 里显式处理，**没有任何全局单例**。`defer` 顺序是构造顺序的**倒序**——保持这个约束。

---

## 7. 运行/验证（最小化）

```bash
# 1. 起基础设施
cd code_agent && docker compose up -d redis qdrant postgres temporal

# 2. 配置
cp configs/config.example.yaml configs/config.yaml
# 编辑 configs/config.yaml：填 LLM base_url / api_key

# 3. 起服务
go run ./cmd/agent --config configs/config.yaml

# 4. 健康检查
curl localhost:8080/healthz
curl localhost:8080/readyz

# 5. 一次聊天
curl -X POST localhost:8080/api/v1/chat \
  -H 'Content-Type: application/json' \
  -d '{"message":"你好"}'

# 6. 一次项目生成（异步）—— 注意：当前 handler 未在响应里回 ID
curl -X POST localhost:8080/api/v1/projects/generate \
  -H 'Content-Type: application/json' \
  -d '{"description":"Go REST server with JWT auth"}'
# → 202 {"status":"generation_started", "message":"... use the SSE endpoint ..."}
# 想拿到实时进度只能走 SSE：
curl -N -X POST localhost:8080/api/v1/projects/generate/stream \
  -H 'Content-Type: application/json' -d '{"description":"..."}'
# /api/v1/projects/:id/status 路由存在但当前响应里没有 ID 可用（见 §28 已知缺陷）
```

---

## 8. 文档导航

每个 `NN_*.md` 都是独立、可单独消费的。遇到 "XXX 是怎么实现的" 的疑问时，直接翻对应的包文档即可。

**按主题快速跳转**：

- **想知道 ReAct 怎么循环 / 工具怎么注册** → [09 Orchestrator](09_orchestrator.md) + [07 Tools](07_tools.md)
- **想知道为什么有时候自动跑测试 / 失败后会换策略** → [21 Agentloop](21_agentloop.md)
- **想加一个新的 sub-agent 角色** → [22 Multiagent](22_multiagent.md)
- **想看看哪个工具用得最多 / 最不靠谱** → [23 ToolLearn](23_toollearn.md)
- **想知道 RAG chunk 怎么切的 / 为什么搜索这么准** → [04 RAG](04_rag.md) + [24 Tree-sitter](24_treesitter.md)
- **想知道为什么换个 session 还记得我之前说什么** → [25 Memory](25_memory.md)
- **想知道为什么 `cd` 完下条 `shell_exec` 还在那个目录** → [26 PTY](26_pty.md)
- **想知道 `goto_definition` 是怎么实现的 / 为啥老报 not implemented** → [27 LSP](27_lsp.md)
- **想知道项目生成器怎么从一句话生成可跑骨架** → [28 Generator](28_generator.md)
- **想看部署 / Docker / Kubernetes** → [20 Deploy](20_deploy.md)
- **想看最近修过哪些 P0** → [30 Recent Improvements](30_recent_improvements.md)

---

## 9. 系统级实现剖析与演进全景

### 9.1 全栈延迟预算（一次典型 chat 请求）

```text
╔══════════════════════════════════════════════════════════════════╗
║  P50 端到端：约 2-5 秒                                            ║
║  ├─ 网关 / TLS / Ingress              ~10 ms                    ║
║  ├─ Auth middleware                    ~2 ms                    ║
║  ├─ Intent 分类（首次，cache miss）    ~500 ms (LLM)            ║
║  ├─ Intent 分类（cache hit）           <1 ms                    ║
║  ├─ Memory 召回（向量搜索）            ~50-100 ms (Qdrant)      ║
║  ├─ 构建 prompt + Pruner              ~5 ms                     ║
║  ├─ RAG 检索（如果触发）               ~400 ms (P99 1.5 s)      ║
║  ├─ LLM 主调用                         ~1-3 s  (Opus streaming) ║
║  ├─ 工具执行                                                    ║
║  │   - read_file / grep                ~10 ms                   ║
║  │   - shell_exec (PTY，首次创建)      ~100-1500 ms             ║
║  │   - shell_exec (PTY，已有会话)      ~10-50 ms                ║
║  │   - sandbox run_tests               ~5-30 s ★ 慢             ║
║  │   - MCP call                        ~100-500 ms              ║
║  │   - goto_definition (LSP)           ~50-2000 ms ★（未启用）  ║
║  │   - ExtractSymbols (tree-sitter)    ~2-25 ms                 ║
║  ├─ Session.AddMessage (Redis)         ~1 ms                    ║
║  └─ SSE flush                          <1 ms                    ║
╚══════════════════════════════════════════════════════════════════╝

╔══════════════════════════════════════════════════════════════════╗
║  项目生成（generator，§28）：                                     ║
║  ├─ Blueprint (LLM JSON, files: 15-30)    ~10-30 s              ║
║  ├─ Scaffold (模板写文件)                 <100 ms               ║
║  ├─ Implementation (15 文件 × ~5s/文件)   ~75-180 s             ║
║  ├─ Validation (每 5 文件一轮编译)        ~5-30 s × 3-6 轮     ║
║  └─ Polish (README)                       ~5 s                  ║
║                                          ─────────              ║
║                                            约 2-8 min          ║
╚══════════════════════════════════════════════════════════════════╝
```

**性能瓶颈排序**：
1. **项目生成** — 文件数 × LLM 延迟，不可压缩（除非并行，见 [28 §11](28_generator.md#11-后续演进)）
2. **sandbox run_tests** — 运行用户代码，不可压缩
3. **LLM 主调用** — 受 TTFT 和 output tokens 影响
4. **RAG 检索 + rerank** — embedding + rerank 各 200ms
5. **cold container 启动** — image 没预热时 30s
6. **PTY 首次创建** — local backend ~100ms，docker backend 1-2s

### 9.2 跨模块的"全局改进方向"（已汇总）

所有 30 篇文档各自的"改进点"按类别聚合：

**安全（Security）**
- Egress validator 接入 LLM / MCP / rerank 客户端（见 [18 §13](18_auth_security.md)）
- Redis rate limiter 接入 router middleware（见 [17 §13](17_api.md)）
- HMAC nonce 缓存防窗口内重放（见 [18 §13](18_auth_security.md)）
- 沙箱加 seccomp profile / gVisor 选项（见 [05 §12](05_sandbox.md)）
- JWT 撤销接入 Redis（见 [18 §13](18_auth_security.md)）
- PTY local backend cgroup 限额（见 [26 §10](26_pty.md)）
- PTY 命令黑名单（`rm -rf /` 等，见 [26 §10](26_pty.md)）
- **`.dockerignore` 排除 `configs/config.yaml`**——当前镜像仍会烤入密钥，公开发布前必须修

**可靠性（Reliability）**
- gobreaker Interval/Timeout 分开配置（见 [03 §12](03_llm.md)）
- SharedBreaker 接入 streaming（见 [03 §12](03_llm.md)）
- HTTP shutdown drain detached goroutines（见 [17 §13](17_api.md)）
- 压缩失败兜底 + error metric（见 [12 §13](12_session.md)）
- MCP per-server 熔断（见 [06 §13](06_mcp.md)）
- Generator 异步任务持久化（重启后能续传，见 [28 §11](28_generator.md)）
- LSP server crash 自动重启（见 [27 §11](27_lsp.md)）
- Memory 召回结果分数阈值过滤（见 [25](25_memory.md)）

**性能（Performance）**
- Sandbox warm pool，cold 启动 400ms → 50ms（见 [05 §12](05_sandbox.md)）
- 并行 tool_calls（见 [09 §15](09_orchestrator.md)）
- Embedding Redis 二级缓存（见 [04 §12](04_rag.md)）
- Pruner O(N²) → O(N)（见 [13 §11](13_context.md)）
- tiktoken-go 替换启发式计数（见 [03 §12](03_llm.md), [13 §11](13_context.md)）
- Tree-sitter 增量解析（见 [24 §11](24_treesitter.md)）
- Generator 并行实现独立文件（见 [28 §11](28_generator.md)）

**质量（Quality）**
- AST parser 扩展到更多语言（已支持 8 种，见 [24 §11](24_treesitter.md)）
- BM25 迁移到 Qdrant sparse vector（> 100k chunks 时，见 [04 §12](04_rag.md)）
- Rerank 学习加权（见 [04 §12](04_rag.md)）
- MCP tool 结果流式（见 [06 §13](06_mcp.md)）
- LSP 真接 wire protocol + tree-sitter 降级路径（见 [27 §11](27_lsp.md)）
- Generator validate-once → fix-iterate-until-pass（见 [28 §11](28_generator.md)）

**可观测（Observability）**
- 各模块 metric 完善（详见各篇 §演进）
- Tail-based tracing sampling（见 [19 §1.5](19_observability.md)）
- Grafana dashboard 随项目发布
- `pty_active_sessions{backend,workspace}` / `lsp_requests_total{method,language,status}` / `generator_phase_duration_seconds`

**开发体验（DX）**
- `code-agent dev` 一键起本地栈
- Skill / MCP CRUD 前端 UI 完善
- 文档交叉引用 + API_TEST_GUIDE 持续维护
- Dockerfile.cgo 提供 CGO 版镜像（见 [24 §7](24_treesitter.md)）

### 9.3 系统演进优先级矩阵

```
           影响大 ↑
                │
    "接线类"   │   "重构类"
    (P0, 小)    │   (P1, 大)
                │
    · Egress   │   · Warm pool
    · RateRedis│   · 并行 tool_calls
    · JWT Rev  │   · O(N) pruner
    · LSP wire │   · Generator 并行
                │
    ————————————┼————————————→ 工作量大
                │
    "修 bug"   │   "新能力"
    (P0, 中)    │   (P2, 大)
                │
    · gobreaker│   · eBPF egress
    · nonce    │   · Multi-region
    · drain    │   · gVisor
    · dockerignore│ · Cross-language LSP
                │
           影响小 ↓
```

**建议**：先做**左上象限**（影响大工作量小的"接线类"）—— 很多类库已写好，只差 10-20 行接入代码。再做**右上象限**（重构类）—— 带明显性能收益。右下"新能力"留给业务驱动。

详细的"为什么这样设计 / 当前缺什么 / 怎么补"在 [29 Conclusion](29_conclusion.md) + [30 Recent Improvements](30_recent_improvements.md) 两篇中。

---

下一篇：[`01_config.md`](01_config.md) —— 配置加载、多层覆盖、schema 校验。
