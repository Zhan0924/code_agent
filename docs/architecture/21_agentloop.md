# 21 · ReAct 循环引擎 `internal/agentloop`

> 代码：
> - `runner.go` (225) — `Runner` ReAct 主循环（LLM 调用 + 工具执行 + 反思插点）
> - `config.go` (60) — `Config` / `ContextBudget` 配置与按比例预算
> - `interfaces.go` (42) — 四个核心接口：`LLMCaller` / `ToolExecutor` / `ToolProvider` / `EventSink`
> - `failure_tracker.go` (47) — 连续失败检测与 step-back 提示
> - `tool_error.go` (84) — 工具错误结构化分类
> - `adaptive_feedback.go` (124) — 自适应反馈与黑名单
> - `metacognition.go` (170) — 元认知状态（confidence / stuck score / 反思 prompt）
> - `pruner.go` (83) — 消息窗口裁剪
> - `trajectory_memory.go` (90) — 历史成功轨迹复用
>
> 测试：`runner_test.go` (190) / `tool_error_test.go` (210) / `adaptive_feedback_test.go` (140) / `trajectory_memory_test.go` (95)

---

## 1. 模块定位

**"把 ReAct 循环骨架 + 失败追踪 + 元认知 + 轨迹记忆这些通用原语抽成可复用包——主 agent 直接拼装它们到自己的循环里，sub-agent 直接跑成品 Runner。"**

`internal/agentloop` 是从 `internal/orchestrator` 剥离出来的**可复用 ReAct 原语 + 成品 Runner**。它两种使用方式：

```
[使用方式 A：主 agent]
orchestrator.reactLoopCore(...) {
    aff := agentloop.AdaptiveFeedback{}        // 原语
    failTracker := agentloop.ConsecutiveFailureTracker{}
    trajMem := agentloop.NewTrajectoryMemory()
    for step := 0..N {
        agentloop.PruneMessages(...)            // 原语
        toolErr := agentloop.ClassifyToolError(...)
        ...                                     // 把 SSE/审计/HITL 编织在每步上
    }
}

[使用方式 B：sub-agent]
runner := agentloop.NewRunner(llm, tools, prov, agentloop.DefaultSubAgentConfig(), logger)
runner.Run(ctx, opts, sink)                    // 成品 Runner
```

**为什么主 agent 不直接用 Runner？** Runner 的循环骨架是"通用的"——但主 agent 需要在每一步上插入大量业务编织：SSE 流式 emit、Temporal HITL 拦截、Memory 召回、Multiagent supervisor 分派、Verification 触发……这些不属于通用循环范畴。把它们硬塞 Runner 会让 Runner 不再"通用"。所以主 agent 选择**直接消费原语**自己写循环；sub-agent 不需要这些编织，可以走 Runner。

剥离的动机源自 multiagent 子系统（见 [22_multiagent](22_multiagent.md)）—— supervisor 需要在 spawn sub-agent 时给它一个独立的小 ReAct 循环（`DefaultSubAgentConfig`：8 步、32K token），但又要避免 copy-paste orchestrator 几百行循环代码。

`agentloop` 因此被设计成"四个接口 + 一组原语 + 一个 Runner"：

| 接口 | 实现者 | 职责 |
|------|--------|------|
| `LLMCaller` | `llm.Client` | `ChatCompletion(ctx, req)` |
| `ToolExecutor` | orchestrator 适配器 / sub-agent 适配器 | 把一个 `ToolCall` 跑成 `ToolResult` |
| `ToolProvider` | `tools.Registry` 或受白名单过滤的子集 | 返回 `[]ToolDefinition` |
| `EventSink` | SSE 写入器 / `NoopSink` | 接收 `step_start / thinking / tool_call / tool_result / error` |

加上一个 `Config` 控制步数 / token / 重试，就足以驱动整条 ReAct。

---

## 1.5 核心设计问题

### 为什么主 agent 不直接用 Runner？

旧世界里 `orchestrator.reactLoop()` 是 600+ 行的巨函数：LLM 调用、工具分发、失败处理、SSE emit、反思插点、token 裁剪——全在一坨。

