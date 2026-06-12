# 09 — Orchestrator（`internal/orchestrator`）

> ReAct 主循环 + 三级工具分发 + HITL 拦截 + Speculative cache + 中断 + 失败追踪 + 自动测试 —— 把前 8 篇的 LLM client / RAG / Sandbox / MCP / Tools / Skill 全部捏合成一次 `/chat` 请求的完整闭环。

代码：`internal/orchestrator/` 共 42 个文件、约 1.07 万行。核心三件套：

| 文件                       | 行数  | 角色                                                 |
| -------------------------- | ----- | ---------------------------------------------------- |
| `orchestrator.go`          | 1649  | 主结构体 + `ProcessMessage` + `reactLoop` + HITL     |
| `react_core.go`            | 361   | `reactLoopCore` 共享的逐步循环（同步/流式两条路复用） |
| `_principles.go`           | 887   | 设计 RFC（不参与编译）                               |

辅助文件按职责分组：

- **工具子集**：`file_tools.go`（1005）、`git_tools.go`（362）、`lsp_tools.go`（206）、`pty_tools.go`（116）、`builtin_tools.go`（116）、`edit_engine.go`（696）
- **桥接子集**：`planner_bridge.go`（276）、`multiagent_bridge.go`（182）、`temporal_bridge.go`（85）、`memory_bridge.go`（94）、`p1_bridge.go`（61）
- **能力子集**：`speculative_cache.go`（254）、`auto_test_runner.go`（374）、`verification.go`（170）、`failure_tracker.go`（29）、`metacognition.go`（11）、`micro_plan.go`（38）、`context_compaction.go`（134）、`interrupt.go`（110）、`tool_transaction.go`（128）、`project_rules.go`（305）、`parallel_tools.go`（52）、`tool_metadata.go`（77）、`tool_progress.go`（21）、`message_pruner.go`（13）

下文按"主体职责 + 关键路径 + 设计权衡"组织，**不**逐文件介绍 —— 每个辅助文件在主流程的对应位置都会被点到。

---

## 1. 模块定位

Orchestrator 是 Code Agent 的"大脑"。它的职责可以用一行话说清：

> **接收用户消息 → 维护一个 ReAct 循环 → 让 LLM 用工具达成目标 → 返回最终回答。**

但这一行话的下面藏着 13 个并行职责（按重要性排序）：

1. **意图分类**（`parseIntent`）—— 用 LLM 给输入打标签（`code_query` / `code_execute` / `diagnose` / `mcp_call` / `deploy` / `conversation`）；
2. **上下文装配**（`promptBuilder.BuildPrompt`）—— 拉 session 历史 + RAG 命中 + 长期记忆，按 KV-cache-friendly 顺序拼接；
3. **可用工具枚举**（`GetAvailableTools`）—— 合并三个 Registry 的工具定义喂给 LLM；
4. **ReAct 循环**（`reactLoopCore`）—— `LLM → tool_calls → dedupe → execute → observe → repeat`；
5. **工具分发**（`dispatchTool`）—— MCP > internal/tools > skill 三级 fallback；
6. **HITL 拦截**（`suspendForApproval`）—— 命中敏感模式或 `IntentDeploy` 时挂起等审批；
7. **Speculative cache**（`toolCache.Get/Put/Invalidate`）—— 幂等工具结果按 workspace 复用；
8. **并发工具执行**（`parallelExecuteTools`）—— 同批纯读工具并发跑；
9. **失败检测**（`consecutiveFailureTracker`）—— 连续相同工具失败时注入"停一停"提示；
10. **元认知**（`MetacognitiveState`）—— 跟踪信心、重复、不确定性，触发反思；
11. **自动测试**（`RunAutoTestAfterEdit`）—— 文件写入后跑测试，结果回喂 LLM；
12. **中断**（`InterruptSession`）—— 用户从 UI 取消/重定向/暂停；
13. **持久化 / 审计**（`persistTaskCreate` / `persistAudit`）—— 写入 PostgreSQL（best-effort）。

### 1.1 ReAct 循环的真实形状

```
ProcessMessage(user_msg)
    │
    ├─ parseIntent → IntentXxx                        # LLM 调用 #1（含 intent cache）
    ├─ sensitive check → 命中 → suspendForApproval    # HITL 出口
    ├─ MaybeUsePlanner → 走 DAG planner → 早退         # Planner 出口（可选）
    │
    └─ reactLoop
         ├─ Build prompt（system + RAG + history + tools）
         ├─ GetAvailableTools                          # 3 个 Registry 合并
         ├─ reactLoopCore for step in maxSteps:
         │    ├─ context.Done / interrupt 检查
         │    ├─ context compaction（每 5 步）
         │    ├─ reflection checkpoint（每 10 步）
         │    ├─ metacognition reflection（adaptive）
         │    ├─ micro plan 提示（每 8 步）
         │    ├─ tool policy hint（toollearn 来）
         │    ├─ tool distiller recommendation（首步）
         │    ├─ token budget prune
         │    ├─ LLM call（含 3 次指数退避重试）         # LLM 调用 #N
         │    │   ├─ emit llm_call_started              # 见 §1.7 LLM 进度三件套
         │    │   ├─ 每 3s emit llm_call_progress
         │    │   └─ emit llm_call_completed
         │    ├─ if no tool_calls → 终止返回           # 正常出口
         │    ├─ DedupeToolCalls（同步 Runner 走同一 helper）
         │    ├─ 并发/串行 executeTool × N
         │    ├─ 失败追踪 / metacognition 记录
         │    ├─ 自动测试（如果有 file_write）          # 可能 LLM 调用 #N+1
         │    └─ 每 5 步更新 tool policy
         │
         ├─ 步数耗尽 → saveProgressForContinuation
         └─ done → verifyOutput（条件性）              # 可能 LLM 调用 #N+2
```

最大步数由 `getMaxSteps(intent)` 自适应：

| Intent          | maxSteps |
| --------------- | -------- |
| code_query      | 10       |
| code_execute    | 20       |
| diagnose        | 25       |
| mcp_call        | 15       |
| deploy          | 20       |
| 其它（对话/编码） | 50       |

### 1.2 与"统一 Registry 神话"的差距（再次声明）

Orchestrator **不持有**单一 Tool Registry。它持有三个独立来源：

1. `o.toolRegistry *tools.Registry` —— 内置工具（read_file / write_file / git_* / run_tests / shell_exec 等）；
2. `o.mcpGateway *mcp.Gateway` —— 通过匿名 interface 引用，MCP 工具；
3. `o.skillRegistry` —— 通过匿名 interface 引用，用户运行时注册的 webhook/function。

`GetAvailableTools()` 把三者**临时合并**（`orchestrator.go:1532-1545`）返回给 LLM；`dispatchTool` 按 MCP > tools.Registry > skill 顺序逐个尝试匹配（`orchestrator.go:1426-1452`）。

详见 07_tools.md §1.1、08_skill.md §1.1。本文不重复展开。

---

## 1.5 设计哲学

### Q1：为什么选 ReAct 而不是更先进的 Plan-and-Execute？

答：**模型自主决策 vs 显式规划的取舍**。

- **ReAct**（本系统主路径）：每步 LLM 自己决定下一步做什么，工具结果直接回喂；优点是低延迟（无规划阶段）、容错强（LLM 看到错误就换策略）；缺点是步数可能膨胀、容易陷入"用同一锤子敲同一钉子"循环。
- **Plan-and-Execute**（`planner_bridge.go`）：先让 LLM 生成 DAG，再按图执行；优点是步骤可视化、可并行；缺点是初次规划成本高、中间失败需要重新规划。

本系统**两条都接**：默认走 ReAct（`reactLoop`），当 `MaybeUsePlanner` 判定输入足够复杂时切换 Planner 路径。判定规则定义在 `planner_bridge.go`，简化版："用户输入 > N 字 + 含"plan/步骤/regenerate"关键词"。

