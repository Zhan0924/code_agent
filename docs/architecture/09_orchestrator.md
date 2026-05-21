# 09 · Orchestrator `internal/orchestrator`

> 代码（10 个文件，5542 行，本仓库最核心的包）：
> - `orchestrator.go` (1403) — 主结构体 + ReAct 主循环 + HITL + 流式入口
> - `file_tools.go` (746) — read_file / write_to_file / replace_in_file / list_files / execute_command / run_tests / edit_file
> - `edit_engine.go` — 原子化多文件编辑（备份+回滚+lint）
> - `auto_test_runner.go` — 编辑后自动跑测试（Go/Py/JS/Rust）
> - `git_tools.go` (308) — git status/diff/log/branch/commit + auto-commit
> - `project_rules.go` (305) — 扫描 `.cursorrules` / `.golangci.yml` / `pyproject.toml` 等注入 prompt
> - `planner_bridge.go` (214) — 按意图切换到 Planner（多步规划）分支
> - `message_pruner.go` (80) — token budget 滑动窗口
> - `failure_tracker.go` (54) — 连续同工具失败 → 注入 "step back" 提示
>
> 测试：`orchestrator_test.go` / `file_tools_test.go` / `git_tools_test.go` / `edit_engine_test.go` / `auto_test_runner_test.go` / `project_rules_test.go`

---

## 1. 模块定位

**"Agent 的大脑。每个用户请求都在这里被翻译成一连串工具调用，直到 LLM 自己说'done'。"**

Orchestrator 同时承担**两份工作**：

1. **ReAct 循环**（**默认路径**）：LLM 边思考边调工具，最多 `maxSteps` 轮 —— 适合需要现场试错的任务；
2. **Planner 桥接**（**可选路径**）：先让 LLM 产出**静态计划**，Executor 按步执行 —— 适合已知清单的批量任务（见 `10_planner.md`）。

并在其中做了一堆"**真正生产级 Agent 必需**"的工作：

- **工具失败检测**（`consecutiveFailureTracker`）：同名工具连错 3 次就注入 "step back" 提示让 LLM 换思路；
- **Token 预算管理**（`pruneMessages`）：逼近 128K 上限时按策略删旧消息；
- **反思检查点**（`reflectionCheckpoint`）：每 10 步注入"你离目标多远？"的 meta-prompt；
- **自适应元认知反思**（`MetacognitiveState`）：基于工具成功率和卡住程度动态注入反思，不依赖固定步数间隔。当置信度 < 30% 或卡住度 > 70% 时触发，提示 LLM 换策略或请求用户澄清；
- **敏感内容拦截**（`containsSensitiveContent` → `suspendForApproval`）：HITL 同步挂起等前端授权；
- **自动测试**（`AutoTestRunner`）：写完代码顺手跑 `go test ./...` 把结果塞回 LLM；
- **项目规则注入**（`ProjectRules`）：把仓库根目录的 lint/cursor 配置摘要放进 system prompt；
- **进度续写**（`saveProgressForContinuation`）：超时前把状态写 `.progress.json`，用户说 "continue" 就接着跑；
- **意图缓存**（`intentCache`）：同一 session 短时间内不重复调 LLM 做意图分类；
- **流式事件通道**（`ProcessMessageStreamFull`）：逐步把 think/tool_call/tool_result 推给前端。

---

## 1.5 设计哲学：把 ReAct 从 notebook demo 拉到生产

学术界 ReAct 论文（Yao et al. 2022）只需要 50 行 Python 就能跑。但要在
生产用，要补上**10 个必备件**。本节阐述 Orchestrator 为何这样设计。

### 为什么 ReAct 而不是纯 Planner？

- **ReAct**：每步问 LLM "下一步干啥"，边想边做。
  - ✅ 处理未知的探索性任务（"帮我理解这个仓库"）
  - ❌ 步数不可预测，平均 5-15 步，长 tail 到 50 步
  - ❌ 每步都是 LLM 调用，贵

- **Planner**（见 10_planner）：先产 DAG，批量执行。
  - ✅ 可预测成本
  - ❌ 静态计划，中途出错无法动态调整
  - ❌ 写 DAG 需要 LLM 一次就"想清楚"

**本系统：默认 ReAct，特定场景切 Planner**（`planner_bridge.go` 判定意图）。

### 10 个生产级 ReAct 要素（对应实现）

| 要素 | 没它会怎样 | 本系统实现 |
|---|---|---|
| 1. 最大步数上限 | LLM 环路死循环 | `maxSteps` 意图动态（10-50） |
| 2. 同工具连续失败检测 | 5 次 `read_file(x)` 都 404 还在调 | `failureTracker.go`：连续 3 次同 tool 同 err → 注入 "step back" |
| 3. Token 预算管理 | 超上下文直接 `context_length_exceeded` | `pruneMessages` 滑动窗口 |
| 4. 反思 checkpoint | 跑到第 30 步忘了目标 | 每 10 步注入 meta-prompt "离目标还多远" |
| 5. 敏感内容拦截 | LLM 说要 `rm -rf /` | `sensitiveRules` 正则 + HITL |
| 6. 工具结果截断 | `cat /dev/zero` 撑爆 | `smartTruncateOutput` 头尾保留中间省 |
| 7. 幂等缓存 | 重复 `read_file` 浪费 token | `SpeculativeToolCache`（白名单） |
| 8. 意图分类 + 缓存 | 每步都重新判定意图 | `parseIntent` + 按 message hash 缓存 |
| 9. 进度续写 | 服务器重启丢任务 | `saveProgressForContinuation` 写 `.progress.json` |
| 10. 流式可视化 | 用户以为服务卡死 | SSE 每步推 think/tool/result 事件 |

### 决策树：一个用户消息如何走到 Orchestrator