当 multiagent 出现要给每个 sub-agent 一个**精简的**循环时，最直接的选择是把"循环骨架 + 通用辅助状态"做成 `Runner`，sub-agent 直接复用。**但主 agent 没法直接换成 Runner**——它的每一步要插 SSE flush、Temporal Signal 拦截、Memory 召回、Verification 触发等大量业务编织。把这些塞 Runner 会让 Runner 跟主 agent 的具体 IO 强耦合，丧失"通用"价值。

最终演化的形态是**原语 + 成品分层**：
- **原语**（`PruneMessages` / `AdaptiveFeedback` / `ConsecutiveFailureTracker` / `ClassifyToolError` / `NewTrajectoryMemory` / `NewMetacognitiveState`）——主 agent 在 `reactLoopCore` 里逐个调用，自己控制每步顺序；
- **成品 Runner**（`agentloop.NewRunner` + `DefaultSubAgentConfig`）——sub-agent 直接跑，不需要自己写循环。

抽出来后的好处：
- **测试单一职责**：`runner_test.go` 用 mock LLM/Tool 验证 step counting、重试、反思插点；`adaptive_feedback_test.go` / `tool_error_test.go` 验证原语；orchestrator 测试不再重复这些。
- **配置差异化**：sub-agent 用 `DefaultSubAgentConfig`（8 步、32K、关反思）；主 agent 不通过 Runner 路径，它的 maxSteps 由 `getMaxSteps(intent)`（`orchestrator.go:44`）按 TaskIntent 硬编码切换（问答 10、MCP 15、编码/部署 20、诊断 25、其它 50），token 预算在 `context_compaction.go` 自适应裁剪——不是从 `cfg.Orchestrator` 读统一上限。
- **未来易扩展**：planner / multiagent / skill executor 都可以基于 Runner 装自己的语义层；主 agent 也可以渐进地把更多业务下沉为原语。

### 为什么把 failure tracker 与 metacognitive state 也搬进来？

它们都是**循环内的"过程数据"**：失败计数、置信度、stuck 分数——只在循环跑的过程中有意义，循环结束就丢。这类状态必须和 Runner 同一作用域，否则就要在每步开始时显式 reload，破坏接口的简洁。

`orchestrator` 旧目录下其实还存在同名的 `failure_tracker.go` / `metacognition.go`——它们是**桥接层**，把 agentloop 的状态投影到 orchestrator 自己的事件/指标系统。orchestrator 不再实现这些状态机，只复用类型。

### Sub-agent 用同一个 LLM 客户端会不会撞速率？

会。所以 `agentloop` 本身**不做速率控制**，把流控留给底层 `llm.Client`（circuit breaker + per-model bucket，见 [03_llm](03_llm.md)）。Runner 在循环里只关心一件事：LLM 返回错就重试（默认 3 次，指数退避 2/4/8s）。

---

## 2. 依赖架构

```
              ┌─────────────────────────────────────┐
              │  multiagent.sub-agent (Runner 用户)│
              │  orchestrator.reactLoopCore (直接消│
              │   费 agentloop 原语，不起 Runner) │
              └────────────────┬────────────────────┘
                               │ NewRunner(...) — 仅 multiagent
                               ▼
              ┌──────────────────────────────────────┐
              │   agentloop.Runner（仅 sub-agent 用）│
              │   ┌──────────────────────────────┐   │
              │   │  Run(ctx, opts, sink)        │   │
              │   │   for step := 0..MaxSteps:   │   │
              │   │     1. 反思检查 (10 步周期 + │   │
              │   │        meta.NeedsReflection) │   │
              │   │     2. token 预算 / 裁剪      │   │
              │   │     3. LLM 调用 (含重试)     │   │
              │   │     4. tool_call 分发        │   │
              │   │     5. ClassifyToolError +   │   │
              │   │        adaptiveFB.Record     │   │
              │   │     6. meta.RecordOutcome    │   │
              │   │     7. failTracker.Track     │   │
              │   └──────────────────────────────┘   │
              └────┬──────┬──────┬──────┬────────────┘
                   │      │      │      │
            ┌──────┘      │      │      └──────┐
            ▼             ▼      ▼             ▼
        LLMCaller   ToolExecutor   ToolProvider   EventSink
        (llm.Client) (orch adapter) (tools.Reg)   (SSE / Noop)
```