### Q2：为什么 ReAct 步数上限按 Intent 动态？

固定步数（如旧版的 10 步）在简单问答上浪费 token（LLM 已经回答完了还要被迫继续），在编码任务上又远远不够（实测一个完整功能落地需要 30+ 步：read → edit → test → fix → re-test → ...）。

把"目标复杂度"提前编码到 Intent 标签，再让步数随之扩张，是 LLM-agent 工程的常见模式（如 GPT-Engineer、AutoGPT）。本系统的取值（10/20/25/50）来自生产实测的 P95 步数 + 20% buffer。

### Q3：为什么 Speculative cache 的 scope 是 workspace 而不是 session？

详细推理见 `speculative_cache.go:124-136` 注释，本文摘要：

- **scope = sessionID**：同一 user 开两个 session 同时编辑同一项目，session-A 写 foo.txt 不会失效 session-B 的 foo.txt 读缓存 → **脏读**；
- **scope = workspaceID**：任一 session 对 workspace X 的写都失效 X 的整个读缓存，正确性保证；
- 代价：跨 workspace 的同名读操作无法共享（如 user 切换项目时 `read_file("README.md")` 重头跑），但语义正确性 > 命中率。

实现：`cacheScope()` 从当前 session 解析出 workspaceID（`orchestrator.go` 内）；如果 session 未关联 workspace，fallback 到 sessionID。

### Q4：为什么 LLM 调用要 3 次指数退避重试？

LLM provider（OpenAI / Anthropic / 自部署 vLLM）在以下场景会瞬时失败：

- **rate limit**（429）—— 退避后通常自愈；
- **transient network**（5xx）—— 退避后通常自愈；
- **circuit breaker tripped**（来自 `llm.Client`）—— 退避期间 breaker 可能 half-open 成功。

`react_core.go:170-183` 写死 3 次重试 + `2^attempt` 秒退避（1s → 2s → 4s）。**不**对所有错误重试（如 401/400），但当前代码没区分错误类型 —— 是 P1 待办（见 §11）。

3 次足够覆盖 99% 瞬时故障；超过 3 次说明 provider 真挂了，退化让 ReAct loop 把错误作为 observation 回喂上一层（如果用户在 SSE 流，会看到 `error` event）。

### Q4.5：LLM 进度三件套（finalize 假死的根本对策）

**问题背景**：ReAct 主循环使用**非流式** `llm.Client.ChatCompletion`。当模型在 finalize 阶段生成长答案时，单次调用可能阻塞数分钟乃至 20+ 分钟。这期间业务事件流（thinking/tool_call/tool_result）天然为空，前端 UI 表现为 "Step N/M" 卡住不动，用户与排查者都无法分辨"任务死了" vs "LLM 还在算"。

**对策**：不动 LLM 接口（避免触动 provider streaming、circuit breaker、双链路 fallback），而是在每次 ChatCompletion 调用前后包一层**进度三件套**事件，由 `react_core.go` 的 `callLLMWithProgress` 实现：

| 事件 | 时机 | Content（JSON） |
|---|---|---|
| `llm_call_started` | 调用前同步 emit | `{"attempt":N,"messages":N,"tools":N}` |
| `llm_call_progress` | 调用期间周期 emit（默认 3s 一次） | `{"attempt":N,"elapsed_ms":N}` |
| `llm_call_completed` | 调用返回后同步 emit | `{"attempt":N,"elapsed_ms":N,"err":bool}` |

**关键设计**：

- 周期心跳走 `time.NewTicker(llmProgressInterval)` + 独立 goroutine；返回路径通过 `progressCancel()` + `<-progressDone` 强同步收敛，**无 goroutine 泄漏**。
- 进度事件经由 `persistingSink` 自动入 Redis Stream（`stream_cache.go`），SSE 断线 → Replay/Follow 链路一样能看见已发出的 progress，UI 重连后可恢复当时的 elapsed_ms。
- 3s 间隔的取舍：20min LLM 调用产 ~400 条事件 ~32KB，远低于 `streamMaxLen=2000` 截断阈值；缩短到 1s 会给 Redis 带来 3 倍写入压力，拉长到 10s UX 反馈迟钝。
- `llmProgressInterval` 是包级 `var` 而非 `const`，仅用于测试时缩短间隔（见 `react_core_test.go: TestCallLLMWithProgress_*`）。

**前端契约**：UI 不在 trace 流里新增 step 卡片，而是把最新 `llm_call_progress.elapsed_ms` 内联到当前 step 的 spinner 旁边渲染为 "LLM 处理中 (Ns)"；`llm_call_completed` 后清空标签。详见 `code_agent_ui/src/pages/ChatPage.tsx` 的 ReActTrace 组件。

### Q4.6：工具长跑期间 stdout 沉默,怎么防止前端 watchdog 误判?

**问题**:`toolRunWorkspaceCmd` 跑 `go run`/`go test` 这类命令时,编译冷启动期可能几十秒零 stdout 输出 → 前端 90s watchdog 在沉默 90s 后触发 "网络静默超时"。Q4.5 的 LLM 心跳只覆盖 LLM 调用阶段,工具阶段无对应保护。

**方案(`file_tools.go::toolRunWorkspaceCmd`)**:

- `bufio.Scanner` 跑独立 goroutine,通过 `chan string` 把每一行送给主循环
- 主循环 `select { case <-lineCh; case <-tick.C; case <-cmdCtx.Done() }` 三通道
- `workspaceCmdHeartbeatInterval = 5 * time.Second`:无 stdout 新行也按 5s 周期通过 `progressCb` 推一条心跳文案(`⏱ workspace cmd running %ds, no new output yet\n`)
- 心跳走同一个 `progressCb` → `react_core.go WithProgressCallback` 包装为 `tool_progress` event → `persistingSink` 写 Redis Stream → SSE 推前端 → 前端 onByte 回调重置 `lastEventAt`,watchdog 不再误判

**节奏取舍**:5s × 5min 命令 = 60 条心跳,每条 ~80 字节 ~5KB,远低于 `streamMaxLen=2000` 截断阈值;1s 给前端 + Redis 三倍压力,30s 让 90s watchdog 留余量太小。

**心跳文案不污染 stdout**:心跳走 progressCb,**不**写入 `stdout.WriteString`,LLM 看到的工具结果仍然是纯净的 stdout/stderr,只是用户在 UI trace 中看到一条 progress hint。

**心跳间隔可调**:`workspaceCmdHeartbeatInterval` 是包级 `var`(`file_tools.go:37`),仅用于测试压到 200ms 跑短命令断言节奏,不要改成 `const`。

**命令组合 SIGPIPE warning(B2)**:`validateWorkspaceCommand`(`file_tools.go:127`)签名为 `(rejection, warning string)`:
- `rejection != ""` → 命令被拒绝,直接返错;
- `rejection == "" && warning != ""` → 命令放行,但检测到 `| head` / `| tail` 类管道 —— 长跑生产者会在 N 行后被 `SIGPIPE` 截断,导致"命令成功但输出残缺"的歧义语义。
- 调用方 `toolRunWorkspaceCmd`(`file_tools.go:776-777`)把 warning 拼到 ToolResult.Content 末尾(`⚠️ workspace cmd warning: %s`),让 LLM 下一步可改命令或忽略;PTY 工具 `pty_tools.go` 调用点用 `_` 忽略 warning。

**测试矩阵**:
- `file_tools_test.go::TestToolRunWorkspaceCmd_EmitsHeartbeat` 压间隔到 200ms 跑 `bash -c 'sleep 2'`,断言 heartbeats >= 5 且 `⏱` 不进 ToolResult.Content。
- `file_tools_test.go::TestValidateWorkspaceCommand_*` 覆盖 reject / warn / pass 三态。