```
ChatRequest 到达
  │
  ▼
resolveWorkspace(req.WorkspaceID)
  │ (无 → 新建；失败 → 400，不跨租户 fallback P0 #15)
  ▼
parseIntent(sessionID, message)
  │ key = sessionID + sha256(message)  ← P0 #12
  │ TTL 2min
  │ cache miss → LLM 分类
  ▼
intent ──┬─ code_query     → buildSystemMessage(query) → reactLoop
         ├─ code_execute   → 同上，但工具更丰富
         ├─ diagnose       → 同上，带日志工具
         ├─ deploy         → ★ 强制 HITL
         ├─ mcp_call       → 直接走 MCP gateway
         └─ conversation   → 直答，不进 reactLoop
  │
  ▼
reactLoop(ctx, sess, taskID) 主循环
  │
  │ for step := 0; step < maxSteps; step++:
  │   messages = sess.Messages + systemPrompt + repomap + rag
  │   pruneMessages(messages, maxTokens)
  │
  │   resp = llmClient.ChatCompletion(messages, tools)
  │
  │   if resp.ToolCalls == 0:
  │     return resp.Content   # LLM 说 "done"
  │
  │   for tc := range resp.ToolCalls:
  │     if sensitive(tc.Arguments): suspendForApproval()  # HITL
  │     result = executeTool(tc)  # ← 这里是工具调度核心
  │     speculativeCache.Put(ws, tc.Name, tc.Args, result)
  │     messages = append(messages, tool_result)
  │
  │   failureTracker.Track(tc.Name, result.IsError)
  │   if step % 10 == 0: injectReflectionPrompt()
  │
  ▼
return final assistant message
```

### "Orchestrator 持有一切" 的代价

当前 `Orchestrator` 结构体持有 10+ 依赖（llmClient, sessionMgr, ragEngine,
sandboxMgr, mcpGateway, skillRegistry, workspaceMgr, store, editEngine,
autoTestRunner, ...）。这不美，但权衡后接受：
- 替代方案（每个方法单独 constructor 传参）让 method signature 变得难读
- 所有字段都是 goroutine-safe 组件，没有并发风险
- 测试时用 `NewOrchestrator(...)` 一次性装配 mock 即可

未来如果 Orchestrator 膨胀到 30+ 依赖，再拆成 `ToolOrchestrator` +
`FlowOrchestrator` 两层。

---

## 2. 依赖架构

```
┌─────────────── API 层 (handlers.go) ─────────────────────┐
│  /chat (sync)       → ProcessMessage                       │
│  /chat/stream (SSE) → ProcessMessageStreamFull             │
│  /chat/approve      → HandleApproval                       │
└──────────────────────┬───────────────────────────────────┘
                       │
                       ▼
            ┌────────────────────────┐
            │     Orchestrator       │  ← 本模块
            │  ┌───────────────────┐ │
            │  │  reactLoop        │ │  ★ 主引擎
            │  │  planner_bridge   │ │
            │  │  suspendForApprv  │ │
            │  │  saveProgress     │ │
            │  └─────────┬─────────┘ │
            └────────────┼───────────┘
                         │
        ┌────────┬───────┼───────┬────────┬────────┐
        ▼        ▼       ▼       ▼        ▼        ▼
    llm.Client rag.Engine sandbox mcp.Gw skill.Reg workspace.Mgr
    session.Mgr           Mgr                      store.Store
    PromptBuilder                                  audit.Logger
      │
      ├─ ProjectRules (rule_loader)
      ├─ EditEngine (precision edit + lint)
      ├─ AutoTestRunner (tdd loop)
      └─ failureTracker (fix-loop detector)
```

---

## 2.5 数据流总览

### 2.5.1 端到端请求处理

```text
┌───────────────────────┐
│ ChatRequest           │
│ {sessionID, message,  │
│  workspaceID}         │
└───────────┬───────────┘
            │
            ▼
┌─────────────────────────────────────────────────────────────┐
│ resolveWorkspace(req.WorkspaceID)                            │
│   无 → CreateForSession; 失败 → 400 error                   │
└──────────────────────────┬──────────────────────────────────┘
                           │ (*Workspace)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ parseIntent(sessionID, msg)                                  │
│   cache key = sessionID + sha256(msg)                       │
│   cache hit → 直接返回                                       │
│   cache miss → 调 LLM 分类 → 缓存结果                       │
└──────────────────────────┬──────────────────────────────────┘
                           │ (TaskIntent)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Intent 路由:                                                 │
│                                                              │
│  conversation → 直答 (不进 ReAct)                            │
│  code_query / code_execute / diagnose → ReAct ★             │
│  deploy / admin (危险) → 强制 HITL 审批                      │
│  complex + multi-step → Planner 路径                         │
└─────────┬─────────────────────────┬─────────────────────────┘
          │ (ReAct 路径 ★)           │ (Planner 路径)
          │                         ▼
          │              ┌─────────────────────┐
          │              │ planner.CreatePlan  │
          │              │ → DAG Execute       │
          │              │ (详见 10_planner)   │
          │              └──────────┬──────────┘
          │                         │
          ▼                         │
┌──────────────────────┐            │
│ reactLoop() ★       │            │
│ (见 §2.5.2)         │            │
└──────────┬───────────┘            │
           │                        │
           └────────────┬───────────┘
                        │ (final content)
                        ▼
┌─────────────────────────────────────────────────────────────┐
│ sess.AddMessage(assistant, content)                           │
│ store.UpdateTaskState(Completed)                             │
│ audit.Log(task_completed, ...)                               │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
              ┌────────────────────────┐
              │ ChatResponse / SSE done│
              └────────────────────────┘
```

### 2.5.2 ReAct 主循环内部数据流