---

## 2.5 数据流总览

一次 `runner.Run()` 调用的事件序列（典型 ReAct 双步 + 一次裁剪）：

```text
═══════════════════════ 单次 Run 时序 ═══════════════════════

t0  sink ← step_start{step=1}
    pruner: tokens=12K < 128K → skip
    llm.ChatCompletion(msg=系统+用户) → resp{tool_calls=[read_file]}
    sink ← thinking{"我先读文件了解结构"}
    sink ← tool_call{name=read_file, args={path:"main.go"}}
    toolExec.Execute(read_file) → {Content:"package main..."}
    sink ← tool_result{name=read_file, isError=false}
    failTracker.Track(read_file, false) → FailCount=0
    meta.RecordOutcome(read_file, ok=true) → Confidence=0.75

t1  sink ← step_start{step=2}
    llm.ChatCompletion(msg=+assistant+tool) → resp{tool_calls=[edit_file]}
    toolExec.Execute(edit_file) → {IsError:true, Content:"path not found"}
    ClassifyToolError → ToolError{Category=not_found, Suggestion="..."}
    adaptiveFB.Record(te) → blacklist{edit_file:1}
    content += "\n\n[SYSTEM HINT] 资源不存在：path not found..."
    sink ← tool_result{isError=true}
    failTracker.Track(edit_file, true) → FailCount=1 (未达阈值3，不注入step-back)
    meta.RecordOutcome(edit_file, ok=false) → Confidence=0.50

... 假设 step 3 / 4 / 5 重复 edit_file 失败 ...

t5  failTracker.FailCount=3 → 返回 true
    messages ← failTracker.StepBackMessage()  ← 强制注入"FIX LOOP DETECTED"
    meta.StuckScore=0.6 + isRepeat=true → 0.8 → NeedsReflection()=true

t6  sink ← step_start{step=6}
    反思插点：messages ← meta.AdaptiveReflectionMessage()
       "[METACOGNITIVE CHECKPOINT — Step 6/50, confidence=20%, stuck=80%]"
       "⚠️ You appear to be stuck. STOP repeating the same approach."
    llm.ChatCompletion(...) → resp{Content:"我应该先 list_dir 确认路径..."}
    
t7  ... 成功路径 ...

t10 resp.ToolCalls = [] (LLM 决定收尾)
    return RunResult{Done=true, Content=..., StepsUsed=10}
```

---

## 3. 核心接口（`interfaces.go`）

### 3.1 `LLMCaller` — LLM 抽象

```go
type LLMCaller interface {
    ChatCompletion(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}
```

`llm.Client` 直接满足；测试中可注入 mock 控制返回的 `tool_calls` 序列。

### 3.2 `ToolExecutor` — 工具单次执行

```go
type ToolExecutor interface {
    Execute(ctx context.Context, tc models.ToolCall) (*models.ToolResult, error)
}
```

orchestrator 提供 `orchToolExecutor`（见 `internal/orchestrator/multiagent_bridge.go`），它把 ToolCall 派给 orchestrator 自己的内置 switch / MCP / Skill 三级分发——见 [09_orchestrator](09_orchestrator.md) §工具分发。Runner 不知道也不关心三级路由。

### 3.3 `ToolProvider` — 工具菜单

```go
type ToolProvider interface {
    Definitions() []models.ToolDefinition
}
```

主 agent 通常直接传 `tools.Registry`；sub-agent 会用一个**过滤包装**（`multiagent.toolFilter`，见 [22_multiagent](22_multiagent.md) §tool_filter）只暴露白名单子集。

### 3.4 `EventSink` — 事件输出

```go
type EventSink interface { Emit(event models.ReactStreamEvent) }
```

两个内置实现：
- `NoopSink` — 静默（sub-agent 默认）；
- `ChannelSink` — 写入带缓冲 channel（SSE handler 从 channel 拉数据写 HTTP 响应）。

