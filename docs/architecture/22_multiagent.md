# 22 · 多智能体协作 `internal/multiagent`

> 代码：
> - `types.go` (82) — `AgentType` / `DelegationRequest` / `AgentDeps` / `SupervisorConfig`
> - `supervisor.go` (275) — `Supervisor` 计划编排、DAG 分级执行、冲突注入
> - `agent_pool.go` (65) — 有界并发的 SubAgent 池
> - `sub_agent.go` (154) — `SubAgent` 双模式执行（ReAct vs 直接派发）
> - `message_bus.go` (95) — 进程内 pub/sub 消息总线
> - `conflict_resolver.go` (225) — 文件级并发写冲突检测与解决
> - `role_selector.go` (184) — 基于历史表现的动态角色选择
> - `tool_filter.go` (72) — 工具白名单包装器
> - `prompts.go` (34) — 各角色的系统 prompt 模板
>
> 测试：`multiagent_test.go` / `sub_agent_test.go` / `tool_filter_test.go` / `collaboration_test.go`

---

## 1. 模块定位

**"把一份大任务切成几片，分给不同性格的 sub-agent 同时干，再把冲突和结果挑掉。"**

`internal/multiagent` 与 [10_planner](10_planner.md) 配套使用：planner 产出 DAG 计划，本模块负责**执行**这个 DAG——按拓扑序分级 fan-out 给多个 sub-agent，处理它们之间的文件写冲突、共享指标、消息互通。

它**不取代** orchestrator 的 ReAct 主循环；恰恰相反，每个 sub-agent 内部都跑一个**精简版的 ReAct 循环**（`agentloop.DefaultSubAgentConfig`：8 步、32K token、关反思），见 [21_agentloop](21_agentloop.md)。

典型场景：

> 用户：「重构 `user_service.go`、给它写单元测试、并且审查这次改动」
>
> Planner 产出 3 步骤 DAG：(A) refactor → (B) tests → (C) review；其中 B/C 依赖 A 完成。
>
> Supervisor 第一级跑 A（code agent），完成后第二级**并发**跑 B（test agent）+ C（review agent）。

---

## 1.5 核心设计问题

### 为什么不直接让 orchestrator 串行干完？

串行的复合任务在两类场景下严重浪费：

1. **天然并行**：测试和静态审查可以同时进行，两边都只读改动后的文件；
2. **专业化分工**：同一个 LLM session 在 code/test/review 之间频繁切换，prompt cache 命中率低，且 system prompt 必须包含所有职责说明（token 浪费）。

让每个 sub-agent 跑独立的小 ReAct，可以：

- 各自用**专用 system prompt**（code agent 的 prompt 比 review agent 的紧凑得多）；
- 各自配**工具白名单**（test agent 不能 write_file，review agent 不能 patch_file）；
- 各自的失败计数 / 元认知**互不污染**。

### 为什么文件写冲突要检测而不是简单串行？

DAG 同层（无依赖）的步骤是**故意并发**的，但 LLM 可能让两个 sub-agent 都改 `auth.go`——这是 planner 没看出来的潜在冲突（语义级冲突，依赖图层面看不出来）。

`ConflictResolver` 在 supervisor 派单时拦截**文件写入类操作**（`write_file` / `edit_file` / `patch_file` / `apply_diff` / `rename_symbol`），按 `StrategyPriority` 解决：code > test > review。失败的一方拿到 `conflict on X: resolved in favor of Y` 错误信息，可在后续级别重试或退让。

### 为什么 role_selector 要做"动态选择"而不是 planner 直接指定？

Planner 输出的 `action`（如 `write_file`）有多个 agent 类型都能处理（code/test 都能 write）。Role selector 用三档加权：

| 权重 | 信号 |
|------|------|
| 60% | 最近 10 步成功率 |
| 20% | action affinity（write_file 偏 code，run_tests 偏 test，read_file 偏 review）|
| 20% | recency（多久没用了——避免某一类 agent 长期闲置） |

→ 如果 review agent 最近成功率 90%，code agent 是 40%，那么写文件的活也可能交给 review agent。这是**短期内**对失败 agent 的"降权"，比 hard-coded 路由灵活。

---

## 2. 依赖架构