```text
         ┌─── step = 0 ──────────────────────────────────────┐
         │                                                    │
         ▼                                                    │
┌─────────────────────────────────────────┐                   │
│ buildMessages:                          │                   │
│  systemPrompt (immutable prefix)        │                   │
│  + projectRules (CLAUDE.md 等)          │                   │
│  + repoMap (结构视图)                    │                   │
│  + RAG chunks (相关代码)                 │                   │
│  + session history                      │                   │
│  + current user message                 │                   │
│                                          │                   │
│ pruneMessages(128K token budget)         │                   │
│ reflectionCheckpoint (每10步注入反思)     │                   │
└──────────────────┬──────────────────────┘                   │
                   │ (messages + tools definitions)            │
                   ▼                                           │
┌─────────────────────────────────────────┐                   │
│ 【LLM API】 ChatCompletion              │                   │
│  失败 → 重试 3次 (指数退避)             │                   │
│  彻底失败 → saveProgress → return error │                   │
└──────────────────┬──────────────────────┘                   │
                   │ (ChatResponse)                            │
                   ▼                                           │
       ┌───────────┴───────────┐                              │
       │ ToolCalls == 0?       │                              │
       ├── YES ───▶ return content (完成)                     │
       └── NO ────┘                                           │
                   │                                           │
                   ▼  for each tool_call:                      │
┌─────────────────────────────────────────┐                   │
│ ① containsSensitiveContent?             │                   │
│    + requireApprovalCommands match?     │                   │
│    YES → suspendForApproval → 等待信号  │                   │
│    (前端 approve/reject → 恢复/中止)    │                   │
│                                          │                   │
│ ② executeTool(toolCall):                │                   │
│    switch name:                         │                   │
│      builtin (read/write/patch/run_     │                   │
│        tests/git_*) → 本地执行          │                   │
│      "sandbox_*" → sandbox.Execute     │                   │
│      "rag_*"     → rag.Retrieve        │                   │
│      "mcp_*"     → mcp.CallTool        │                   │
│      其他        → skill.Execute        │                   │
│                                          │                   │
│ ③ smartTruncate(result):                │                   │
│    result > 20K chars?                  │                   │
│    → 保留 head 8K + tail 12K + 省略标记 │                   │
│                                          │                   │
│ ④ failureTracker.Track():               │                   │
│    连续 3次失败 → inject step-back msg  │                   │
│    (引导 LLM 换策略)                     │                   │
│                                          │                   │
│ ⑤ EditEngine 后续 (如有文件编辑):       │                   │
│    → lint 检查 → 失败则 rollback .bak   │                   │
│    → AutoTestRunner: 跑相关测试          │                   │
│    → 测试结果注入为 system message       │                   │
└──────────────────┬──────────────────────┘                   │
                   │ (tool_result messages 追加)               │
                   │                                           │
                   │  step++ < maxSteps?                       │
                   └── YES ───────────────────────────────────┘
                   │
                   ▼ (超过 maxSteps)
          saveProgressForContinuation()
          return "Hit max steps, send 'continue'"
```

---

## 3. Orchestrator 结构体

```go
// orchestrator.go:49-83
type Orchestrator struct {
    llmClient      *llm.Client
    sessionMgr     *session.Manager
    ragEngine      *rag.Engine
    sandboxMgr     *sandbox.Manager
    mcpGateway     *mcp.Gateway
    promptBuilder  *agentctx.PromptBuilder         // 见 13_context
    securityCfg    *config.SecurityConfig
    sensitiveRules []*regexp.Regexp                // pre-compiled 正则
    workspaceMgr   *workspace.Manager              // 见 14_workspace
    skillRegistry  interface{ /* 鸭子类型避免循环 import */ }
    store          *store.Store                    // 可 nil
    logger         *zap.Logger

    editEngine     *EditEngine                     // 精准编辑
    autoTestRunner *AutoTestRunner                 // TDD 闭环

    // HITL
    approvalMu sync.RWMutex
    approvalCh map[string]chan models.ApprovalResponse

    // 意图缓存
    intentCacheMu sync.RWMutex
    intentCache   map[string]intentCacheEntry

    // 可选 planner
    planner *plannerComponents
}
```

几个值得停下细说的字段：

| 字段 | 用途 | 避坑点 |
|---|---|---|
| `skillRegistry` 是 interface | 避免 orchestrator 包直接 import skill 包（**防止循环依赖**） | 换实现只要满足 `GetToolDefinitions / FindSkill / Execute` 三个方法 |
| `sensitiveRules []*regexp.Regexp` | 启动时一次性编译，避免每次请求重编译 | 见 `NewOrchestrator` 的循环编译 |
| `approvalCh` 是 `map[taskID]chan` | 一个 task 一个 channel，防止不同 task 串扰 | **必须**在 `HandleApproval` 里 close 并 delete，否则 goroutine/memory 泄漏 |
| `intentCache` ⚠ P0 #12 修复 | 缓存 key 现在是 `sessionID + sha256(message)`，不只是 sessionID。避免"首条 code_query → 后续 deploy 直接绕过 HITL"。另带 `intentCacheMaxEntries=2048` LRU 淘汰防内存泄漏 | 详见 §9.4 |

### 3.1 intentCache 的回归故事（P0 #12）

```text
（修复前）
T0: user → "show me the code"        → intent=code_query（入缓存 key=sessionID）
T5: user → "deploy to prod"          → 缓存命中 code_query（✗ 应该是 IntentDeploy → 触发 HITL）
                                         ← 结果：敏感指令直接绕过审批

（修复后）
T0: user → "show me the code"        → key = sessionID:sha256("show...") → 入缓存
T5: user → "deploy to prod"          → key = sessionID:sha256("deploy...") → 不同 key → MISS
                                       → 重走 LLM 分类 → IntentDeploy → HITL
```

**代码位置**：`orchestrator.go:960-1000`
```go
func intentCacheKey(sessionID, userMessage string) string {
    h := sha256.Sum256([]byte(userMessage))
    return sessionID + ":" + hex.EncodeToString(h[:16])
}
```

测试回归：`TestIntentCacheKey_IncludesMessage`（两条不同消息产生不同 key）
+ `TestEvictIntentCacheLocked_BoundsGrowth`（LRU 不超过 2048 条）。

---

## 4. 入口：`ProcessMessage` & `ProcessMessageStreamFull`

### 4.1 `ProcessMessage` (L154) — 同步一次请求