---

## 4. `Config` —— 节流与预算

```go
type Config struct {
    MaxSteps         int   // 默认 50（主）/ 8（子）
    MaxContextTokens int   // 默认 128000 / 32000
    EnableReflection bool  // 默认 true / false
    LLMRetries       int   // 默认 3 / 2
}
```

辅助方法：

- `WithContextWindow(n)` — 按模型上下文窗口 ×95% 预留 5% 安全边距；
- `ComputeBudget(window)` — 返回 `ContextBudget{System:10%, RAG:20%, History:60%, CurrentMsg:10%}`，给上游 prompt builder 做分区裁剪。

**为什么留 5%？** Tokenizer 估算误差（不同模型 BPE 不同；中文混编低估约 15%）+ tool result 突发，避免精确卡到 limit 后被 API 直接 reject。

**为什么 60% 给历史而不是均分？** 历史是 ReAct 的"工作记忆"，被裁掉等于让 LLM 失忆；System+RAG+Current 加起来 40% 已是合理上限。

---

## 5. `Runner.Run()` 详解

整个方法只有 150 行，每步循环包含 7 个步骤：

### 5.1 步骤 1：反思检查（仅 `EnableReflection=true`）

```go
if step > 0 && step%10 == 0 {
    messages = append(messages, *meta.AdaptiveReflectionMessage(...))
}
if meta.NeedsReflection() { // confidence<0.3 OR stuck>0.7
    messages = append(messages, *meta.AdaptiveReflectionMessage(...))
}
```

两种触发源**互补**：
- **周期反思**（每 10 步）— 防止 LLM 一头扎进死胡同太久；
- **自适应反思**（按状态）— stuck 时立刻打断，不等周期。

子 agent 不开启反思（任务短、反思 prompt 反而把 token 吃掉）。

### 5.2 步骤 2：token 预算 / 裁剪

```go
totalTokens := sum(llm.ExactTokenCount(m.Content))
if totalTokens > MaxContextTokens {
    messages = PruneMessages(messages, MaxContextTokens)
}
```

`PruneMessages` 策略见 §7。

### 5.3 步骤 3：LLM 调用（带重试）

```go
for attempt := range LLMRetries {
    resp, err = llm.ChatCompletion(...)
    if err == nil { break }
    time.Sleep(time.Duration(2<<attempt) * time.Second) // 2s, 4s, 8s
}
```

重试的边界由 `LLMRetries` 控制，**只重试网络/超时类错误**——LLM 返回 400/401 类不可重试错误由 `llm.Client` 内部处理（不抛给 Runner）。

### 5.4 步骤 4：判定收敛

```go
if len(resp.ToolCalls) == 0 {
    return RunResult{Done: true, Content: resp.Content, ...}
}
```

LLM 决定不再调工具就是 ReAct 的退出条件。

### 5.5 步骤 5：工具执行循环

每个 `tool_call` 串行执行（注：当前是串行；多 tool_call 并发是 [10_planner](10_planner.md) 的活）：

```go
result, execErr := toolExec.Execute(ctx, tc)
content := result.Content
if execErr != nil { content = "Error: " + execErr.Error() }

// 智能截断：>8K token 时取头 8K + 尾 12K
if llm.FastEstimate(content) > 8000 { ... }

// 错误分类与适应性反馈
if isErr {
    toolErr := ClassifyToolError(tc.Name, result, execErr)
    adaptiveFB.Record(toolErr)
    content += "\n\n[SYSTEM HINT] " + adaptiveFB.BuildFeedback(toolErr)
}

// 元认知与失败追踪
meta.RecordOutcome(tc.Name, !isErr, ...)
if failTracker.Track(tc.Name, isErr) {
    messages = append(messages, failTracker.StepBackMessage())
}
```

**头尾截断而非头部截断**：错误信息通常在工具输出的**末尾**（stderr / exit code / 最后几行 stacktrace），而文件描述/上下文在**头部**。掐中间能同时保住"做了什么"和"为什么挂"。