```
                    ┌──────────────────────────────┐
                    │     planner.Plan (DAG)       │
                    │   Steps: [A, B, C, D]        │
                    │   Deps:  A→B, A→C, B→D, C→D  │
                    └─────────────┬────────────────┘
                                  │
                                  ▼
                    ┌──────────────────────────────┐
                    │  Supervisor.Execute(plan)    │
                    │  1. TopologicalSort → levels │
                    │  2. for level in levels:     │
                    │       executeLevel(parallel) │
                    └──┬──────────┬────────┬───────┘
                       │          │        │
                       ▼          ▼        ▼
                   ┌──────┐  ┌──────┐  ┌──────┐
                   │Agent │  │Agent │  │Agent │   ← 同层并发执行
                   │ pool │  │ pool │  │ pool │      (sem-bounded)
                   └──┬───┘  └──┬───┘  └──┬───┘
                      │         │         │
                      ▼         ▼         ▼
              ┌────────────────────────────────┐
              │   SubAgent.ExecuteWithDeps     │
              │   ┌────────────────────────┐   │
              │   │ if ReasoningRequired:  │   │
              │   │   agentloop.Runner.Run │   │   ← 内置 ReAct
              │   │ else:                  │   │
              │   │   dispatcher.Dispatch  │   │   ← 单工具直派
              │   └────────────────────────┘   │
              └──┬─────────────────────────────┘
                 │
       ┌─────────┼──────────┬─────────────┐
       ▼         ▼          ▼             ▼
   tool_filter  conflict_resolver  message_bus  role_selector
   (限制可用)  (检测冲突 + 选赢家) (通知 hub)   (记录绩效)
```

---

## 2.5 数据流总览

```text
═══════════════ Supervisor.Execute() 全景 ═══════════════

[planner.Plan]
   ↓ TopologicalSort
[ levels = [[A], [B, C], [D]] ]

for levelIdx, level := range levels:
   ┌────────── executeLevel(level) ──────────┐
   │ var wg sync.WaitGroup                   │
   │ for step := range level:                │
   │   go func(s):                           │
   │     ┌── executeStep(s) ──────────────┐  │
   │     │ 1. candidates =                │  │
   │     │    CandidatesForAction(action) │  │
   │     │    → [code, test]              │  │
   │     │                                │  │
   │     │ 2. agentType =                 │  │
   │     │    roleSelector.SelectBest()   │  │
   │     │    → AgentCode                 │  │
   │     │                                │  │
   │     │ 3. agent = pool.Acquire(type)  │  │
   │     │    ⇒ 阻塞直到有空槽（sem）       │  │
   │     │                                │  │
   │     │ 4. if isFileWriteAction:       │  │
   │     │      conflict =                │  │
   │     │      conflictResolver.RecordE..│  │
   │     │      if conflict:              │  │
   │     │        winner = Resolve()      │  │
   │     │        if winner != me:        │  │
   │     │          → 标记 blocked         │  │
   │     │                                │  │
   │     │ 5. (未 blocked) :              │  │
   │     │    deps = buildAgentDeps()     │  │
   │     │    out, err = agent.Exec..()   │  │
   │     │      ↓                         │  │
   │     │    [SubAgent 内部走 agentloop] │  │
   │     │                                │  │
   │     │ 6. pool.Release(agent)         │  │
   │     │ 7. roleSelector.RecordResult() │  │
   │     │ 8. bus.Publish(step_complete)  │  │
   │     └────────────────────────────────┘  │
   │ wg.Wait()                                │
   │ if any !Success: return failure          │
   └─────────────────────────────────────────┘

[ allResults []AgentResult ]
return SupervisorResult{Success, Results, Summary}
```

---

## 3. 三个核心数据结构

### 3.1 `AgentType` — 三类 sub-agent

```go
const (
    AgentCode   AgentType = "code"    // 写代码
    AgentTest   AgentType = "test"    // 跑测试
    AgentReview AgentType = "review"  // 读代码 + 审查
)
```

每类自带：
- **工具白名单**（`sub_agent.go:allowedTools()`）
- **角色 prompt**（`prompts.go:agentTypePrompt()`）
- **冲突优先级**（`conflict_resolver.go:priority`）

新增 agent type 需同时改这 4 处。

### 3.2 `DelegationRequest` — 派单参数

```go
type DelegationRequest struct {
    StepID            string       // planner.Step.ID
    AgentType         AgentType    // 接单角色
    Action            string       // 默认工具名（直派模式）
    Task              string       // 自然语言任务描述（ReAct 模式）
    Parameters        json.RawMessage
    Context           string
    Timeout           time.Duration
    AllowedTools      []string     // 覆盖 type 默认白名单
    ReasoningRequired bool         // true → ReAct, false → 单工具直派
}
```

