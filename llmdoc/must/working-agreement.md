# 工作约定

修改本代码库前，必须了解以下约定和陷阱。

## Setter-based DI 模式

`cmd/agent/main.go` 使用 setter 打破循环依赖（例如 Orchestrator 需要 SkillRegistry，而 SkillRegistry 构造需要 Orchestrator 的工具定义）。关键 setter 调用：

- `apiServer.SetIndexer(idx)`
- `orch.SetSkillRegistry(skillReg)` + `apiServer.SetSkillRegistry(skillReg)`
- `apiServer.SetMCPGateway(mcpGateway)`
- `orch.SetWorkspaceManager(wsMgr)` + `apiServer.SetWorkspaceManager(wsMgr)`
- `apiServer.SetGenerator(gen)`

**不变量**：Handler 和 Orchestrator 必须对通过 setter 注入的依赖做 nil 检查，因为这些依赖可能在降级模式下为 nil。

## KV-Cache 友好的 Prompt 结构

`internal/context/prompt_builder.go` (`PromptBuilder`) 按 5 个区域组装 prompt，顺序不可更改：

1. **不可变系统提示** — 静态，永不变，最大化 KV cache 前缀命中
2. **半稳定长期记忆** — 会话摘要，仅在摘要更新时变化
3. **裁剪后代码上下文** — RAG 检索 chunk，经 TokenPruner 多信号评分裁剪
4. **近期对话** — 历史消息，从最近向前填充 token 预算
5. **当前用户消息** — 新输入

`prefixHash`（sha256 of 区域1+2）用于监控 cache 命中率。维护 `prompt_cache_prefix_hash` 指标。

**不变量**：`tools.Registry.Definitions()` 返回排序后的切片以保证确定性。不要在 Definitions 路径中引入 map 遍历。

## 工具分发拆分

工具系统存在两套并行机制：

| 机制 | 位置 | 用途 |
|------|------|------|
| Orchestrator `executeTool()` switch | `internal/orchestrator/orchestrator.go` | ReAct 循环内置工具分发 |
| `tools.Registry` | `internal/tools/registry.go` | API 层暴露的动态工具（MCP、Skill） |

Orchestrator 的 `executeTool()` 优先级：(1) MCP gateway `FindServerForTool()` → (2) 内置 switch（execute_code, read_file, write_file 等）→ (3) skill registry 回退。

**不变量**：核心 ReAct 循环不走 `tools.Registry.Execute()`，而是走 orchestrator 自己的 switch。修改工具分发时需同时考虑两套机制。

**工具名常量化（2026-06-04 PR 8）**：orchestrator 包内的硬编码工具名字符串（原散落 18 个文件）已收敛到 `internal/orchestrator/tool_names.go::Tool*` 常量，新增内置工具应在该文件登记。跨包硬编码（multiagent / agentloop / planner）仍待迁移，见 `llmdoc/memory/doc-gaps.md::TOOL-NAMES`。

### 工具元数据中心化（2026-06 重构）

新增内置工具时，**在工具定义上声明行为 metadata bit 即可**，无需手动同步多处白名单。`models.ToolDefinition` 携带四个行为位：

| Bit | 含义 | 消费点 |
|---|---|---|
| `IsFileWrite` | 写文件类工具 | `Orchestrator.captureForTransaction` / `multiagent.Supervisor.fileWriteClassifier` |
| `IsIdempotentRead` | 幂等纯读 | `SpeculativeToolCache.isIdempotent`（推测缓存白名单） |
| `TriggersAutoTest` | 写完触发 auto-test | `react_core.go` 编辑追踪 |
| `InvalidatesCache` | 写后清缓存 | `SpeculativeToolCache.shouldInvalidate` |

**源头**：
- `internal/orchestrator/file_tools.go::fileToolDefinitions()` — read/write/patch/edit/apply_diff
- `internal/orchestrator/git_tools.go::gitToolDefinitions()` — git_status/diff/commit/log/branch
- `internal/orchestrator/lsp_tools.go::RegisterLSPTools` — goto_definition/find_references/hover_info/rename_symbol

