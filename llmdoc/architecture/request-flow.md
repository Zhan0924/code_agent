# 请求流程与 ReAct 循环

本文档覆盖从 HTTP 请求进入到 LLM 响应返回的完整生命周期。

## HTTP 入口

三个聊天端点，对应不同流模式：

| 端点 | Handler | 模式 |
|------|---------|------|
| `POST /api/v1/chat` | `handleChat` | 同步，detached context（10min 超时） |
| `POST /api/v1/chat/stream` | `handleChatStream` | SSE token 流（session/message/error/done） |
| `POST /api/v1/chat/react-stream` | `handleChatReactStream` | SSE ReAct 事件流（intent/thinking/tool_call/tool_result/final/approval_request/done） |

**detached context 模式**：`handleChat` 在 `context.Background()` 上创建 10 分钟超时 context，使客户端断连不会终止进行中的 LLM 调用。这是有意设计——ReAct 循环可能需要数分钟完成。

## 中间件链

`internal/api/router.go` (`setupMiddleware`) 中间件按以下顺序执行：

1. `recoveryMiddleware` — panic 恢复，最外层
2. `requestIDMiddleware` — 读取 `X-Request-ID` 或生成 UUID
3. `tracing.GinMiddleware` — OTel span 创建 + W3C 传播
4. `metricsMiddleware` — Prometheus `api_request_total` / `api_request_duration_seconds`，使用 `c.FullPath()` 控制基数
5. `rateLimiterMiddleware` — 进程内 token bucket（10 rps, burst 20），按 IP 限流
6. `loggingMiddleware` — 结构化请求日志
7. `corsMiddleware` — 硬编码 `allowedOrigins`（localhost:3000/5173/8080）

当 `authEnabled=true` 时，`/api/v1` 路由组额外加载 `auth.AuthMiddleware`（JWT + API Key）。

## Orchestrator 主入口

`internal/orchestrator/orchestrator.go`:

- `ProcessMessage()` (line 156) — 同步入口
- `ProcessMessageStreamFull()` (line 725) — SSE 流式入口

两者共享核心逻辑，差异在于事件发射方式。

### 处理流程（ProcessMessage）

```
用户消息 → 创建 Task → 存入 DB → 检测 "continue" → 保存用户消息到 Session
→ parseIntent() → 检查敏感内容/部署意图 → HITL 审批（如需）→ reactLoop()
```

### Intent 解析

`parseIntent()` (line 968) 通过低温度 (0.0) LLM 调用分类用户意图，最大 20 token 输出。结果按 `sha256(session_id + message)` 缓存，TTL 2 分钟，上限 2048 条目（过期清理 + 最旧 25% 淘汰）。

六种意图及其步数限制：

| Intent | 最大步数 | 说明 |
|--------|----------|------|
| `code_query` | 10 | 代码查询 |
| `code_execute` | 20 | 代码执行 |
| `diagnose` | 25 | 诊断 |
| `deploy` | 20 | 部署（触发 HITL） |
| `mcp_call` | 15 | MCP 工具调用 |
| `conversation` | 50（默认） | 对话/编码 |

## ReAct 循环

`reactLoop()` (line 274) 是核心执行引擎。

### 循环前准备

1. 对 code_query / diagnose 意图，先执行 RAG 检索获取代码 chunk
2. 通过 `PromptBuilder.BuildPrompt()` 组装 prompt（见下文 5 区域结构）
3. 通过 `buildSystemMessage()` 按意图覆盖系统消息

### 每次迭代

```
[检查 token 预算 128K] → [每 10 步注入反思检查点]
→ llmClient.ChatCompletion()（最多 3 次重试，2s/4s 指数退避）
→ [有 tool_calls?] → executeTool() → [记录失败] → [自动运行测试]
→ [应用智能输出截断] → [下一迭代或结束]
```

### 流式变体（ProcessMessageStreamFull）