`ReasoningRequired` 是双模式的开关：
- `false`（默认）— **fast path**：直接调 `dispatcher.Dispatch(tool, args)`，0 LLM 调用，毫秒级返回；
- `true`— **reasoning path**：起一个 `agentloop.Runner`，最多 8 步 ReAct。

### 3.3 `AgentDeps` — ReAct 模式的依赖打包

```go
type AgentDeps struct {
    LLM          agentloop.LLMCaller
    ToolExecutor agentloop.ToolExecutor
    ToolProvider agentloop.ToolProvider
    EventSink    agentloop.EventSink
}
```

Supervisor 在 `buildAgentDeps()` 里检查三者齐备才构造；缺一律 `return nil`，sub-agent 退回 fast path。

---

## 4. `Supervisor` —— DAG 编排器

### 4.1 拓扑分级

```go
levels, _ := planner.TopologicalSort(plan.Steps)
// → [[A], [B, C], [D]]
```

planner 的 `Step.DependsOn []string` 决定 DAG 边；拓扑序保证一级内的步骤**完全无依赖**，可安全并发。

### 4.2 一级并发的两条约束

```go
ctx, cancel := context.WithTimeout(ctx, s.config.TotalTimeout)  // 总超时
stepCtx, _ := context.WithTimeout(ctx, s.config.StepTimeout)    // 单步超时
```

默认 30 分钟总 / 5 分钟单步。这是兜底，避免 ReAct 卡死。

并发上限由 `AgentPool` 的 channel semaphore 控制（默认 `MaxParallel=3`）—— 即使一级有 10 个步骤，同时只跑 3 个。这是为了：

1. **LLM 配额**：3 个 sub-agent 同时调 LLM 已经吃掉相当于一个主 agent 3 倍的 RPM；
2. **工具竞争**：文件操作有冲突解决但 sandbox 容器没有，太多并发会让 Docker daemon 撑不住；
3. **可观察性**：3 路并发的日志还能人工跟读，10 路就只能依赖结构化追踪。

### 4.3 早返回策略

```go
for _, r := range levelResults {
    if !r.Success {
        return SupervisorResult{Success: false, ...}, nil
    }
}
```

**任一步失败 → 整个 plan 失败**。不重试、不部分提交。这是有意为之：
- 失败时让上层（orchestrator / planner）拿到完整的失败上下文（哪步、哪个 agent、什么错误）；
- 重试策略由上层决定（重新规划？换 agent type？降级到串行 ReAct？）。

---

## 5. `AgentPool` —— Channel Semaphore

```go
type AgentPool struct {
    sem    chan struct{}      // buffered N → 并发上限
    agents map[AgentType][]*SubAgent  // type → 空闲列表
}

func (p *AgentPool) Acquire(agentType AgentType) *SubAgent {
    p.sem <- struct{}{}        // ① 阻塞直到 sem 有槽
    if 有空闲 agent of type:
        return 复用
    return NewSubAgent(type)   // 否则新建
}

func (p *AgentPool) Release(agent *SubAgent) {
    放回 idle list
    <-p.sem                    // ② 释放槽
}
```

**为什么手写而不是用 `golang.org/x/sync/semaphore`？**

- 标准库 semaphore 不区分 agent type；本池要的是"按类型回收"以利复用 + 全局并发上限同时控制；
- channel sem 比 `sync/semaphore.Weighted` 实现更简单，性能持平，可读性更好。

**SubAgent 实例为什么要 pool？** 当前 `SubAgent` 状态很少（ID + Type + logger），new 一个的代价很低。pool 主要为以后扩展——若 sub-agent 开始持有连接（如某些 LSP client）就可以零改动复用。

---

## 6. `SubAgent` 的双模式

### 6.1 Fast path（`dispatchDirect`）

```go
if !req.ReasoningRequired:
    if params.Tool 在白名单:
        return dispatcher.Dispatch(ctx, params.Tool, params.Args)
    return error "tool not allowed"
```

`ToolDispatcher` 是 orchestrator 实现的接口（`orchestrator.executeTool` 适配），意味着 fast path 走的还是主 agent 的三级分发，但**不绕 ReAct 循环**——没有 LLM 调用、没有反思、没有失败计数。

适用于 planner 能拍板"就这一个工具能搞定"的步骤，比如 `run_tests` 单步。

### 6.2 Reasoning path（`executeReAct`）

```go
toolExec := NewFilteredToolExecutor(deps.ToolExecutor, allowedTools)
toolProv := NewFilteredToolProvider(deps.ToolProvider, allowedTools)
runner := agentloop.NewRunner(deps.LLM, toolExec, toolProv, DefaultSubAgentConfig(), logger)
messages := [系统 prompt(角色 + 任务), 用户消息(任务参数)]
result := runner.Run(ctx, RunOpts{Messages: messages, TaskID: req.StepID}, sink)
```