```go
ProcessMessage(ctx, sessionID, userMsg) → *ChatResponse:
  1. sess := sessionMgr.GetOrCreate(sessionID)
  2. sess.AppendUser(userMsg)
  3. task := Task{ID, SessionID, UserInput, State: Pending}
     persistTaskCreate(task)
  4. intent := parseIntent(ctx, sessionID, userMsg)          // 带缓存
     task.Intent = intent

  5. if containsSensitiveContent(userMsg):                   // 敏感 → HITL
        return suspendForApproval(ctx, task)

  6. // 先问 planner 愿不愿意接
     if resp, used, err := MaybeUsePlanner(ctx, task); used:
         return resp, err

  7. content, err := reactLoop(ctx, task)                    // ★ ReAct
  8. persistTaskState(task, Completed / Failed)
     persistAudit(...)
  9. sess.AppendAssistant(content)
     return &ChatResponse{Content: content, TaskID: task.ID}
```

### 4.2 `ProcessMessageStreamFull` (L723) — 流式事件通道

返回 `<-chan models.ReactStreamEvent`，事件类型包括：

| type | payload | 时机 |
|---|---|---|
| `thinking` | LLM 的自由文本增量 | 流式 token |
| `tool_call` | `{name, args}` | LLM 决定调工具时 |
| `tool_result` | `{tool_call_id, content, is_error}` | 工具返回 |
| `step_done` | `{step, max_steps}` | 每轮结束 |
| `suspended` | `{task_id, reason}` | 命中敏感规则 |
| `done` | `{content}` | 全部完成 |
| `error` | `{message}` | 异常 |

前端 ChatPage 用 EventSource 接这个通道 —— 见 `17_api`。

---

## 5. ★ ReAct 主循环 `reactLoop` (L272-454)

这是**本包最核心的 180 行**，逐段拆解：

### 5.1 准备阶段

```go
// 5.1.1 拿 session
sess, _ := sessionMgr.Get(ctx, task.SessionID)

// 5.1.2 仅对"代码查询/诊断"类意图调 RAG
var codeChunks []models.CodeChunk
var relevanceScores []float64
if intent in {IntentCodeQuery, IntentDiagnose}:
    results, _ := ragEngine.Retrieve(ctx, userInput, nil)
    metrics.RAGChunksReturned.Observe(len(results))
    ...

// 5.1.3 让 PromptBuilder 装配
promptBuilder.UpdateLongTermMemory(sess.Summary)
messages := promptBuilder.BuildPrompt(sess, codeChunks, scores, userInput)
messages[0] = buildSystemMessage(intent)            // 覆盖 system 为意图定制版

// 5.1.4 工具清单
tools := getAvailableTools()

// 5.1.5 运行时状态
maxContextTokens := 128000                          // Opus 200K，留 72K 给输出/headroom
failTracker      := &consecutiveFailureTracker{}
maxSteps         := getMaxSteps(intent)             // 10 / 15 / 20 / 25
```

### 5.2 主循环（每轮 = 一次 LLM + 多工具）

```
for step := 0; step < maxSteps; step++:

  ┌─ Reflection injection every 10 steps
  │    if reflectionCheckpoint(step) ≠ nil:
  │        messages = append(messages, reflection)
  │
  ├─ Token budget check
  │    if Σtokens(messages) > 128K:
  │        messages = pruneMessages(messages, 128K)     # 见 §10
  │
  ├─ LLM call WITH RETRY (3 次指数退避 2s/4s)
  │    resp, err := llm.ChatCompletion({messages, tools})
  │    if err after 3 attempts:
  │        saveProgressForContinuation(task)            # 写 .progress.json
  │        return "LLM failed, send 'continue' to resume"
  │
  ├─ Termination
  │    if len(resp.ToolCalls) == 0:
  │        return resp.Content            # 🎉 LLM 决定"done"
  │
  ├─ Append assistant message (含 ToolCalls)
  │
  └─ For each tool_call tc:
        result, execErr := executeTool(ctx, tc)
        content := result.Content or "Error: ..."

        # 记录编辑过的文件 → 为后面 auto-test 准备
        if tc.Name in {edit_file, write_file, patch_file} and ok:
            editedFilePaths.append(args.path)

        # Smart truncation: 8K→32K 字符，保头 8K + 尾 12K
        if tokens > 8K:
            content = head[8K] + "…middle truncated…" + tail[12K]

        # 塞一条 tool-role 消息
        messages.append({role:"tool", content, tool_call_id: tc.ID})

        # Fix-loop 检测 → 连错 3 次注入 step-back
        if failTracker.track(tc.Name, isErr):
            messages.append(failTracker.stepBackMessage())

  # 每轮末：对编辑过的文件自动跑测试
  if editedFilePaths ≠ []:
      testResult := RunAutoTestAfterEdit(editedFilePaths)
      if testResult:
          messages.append({role:"system", content: testResult.FormatForLLM()})

end for

# ── maxSteps 用完 ──
saveProgressForContinuation(task)
return "⚠️ Hit max 50 steps. Send 'continue' to resume."
```

### 5.3 `getMaxSteps` 分档

```go
// orchestrator.go:31
exploration / analysis → 15
coding                → 20
debugging             → 25
default               → 10
```

设计动机：

- debugging 因为常要反复改错+跑测试，给 25；
- 纯聊天 / 简单查询给 10，省 LLM 花销；
- 数字不暴露给配置，**故意硬编码** —— 避免运营同学调大到 1000 造成灾难。

### 5.4 智能截断为什么是 head/tail 而不是 head

很多错误信息（stack trace / test FAIL）**都在输出末尾**。只留 head 的话 LLM 看不到关键的 `expected X got Y`，无法自修复。所以故意保留**尾部更大权重**（head 8K / tail 12K）。

---

## 6. HITL 挂起/恢复

### 6.1 `suspendForApproval` (L578) — 发现敏感指令