### 5.6 步骤 6：消息回填

每次 tool_call 完成后 `messages = append(messages, {Role:"tool", Content:content, ToolCallID:tc.ID})`。OpenAI / Anthropic API 要求 tool_call 和 tool_result 一一对应、ID 匹配。

### 5.7 步骤 7：失败追踪

`failTracker.Track()` 返回 true 时（同一工具连续 3 次失败），强制注入 step-back 系统消息——把"换种方法"显式写进对话。

---

## 6. 工具错误分类与自适应反馈

### 6.1 `ToolError` 六分类（`tool_error.go`）

| Category | 触发关键词 | Retryable | 默认建议 |
|----------|------------|-----------|----------|
| `invalid_args` | `invalid` / `missing required` / `parse error` | ✅ | 检查参数格式 |
| `not_found` | `no such file` / `not found` / `does not exist` | ✅ | 先用 list/search 确认路径 |
| `permission` | `permission denied` / `forbidden` | ❌ | 不要重试，找替代 |
| `timeout` | `timeout` / `deadline exceeded` | ✅ | 可重试一次 |
| `exec_failed` | `command failed` / `exit status` | ✅ | 分析错误输出 |
| `internal` | （默认） | ❌ | 内部错误，换方法 |

**为什么用关键词匹配而非错误类型？** Tool 实现千变万化（内置 / MCP / Skill），强类型 errors 难以统一；关键词匹配是"够好就行"的鲁棒方案——遗漏会落到 `internal` 桶，影响仅是建议不够具体而非崩溃。

### 6.2 `AdaptiveFeedback` 黑名单（`adaptive_feedback.go`）

维护两层结构：
- `history []ToolError` — 最近 10 个错误的滑动窗口；
- `blacklist map[name]int` — 每个工具的连续失败计数。

阈值 `blacklistThreshold=3` 触发后，`BuildFeedback()` 不再给具体建议，直接给**替代方案**：

| 工具 | 替代建议 |
|------|----------|
| `read_file` | 改用 `list_dir` 或 `grep` 定位 |
| `execute_code` | 改用 `run_workspace_cmd` |
| `edit_file/write_file/patch_file` | 检查路径或 list_dir 确认 |
| `grep` | 改用 `rag_search` 或 `read_file` |

非黑名单时再细分"首次失败"vs"重复同类失败"两种文案，前者给修复提示，后者强调"换方法"。

### 6.3 与 `failure_tracker` 的区别

| 维度 | `failure_tracker` | `adaptive_feedback` |
|------|-------------------|---------------------|
| 触发粒度 | 同一工具连续失败 | 同类错误重复 |
| 反应 | 系统消息（强提示换思路） | 工具结果尾部追加 HINT |
| 阈值 | 3 次 | 1 次起反馈，3 次黑名单 |
| 状态 | 仅记最近一个工具 | 滑动窗口 10 个 |

两者**叠加生效**：黑名单 + step-back 同时触发时，LLM 会看到工具结果末尾的"换工具"建议 + 下一轮开头的"FIX LOOP DETECTED"系统消息。

---

## 7. `PruneMessages` —— 窗口裁剪策略

```go
PruneMessages(messages, maxTokens):
  keepBudget = maxTokens * 60%       // 给"最近上下文"
  pinnedTokens = sum(pinned 消息)
  adjustedBudget = keepBudget - pinnedTokens
  
  从后往前累加非 pinned 消息，直到吃掉 adjustedBudget
  至少保留最后 4 条
  
  result = [messages[0]] + [裁剪通知] + pinned + 末尾保留
```

四个不变量：

1. **`messages[0]` 永不裁** —— 它是 system prompt（KV cache 前缀）；
2. **pinned 消息永不裁** —— 用户/上游显式标记 `Pinned=true` 的关键上下文；
3. **至少保留最后 4 条** —— 避免裁过头导致 LLM 看不到刚发生的 tool_call/tool_result 配对；
4. **替换为一条裁剪通知** —— 让 LLM 知道"上文丢了多少 + 还能从 .plan.md 重读"。