关键点：

- **工具被双层过滤**：executor 拒绝调用 + provider 隐藏定义（同一份 allowlist）。LLM 看不到也调不到非白名单工具；
- **system prompt 极简**（见 `prompts.go`）——只讲角色 + 当前任务 + "最多 8 步、走不通就报告"；
- **NoopSink** 默认——sub-agent 的中间事件不向 SSE 推送，避免污染前端事件流。

### 6.3 默认工具白名单

| AgentType | 工具集 |
|-----------|--------|
| `AgentCode` | read_file, write_file, edit_file, patch_file, apply_diff, list_files, create_directory, git_status/diff/commit/branch/log, run_workspace_cmd, **shell_exec**, **goto_definition/find_references/hover_info/rename_symbol** |
| `AgentTest` | run_tests, execute_code, read_file, run_workspace_cmd, shell_exec |
| `AgentReview` | read_file, search_code, list_files, git_diff/log, goto_definition/find_references/hover_info |

加粗工具是 P1 特性（PTY / LSP），见 [26_pty](26_pty.md) / [27_lsp](27_lsp.md)。

请求里 `req.AllowedTools` 不为空时覆盖默认列表——给上层（planner）留了显式收紧的口子。

---

## 7. `ConflictResolver` —— 文件级冲突

### 7.1 检测策略（保守版）

```go
isConflicting(a, b FileEdit) bool {
    if a.Action == "delete" || b.Action == "delete" { return true }
    return true   // ⚠️ 两个写就当冲突
}
```

**注释**：当前实现是 "any two writes from different agents to same file = conflict"，没有做"实际编辑范围重叠"检查。这是保守策略：

- ✅ 永不漏报；
- ⚠️ 会误报（两个 agent 写同一文件的不同函数也判冲突）；
- 🎯 决策：误报代价（一个 agent 走不下去）远小于漏报代价（两个 edit 互相覆盖造成静默数据损坏）。

### 7.2 三种解决策略

```go
const (
    StrategyLastWriter  // 后写赢
    StrategyFirstWriter // 先写赢
    StrategyPriority    // 按角色优先级 (code > test > review)
)
```

Supervisor 默认 `StrategyPriority`。优先级是**绝对的**：code agent 永远赢，跟先后无关。

**为什么 code 赢？** 三类 agent 的"权威性"：
- code agent 的本职就是改代码；
- test agent 的写文件通常是写测试 fixture（也合法但弱）；
- review agent 一般只读，写场景是修注释——优先级最低。

### 7.3 失败者的体验

```go
result.Success = false
result.Error = fmt.Sprintf("conflict on %s: resolved in favor of %s", filePath, winner.AgentID)
```

`SubAgent.Execute` **不被调用**——直接拿到 `Success=false`。这意味着失败方**没机会 retry**；orchestrator 拿到完整失败结果后决定下一步（多半是回退到串行 ReAct）。

---

## 8. `RoleSelector` —— 动态选角

### 8.1 评分公式

```go
score = SuccessRate*0.6 + Affinity*0.2 + Recency*0.2
```

- `SuccessRate`：滑动窗口 10 步内的成功比例（启动初期没数据时**全用 affinity**）；
- `Affinity`：`defaultAffinity(type, action)`，硬编码的 0-1 分；
- `Recency`：上次用过这个 type 的时长，1m / 5m / 30m 三档（1.0/0.8/0.5/0.2）。

**为什么 success rate 占大头？** 一旦 code agent 在最近 10 次里掉到 30% 成功率（不论原因），即使理论上它最适合做这件事，也应该让 review agent 或 test agent 试试。这是"探索-利用"的简单实现。

### 8.2 candidates 过滤

```go
func CandidatesForAction(action string) []AgentType {
    for type in [code, test, review]:
        if defaultAffinity(type, action) > 0.3:
            add to candidates
    return candidates
}
```

`affinity > 0.3` 才进候选——避免让 review agent 去 write_file（affinity ≈ 0）这种明显不合适的场景。

---

## 9. `MessageBus` —— Pub/Sub 总线

### 9.1 API

```go
ch := bus.Subscribe(recipient)
bus.Publish(Message{From, To, Type, Content, Timestamp})
bus.Broadcast(msg)  // 所有订阅者
bus.History()       // 最近 100 条
```