### Q5：为什么并发执行只覆盖纯读工具？

写工具（`edit_file` / `write_file` / `git_commit`）有副作用，并发执行会触发竞态：

- 两个 `edit_file` 同时改同一文件 → 文件锁竞争 + 最后写赢；
- `git_add` + `git_commit` 同时跑 → git index lock 冲突；
- `run_tests` 并发 → 测试输出穿插、临时文件冲突。

`canParallelExecute`（`parallel_tools.go:18-29`）只放过 `IsIdempotentTool` 通过的工具（白名单：read_file/list_dir/grep/git_status/git_diff/rag_search/rag_query/repomap/ast_outline）。

收益：一次 LLM 决定调用 3 个 read_file 时，串行 = 3×RTT，并发 = 1×RTT。实测 ReAct 平均步数因此从 5.3 降到 3.8。

---

## 2. 依赖架构

### 2.1 包级依赖（出向）

```
                  ┌─────────────────────────────┐
                  │      Orchestrator            │
                  └─┬───┬───┬───┬───┬───┬───┬───┘
                    │   │   │   │   │   │   │
       ┌────────────┘   │   │   │   │   │   └──────────────┐
       │                │   │   │   │   │                  │
       ▼                ▼   ▼   ▼   ▼   ▼                  ▼
 ┌──────────┐    ┌──────────┐ ┌──────┐ ┌──────────┐  ┌──────────┐
 │ llm      │    │ rag      │ │ mcp  │ │ session  │  │ sandbox  │
 │ .Client  │    │ .Engine  │ │      │ │ .Manager │  │ .Manager │
 └──────────┘    └──────────┘ └──────┘ └──────────┘  └──────────┘

           ┌──────────┐ ┌──────────┐ ┌─────────┐ ┌──────────┐ ┌──────────┐
           │ tools    │ │ skill    │ │ store   │ │ context  │ │ workspace│
           │ .Registry│ │ .Registry│ │ pg ←opt │ │ .Builder │ │ .Manager │
           └──────────┘ └──────────┘ └─────────┘ └──────────┘ └──────────┘

           ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐
           │ memory   │ │ toollearn│ │ agentloop│ │ multiagent│  ← 桥接，按需注入
           │ ←opt     │ │ ←opt     │ │          │ │ ←opt     │
           └──────────┘ └──────────┘ └──────────┘ └──────────┘

           ┌──────────────────────────────────────────┐
           │ Temporal (via TemporalClient interface)  │ ← 占位，未真正接线
           └──────────────────────────────────────────┘
```

### 2.2 强依赖 vs 可选依赖

`NewOrchestrator` 必填参数（`orchestrator.go:157-167`）：

```go
func NewOrchestrator(
    llmClient *llm.Client,         // ❗ 必填
    sessionMgr *session.Manager,   // ❗ 必填（Redis-backed）
    ragEngine *rag.Engine,         // ⚠ 允许 nil（启动时 Qdrant 不通会被 main.go 置 nil）
    sandboxMgr *sandbox.Manager,   // ⚠ 允许 nil（Docker 不通时）
    mcpGateway *mcp.Gateway,       // ⚠ 允许 nil
    securityCfg *config.SecurityConfig,  // ❗ 必填
    logger *zap.Logger,            // ❗ 必填
    pgStore ...*store.Store,       // ⚠ varargs，nil 视为无持久化
) *Orchestrator
```

通过 setter 后注入的（避免 import cycle / 启动顺序问题）：

```go
SetSkillRegistry(...)           // skill 包通过 interface 注入
SetWorkspaceManager(...)        // workspace 包
SetTemporalClient(...)          // temporal_bridge.go
SetMemoryStore / SetMemoryExtractor / SetMultiagent / AttachPlanner / AttachSupervisor
SetToolLearnStore(...)          // toollearn 持久化
SetPTYManager / SetLSPClient / SetTreeSitterParser  // P1 能力
```

每个可选依赖在使用前都 `if o.xxx == nil { return ... }`，缺失不 panic。这是 main.go 用 `Warn + continue` 而不是 `Fatal` 的前提条件。

### 2.3 入向依赖

只有一个真实调用方：`internal/api`。所有 API handler（chat / approval / interrupt / tools-list）都通过 `Server.orchestrator` 调用本包。

`internal/temporal` 反向引用 Orchestrator（通过 TemporalClient interface 桥接），是为了让 Temporal workflow activity 能调用回 `ProcessMessage`（绕过 HITL 二次拦截）。

---

## 2.5 数据流总览

### 流 1：标准 `/chat` 请求（同步）

```
HTTP POST /api/v1/chat
    │
    ▼
api/chat_handler → orch.ProcessMessage(sessionID, userMsg)
    │
    ├─ task := new Task{UUID, sessionID, ...}
    ├─ persistTaskCreate (PostgreSQL, best-effort)
    │
    ├─ if userMsg == "continue" → buildContinuationPrompt
    │     从 workspace 读 .progress.json + .plan.md + tree
    │     拼接成富 context 回灌 userMsg
    │
    ├─ session.AddMessage(role=user)
    │
    ├─ parseIntent(userMsg)
    │     ├─ intentCache 查 LRU（key = hash(userMsg), TTL=5min）
    │     ├─ miss → LLM 调用判定 → cache.Put
    │     └─ task.Intent = 结果
    │
    ├─ containsSensitiveContent(userMsg) || Intent==Deploy
    │     → suspendForApproval
    │     ├─ Temporal path?  → temporal_bridge.go
    │     └─ in-process path：approvalCh + 30 分钟 timeout
    │
    ├─ MaybeUsePlanner(task)?  → planner 路径，早退
    │
    └─ reactLoop(task)
         ├─ RAG retrieve（仅 IntentCodeQuery / Diagnose）
         ├─ promptBuilder.BuildPrompt 装配 messages
         ├─ GetAvailableTools 合并 3 个 Registry
         ├─ reactLoopCore（详见流 2）
         │     ↓ returns content
         │
         ├─ shouldVerifyOutput? → verifyOutput（LLM 调用）
         │     失败 → emit verification_warning SSE 事件（前端展示 ⚠️ 评分+issues）
         │            ├─ decideVerificationFollowup 通过：
         │            │   把 critique 作为 RoleUser 入 messages，continue 外层 for（仅 stream，每 task 限 1 次）
         │            └─ 否则：仅 logger.Warn，正常 done
         │     sync 路径不 retry（保留延迟预算）
         │
         └─ session.AddMessage(role=assistant) + extractMemoriesAsync
    │
    ▼
HTTP 200 { session_id, task_id, message, state }
```

### 流 2：reactLoopCore 单步细节