裁剪通知的文本暗示了一个**契约**：长任务必须把关键状态落到 `.plan.md` 这类持久文件，单纯靠 messages 记忆不行。这是 ReAct 引擎层就编码的"良性 prompt 引导"。

---

## 8. `MetacognitiveState` —— 自我评估

`metacognition.go` 维护三个核心数：

| 字段 | 范围 | 触发条件 |
|------|------|----------|
| `Confidence` | 0-1 | 最近 8 步成功率 |
| `StuckScore` | 0-1 | 末尾连续失败数 / 8（+0.2 if isRepeat） |
| `UncertainAreas` | string[] | `AddUncertainty()` 显式调用（runner 在工具失败时打 "recent tool failure: X"） |

派生触发：

- `NeedsReflection()` — `Confidence<0.3 OR StuckScore>0.7`
- `ShouldRequestClarification()` — `Confidence<0.2 AND steps>4`（runner 目前未使用，留给上层 HITL 决定是否触发 `approval_request`）

反思 prompt 由 `AdaptiveReflectionMessage()` 动态拼接：

```
[METACOGNITIVE CHECKPOINT — Step 6/50, confidence=20%, stuck=80%]
⚠️ You appear to be stuck. STOP repeating the same approach.
Consider: (1) re-read the error messages carefully, (2) try a completely different strategy, (3) ask the user for clarification.
Your recent actions have low success rate. Before the next tool call:
- State what you're trying to achieve in one sentence
- Explain WHY you believe the next action will succeed
- If unsure, say so explicitly rather than guessing
Known uncertainties: recent tool failure: edit_file, recent tool failure: edit_file
⚠️ Only 9 steps remaining. Focus on delivering a working result, not perfection.
```

不同条件组合产生不同长度的提示——不需要永远都把所有诊断信息都塞进去（浪费 token）。

---

## 9. `TrajectoryMemory` —— 跨任务复用

历史的成功工具序列被保存为 `TrajectoryEntry{Intent, Tools, StepCount}`，按 intent 索引。orchestrator 在任务开始前可以调用：

```go
hint := trajectoryMem.FormatHint(intent)
if hint != "" { messages = prepend(systemMsg(hint), messages) }
```

输出形如：

```
[TRAJECTORY HINT] 历史上类似任务的成功工具序列：
  1. read_file → search_code → edit_file → run_tests（4 步）
  2. list_dir → read_file → patch_file → run_tests（4 步）
可参考上述模式，但应根据当前具体情况灵活调整。
```

**为什么不直接搞 in-context demonstration？** 完整 demonstration 是几 KB；这里只塞 tool 名称（几十字节），让 LLM 自己决定每一步的具体参数。性价比远超过 few-shot。

**限额**：`maxTrajectories=50`（FIFO），`trajectoryTopK=3`，`maxToolsPerEpisode=20`。Runner 自己不写 TrajectoryMemory，由 orchestrator 在任务完成时调 `Record(intent, tools, success)`。

---

## 10. 与其他模块的边界

### 10.1 上游：orchestrator

```go
// internal/orchestrator/multiagent_bridge.go
type orchToolExecutor struct{ orch *Orchestrator }
func (e *orchToolExecutor) Execute(ctx, tc) (*ToolResult, error) {
    return e.orch.executeTool(ctx, tc)  // 三级分发
}
```

orchestrator **没有**用 `agentloop.NewRunner`——它在 `react_core.go` 的 `reactLoopCore` 里**直接消费**这些原语（`TrajectoryMemory` / `AdaptiveFeedback` / `ConsecutiveFailureTracker` / `ClassifyToolError` / `PruneMessages` / `NewMetacognitiveState`），把主循环和 SSE/审计/Temporal 拦截**编织在一起**。Runner 则是**为 sub-agent 准备的**——sub-agent 不需要 SSE/审计，可以走简化的 Runner 接口。