**用途**：
1. supervisor → 自己（订阅 step_complete 用于审计）；
2. sub-agent ↔ sub-agent（**当前未启用**，类型设计就绪但 prompts 没教 LLM 用）；
3. 调试 / 测试断言。

### 9.2 设计抉择

- **非阻塞 send**：缓冲 channel + `select { case ch <- msg: default: drop }`，避免一个慢订阅者拖死整个 bus；
- **history 限 100**：避免长任务把 bus 撑爆；旧消息直接丢；
- **无持久化**：内存即丢；要审计请走 [19_observability](19_observability.md) 的 audit log。

**为什么不用 redis pub/sub？** 当前 multiagent 严格**单进程内**协作，没有跨主机需求。Redis 增加了序列化 + 网络 RTT。一旦未来要把 sub-agent 放到独立容器，再换 Redis 也来得及（接口足够薄）。

---

## 10. 与其他模块的边界

### 10.1 向上：orchestrator / planner

```go
// orchestrator 在判定需要多 agent 协作时（PathSelector 决定）
plan := planner.GeneratePlan(intent)
supervisor := multiagent.NewSupervisor(
    dispatcher,             // 包装 orch.executeTool
    DefaultSupervisorConfig(),
    logger,
    multiagent.WithLLM(orch.llmClient),
    multiagent.WithToolExecutor(orchToolExec),
    multiagent.WithToolProvider(orch.toolRegistry),
)
result := supervisor.Execute(ctx, plan)
```

Supervisor 是 stateless 的（每次新建），其内部状态（pool / bus / metrics）随对象消亡。

### 10.2 向下：agentloop

每个 SubAgent 内部独立的 `agentloop.Runner`，配置由 `agentloop.DefaultSubAgentConfig()` 给（8 步 / 32K / 关反思 / 2 重试）。互不影响。

### 10.3 平行：planner

- planner 产 DAG（结构化的 Step 数组 + dep 边）；
- multiagent 执行 DAG（拓扑序 + 并发 + 冲突解决）。

两者**不感知对方实现**——planner 不知道执行用了几个 agent，multiagent 不知道 DAG 怎么生成。

---

## 11. 设计权衡

| 抉择 | 动机 |
|------|------|
| 双模式（fast / reasoning）共存 | 直派工具不该走 LLM 浪费 token；多步思考必须 LLM 兜底 |
| 文件冲突**保守判定** | 漏报代价远高于误报 |
| `StrategyPriority` 默认 | code 优先级最高符合直觉；first/last writer 易让弱角色"抢跑" |
| 总超时 + 单步超时双层兜底 | 单步卡死不应拖累整体；整体太久也不应放任 |
| pool 按 type 索引 | 给将来 stateful sub-agent 留接口；当前无害 |
| message bus 不持久化 | 持久化让设计复杂数倍，当前需求不足 |
| sub-agent 失败**不重试** | 重试逻辑应该是上层语义（换 agent? 重新规划?）而非通用机制 |
| `EventSink` 默认 NoopSink | 不污染主 agent 的 SSE 流；sub-agent 事件按需通过 supervisor 转发 |
| 同层内 supervisor 不感知顺序 | 单层完全无依赖，顺序无意义；进一步释放并发空间 |

---

## 12. 后续演进

- [ ] **Sub-agent 之间通信**：MessageBus 已就绪但 prompts 未教 LLM 用——将来 code agent 可以 publish "我改了哪些文件" 给 review agent 看
- [ ] **细粒度冲突**：现在两写就判冲突；将来用 AST diff 检查实际编辑范围（tree-sitter 已在 [24_treesitter](24_treesitter.md) 接入）
- [ ] **Sub-agent 持久化**：跨任务复用学到的工具偏好（接 [23_toollearn](23_toollearn.md)）
- [ ] **流式 SubAgent 事件**：把每个 sub-agent 的 thinking/tool_call 通过 supervisor sink 转发到主 SSE，前端可以画"几个 agent 同时干"的视图
- [ ] **第四类 agent**：`AgentSecurity`（专门跑 gitleaks + sast）；新增需要改 4 处白名单
- [ ] **跨进程**：把 supervisor 与 sub-agent 拆成独立 container，bus 换 redis stream（参考 [16_store](16_store.md) §跨进程协作）
- [ ] **可观察性**：暴露 `multiagent_step_duration_seconds{type,success}` / `multiagent_conflicts_total` 等指标

---

下一篇：[`23_toollearn.md`](23_toollearn.md) —— 工具使用学习系统：从历史数据中蒸馏工具使用模式。