```go
suspendForApproval(ctx, task):
  # 1. 生成一个 buffered channel
  ch := make(chan models.ApprovalResponse, 1)
  approvalMu.Lock()
  approvalCh[task.ID] = ch
  approvalMu.Unlock()

  # 2. 更新 task 状态 + 审计
  persistTaskState(task, Suspended)
  persistAudit(ctx, task.ID, userID, "SUSPEND", "HIGH")

  # 3. 返回给前端（前端此时显示"需要您的批准"按钮）
  return ChatResponse{
    State:   Suspended,
    TaskID:  task.ID,
    Message: "Sensitive operation detected, awaiting approval.",
    PendingCommand: extractedCommand,
  }
```

### 6.2 `HandleApproval` (L642) — 前端回调恢复

```
POST /chat/approve { task_id, approved, comment }
   → HandleApproval(ctx, resp):
        approvalMu.RLock
        ch := approvalCh[taskID]
        approvalMu.RUnlock
        if ch == nil: 404

        ch <- resp        # 唤醒挂起的 goroutine
        approvalMu.Lock
        delete(approvalCh, taskID); close(ch)
        approvalMu.Unlock
```

**并不是真的同步等 channel**，而是：挂起后直接返回 `State=Suspended`，前端显示授权 UI，再发一次 `/chat` 带上 `task_id + resume` 就重新进入 reactLoop。

### 6.3 敏感规则

```go
containsSensitiveContent(input):
  for _, re := range sensitiveRules:
      if re.MatchString(input):
          return true
  return false
```

`sensitiveRules` 来源于 `config.SecurityConfig.SensitivePatterns` —— 默认包含 `DROP\s+(DATABASE|TABLE)`, `rm\s+-rf\s+/`, `kubectl\s+delete`, `prod|production` 等，可外部配置扩展。

---

## 7. 意图解析与缓存 `parseIntent` (L950)

```
parseIntent(ctx, sessionID, userMsg):
  key := sha256(sessionID + userMsg)
  if cache[key].expiresAt > now: return cached

  # 用一个廉价的小模型（gpt-4o-mini）专门做 intent 分类
  resp := llmClient.ChatCompletion({Model: "cheap", Messages: [...]}
  intent := parse(resp.Content)   # 提取 JSON

  cache[key] = {intent, expiresAt: now + 10s}
  return intent
```

**10s TTL** 的来由：

- 用户往往在几秒内重复按 Enter（多次点击）；
- 10s 既不会因为话题切换保留陈旧意图，又能拦住大部分重复；
- 若 LLM 本身跑在本地/免费 → 其实可以直接关掉缓存，代价几乎为 0。

---

## 8. 系统提示组装 `buildSystemMessage` (L1013)

整合四块信息：

```
SYSTEM = base_prompt_for_intent        # 按意图选模板（coding/debug/exploration）
       + workspace_context              # 当前 workspace 的 repo map 摘要
       + project_rules_summary          # ProjectRules.FormatForSystemPrompt()
       + rag_hint                       # "Relevant code snippets are already attached"
```

其中 `project_rules_summary` 来自 `project_rules.go`：

| 文件 | 抽什么摘要 |
|---|---|
| `.golangci.yml` | 启用的 linters + 关键规则 |
| `.cursorrules` / `CLAUDE.md` | 原文注入（<2KB 截断） |
| `pyproject.toml` | `[tool.ruff] / [tool.black]` 配置行 |
| `tsconfig.json` | `strict` / `noImplicitAny` 等关键开关 |
| `.eslintrc*` | 原文注入 |

这些摘要**只在 system prompt 里出现一次**，不占后续 tool result 的 budget。

---

## 9. 工具调度子系统

### 9.1 `executeTool` (L1222) — 分派器

```
executeTool(ctx, tc):
  # 顺序：builtin → skill → mcp
  switch tc.Name:
    case "execute_code":    return toolExecuteCode(ctx, tc.Args)    # sandbox
    case "search_code":     return toolSearchCode(ctx, tc.Args)     # rag
    case "read_file"/"write_file"/"patch_file"/"list_files"/
         "create_directory"/"run_tests"/"edit_file"/"run_cmd":
        return file/git/edit handlers                                # workspace
    case "git_*":           return gitTool(ctx, ws, tc.Args)
    default:
        # skill 优先
        if skill, ok := skillRegistry.FindSkill(tc.Name); ok:
            return skillRegistry.Execute(ctx, tc.Name, tc.Args)
        # 最后 mcp
        if mcpGateway != nil:
            return mcpGateway.Call(ctx, tc.Name, tc.Args)

        return &ToolResult{IsError: true, Content: "unknown tool"}, nil
```

> **TODO**（见 §15）：这个 switch 目前还没抽到 `tools.Registry`；`07_tools` 已经给出抽象，未来 orchestrator 会彻底变成 `registry.Execute` 的薄包装。

### 9.2 文件工具 `file_tools.go` (746 行)

8 个内置工具：

| 工具 | 说明 |
|---|---|
| `read_file` | 带 `start_line/end_line` 切片，自动打 `N | ` 行号前缀给 LLM |
| `write_to_file` | 完整覆盖；自动 mkdir -p；写完跑 `LintChecker` |
| `replace_in_file` (patch_file) | `SEARCH/REPLACE` 块语法，**必须唯一匹配**否则报错 |
| `edit_file` | 走 `EditEngine` 的精准多文件编辑 + 回滚 |
| `list_files` | gitignore-aware，支持递归 |
| `create_directory` | 幂等 mkdir -p |
| `run_tests` | 调 `AutoTestRunner` |
| `run_cmd` | 调 sandbox（workspace 挂载模式） |

两个关键辅助：

- **`smartTruncateOutput(out, maxLen)`**：工具返回超长时保头尾截中间，与主循环 §5.4 同策略；
- **`autoDepManagement(ws, path)`**：写完 `go.mod` 改动自动 `go mod tidy`；写完 `requirements.txt` 自动 `pip install`；减少 LLM 忘记的坑。

### 9.3 精准编辑引擎 `edit_engine.go`