外层自动续接循环（`absoluteMaxSteps = 200` 硬上限），内层批次循环使用 `getMaxSteps(intent)`。每个 ReAct 步骤发射 `ReactStreamEvent`（step_start / thinking / tool_call / tool_result / tool_progress / final / done）。

**tool_progress 事件**：长时间运行的工具（如 `run_workspace_cmd`）通过 context 注入的 `ProgressCallback` 逐行流式输出 stdout，生成 `tool_progress` SSE 事件（含 step/toolCallID/toolName/content）。当前仅 `run_workspace_cmd` 支持此机制。定义见 `internal/orchestrator/tool_progress.go`。

## PromptBuilder 5 区域结构

`internal/context/prompt_builder.go` (`PromptBuilder`)，128K token 预算：

| 区域 | 内容 | 变化频率 | KV-cache 影响 |
|------|------|----------|---------------|
| 1 — 不可变系统提示 | 静态系统消息 | 永不变 | 最大化前缀命中 |
| 2 — 半稳定长期记忆 | 会话摘要 | 仅摘要更新时 | 高命中 |
| 3 — 裁剪后代码上下文 | RAG chunk，经 TokenPruner 评分 | 每次查询 | 变化较大 |
| 4 — 近期对话 | 历史消息（最近优先填充） | 每轮 | 尾部变化 |
| 5 — 当前用户消息 | 新输入 | 每轮 | 最后 |

`prefixHash` = sha256(区域1 + 区域2)，暴露为 `prompt_cache_prefix_hash` Prometheus 指标。

使用 `sync.Pool` 复用 `strings.Builder` 减少 GC 压力。

## TokenPruner

`internal/context/pruner.go` (`TokenPruner`) 提供两种裁剪：

### 代码 Chunk 裁剪（PruneCodeChunks）

多信号重要性评分，权重可配置（默认）：
- 查询相关性 (0.45) — RAG 检索分数
- 调用频率 (0.25) — 符号在其他 chunk 中被引用的次数
- 作用域深度 (0.15) — 深层嵌套权重低
- 新近度 (0.15) — 位置代理

贪心背包法在 token 预算内选择最高分 chunk。

### 消息裁剪（PruneMessages）

1. 先通过 `TruncateLargeToolResult` 折叠过大的工具结果（头尾各 2KB + 截断标记）
2. 无条件保留：系统消息 + 最后 4 条消息
3. 剩余预算从最近向前填充

## 工具分发

`executeTool()` (line 1281) 按以下优先级分发：

1. **MCP Gateway** — `FindServerForTool()` 匹配 → `CallTool()`
2. **内置 switch** — `execute_code`, `search_code`, `read_file`, `write_file`, `patch_file`, `edit_file`, `apply_diff`, `list_files`, `create_directory`, `run_tests`, `run_workspace_cmd`
3. **Skill Registry** — 回退到技能注册表

**注意**：新增内置工具需在 9 处注册点更新（见 `must/working-agreement.md` "分布式工具白名单"节）。

### 内置工具行为

| 工具 | 关键行为 |
|------|----------|
| `read_file` | 支持行范围，50KB 截断 |
| `write_file` | 写入后触发 `autoDepManagement`（go mod tidy / npm install / pip install） |
| `patch_file` | 字符串替换，仅首次出现 |
| `edit_file` | 委托 EditEngine：唯一匹配验证 → .bak 备份 → lint 检查 → 失败自动回滚 |
| `apply_diff` | 委托 EditEngine：解析 unified diff (`sourcegraph/go-diff`) → 逐 hunk 应用 → .bak 备份 → lint 检查 → 失败回滚 |
| `run_tests` | Docker sandbox 内执行，卷挂载 |
| `run_workspace_cmd` | 宿主机 `sh -c` 执行，默认 5min 超时（`workspace.cmd_timeout` 可调；LLM 可传 `timeout_seconds` 在上限内自定义更短），环境变量白名单，禁止命令检查，超时 SIGKILL 整个进程组 |
| `execute_code` | Sandbox 隔离执行 |
| `search_code` | RAG 检索 |