```
for step := 0; step < maxSteps; step++:
    globalStep++

    ├─ ctx.Done? → 提早返回
    ├─ interruptCh 非空? → 处理 cancel/redirect/pause

    ├─ compactEarlyMessages(messages, step)
    │     ├─ truncate 模式（默认）：head 400 + 中间省略 + tail 400
    │     └─ summarize 模式：LLM 调用生成 2-3 句摘要（5s timeout）
    │     仅对 RoleTool 消息 + 保留最近 3 条

    ├─ reflectionCheckpoint：每 10 步注入「检查计划」system 消息
    ├─ meta.NeedsReflection? 注入自适应反思
    ├─ microPlanPrompt：每 8 步让 LLM 申报"接下来 3 步计划"
    ├─ toolPolicy.FormatContextHint(lastToolName)
    ├─ toolDistiller.FormatRecommendation（首步）

    ├─ Token budget check：总 token > maxContextTokens → pruneMessages

    ├─ LLM 调用（3 次重试，指数退避）
    │     ├─ tools 字段 = GetAvailableTools 结果
    │     └─ responseFormat 可选（结构化输出）

    ├─ resp.Content != "" → emit thinking / message event

    ├─ len(resp.ToolCalls) == 0:
    │     ├─ meta.NeedsReflection && step > 2 && !verificationAttempted:
    │     │     注入「确认信心」system 消息，continue（不返回，再问一轮）
    │     └─ 返回 final answer

    ├─ dedupedCalls = agentloop.DedupeToolCalls(resp.ToolCalls, logger)
    │     （按 Name + 规范化 JSON Args 折叠重复——长 ReAct loop 偶发 LLM
    │      在同一步重复输出相同工具调用；不去重的话 HITL 审批会被要求
    │      两次、SSE UI 渲染幽灵卡片、tool_calls↔tool_results 对应错乱）

    ├─ messages.append(assistant, resp.Content, dedupedCalls)

    ├─ canParallelExecute(dedupedCalls)?
    │     ├─ Y: parallelExecuteTools → 并发跑
    │     └─ N: 串行 executeTool，注入 progress callback

    ├─ for each tool result:
    │     ├─ 智能截断（>32K chars 时保留头 8K + 尾 12K）
    │     ├─ ClassifyToolError + adaptiveFB.BuildFeedback（追加 [SYSTEM HINT]）
    │     ├─ meta.RecordOutcome
    │     ├─ emit tool_result event
    │     ├─ messages.append(tool, content, toolCallID)
    │     ├─ failTracker.track → 重复失败时插入 stepBackMessage

    ├─ if 有 file_write tools (triggersAutoTest):
    │     ├─ RunAutoTestAfterEdit
    │     └─ messages.append(system, test report)

    └─ if step % 5 == 0: toolPolicy.Update()

步数耗尽 → return hitStepLimit=true → 上层 saveProgressForContinuation
```

### 流 3：executeTool 内部

```
executeTool(tc):
    ├─ RiskLevel >= 2 && !skipHITL(ctx)?
    │     ├─ ctx 含 task + sink（react_core 已注入）?
    │     │     → waitToolApproval(task, tc, sink):
    │     │         · SSE Emit approval_request 事件（含 tool_name / args / risk）
    │     │         · toolApprovalCh[task.ID] = chan，阻塞最多 5 分钟
    │     │         · HandleApproval 通过 /tasks/:id/approve 投递 → 唤醒
    │     │         · approved → fallthrough；rejected → IsError；timeout → IsError
    │     └─ 否（multiagent / planner 等 caller，ctx 无 task/sink）:
    │           → 旧 fail-safe：直接返回 IsError=true 提示 "requires approval"
    │
    ├─ toolCache.Get(scope, name, args)?  → 命中直接返回
    │
    ├─ captureForTransaction(tc)  → 写工具时记录 baseline 供回滚
    │
    ├─ dispatchTool:
    │     ├─ Tier 1: mcpGateway.FindServerForTool → CallTool
    │     ├─ Tier 2: toolRegistry.Get → Execute
    │     ├─ Tier 3: skillRegistry.FindSkill → Execute
    │     └─ 全部 miss → ToolResult{IsError, "Unknown tool"}
    │
    ├─ toolCollector.Record（toollearn 反馈）
    │
    ├─ toolCache.Put（仅成功 + 幂等工具）
    │
    └─ toolCache.shouldInvalidate? → toolCache.Invalidate(scope)
```

### 流 4：HITL Approval 闭环（两条独立路径）

```
路径 A —— 任务级（sensitive content / IntentDeploy）
─────────────────────────────────────────────────
sensitive check 命中
    │
    ▼
suspendForApproval(task)               → 整个 task 暂停，HTTP 立即返回
    │
    ├─ temporalClient != nil?
    │     → suspendForApprovalTemporal（temporal_bridge.go，main.go 已 wire worker）
    │
    └─ suspendForApprovalInProcess:
         ├─ approvalCh[taskID] = make(chan, 1)
         └─ go: 等 30 分钟
              select <-ch: approved → reactLoop(skipHITL ctx) → 后台跑完入 session
              select <-ch: rejected → done
              <-timeout → HITLApprovalTotal{"timeout"} ++

路径 B —— 工具级（RiskLevel >= 2，2026-06-03 引入）
─────────────────────────────────────────────────
ReAct 循环里 LLM 选了一个高风险工具
    │
    ▼
executeTool → waitToolApproval(task, tc, sink)   → 单次 tool call 同步阻塞
    │
    ├─ sink.Emit(ReactStreamEvent{Type: "approval_request", task_id, tool_name, args, risk})
    ├─ toolApprovalCh[task.ID] = make(chan, 1)
    └─ select 等 5 分钟

前端 SSE 收到 approval_request → ChatPage 渲染 Approval 模态框
    │  （tool_approval.go + ChatPage.tsx）
    ▼
用户点击 → POST /api/v1/tasks/:id/approve
    ▼
HandleApproval(resp):
    ├─ toolApprovalCh[taskID] 存在? → 投递 → 唤醒 waitToolApproval（路径 B）
    ├─ Temporal? → HandleApprovalTemporal（路径 A durable）
    └─ approvalCh[taskID] → 投递 → 唤醒 suspendForApprovalInProcess goroutine（路径 A in-process）
    ▼
路径 A：ChatResponse{State: Executing, "approved"} + 后台 reactLoop 续跑
路径 B：waitToolApproval 返回，executeTool 继续走原流程（命中缓存 / dispatch）
```

> 同一 task 同时只能有一个工具级 pending approval（ReAct 是串行的）。如果重复触发，`waitToolApproval` 返回 "another tool approval already pending" 错误。

### 流 5：中断信号

```
用户点击 UI "Cancel"
    │
    ▼
HTTP POST /api/v1/sessions/:id/interrupt {type: "cancel"}
    │
    ▼
api → orch.InterruptSession(sessionID, signal)
    │
    ├─ interruptCh[sessionID] 存在?
    │     ├─ 是：select case ch <- signal: default
    │     │      （buffer=1，已有信号则 drop 新的）
    │     └─ 否：return false（无活跃 loop）
    │
    ▼
reactLoopCore 下一次迭代开头：
    select <-interruptCh:
        case "cancel":   返回 "Task cancelled by user"
        case "redirect": 返回 "" + 触发外层 redirect（实际未接入主流程）
        case "pause":    返回 "Task paused"
```

⚠ **interrupt 仅在迭代边界检查**，正在执行中的 tool（如长 shell 命令）不会被打断。这是 Go 标准 context.Done 模式 —— 工具本身需检查 ctx 才能响应。当前 `executeTool` 已传递 ctx 给下游，但 `parallelExecuteTools` 内的 wg.Wait 不响应 ctx，是潜在 P1。

---

## 3. Orchestrator 结构体字段（`orchestrator.go:62-148`）

按职责分组解读 35 个字段（实际数量随版本变化）：

### 3.1 必填依赖

```go
llmClient      *llm.Client
sessionMgr     *session.Manager
ragEngine      *rag.Engine               // 允许 nil
sandboxMgr     *sandbox.Manager          // 允许 nil
mcpGateway     *mcp.Gateway              // 允许 nil
promptBuilder  *agentctx.PromptBuilder
securityCfg    *config.SecurityConfig
sensitiveRules []*regexp.Regexp          // 启动时编译，hot path 直接 match
logger         *zap.Logger
store          *store.Store              // PostgreSQL，nil = 禁用持久化
```

### 3.2 三个 Tool 来源

```go
toolRegistry  *tools.Registry            // 内置工具
mcpGateway    *mcp.Gateway               // 同上
skillRegistry interface{...}             // 通过 setter 注入，匿名 interface
```

### 3.3 ReAct 辅助能力