```go
EditOperation:
  FilePath   string
  OldContent string    # 必须唯一匹配
  NewContent string

ApplyEdit(ctx, ws, op):
  1. read original
  2. 验证 OldContent 在文件中**恰好出现一次**（否则 ambiguous → 失败）
  3. 备份到 /tmp/orch_backup/...
  4. write new content
  5. LintChecker.Check()                  # golangci-lint / ruff / eslint / cargo check
  6. if lint 有 error-level:
         rollback（从备份恢复）
  7. return EditResult{Applied, DiffUnified, LintWarnings, BackupPath}

ApplyMultiEdit(ctx, ws, ops[]):
  for each op: tryApply
  if any fails: rollbackAll(backups)       # 全有全无语义
```

**为什么坚持"唯一匹配"**？因为 LLM 产生的 `old_content` 可能在文件里有多处相同文本（大括号、空行），模糊匹配会改错地方。强制唯一让 LLM 必须提供**足够多的上下文**，这也是和 Cursor / Aider 对齐的做法。

#### 9.3.1 并发编辑保护（P0 #13 修复）

> ⚠️ **修复前的 bug**：`EditEngine` 没有任何 per-file 锁。两个 goroutine
> 并发 ApplyEdit 同一文件时：
>
> ```
> T1 [A]: read file → sees "foo"
> T2 [B]: read file → sees "foo"     ← 同时读
> T3 [A]: count("foo") == 1 ✓ → write "bar-A"
> T4 [B]: count("foo") == 1 ✓ → write "bar-B"   ← 覆盖 A
> T5 [A]: 返回 success                             ← 但磁盘上是 B 的内容
> ```
>
> 调用方 A 以为成功，实际内容丢失。`.bak` 文件也互相覆盖，rollback
> 无法恢复到原始版本。

**修复**：per-path `sync.Mutex`，字典序加锁防死锁。

```go
type EditEngine struct {
    pathLocksMu sync.Mutex
    pathLocks   map[string]*sync.Mutex  // absolute path → mutex
    ...
}

func (e *EditEngine) lockPath(absPath string) (unlock func()) {
    e.pathLocksMu.Lock()
    mu, ok := e.pathLocks[absPath]
    if !ok {
        mu = &sync.Mutex{}
        e.pathLocks[absPath] = mu
    }
    e.pathLocksMu.Unlock()
    mu.Lock()
    return mu.Unlock
}

func (e *EditEngine) ApplyEdit(ctx, ws, op EditOperation) *EditResult {
    absPath := filepath.Join(ws.RootDir, op.Path)
    defer e.lockPath(absPath)()   // ← 加锁，保证 read-check-write 原子
    // ... 下面的逻辑不变
}

// ApplyMultiEdit 也一样，但按 **字典序** 加锁所有涉及的路径，避免死锁：
// 两个 MultiEdit 都涉及 {a.go, b.go}，都按 a → b 加锁，不会互相等。
func (e *EditEngine) lockPaths(ws *workspace.Workspace, ops []EditOperation) func() {
    paths := unique sorted abs paths from ops
    unlocks := []
    for _, p := range paths { unlocks = append(unlocks, e.lockPath(p)) }
    return func() { reverse order release }
}
```

**测试**：`TestEditEngine_ConcurrentEditsSerialized`——两个并发 goroutine
同时 `ApplyEdit("file", "alpha"→"A")` 和 `ApplyEdit("file", "alpha"→"B")`，
断言：**严格只有一个成功**，另一个报 `old_text not found`（因为看到的是
第一个写入后的新内容 "A\nbeta\n"，找不到原来的 "alpha"）。

#### 9.3.2 Unified Diff 重写（P0 #16 修复）

> ⚠️ **修复前**：`generateUnifiedDiff` 用"尾部对齐"启发式计算 `lastDiff`，
> 当**插入行数 ≠ 删除行数**时，hunk header `@@ -start,N +start,M @@`
> 的 N/M 与实际 body 行数不一致。标准 diff 工具（patch、git apply）会
> 把这视为语法错误，或把后面的 context 误算进 hunk。

**修复**：经典"公共前缀 + 公共后缀" 双指针：
```go
firstDiff := 0
for ... && old[firstDiff] == new[firstDiff] { firstDiff++ }
tailMatch := 0
for ... && old[len(old)-1-tailMatch] == new[len(new)-1-tailMatch] { tailMatch++ }
lastOld := len(old) - tailMatch  // exclusive
lastNew := len(new) - tailMatch
// hunk header 的 oldCount / newCount 是实际输出行数：
//   context_before + (lastOld - firstDiff) + context_after
oldCount := endOld - start
newCount := endNew - start
```

**测试**：`TestUnifiedDiff_InsertsNotEqualDeletes` 解析 header 的 count，
与 body 实际 `" " / "+" / "-"` 行数交叉校验。

### 9.4 自动测试 `auto_test_runner.go`

```
AfterEdit(ctx, ws, editedFiles):
  srcFiles  := filter(isTestableCodeFile)           # 非 _test.go / 非 .test.ts
  testFiles := findRelatedTests(srcFiles)           # 约定匹配 test_*.py / *_test.go / *.test.ts
  lang      := detectLanguage(editedFiles[0])
  cmd, args := buildTestCommand(lang, srcFiles, testFiles)
  result    := runTest(ctx, workDir, cmd)           # 通过 sandbox 执行
  return TestResult{Passed, Output, Duration, FailedCases}

TestResult.FormatForLLM():
  if Passed: "✅ All tests passed (3 tests, 0.8s)"
  else:      "❌ 2/5 tests failed:\n<FAIL output snippets>"
```

**为什么不直接让 LLM 自己调 `run_tests`**？因为 LLM 经常忘。Orchestrator 在**每轮末**无脑跑一次测试，结果塞成 **system message** 而非 tool result —— 不给 LLM 留"我没跑过"的余地。

### 9.5 Git 工具 `git_tools.go`

五个工具：`git_status / git_diff / git_log / git_branch / git_commit`。

`AutoCommitAfterEdit(ws, editedFiles, desc)`：

```go
1. ensureGitInit(ws)              # 首次操作前 git init
2. git add editedFiles
3. git commit -m "<agent> " + desc
```

可通过 `config.Workspace.AutoCommit = false` 关闭。

---