### EditEngine

`internal/orchestrator/edit_engine.go` (`EditEngine`)：精确编辑引擎。

- 唯一匹配验证（old_text 必须恰好出现 1 次）
- 写前备份（.bak 文件）
- 写后 lint/编译检查（Go: go vet, Python: ruff, TS: tsc --noEmit, JS: node --check, Rust: cargo check），30 秒超时
- lint 失败自动回滚
- 路径级互斥锁（`pathLocks`），多文件编辑按排序顺序加锁防止死锁

### 智能输出截断

大工具输出使用 HEAD+TAIL 策略：保留前 8K + 后 12K，中间插入截断标记。

## HITL 审批

触发条件：`containsSensitiveContent()` 匹配（正则模式见 `configs/config.yaml:107-114`）或 `IntentDeploy`。

`suspendForApproval()` (line 580) 流程：
1. 创建 buffered channel
2. 启动 goroutine，30 分钟超时
3. 通过 SSE `approval_request` 事件通知前端
4. `HandleApproval()` 通过 channel 发送审批结果
5. 审批通过后恢复 `reactLoop()`

指标：`hitl_pending_count`（gauge），`hitl_approval_total{approved|rejected|timeout}`。

## 失败跟踪

`internal/orchestrator/failure_tracker.go` (`consecutiveFailureTracker`)：追踪同一工具的连续失败。3 次连续失败后注入"退后一步"系统消息，强制 LLM 重新审视错误并尝试不同方法。

## 自动测试

`internal/orchestrator/auto_test_runner.go` (`AutoTestRunner`)：ReAct 循环中文件编辑后自动发现并运行相关测试。

- 发现规则：Go `*_test.go`，Python `test_*.py` / `*_test.py`，JS/TS `*.test.*` / `*.spec.*`
- 90 秒超时
- 结果作为系统消息注入回 ReAct 对话

## 反思检查点

`reflectionCheckpoint()` (line 538)：每 10 步注入系统消息，提示 LLM 评估当前进展。

## 续接

`buildContinuationPrompt()` (line 496)：用户发送 "continue" 时，从 workspace 读取 `.progress.json` + `.plan.md`，注入续接上下文。

`saveProgressForContinuation()` (line 460)：步数限制或 LLM 失败时写入 `.progress.json`。

## LLM 客户端

`internal/llm/client.go` (`Client`)：

- **双 Provider 架构**：primary + fallback，均为 OpenAI 兼容 API
- **本地熔断**：gobreaker，按连续失败次数触发（`cfg.CircuitBreaker.MaxFailures`）
- **共享熔断**：`SharedCircuitBreaker`（Redis 固定窗口，30s 窗口 / 20 失败阈值），跨副本聚合，Redis 故障时 fail-open
- **流式回退限制**：`ChatCompletionStream` 仅在建连失败时回退到 fallback，流中错误不回退（避免矛盾输出）

## Session 管理

`internal/session/manager.go` (`Manager`)：

- **Hot/Cold 分离**：热数据（最近 10 条消息）在 `sess:hot:{id}`，冷数据（归档 + 摘要）在 `sess:cold:{id}`（2x TTL）
- **Key 分片**：4 分片，`sess:msg:{id}:{shard}`（shard = msgIndex % 4），为 Redis Cluster 优化
- **原子追加**：Lua 脚本 GET→修改→SET，失败回退非原子路径
- **上下文压缩**：热消息超 10 条触发异步 `performHotColdSeparation()` → 超 `SummaryThresholdTokens`（28K）触发 `compressContext()`（滑动窗口，保留最近 `MaxHistoryTokens/2` 的消息，其余压缩为摘要）
- **GetContextWindow**：返回摘要（作为系统消息）+ 热消息