```go
editEngine       *EditEngine             // 精准编辑（unique-match + backup + lint）
autoTestRunner   *AutoTestRunner         // TDD 自检环
toolCache        *SpeculativeToolCache   // 幂等工具结果缓存
ruleLoader       *RuleLoader             // 项目规则注入（.agentrules.md）
trajectoryMem    *agentloop.TrajectoryMemory   // 成功轨迹复用
maxContextTokens int                     // 动态 token budget（来自 LLM provider config）
compactionMode   string                  // "truncate"（默认）或 "summarize"
```

### 3.4 P1 能力（按 interface 持有，运行时注入）

```go
ptyManager  PTYManager       // 持久 shell 会话（pty_tools.go）
lspClient   LSPClient        // LSP-aware 代码智能（lsp_tools.go）
tsParser    TreeSitterParser // tree-sitter AST（与 RAG 共用）
```

三个都允许 nil。如 LSP 未配置，`lsp_tools.go` 内的 `goto_definition` 等工具返回 "LSP not available"。

### 3.5 P2-D Tool Learning（`internal/toollearn`）

```go
toolCollector *toollearn.Collector       // 收集每次工具调用反馈
toolAdvisor   *toollearn.Advisor         // 给 LLM 推荐工具
toolPolicy    *toollearn.AdaptivePolicy  // 自适应工具偏好
toolDistiller *toollearn.Distiller       // 提取策略知识
```

构造时全部 `NewXxx`，启动即生效。学习数据存内存（`Collector.SetStore` 可挂 PG）。

### 3.6 并发控制 map

```go
approvalCh    map[string]chan models.ApprovalResponse  // HITL，按 taskID
interruptCh   map[string]chan InterruptSignal          // 中断，按 sessionID
txMap         map[string]*ToolTransaction              // 写工具回滚，按 sessionID
intentCache   map[string]intentCacheEntry              // 意图分类 LRU
approvalMu/interruptMu/txMu/intentCacheMu              // 各自的 RWMutex
```

每个 map 独立锁。整个 Orchestrator 不持有"全局锁"，每条 hot path 锁不同的 map，互不阻塞。

### 3.7 桥接组件（按需注入，nil 默认禁用）

```go
planner         *plannerComponents       // planner_bridge.go
supervisor      *multiagent.Supervisor   // multiagent_bridge.go
temporalClient  TemporalClient           // temporal_bridge.go
memoryStore     MemoryRetriever          // memory_bridge.go
memoryExtractor *memory.Extractor
```

---

## 4. 核心入口对比：`ProcessMessage` vs `ProcessMessageStreamFull`

两个入口共享 `reactLoopCore`，差异在外层装配 + 事件发射：

| 维度          | ProcessMessage（同步）      | ProcessMessageStreamFull（流式）        |
| ------------- | --------------------------- | -------------------------------------- |
| 调用方        | `/api/v1/chat`              | `/api/v1/chat/react-stream` (SSE)       |
| 返回          | `*models.ChatResponse`      | `<-chan models.ReactStreamEvent`        |
| sink          | `noopSink{}`                | `&channelSink{ch, droppedCtx}`          |
| 适用场景      | 工具脚本、CI、HTTP 客户端   | 浏览器 UI、终端交互                     |
| HITL 处理     | 同上                        | 同上（同样的 suspendForApproval）       |
| 中断响应      | 同上                        | 同上                                    |

`channelSink.Emit` 把每个内部事件（step_start / thinking / tool_call / tool_progress / tool_result / message / error）按 SSE 格式写入响应流。前端按 event type 分类渲染。

**关键**：`noopSink` 让同步路径**无开销**地复用流式逻辑——`sink.Emit` 调用变成空函数。

### 4.1 业务 ctx 与 HTTP ctx 解耦（2026-06-04）

**症状**：`Processing... (step 40/200)` 永久卡死，docker 日志显示 t+619s 时 SSE 被 `server.write_timeout: 600s` 撕掉，HTTP ctx 取消并一路传播到 LLM/工具，orchestrator 静默 return 未发终态，UI 永远旋转。

**根因**：`ProcessMessageStreamFull` 直接收下 `c.Request.Context()` 当业务 ctx，业务存活强绑 SSE TCP 连接活性。

**拓扑（修复后）**：

```
reqCtx (HTTP)                   workCtx (background + 30min)
    │                                │
    │ (cancel when SSE 撕掉/客户端关页)
    │
    ▼
桥接 goroutine                   ▼
    │                       parseIntent
    │ dropCancel()          reactLoopCore  ←─ 所有业务调用走 workCtx
    │                       sessionMgr/RAG
    ▼                       HITL/Temporal
droppedCtx                       │
    │                            │
    └─► channelSink.Emit         ▼
        客户端断后非阻塞丢弃事件   终态事件 + AddMessage 持久化
```

**关键设计点**：

1. `workCtx` 仅在 (a) 业务自然结束 (b) 30min 兜底超时 时被 cancel；`reqCtx` 死掉不传染。
2. `channelSink{droppedCtx}` 把客户端断连翻译为"非阻塞丢事件"，避免 cap=64 buffer 满后阻塞业务 goroutine（这是旧实现"任务挂死"的真凶之一）。
3. `react_core.go::reactLoopCore` 步循环 `case <-ctx.Done()` 与 LLM retry `case <-ctx.Done()` 均补 `sink.Emit(error)`，保证"无论 ctx 怎么死都有终态事件可投递"——客户端能收到就收到，已断的也无所谓（业务结果由 `sessionMgr.AddMessage` 持久化，重连后可查到）。
4. `tool_approval.go::waitToolApproval` 无需改：它通过 `reactLoopCore` 传入 ctx，新拓扑下天然就是 `workCtx`，30min 边界与 `toolApprovalTimeout` 双重保护。
5. SSE 心跳（`internal/api/handlers.go::runSSEHeartbeat`，25s 周期，注释行 `: ping\n\n`）消除了 `write_timeout: 600s` 在合法长任务中的触发条件——只要业务 goroutine 还在 push 心跳，连接不会因 idle write 超时被撕。

**契约测试**：`internal/orchestrator/stream_decouple_test.go` 四例覆盖 `channelSink` 非阻塞语义、保序投递、nil droppedCtx 退化为阻塞 send、桥接拓扑下 workCtx 不被 reqCtx 取消传染。

**不做**：Redis pub/sub 任务事件流（"真断线续看"独立议题）；改 `toolApprovalCh` 的内存 map 结构。

---

## 5. dispatchTool 三级 fallback（再次精讲）

`orchestrator.go:1426-1452`：

```go
func (o *Orchestrator) dispatchTool(ctx, tc, start) (*ToolResult, error) {
    // Tier 1: MCP（最优先，因为 MCP 工具可能 shadow 内置名）
    if o.mcpGateway != nil {
        if serverName, ok := o.mcpGateway.FindServerForTool(tc.Name); ok {
            result, err := o.mcpGateway.CallTool(ctx, serverName, tc.Name, tc.Args)
            metrics.MCPCallTotal.WithLabelValues(serverName, tc.Name, statusLabel(err)).Inc()
            metrics.MCPCallDuration.WithLabelValues(serverName).Observe(time.Since(start).Seconds())
            return result, err
        }
    }
    // Tier 2: 内置 + file + git + lsp + pty
    if o.toolRegistry != nil {
        if _, ok := o.toolRegistry.Get(tc.Name); ok {
            return o.toolRegistry.Execute(ctx, tc.Name, tc.Args)
        }
    }
    // Tier 3: 动态 skill
    if o.skillRegistry != nil {
        if _, ok := o.skillRegistry.FindSkill(tc.Name); ok {
            return o.skillRegistry.Execute(ctx, tc.Name, tc.Args)
        }
    }
    return &ToolResult{Content: fmt.Sprintf("Unknown tool: %s", tc.Name), IsError: true}, nil
}
```

**设计决策**：