orchestrator 的 reactLoopCore 职责：
1. 构造 prompt（系统 prompt + RAG + history）；
2. 步进 ReAct 主循环（直接调 llm.ChatCompletion + 分发 tool_calls，没有 Runner 包装）；
3. 调用 `AdaptiveFeedback` / `ConsecutiveFailureTracker` 决策 step-back；
4. 把 `EventSink`/SSE/审计/HITL 拦截编织在循环每一步上；
5. 处理收尾（写 session、写 `TrajectoryMemory`、触发 verification）。

### 10.2 下游：multiagent

```go
// internal/multiagent/sub_agent.go
runner := agentloop.NewRunner(deps.LLM, toolExec, toolProv, agentloop.DefaultSubAgentConfig(), logger)
result := runner.Run(ctx, agentloop.RunOpts{SystemPrompt: a.prompt, Messages: msgs}, sink)
```

每个 sub-agent 都是一个独立的 Runner 实例，配置精简、工具白名单过滤、Sink 静默。

### 10.3 平行：planner

planner（[10_planner](10_planner.md)）是 ReAct 的**替代品**而非用户——DAG 多步规划时 planner 自己接管工具调度，不走 Runner。两者**互不依赖**。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| Runner 不持有 session / 持久化 | 持久化是 orchestrator 责任，Runner 纯内存 |
| 工具执行**串行**而非并发 | 同一工具步内并发会让 messages 顺序混乱；并发是 planner 的范畴 |
| 反思插点固化为每 10 步 + 自适应 | 用户实测：>15 步过于频繁、<5 步浪费；10 步是经验值 |
| `LLMRetries` 默认 3 + 指数退避 | 与 `llm.Client` 自身的 circuit breaker 叠加，避免雪崩 |
| 工具结果**头尾截断**而非头部截断 | 错误信息通常在尾部，头部是上下文；保两端 |
| `containsAny` 关键词匹配错误 | 跨实现兼容（内置/MCP/Skill 错误格式不一） |
| 失败计数**只看连续同名** | 跨工具的"换了好几个还不行"由 metacognition 的 stuck score 接 |
| `EnableReflection=false` 时 metacognition **仍记录** | sub-agent 不主动反思但保留状态供 supervisor 读取 |

---

## 12. 后续演进

- [ ] **并发工具执行**：单步内的多个 tool_call 如果互不依赖，可并发跑（与 planner 的 DAG 调度合并到这层）
- [ ] **工具执行预算**：单步内累计耗时 > 30s 自动 step_start 下一轮，避免 LLM 卡在工具 IO
- [ ] **裁剪策略可插拔**：当前 `PruneMessages` 硬编码 60%/4 条；将来可接 `PrunerStrategy` 接口让 RAG 重排
- [ ] **`TrajectoryMemory` 持久化**：当前仅内存，重启即丢；接 Postgres 实现跨进程复用
- [ ] **元认知阈值自适应**：`<0.3` 触发反思是经验值，可按任务类型差异化
- [ ] **黑名单跨 session 共享**：当前 `AdaptiveFeedback` 是循环内状态；将"长期不靠谱的工具"反馈到 toollearn ([23_toollearn](23_toollearn.md))
- [ ] **裁剪通知国际化**：当前中英混编，i18n 化便于多语言 UI

---

## 13. 设计教训

`agentloop` 抽出之前，`orchestrator` 内同名文件（`failure_tracker.go` / `metacognition.go` / `message_pruner.go`）曾经做过两次大改：

1. **第一次**：把"失败检测"硬编码进 reactLoop 里，5 行 if-else 完成判断。结果跨场景失效（不同工具失败的判定不一致）。
2. **第二次**：抽出 `FailureTracker` 类型，但仍在 orchestrator 包内。结果 multiagent 想复用时只能 import orchestrator，循环依赖。
3. **第三次（当前）**：抽到 `agentloop` 独立包，orchestrator 保留同名桥接文件做 metrics emit。

教训：**循环内的过程状态**（failure / metacognition / pruner）和**循环外的业务状态**（session / verification / approval）必须分包。否则前者一定会被多个调用者复用，后者要么膨胀要么循环依赖。

---

下一篇：[`22_multiagent.md`](22_multiagent.md) —— 子 Agent 池、消息总线、冲突解决：多智能体协作的实现。