## 10. 消息裁剪 `pruneMessages` (L33 in message_pruner.go)

```
pruneMessages(msgs, maxTokens):
  保留 msgs[0] (system) 不动
  从 msgs[1:] 最老那端开始剔除，直到 Σtokens < maxTokens
  最少保留最后 6 条（避免断了 tool_call/tool_result 配对）
  如果 system 自己就 > maxTokens / 2：截断 system.Content[:half]
```

**为什么不在循环外每次都 prune**？因为每次都调 `EstimateTokens` 成本不低，只在真的超标时才动。

---

## 11. 项目规则 `project_rules.go`

```
RuleLoader.Load(rootDir) *ProjectRules:
  if cache hit: return cached
  discover:
    - .cursorrules, CLAUDE.md, CLINE.md (原文 <2KB)
    - discoverLanguageRules():
        - .golangci.yml   → extractGolangciSummary
        - pyproject.toml  → extractPythonToolSummary
        - tsconfig.json   → extractTSConfigSummary
        - .eslintrc*      → 原文
  cache[rootDir] = rules
  return rules

Invalidate(rootDir):  # watcher 检测到文件变更时调
  delete(cache, rootDir)
```

摘要而非全文的原因：tsconfig 上千行但只有十几个 flag 影响 coding 风格；golangci 更夸张。LLM 只需要知道"这个项目 strict=true、disabled: varcheck"就够了。

---

## 12. Planner 桥接 `planner_bridge.go`

```
MaybeUsePlanner(ctx, task) (resp, used, err):
  if planner == nil: return nil, false, nil            # 没挂 planner
  if task.Intent not in {coding, debugging}: false     # 只在复杂任务用
  if tokenCount(task.UserInput) < 200: false           # 太短的任务 ReAct 更直接

  plan, _ := planner.MakePlan(ctx, task.UserInput, workspace, tools)
  if len(plan.Steps) < 3: return nil, false, nil       # 不值得用 plan

  result := planner.Execute(ctx, plan, executePlanStep)
  return &ChatResponse{
      Content: formatPlanResult(result),
      State:   plannedChatState(result.Success),
  }, true, nil
```

何时走 Planner？**静态 + 长计划 + 多工具** 的任务（迁移/重构/批量 rename）；何时走 ReAct？**探索 + 高不确定性**（debugging）。分界的艺术见 `10_planner`。

---

## 13. 持久化与审计

### 13.1 `persistTaskCreate / persistTaskState` (L226, L245)

```go
persistTaskCreate(ctx, task):
  if store == nil: return       # 测试/无 PG 模式直接跳过
  if err := store.Tasks.Insert(ctx, task); err:
      logger.Warn("persist task failed", zap.Error(err))   # 故意不阻塞主流程
```

**关键设计**：DB 落不下 → warn 但继续。如果 PG 临时挂了，Agent 功能**不应**挂。后续补偿靠 `store.Tasks.Reconcile` 或直接读 session replay。

### 13.2 `persistAudit` (L255)

每次敏感操作（HITL suspend / approve / deny / high-risk tool）都写一条 audit。字段：

```
ts, user_id, session_id, task_id, action, risk_level, detail
```

查询接口在 `18_auth_security` 里讲。

---

## 14. 设计权衡

| 抉择 | 动机 |
|---|---|
| **maxSteps 硬编码而非配置** | 防止运营调到 1000 造成一次请求烧掉数十刀 LLM 成本 |
| LLM 失败**重试 3 次指数退避** | 外部 API 偶发 500/timeout 很常见，retry 能救 90% |
| 中间状态**落盘 .progress.json** 而非 Redis | 用户可以用 git 看到自己"跑到哪了"；Redis 丢了就没了 |
| 工具结果 head+tail 智能截断 | stack trace 关键内容在尾部 |
| **连续同工具失败 → step-back 注入** 而非直接中止 | LLM 有能力换路线；直接终止对用户不友好 |
| 编辑必须 OldContent 唯一匹配 | 杜绝 LLM 改错位置 —— 代价是 LLM 要提供更多上下文，值得 |
| 自动测试结果作 system 消息 | 比 tool 消息优先级高，LLM 不容易忽略 |
| intent 用廉价小模型单独分类 | 减少主模型 prompt 长度 + 成本；结果可缓存 |
| skill 用 interface 而非 import | 避免 orchestrator → skill → ? 的循环依赖 |
| HITL 用 channel 而非轮询 DB | 授权延迟从秒降到毫秒 |
| reactLoop 单 goroutine 串行执行 tools | 工具间可能有依赖（如 edit→test）；并行收益不大反而增加状态管理难度 |
| RAG 只在 CodeQuery/Diagnose 意图触发 | 闲聊/写代码时 RAG 注入反而是噪声 |
| Reflection 每 10 步注入 | 让 LLM 有机会 meta-level 审视自己是否跑偏 |
| autoDepManagement 白名单触发 | 保守；避免 `go mod tidy` 把 proxy 跑挂引起用户投诉 |

---

## 15. 后续演进

- [ ] **switch → tools.Registry**：`executeTool` 当前仍是 if/else 大杂烩，下一步完全委托给 `internal/tools.Registry`（见 `07_tools.md`）；
- [ ] **并行工具调用**：LLM 一次返回多 tool_call 时串行执行；对无依赖工具（两个 `search_code`）可并行；
- [ ] **Plan-Execute-Reflect 三段式**：当前是 ReAct 或 Plan 二选一；将来 `Plan → Execute → Reflect → 修订 Plan` 迭代；
- [ ] **Cost-based stop**：累计 LLM 花费超阈值（$X）主动暂停求确认；
- [ ] **Checkpoint 在 workflow 层而非 .progress.json**：迁到 Temporal Workflow（见 `11_temporal.md`）让 crash 恢复更健壮；
- [ ] **自适应 maxSteps**：根据近 N 次相似意图的历史均值动态调；
- [ ] **Multi-agent**：主 orchestrator 可以 spawn "reviewer / planner / debugger" 子 agent，每个维护自己 messages；
- [ ] **Token budget 预估从 Estimate 切到真实 tokenizer**（tiktoken/claude-tokenizer），目前是字符近似；
- [ ] **RAG 异步并行**：当前 RAG 串在 LLM 前，总延迟 = RAG + LLM；可让 RAG 与 first-token 并行，落后注入；
- [ ] **工具 ACL**：同一 task 里限制某些工具只能调 N 次（防止滥刷 `search_code`）。