1. **MCP 最优先** —— 允许通过 MCP 名"覆盖"内置工具（如某 MCP 提供 `read_file` 时优先用）。当前没有这种冲突，但保留了语义。
2. **internal/tools 第二** —— 占绝大多数实际调用，因为内置工具集最完整。
3. **skill 最后** —— webhook skill 是运维端注册，优先级最低，避免误覆盖核心工具。
4. **Unknown tool 不报错（Go-level）** —— 返回 `ToolResult{IsError: true}`，让 LLM 看到"工具不存在"作为 observation，自主换工具。如果返回 Go-level error 会中断 ReAct loop，失去恢复机会。

**未做的事**：

- 没有"按 source 路由"（如果 LLM 给出 `tool_call{name: "skill:foo"}` 这种带前缀的名字，本系统**不**特殊处理，按字面查 map）；
- 没有"工具名冲突告警"——如果同时有 MCP 和内置同名工具，静默走 MCP，不打 warn 日志。

---

## 6. HITL 子系统深入

### 6.1 触发条件（`orchestrator.go:335`）

```go
if !skipHITL(ctx) && (o.containsSensitiveContent(userMessage) || intent == models.IntentDeploy) {
    o.persistTaskState(ctx, task.ID, models.TaskStateSuspended)
    return o.suspendForApproval(ctx, task)
}
```

两个条件：

1. **正则匹配敏感模式**（`containsSensitiveContent`）—— 用 `securityCfg.SensitivePatterns` 编译的 regexp 数组匹配 userMessage。默认配置（`configs/config.example.yaml`）包含 `rm -rf`, `DROP TABLE`, `kubectl delete`, `git push --force` 等；
2. **Intent 是 Deploy** —— 部署类任务一律审批，无条件。

`skipHITL(ctx)` 从 context 取标志位 —— 由 `suspendForApprovalInProcess` 在审批通过后给重入的 `reactLoop` ctx 注入，避免无限循环审批。

### 6.2 In-process Approval（`orchestrator.go:651-711`）

```
approvalCh[taskID] = make(chan, 1)
go func() {
    select {
    case resp := <-ch:
        if resp.Approved:
            reactLoop(ctx_with_skipHITL, task)
            session.AddMessage(assistant, result)
        else:
            log("rejected")
    case <-time.After(30 * time.Minute):
        HITLApprovalTotal{"timeout"}++
    }
}()

return ChatResponse{State: Suspended, Approval: ...}
```

**关键点**：

- 立即返回 `Suspended` 状态给客户端，不阻塞 HTTP；
- 后台 goroutine 等审批，跨 HTTP 请求边界 —— 用户点 Approve 由**另一个**HTTP 请求触发，但执行后续 reactLoop 是在原后台 goroutine 完成的；
- 结果通过 `session.AddMessage` 写回 session，下次客户端轮询/SSE 能看到；
- **30 分钟超时**写死，不可配置。审批超时不写回 session，下次客户端轮询会看到任务卡在 Suspended。

### 6.3 Temporal Approval（`temporal_bridge.go`）

```
suspendForApprovalTemporal:
    1. 启动 workflow.SignalWithStart(workflowID = taskID)
    2. workflow.Await(approval signal, timeout=24h)
    3. signal 收到 → activity.ExecuteTaskActivity(skipHITL ctx)
    4. activity 内调用 orch.ProcessMessage（重入，但跳过 HITL）
```

**接线状态**（2026-06-03 起）：

- `temporal_bridge.go` 代码完整；
- `cmd/agent/main.go:723` 调 `startTemporalWorker` → `Dial` + `RegisterWorkflow(AgentTaskWorkflow)` + `RegisterActivity(activities)` + 非阻塞 `Start`；
- **Dial 失败降级**：`temporalClient` = nil，整个 HITL 仍由 `suspendForApprovalInProcess` 兜底，HTTP 路径保留可用；
- **结果**：默认 compose 起栈 Temporal 在线时走 durable approval；Temporal 不可用时优雅降级，行为可观察。

### 6.4 Tool-level HITL（`orchestrator.go:1369` + `tool_approval.go`）

2026-06-03 起从"硬阻断"升级为真正的审批闭环。`executeTool` 检测 `RiskLevel>=2` 时：

```go
if !skipHITL(ctx) {
    if def, ok := o.getToolRiskLevel(tc.Name); ok && def.RiskLevel >= 2 {
        task := taskFromCtx(ctx); sink := sinkFromCtx(ctx)
        if task != nil && sink != nil {
            approved, err := o.waitToolApproval(ctx, task, tc, def, sink)
            // ... 批准 fallthrough；拒绝 / 超时 / err → IsError 返回
        } else {
            // multiagent / planner 等无 task+sink 的 caller：保留旧 fail-safe 硬阻断
        }
    }
}
```

`waitToolApproval`（`tool_approval.go`）的关键步骤：

1. `sink.Emit(ReactStreamEvent{Type: "approval_request", ToolName, ToolArgs, TaskID, Metadata})` —— 前端 ChatPage 已经监听这个事件并渲染 Approval 模态框
2. 占用 `toolApprovalCh[task.ID]`（同一 task 同时只允许一个 pending 工具，ReAct 是串行的）
3. `select` 等 5 分钟：`<-ch` → 用户答复；`<-waitCtx.Done()` → 超时
4. `HandleApproval(resp)` 收到 `POST /tasks/:id/approve` 时**优先**查 `toolApprovalCh`，命中则投递并立即返回；未命中再走任务级 `approvalCh` 与 Temporal

**两条路径并存**：

| 路径 | 触发  | 暂停粒度 | 通道                     | 持久化 |
| ---- | ----- | -------- | ------------------------ | ------ |
| A    | sensitive content / IntentDeploy | 整个 task | `approvalCh` / Temporal | 任务级，可重启续 |
| B    | 工具 RiskLevel>=2 | 单次 tool call | `toolApprovalCh`（进程内） | 否，进程重启则 timeout |

`RiskLevel` 字段在 `ToolDefinition` 上，由各 builtin 工具自行声明（参见 `file_tools.go` / `git_tools.go`）。当前生产配置：

| 工具                     | RiskLevel | 缓存 | 备注 |
| ------------------------ | --------- | ---- | ---- |
| `read_file` / `list_files` | 0 | 可缓存（`IsIdempotentRead=true`） | safe，纯只读 |
| `search_code`            | 0 | **豁免缓存** | RAG 语义检索，索引重建会改变结果，`tool_metadata_test.go` 明确标注 intentional exemption |
| `run_tests`              | 0 | **不可缓存** | 在 Docker sandbox 内执行（隔离），结果与 sandbox 状态相关，每次都跑 |
| `git_status` / `git_diff` / `git_log` | 0 | — | git 只读 / 元数据查询 |
| `git_branch`             | 0 | **写入即失效 scope**（`InvalidatesCache=true`） | 创建或切换分支会改写 working tree 内容，缓存的文件读结果可能过期 |
| `write_file` / `patch_file` / `edit_file` / `apply_diff` | 1 | 写入即失效 scope | 工作区已隔离 |
| `run_workspace_cmd`      | 1 | 不缓存 | 宿主 shell 命令，2026-06-05 起从 2 降为 1：`validateWorkspaceCommand` 白名单 + `minimalCommandEnv` env scrub 已是静态护栏，常规工作区命令不再走工具级审批（详见 07_tools.md §8） |
| `git_commit`             | 2 | 不缓存（`InvalidatesCache=true`） | 改写本地 git 历史，触发审批 |

> 当前 `git_tools.go` 不注册 `git_push`；若将来加入，必然 RiskLevel=2（远端副作用），同样走工具级审批。

---

## 7. Speculative Cache 集成详细

### 7.1 生命周期