**消费点**（不再硬编码工具名）：
- `Orchestrator.toolMetadata/IsFileWriteTool/IsBuiltinTool/triggersAutoTest` (`tool_metadata.go`)
- `SpeculativeToolCache.SetMetadataLookup`（运行时由 orchestrator 注入注册表查询）
- `multiagent.WithFileWriteClassifier`（main.go 注入 `orch.IsFileWriteTool`）
- `api/dynamic_tool_handlers.go` 用 `s.orchestrator.IsBuiltinTool(name)`
- `api/mcp_skill_handlers.go::handleListTools` 直接遍历 `orchestrator.GetAvailableTools()`

**CI 守卫**：`internal/orchestrator/tool_metadata_test.go::TestBuiltinToolsHaveMetadata` 遍历所有 builtin 定义，要求每个工具至少声明一个 metadata bit（少数纯执行工具如 `run_tests/execute_code` 在 exempt 白名单中）。`TestFileWriteToolsTriggerAutoTest` 校验 `IsFileWrite=true → TriggersAutoTest && InvalidatesCache`。

**遗留**：
- `planner.defaultActions`（`internal/planner/planner.go`）和 `multiagent/sub_agent.allowedTools`、`role_selector.actionScore`——这些是**策略/ACL**，不是工具行为，保持为常量数组。
- `agentloop/runner.go`、`agentloop/adaptive_feedback.go` 内尚有少量硬编码工具名（独立包内的轻量启发式），与本次改动正交。

## 死代码清单（2026-06 大幅更新）

之前文档列出的"死代码"经核实，**大部分已接线**：

| 组件 | 状态 | 接线位置 |
|------|------|---------|
| `llm.Router` | ✅ 已接 | `cmd/agent/main.go::SetRouter`（`cfg.LLM.Router.Enabled()` 触发；默认 nil 跳过） |
| `mcp.ConnPool` | ✅ 已接 | `Gateway.servers` 改为 `map[string]*ConnPool`，`PoolSize<=1` 等价单连接 |
| `toollearn.PGStore` | ✅ 已接 | `main.go::orch.SetToolLearnStore(toollearn.NewPGStore(pgStore.DB()))`，自动 Migrate `tool_feedback` 表 |
| Git 工具 | ✅ 已接 | 通过 `tools.Registry` 走通用分发，LLM 可调用 |
| `LLMSummarizer` | ✅ 已接 | `main.go::sessionMgr.Summarizer = session.NewLLMSummarizer(...)` |
| `SpeculativeToolCache` | ✅ 已接 | Orchestrator 在 `NewOrchestrator` 构造并 `SetMetadataLookup` 给注册表 |
| `RedisRateLimiter` | ✅ 已接 | `api/router.go:190-191` |

**仍然死的**（本表只列"曾在此处声明但已修复"的项；其它跨子系统死代码以 `llmdoc/memory/doc-gaps.md` 为准）：

> ✅ MCP SSE 传输（2026-06）：`internal/mcp/transport_sse.go` 已实现 HTTP+SSE 传输，`dialTransport` 按 `cfg.Transport` 分发；`NewGateway` 不再 skip SSE 配置。详见 `llmdoc/architecture/infrastructure-subsystems.md::传输`。
>
> ✅ `formatVerificationFeedback`（2026-06-07）：曾仅被 `logger.Warn` 路径触及，本日 stream 路径 retry-once 已消费（见下「Verifier retry-once 门控」段）。**不要再当死代码删**。
>
> 其它未接线项（如 `internal/audit`、`internal/pool` 单一 importer 等）见 `llmdoc/memory/doc-gaps.md::死代码`。

## ToolResult.Metadata 契约

跨边界（tool → orchestrator → SSE → 前端）传 typed struct 时，统一用 `json.RawMessage` 透传，**不要**在中间层落成 `map[string]any`——一旦中间层用 map 反序列化再编码回去，前端拿到的 JSON 形状（数字类型 / null / 字段顺序）由中间层决定而非后端 struct 的源真相。