---

## 15. 实现剖析与改进方向

### ReactLoop 每次迭代的核心 40 行

```go
for step := 0; step < maxSteps; step++ {
    // (1) 构建 prompt：拼 system + repomap + rag + history
    messages := o.buildMessages(sess, userMessage, intent)

    // (2) 预算裁剪（见 13_context）
    messages = o.pruneMessages(messages, maxContextTokens)

    // (3) 每 10 步注入反思 prompt
    if step > 0 && step % reflectionInterval == 0 {
        messages = append(messages, reflectionCheckpoint(step, maxSteps))
    }

    // (4) 调 LLM，拿到 tool_calls 或 final answer
    resp, err := o.llmClient.ChatCompletion(ctx, &llm.ChatRequest{
        Messages: messages,
        Tools:    o.getAvailableTools(intent),
        MaxTokens: 4096,
    })
    if err != nil { return "", err }

    // (5) 如果没 tool_calls，说明 LLM 认为任务完成
    if len(resp.ToolCalls) == 0 {
        sess.AddMessage(assistant, resp.Content)
        return resp.Content, nil
    }

    // (6) 串行执行 tool_calls
    for _, tc := range resp.ToolCalls {
        // 6a. 敏感拦截
        if o.containsSensitiveContent(tc.Arguments) {
            o.suspendForApproval(ctx, taskID, tc)
            // 此处可能等待 30min HITL，返回后继续
        }

        // 6b. 幂等缓存
        if result, ok := o.specCache.Get(ws.ID, tc.Name, tc.Arguments); ok {
            sess.AddMessage(tool, result)
            continue
        }

        // 6c. 执行
        result, err := o.executeTool(ctx, tc, ws)
        if err != nil {
            o.failureTracker.Track(tc.Name, err)
            if o.failureTracker.ShouldStepBack(tc.Name) {
                messages = injectStepBackPrompt(messages)
            }
        }

        // 6d. 写缓存 or 失效
        if IsIdempotentTool(tc.Name) {
            o.specCache.Put(ws.ID, tc.Name, tc.Arguments, result)
        } else if ShouldInvalidateAfter(tc.Name) {
            o.specCache.Invalidate(ws.ID)  // 写操作失效整个 scope
        }

        // 6e. 追加 tool_result 到 session
        sess.AddMessage(tool, result.Content)
    }

    // (7) 超时 / cancel 检查
    if ctx.Err() != nil { return "", ctx.Err() }
}

// 超过 maxSteps：保存进度供 "continue" 命令续跑
o.saveProgressForContinuation(taskID, sess)
return "Reached step limit", nil
```

### 性能剖析：一次 5 步 ReAct 会话的时间构成

```
step 1:  [150ms embed]  [200ms LLM]   [20ms read_file]  [5ms cache put]    = 375ms
step 2:  [200ms LLM]    [80ms grep]   [5ms]                                 = 285ms
step 3:  [200ms LLM]    [1.2s edit_file + lint]                             = 1400ms
step 4:  [200ms LLM]    [8s go test]                                        = 8200ms
step 5:  [200ms LLM]    [final answer]                                      = 200ms
─────────────────────────────────────────────────────────────────────────────────────
总计:                                                                         10.5s
其中 LLM 调用: 1.0s (10%)
其中工具执行: 9.5s (90%)
```

观察：**工具执行（尤其 sandbox run_tests）占大头**。优化 ReAct 延迟不是优化
LLM 调用本身，而是：
- 让 LLM 少调工具（更准的意图分类 / 更好的 prompt）
- 让工具本身更快（warm pool / 增量测试）
- 并行工具（当前串行）

### 利弊评估

**优势（Pros）**
- ✅ 10 个生产级要素齐全（见 §1.5 表格）
- ✅ 失败追踪避免 LLM 死循环
- ✅ 意图分类 + 缓存降低 50%+ 分类调用
- ✅ 幂等缓存降低 45%+ 重复读
- ✅ 流式事件让前端有丝滑可视化
- ✅ EditEngine 带 lint + rollback，安全写入

**代价（Cons）**
- ⚠️ Orchestrator 持有 10+ 依赖——god object 嫌疑
- ⚠️ 工具串行执行（一个 step 里 3 个 tool_calls 依次跑，不并发）
- ⚠️ saveProgress 的 continue 功能没真跑通（API 端未接）
- ⚠️ reflectionInterval 硬编码 10；不同任务应该不同
- ⚠️ `skillRegistry` 用 `interface{}` 规避循环依赖，代价是失去类型检查
- ⚠️ message_pruner 的 O(N²) token 估算在 200+ 消息时变慢

### 可改进点

**P0**
1. 并行执行 tool_calls（同一 step 里相互独立的工具）—— 大多数 ReAct 场景
   step 有 2-3 个并行 tool，延迟可降 40%
2. 定义 SkillRegistry interface 替代 `interface{}`

**P1**
3. 意图感知的 maxSteps / reflectionInterval（deploy 短、migrate 长）
4. saveProgress 真接入 `/chat` 的 `continue=true` 参数
5. message_pruner 改用 O(N) token 累加器（而非每次全量估算）
6. failureTracker 的 "step back" prompt 模板按 intent 定制

**P2**
7. 多 turn 的 speculative execution：LLM 还在想时，推测性跑 read_file
8. tool schema 动态裁剪：intent=deploy 时不暴露 run_cmd
9. 按 failure pattern 训练小模型做"我该 step back 还是重试"的决策

---

下一篇：`10_planner.md` —— Planner 静态多步规划：从自然语言 → Step[]，Executor 按步执行 + 失败回退。