```
NewOrchestrator
    └─ toolCache := NewSpeculativeToolCache(0, logger)   # TTL 默认 30s

    └─ toolCache.SetMetadataLookup(o.toolMetadata)
       # 让 cache 通过 toolRegistry 查 ToolDefinition.IsIdempotentRead

executeTool 入口:
    ├─ scope := cacheScope()                # workspace ID 优先
    ├─ if cached := toolCache.Get(scope, name, args); hit:
    │       return cached, nil              # 命中直接返回
    │
    ├─ result, err := dispatchTool(...)
    │
    ├─ if err == nil && !result.IsError:
    │       toolCache.Put(scope, name, args, result)
    │
    └─ if toolCache.shouldInvalidate(name):  # 写工具
            toolCache.Invalidate(scope)
```

### 7.2 metadata 联动

`SetMetadataLookup` 让 cache 不依赖硬编码白名单 —— 直接查 `tools.Registry` 里的 `ToolDefinition.IsIdempotentRead` / `IsFileWrite` / `InvalidatesCache` 三个 metadata bit。新增内置工具时只需在 `tool_metadata.go` 声明，cache 自动正确分类。

如果工具不在 Registry（如 MCP 工具或 webhook skill），fallback 到包级 `idempotentTools` 硬编码白名单。这两类工具基本都是"未知是否幂等" → 保守地视为写工具 → 不缓存。

### 7.3 失效语义

写工具执行后 `toolCache.Invalidate(scope)` **清空整个 workspace 的所有缓存**，不是只清相关键。代价：粒度粗；收益：实现简单，不会出现"应清未清"的脏数据。

实测：单次 `edit_file` 会让 workspace 下后续所有 `read_file/list_dir/grep` 都重新跑。这是想要的语义。

---

## 8. 失败追踪 + 元认知

### 8.1 consecutiveFailureTracker（`failure_tracker.go`）

委托给 `agentloop.ConsecutiveFailureTracker`。逻辑：

- 当 LLM **连续** 3 次调用**同名工具失败**时，自动注入一条 system message：「停一停，让我重新审视方法」；
- 这条 message 让 LLM 跳出"用同一锤子敲同一钉子"循环；
- 计数器在工具名变化时清零，不会无限累加。

### 8.2 MetacognitiveState（`metacognition.go` + `metacognition_test.go`）

跟踪三个指标：

- `successRate` —— 工具成功率（滑动窗口）；
- `repeatRate` —— 同工具重复调用频率；
- `uncertaintyTags` —— 累积的不确定性来源（"recent tool failure: X"）；

当 `successRate < 0.5` 或 `repeatRate > 0.7` 时，`NeedsReflection()` 返回 true，触发：

1. 注入 `AdaptiveReflectionMessage`；
2. LLM 给出 final answer 时，如果 `NeedsReflection`，额外加一轮「确认信心」验证（`react_core.go:200-211`）。

### 8.3 三个交叉

```
失败 → failTracker.failCount++
       meta.AddUncertainty(...)
       meta.RecordOutcome(name, success=false, repeat=true)

每 10 步 → reflectionCheckpoint
每 N 步（自适应）→ meta.NeedsReflection → 注入反思
final answer 但 meta.NeedsReflection → 强制验证一轮
```

这三层是**串联**的：单点失败 → 短期建议（step back）；长期低质量 → meta 反思；最终回答低信心 → 强制验证。

---

## 9. 自动测试 + 验证

### 9.1 RunAutoTestAfterEdit（`auto_test_runner.go`）

触发条件：`o.triggersAutoTest(toolName)` 为 true 且工具成功。`triggersAutoTest` 检查 `ToolDefinition.TriggersAutoTest` metadata bit（设在 file_tools 注册时）。

执行流程：

1. 从所有写工具的 args 提取 `path` 字段（`edit_file` / `write_file` / `apply_diff` 等都用 `path`）；
2. 路径过滤：仅对源代码文件触发（.go/.py/.ts/.js 等）；
3. 调用 `findRelevantTests(path)` —— 同包/同模块的测试文件；
4. 调用 `runTests(testPaths)` —— 通过 `tools.run_tests` 工具执行；
5. 把结果格式化成 system message 注入下一轮 LLM。

**消耗**：增加 LLM 上下文 + 一次 shell 命令耗时（通常几秒至几十秒）。生产实测 ReAct 步数从平均 8 → 6，因为 LLM 不需要主动跑测试。

### 9.2 verifyOutput（`verification.go`）

仅当 `shouldVerifyOutput(intent, stepsUsed)` 为 true 时触发（高风险 Intent —— Deploy/CodeExecute/Diagnose —— 或 stepsUsed > 5）。流程：

1. 用一个独立 LLM 调用让模型自评 "答案是否充分回答用户问题"（`verification.go:24` 入口，`verification.go:29` `verifyOutputWithLLM` 可测实现）；
2. 返回 `{passed, score, issues, suggestions, reasoning}`；
3. `passed=false` 时分两步处理（**仅 stream 路径**，见 `orchestrator.go:1107` `ProcessMessageStreamFull` 内调点）：

   **a. emit `verification_warning` SSE 事件**（`orchestrator.go:1128`，`Metadata = {score, issues, reasoning, retrying}`）。这是「独立审查可见」的接线：前端 ChatPage 渲染 ⚠️ 评分条 + issues 清单，把评判者视角直接展示给用户。

   **b. retry-once 自修复**：通过 `decideVerificationFollowup(retried, globalStep, absoluteMaxSteps, vResult)`（`verification.go:193`）做决策 —— 若 `!task.VerificationRetried && globalStep < absoluteMaxSteps-3`，则：
   - 置 `task.VerificationRetried = true`（`orchestrator.go:1136`，运行时哨兵，定义在 `internal/models/models.go:99` 带 `json:"-"`，**不入 JSON 序列化**避免污染持久化的 task 状态）；
   - 把刚才的 assistant content + `formatVerificationFeedback(vResult)` push 回 `messages`；
   - `continue` 外层 for 循环，让 LLM 看到 critique 后重新决定是否调工具补救。

   每个 task 最多 retry 一次，避免 0.2→0.2→0.2 死循环。若 retry 后第二轮再次失败，只发 warning（`retrying=false`）然后正常 done。

**sync 路径**（`ProcessMessage`，`orchestrator.go:566` 调点）**保持仅 `logger.Warn`**，因为同步 API 通常有严格延迟预算，二次 LLM 调用会让调用方超时。

**关键不变量**：
- `formatVerificationFeedback` 永远以 `RoleUser` 入 messages —— 让 LLM 把它当用户反馈，触发再一轮 ReAct，而非误以为是上一轮自己的话；
- warning payload 与前端 `VerificationWarningMetadata` 类型字段一一对应，新增字段须同步 `code_agent_ui/src/types/index.ts`；
- `verification_warning` 是 `models.ReactStreamEvent.Type` 的合法值之一（`internal/models/models.go:360` 注释枚举）—— 新增需同步该注释 + 前端 ReactStreamEventType union。

**测试矩阵**（`verification_test.go`）：