- 工具侧把结构化产物（如 `EditResult` → `editResultToMetadata`，`internal/orchestrator/file_tools.go`）`json.Marshal` 成 `ToolResult.Metadata`；
- `react_core.go` 把它原样塞进 `ReactStreamEvent.Metadata`；
- 前端按已知 metadata schema 渲染产物块（DiffBlock 等）。

**不变量**：新增工具携带结构化产物时，定义专属 metadata struct + 序列化 helper，**不要**在 orchestrator 里手搓 `map[string]any{...}`。

## SSE 断线恢复与合成 done 兜底

`GET /api/v1/chat/react-stream/status` / `/resume` 复用 `streamReplayFollow`（`internal/api/handlers.go`）的 **Replay → Status → Follow** 三段式：先把 Redis Stream 缓存的历史事件回放给客户端，再读任务 Status 决定是否继续 Follow 实时流。

**合成 `done` 不变量**：

- 触发条件 1（Replay 后）：history 末尾不是 `done`/`error` 且 `Status` not running → emit `{"reason":"synthesized_after_replay"}`；
- 触发条件 2（Follow 退出后）：整段未见终态且 ctx 仍存活 → emit `{"reason":"synthesized_after_follow"}`；
- 守门变量 `hasTerminal` 保证幂等：真 `done` 已在 history 时绝不重复合成；
- `reason` 字段是审计/排查的唯一区分手段，客户端对 `done` 幂等。

**测试惯例**：`streamCacheReplayer` 局部接口仅用于让 `streamReplayFollow` 可单测（miniredis-backed StreamCache），不是为了让外部包替换。新增类似 handler 内重型流程时，首选「**局部接口 + 真实依赖**」测试模式，而非全栈 mock。

## Verifier retry-once 门控

`verifyOutput` 失败时的 retry 决策由唯一纯函数 `decideVerificationFollowup(retried, globalStep, absoluteMaxSteps, vResult)` 给出（`internal/orchestrator/verification.go`）。约束：

- **stream-only**：仅 `ProcessMessageStreamFull` 路径消费；sync `ProcessMessage` 有严格延迟预算，二次 LLM 调用会让调用方超时，**保留旧的 `logger.Warn` 行为**。
- **一次性哨兵**：`task.VerificationRetried bool json:"-"`（`internal/models/models.go`）—— 运行时哨兵，`json:"-"` 不参与持久化，避免跨会话被复用。
- **步数预算**：guard `globalStep < absoluteMaxSteps - 3`，给二次 ReAct 至少 3 步落实 feedback；任何「触发额外子任务」的门控都要为下游预留步数。
- **回流方式**：feedback 以 `RoleUser` push 回 `messages` 后 `continue` 外层 for，触发自然的下一轮 ReAct——**不要** break 出主循环再启一段新流程。

详细架构语境见 `docs/architecture/09_orchestrator.md` §9.2 与 `llmdoc/architecture/request-flow.md`。

## SSE 长任务心跳契约

长任务（finalize 阶段单次非流式 LLM 调用可能阻塞 20+ 分钟）的心跳分两层，**职责不重叠**：

| 层 | 位置 | 形态 | 目的 |
|----|------|------|------|
| 协议级 | `internal/api/handlers.go::runSSEHeartbeat` | 每 25s 写 `": ping\n\n"` 注释行（不进前端 `data:` parser） | 防 `server.write_timeout: 600s` 撕扯合法长任务连接 |
| 业务级 | `internal/orchestrator/react_core.go::callLLMWithProgress` | 每 3s emit `llm_call_started` / `llm_call_progress` / `llm_call_completed` 三件套，progress 载 `{attempt,elapsed_ms}` JSON Content | 让 UI 在 finalize 阻塞时仍能看到稳定心跳 |

**不变量**：

- 两层契约不要试图合并——协议级保的是 TCP/HTTP 不被 timeout 撕、业务级保的是 UI「进度可感知」。
- `llmProgressInterval` 是包级 `var` 而非 `const`，仅用于测试缩短间隔，**不要**改成常量。
- 三件套事件经 `persistingSink` 自动入 Redis Stream，resume 路径同样可见；新增类似机制时要走这条总线而非 SSE 直发，否则断线无法恢复。
- 前端 UI 不把三件套渲染为新 step 卡片，而是内联到当前 step 的 spinner 旁。