| 测试 | 行号 | 覆盖 |
|---|---|---|
| `TestParseVerificationResponse_Valid` | 12 | 标准 JSON 解析 |
| `TestParseVerificationResponse_WithCodeFence` | 37 | LLM 输出带 ```json fence 的鲁棒解析 |
| `TestParseVerificationResponse_ClampsScore` | 53 | score 越界（>1 / <0）夹紧 |
| `TestShouldVerifyOutput` | 73 | Intent + stepsUsed 阈值矩阵 |
| `TestFormatVerificationFeedback_Passed` / `_Failed` | 97 / 119 | 格式化文案（passed 不含 critique，failed 含 issues+suggestions） |
| `TestDecideVerificationFollowup_RetryOnLowScore` | 162 | 首次失败 → 准许 retry |
| `TestDecideVerificationFollowup_NoDoubleRetry` | 192 | `task.VerificationRetried=true` → 第二次失败只 warning，禁止 retry |
| `TestDecideVerificationFollowup_StepBudgetGuard` | 209 | `globalStep >= absoluteMaxSteps-3` → 步数预算耗尽，禁止 retry |
| `TestVerifyOutput_Integration` | 236 | 端到端：mock LLMCaller 串联整个 verify 流程 |

最后三个 `DecideVerificationFollowup_*` 测试构成 retry-once 门控的真值表：先决条件（`!retried`）、循环防御（`!retried && retried`）、预算护栏（`stepsUsed >= max-3`）。

---

## 10. 中断 + 工具事务

### 10.1 ToolTransaction（`tool_transaction.go`）

写工具执行前 `captureForTransaction(tc)` 给当前 session 的 `ToolTransaction` 加一条 baseline：

```
ToolTransaction{
    sessionID,
    actions: [
        {tool: "edit_file", path: "x.go", before: "<原内容>"},
        {tool: "git_add",   path: "x.go", before: "<unstaged>"},
        ...
    ]
}
```

中断信号 = "cancel" 时，理论上可以**反序回滚**所有 action（还原文件、git reset）。**当前实现**：`tool_transaction.go` 提供了 `Rollback` 方法，但 `checkInterrupt` 收到 cancel 时**没有**调用 `Rollback` —— 只是返回 "Task cancelled" 字符串，文件改动保留。

这是 P1 待完善：完整 Rollback 需要先实现 file_tools 端的 before/after 捕获（当前部分捕获），再让 cancel 路径调用 Rollback。

### 10.2 三种中断的语义

| 类型     | 当前行为                                | 设计意图                                  |
| -------- | --------------------------------------- | ----------------------------------------- |
| cancel   | 返回 "Task cancelled by user"，loop 退出 | 完整放弃，应回滚（未接线）                |
| redirect | 返回空，**未真正接入主流程**             | 替换 userMsg 重新跑 reactLoop（未实现）   |
| pause    | 返回 "Task paused"，loop 退出            | 暂停 + 等用户 "continue"（continue 已实现） |

`pause` 实际等价于 `cancel`，但用户收到的字符串提示不同。`redirect` 完全未接入。

---

## 11. 后续演进（P0 / P1 / P2）

### P0（已知风险，必须修复）

1. **Temporal HITL 未接线但代码存在**
   - `temporal_bridge.go` 假装能用；
   - main.go 的 `initTemporalWorker` 是 placeholder（commented out）；
   - 修复：要么真接线（启动 worker、注册 workflow/activity），要么删 bridge + 文档标记 not implemented。

2. **Tool transaction Rollback 未在 cancel 路径调用**
   - `captureForTransaction` 已经写了 before-state；
   - `checkInterrupt(cancel)` 只返回字符串，不 Rollback；
   - 修复：cancel 路径调用 `tx.Rollback(ctx)`。

3. **interrupt 信号不响应正在执行的工具**
   - `parallelExecuteTools` 内 `wg.Wait` 不 select ctx；
   - 长跑工具（shell_exec、run_tests）会无视 cancel；
   - 修复：parallelExecuteTools 增加 ctx 监听 + 让正在跑的工具 ctx-cancel。

4. **LLM 重试不区分错误类型**
   - 401/400 等不该重试的错误也走 3 次退避；
   - 浪费时间，且如果是 prompt injection / 配额错误会延迟暴露；
   - 修复：`react_core.go:170-183` 加错误类型判断（IsRetryable）。

### P1（功能完善）

5. **意图分类 LRU 没有大小上限**
   - `intentCache` 用 map 实现，无淘汰；
   - 长时间运行 + 大量唯一输入会撑大内存；
   - 修复：换成有 size 上限的 LRU（如 `hashicorp/golang-lru`）。

6. **30 分钟审批超时不可配置**
   - 写死；
   - 修复：从 `config.SecurityConfig.ApprovalTimeout` 读取。

7. **redirect 中断未实现**
   - 当前 case "redirect" 只返回空字符串，不真正切换 userMsg；
   - 修复：要么接入（在 reactLoop 外层捕获 redirect，重启 reactLoop with newMessage），要么从 InterruptType 移除。

### P2（优化）

8. **GetAvailableTools 每次都重新合并三个 Registry，未缓存**
   - 一次 ReAct 50 步，调用 50 次（每个 LLM call 前）；
   - tools.Registry 内部 Definitions() 也每次 sort；
   - 修复：在 Orchestrator 加一个 generation-aware cache，三个 Registry 任一变化时失效。

9. **自动测试是顺序追加，不并发**
   - 大型项目跑全测试套件很慢；
   - 修复：autoTestRunner 内部并发执行多个测试目录。

10. **元认知反思阈值硬编码**
    - `NeedsReflection` 的 successRate < 0.5 等阈值不可配置；
    - 修复：暴露到 config。

---

## 12. 设计教训

1. **不要在 ReAct loop 里直接做副作用** —— 所有"改变世界"的操作（持久化、metrics、外部 API）都包成 best-effort：`if store != nil { ... }` + 错误只记日志不返回。这让 ReAct loop 在外部依赖部分不可用时仍能跑。

2. **interface 解耦 > 具体类型** —— `skillRegistry`、`temporalClient`、`PTYManager`、`LSPClient`、`MemoryRetriever` 全部用匿名 interface，避免 import cycle。代价：编译期类型检查弱化（interface satisfy 是隐式的），需测试覆盖每个 setter。

3. **失败应回喂给 LLM，不回喂给上层** —— `dispatchTool` 找不到工具返回 `IsError=true` 不返回 Go-error；webhook 失败同样。这是 LLM-agent 工程的"软失败"哲学：让模型自己决定怎么应对，比代码硬中断更鲁棒。

4. **scope 选错就全错** —— Speculative cache 用 workspaceID 不用 sessionID，是反复在生产踩坑后的修正。注释里写得非常清楚（`speculative_cache.go:124-136`）。这种"看似细节实际致命"的设计决策必须在代码 + 文档双重显式记录。

5. **三层反思（fail tracker / meta / verification）有冗余但有必要** —— 表面上做的事相似（让 LLM "停一停想想"），实际作用域不同：fail tracker 是单点 fix loop，meta 是长期质量，verification 是最终一致性。强行合并会丢分辨率。

6. **"暂停 vs 取消"在 UX 上是同一件事** —— 当前代码把 pause 和 cancel 实现成几乎相同的 loop 退出。是有意的简化：用户对"暂停"的期待是"我等会回来"，对应的是 continuation 流程而不是中断本身。pause 字符串提示让用户知道可以 "continue"，但底层退出和 cancel 没区别。

---

## 13. 相关章节

- **03_llm.md**：Orchestrator 的 `llmClient` 调用细节 + 重试。
- **04_rag.md**：`ragEngine.Retrieve` 在 reactLoop 入口的注入。
- **05_sandbox.md**：`sandboxMgr.Execute` 给 `execute_code` / `shell_exec` 工具。
- **06_mcp.md**：MCP gateway 是 dispatch Tier 1。
- **07_tools.md**：toolRegistry 是 Tier 2，列出所有内置工具。
- **08_skill.md**：skillRegistry 是 Tier 3，动态 webhook 工具。
- **10_planner.md**（待重写）：可选 DAG planner，与 ReAct 互斥。
- **11_temporal.md**（待重写）：HITL 持久化路径（当前未接线）。
- **12_hitl.md**（待重写）：完整 HITL UX 闭环。

---

下一篇：[`10_planner.md`](10_planner.md) —— Planner DAG 编排：可选的"先规划后执行"路径，与 ReAct loop 通过 `MaybeUsePlanner` 切换。