详见 `docs/architecture/09_orchestrator.md` §Q4.5。

## 决策模式：跨传输路径差异化

同一个逻辑分支（如 verifier retry）在 sync 与 stream 路径采取**不同**策略是合法的，判据是「延迟容忍度」而非「行为必须一致」。当前案例：

- **sync** `/chat`：调用方等 HTTP response，二次 LLM 调用 = 用户感知超时 → 不 retry。
- **stream** `/chat/react-stream`：用户已在看事件流，retry 对 UX 是透明加分 → retry-once。

修重构时不要为了「统一」强行把两条路径合并；先评估各自的延迟/状态预算。

## 双重 Token 估算器

存在两个不同精度的 token 估算器：

- `internal/llm/client.go` (`EstimateTokens`) — rune 分类法，CJK 1 token/rune，ASCII 4 chars/token
- `internal/session/manager.go` (`estimateTokens`) — 朴素 `len(text)/4 + 1`，CJK 低估约 3 倍

Session 层使用精度较低的估算器，可能导致 token 预算计算偏差。

## Docker 部署陷阱

### `docker build` ≠ `docker compose build`

镜像标签不同，两者**不会复用**对方的产物：

| 命令 | 镜像名 | 被 compose 使用？ |
|------|--------|-------------------|
| `docker build -t code-agent:latest .` | `code-agent:latest` | ❌ 否（compose 找不到） |
| `docker compose build agent` | `code_agent-agent:latest`（项目名_服务名） | ✅ 是 |

`docker-compose.yml` 中 `build: .` 字段不会复用任意标签的镜像。要让 `docker build` 的产物被 compose 使用，需在 compose 中显式：

```yaml
services:
  agent:
    image: code-agent:latest  # 显式标签
    build: .
```

否则 `make docker-build` 的产物会被 `make docker-up` 完全忽略，导致看似"重新构建"但实际跑的是旧镜像。

### 镜像新鲜度三招验证

排查"代码改了但行为没变"时，按顺序验证：

1. `docker compose images agent` — 看镜像 ID 和 CREATED 时间，是否真是新的
2. 日志 caller 行号 — 在源码新加几行后，编译产物的 `caller: "agent/main.go:NNN"` 必然变化；如果跑的还是旧行号，说明镜像没换
3. `docker exec <ctr> strings /usr/local/bin/code-agent | rg <known_string>` — 二进制必须含新加的日志字符串

### 配置文件部署

镜像构建采用**双保险**：

1. `.dockerignore` 显式排除 `configs/config.yaml` 与 `configs/config.allinone.yaml`——即使 Dockerfile 写错也挡住；
2. `Dockerfile` / `Dockerfile.local` 只 `COPY configs/config.example.yaml`，不复制整个 `configs/` 目录。

运行时通过 docker-compose volume bind mount 注入真实配置：

```yaml
volumes:
  - ./configs/config.yaml:/etc/code-agent/configs/config.yaml:ro
```

bind mount 的作用：(1) 运行时挂载真实配置（镜像内不含此文件）；(2) 让本地修改立即生效，`docker compose restart agent` 无需 rebuild。

**`Dockerfile.allinone` 是例外**：单文件 `COPY config.allinone.yaml`，设计上就是内嵌配置的镜像。allinone 镜像不可公开发布。

## 测试惯例

- 始终启用 race detector：`go test -race`
- 集成测试使用 `httptest.Server` + `miniredis` + `zap/observer`，见 `internal/api/integration_test.go`
- `make test-short` 的 `-short` 标志当前无实际效果（零个测试文件检查 `testing.Short()`）
- `internal/temporal/` 在 lint 中排除 unused 检查（Temporal SDK 类型在注册前看起来未使用）
- `.golangci.yml` 全局禁用 G104（unchecked errors）和 G304（file inclusion）
