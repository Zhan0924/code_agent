# 10 · Planner `internal/planner` + `orchestrator/planner_bridge.go`

> **代码骨架（行号对应 2026-05 真实代码）**
>
> | 文件 | 行数 | 角色 |
> |---|---|---|
> | `planner.go` | 437 | `Plan`/`Step` 数据模型、`Planner.CreatePlan/RevisePlan/improvePlan`、`ValidateDAG`、`TopologicalSort`、JSON 解析容错 |
> | `evaluator.go` | 233 | `PlanEvaluator`：从 4 个维度（Completeness/Feasibility/Efficiency/Robustness）打分，触发 `improvePlan` |
> | `executor.go` | 455 | `Executor.Execute` 拓扑分层 + 同层并发 + 失败 `RevisePlan`；附带 `EstimateComplexity`/`NeedsPlanning` 启发式 |
> | `goal_decomposer.go` | 137 | `GoalDecomposer.Decompose`：让 LLM 把复杂目标拆成 2–4 个 `SubGoal`，每个再生成独立 Plan |
> | `hierarchical.go` | 116 | `HierarchicalPlanner`：编排 Decompose → 逐 SubGoal Plan → Executor → 失败重规划，门槛 `NeedsHierarchical >= 12` |
> | `progress_tracker.go` | 124 | 跟踪 SubGoal 状态 + 连续失败计数，**3 次连续失败触发 replan** |
>
> **桥接到 Orchestrator**
> - `orchestrator/planner_bridge.go` (277) —— `AttachPlanner` 注入 + `MaybeUsePlanner` 入口 + `executePlanStep` 闭包；并把 plan 写到 `o.store`。
> - `orchestrator/micro_plan.go` (39) —— **与本章独立** 的"小计划"机制：周期性插入 system message 让 LLM 自述下一步。**不是 DAG Planner**，但容易和本章混淆，单独说明在 §13。

---

## 1. 模块定位

**"ReAct 的替身：把 LLM 调工具的决策权从'每步问一次'改为'开头问一次'。"**

`internal/planner` 是 Orchestrator 的**可选**第二条腿。当 `MaybeUsePlanner` 通过 `NeedsPlanning(task) → complexity >= 5` 时切到 Plan-and-Execute 模式；否则继续走 ReAct（`09_orchestrator`）。

### 1.1 场景对比

| 用户请求 | ReAct | Planner | Hierarchical |
|---|---|---|---|
| "这段代码为啥报错？" | ✅ 边试边改，思考链短 | ❌ 没有静态计划可下 | ❌ 杀鸡用牛刀 |
| "跑一下测试再告诉我结果" | ✅ 一步完事 | ❌ 多此一举 | ❌ |
| "把项目所有 `log.Print` 换成 `slog.Info`" | ❌ 50 次 LLM 调用 | ✅ 一次出 N 条 edit 计划 | ❌ |
| "新建 FastAPI 项目 + 3 个路由 + pytest" | ⚠️ 易跑偏 | ✅ DAG 可 review | ⚠️ 单层够用 |
| "把 monolith 拆 5 个微服务，配 K8s + CI" | ❌ | ⚠️ DAG 太宽 | ✅ 先 Decompose 4 个子目标 |

### 1.2 切换门槛（来自 `executor.go` 实测代码）

| 函数 | 行号 | 门槛 | 路由 |
|---|---|---|---|
| `NeedsPlanning(task)` | 436 | `EstimateComplexity >= 5` | ReAct → Planner（DAG） |
| `NeedsHierarchical(task)` | 113 | `EstimateComplexity >= 12` | Planner → HierarchicalPlanner（先拆子目标） |

> ⚠️ **`NeedsHierarchical` 未被 `MaybeUsePlanner` 调用** —— `HierarchicalPlanner` 类型存在、单测覆盖，但 `planner_bridge.go` 里**没有任何分支会切到它**。属于"代码就位但未接线"的孤儿能力，列入 §11 P1 项。

### 1.3 Planner 的真正价值

- **少 LLM 调用**：一次出 10 步 plan ≠ 10 次 ReAct。成本/延迟同时下降。
- **可视化 + 可审核**：`Plan.Summary()` (planner.go:344) 直接渲染 markdown，前端可让用户先看 plan 再 approve（**该 UX 未实现** —— 当前 plan 创建后立即执行）。
- **并行加速**：DAG 同层无依赖 step 并发执行（受 `MaxParallelism` 限流）。
- **失败修订**：`RevisePlan` 在保留已完成 step 的前提下让 LLM 重做剩余，最多 `maxRevision=2` 次。
- **质量自检**：`PlanEvaluator` 在 CreatePlan 后自动评分，低于 0.7 触发 `improvePlan` 用 LLM 第二轮改写。

---

## 1.5 核心设计问题

### Q1：为什么是 DAG 而不是线性列表？

DAG 的边表达"A 完成 B 才能开始"。独立分支可以**并发执行**：

```
start
  ├── 改 go.mod ──→ go mod tidy ──→ go test ──┐
  ├── 更新 import ────────────────────────────→─ done
  └── 改文档 ─────────────────────────────────→─┘
```

三个分支并行，收敛到 done。线性列表做不到。`TopologicalSort` (planner.go:281) 返回的不是一维顺序而是 `[][]Step`（**按层分组**），让 Executor 直接 wg.Wait 一层。

### Q2：为什么把 LLM 抽象成 `Call(systemPrompt, userPrompt) string`？

```go
// planner.go:58
type LLMCaller interface {
    Call(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}
```

刻意比 `llm.Client.ChatCompletion` 更窄：

- 测试时 mock 一行函数即可（见 `planner_test.go`），不需要构造 `*llm.ChatRequest`。
- 把 messages 数组、temperature、JSONMode 等细节锁死在 adapter（`llmCallerAdapter`，planner_bridge.go:54），让 planner 包零 LLM SDK 知识。

代价：planner 用不了 streaming、tool calling、function calling。**这是设计意图，不是缺陷**——计划生成天然是一次大 batch，没必要 streaming。

### Q3：为什么先 Evaluate 后 improve，而不是直接信 LLM 的第一次输出？

`evaluator.go` 用纯启发式（关键词覆盖率、动作合法性、冗余检测、依赖链长度）对计划打 4 维分。Overall < 0.7 触发 `improvePlan`，把质量报告（`q.FormatReport()`）丢回给 LLM 让它二改。

**为什么不直接让 LLM "更好地"生成一次**？因为 LLM 自评 plan 经常自满；用代码做一致性检查更便宜更刚性（关键词覆盖、动作合法性都是 O(N) 字符串操作，不花 token）。

### Q4：`maxRevision = 2` 的硬编码

```go
// executor.go:81
maxRevision: 2,
```

理由：

- 三次仍失败多半是**任务本身不可行**（缺工具、缺权限、外部服务挂了）。
- 保护钱包：每次 revision = 1 次 LLM 调用 + 整轮重新拓扑排序与剩余 step 重跑。
- 用户态可见反馈优先于"再试一次"——告诉用户哪一步失败比偷偷重试到 OOM 更有用。

### Q5：为什么不用 Temporal 做 Planner？

Temporal Server（PG/Cassandra 后端）+ Worker 对 "1–20 分钟任务"过度工程。Temporal 的核心价值在**HITL 挂起几小时不消耗资源**——planner 场景没这个需求。

实际折中：**进程内 DAG 执行 + Plan 写 `o.store`（Postgres）做最终态记录**（planner_bridge.go:264 `persistPlan`）。不做断点续跑，crash 即丢；要长寿命任务请直接用 Temporal Workflow（`11_temporal`）。

---

## 2. 依赖架构

```
┌───────────────────────────────────────────────────────────────────────┐
│ Orchestrator.ProcessMessage  (orchestrator.go:293)                    │
│                                                                       │
│   ┌───────────────────────────────────────────────────────────────┐   │
│   │ MaybeUsePlanner(ctx, task)                                    │   │
│   │   if o.planner == nil           → fall through to ReAct       │   │
│   │   if !NeedsPlanning(task.UserInput)  → fall through           │   │
│   │   if o.supervisor != nil && planHasParallelism                │   │
│   │       → executePlanWithSupervisor (多智能体路径，22_multiagent)│   │
│   │   else  → o.planner.executor.Execute                          │   │
│   └───────────────────────────────────────────────────────────────┘   │
└──────────────┬────────────────────────────────────────────────────────┘
               │
               ▼
   ┌──────────────────────────────┐         ┌────────────────────────┐
   │  Planner (planner.go)        │         │ PlanEvaluator          │
   │  · CreatePlan                ├────────▶│  · Evaluate (4 维)      │
   │  · improvePlan               │◀────────┤  · ShouldImprove < 0.7  │
   │  · RevisePlan                │         └────────────────────────┘
   │  · ValidateDAG/TopologicalSort│
   └──────────────┬───────────────┘
                  │
                  ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │  Executor (executor.go)                                         │
   │  · Execute: for revision in [0..maxRevision]:                   │
   │      levels := TopologicalSort                                  │
   │      for each level: executeLevel (WaitGroup + semaphore)       │
   │      if failures: RevisePlan ← Planner                          │
   └──────────────┬──────────────────────────────────────────────────┘
                  │ per-step
                  ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │  StepExecutor (闭包，由 orchestrator 注入)                       │
   │  ≡ o.executePlanStep:                                           │
   │      models.ToolCall{Name: step.Action, Args: step.Parameters}  │
   │      → o.executeTool (orchestrator.go:1360)                     │
   │      → MCP / tools.Registry / skill.Registry 三级分派 (09_orch §5)│
   └─────────────────────────────────────────────────────────────────┘

   ┌──── 可选 ── 复杂度 >= 12 但目前未接线 ──────────────────────────┐
   │  HierarchicalPlanner (hierarchical.go)                          │
   │  · Decompose (LLM 拆 2-4 SubGoal)                              │
   │  · 逐 SubGoal CreatePlan + Executor                            │
   │  · ProgressTracker：3 次连续失败 → replanSubGoal                │
   └─────────────────────────────────────────────────────────────────┘
```

**关键解耦**：planner 包**不导入** sandbox/skill/mcp 任何一个。它只持有 `StepExecutor` 这个函数类型（executor.go:56）；具体怎么跑工具完全由 orchestrator 注入的闭包决定。

---

## 2.5 数据流总览

### 2.5.1 主路径：`MaybeUsePlanner` → Plan 落地

```
1. orchestrator.MaybeUsePlanner
   │
   │ ① NeedsPlanning(task.UserInput) — 关键词 + 长度启发式 score >= 5
   │ ② contextInfo = session.Summary  (RAG 摘要，供 plan 参考)
   ▼
2. Planner.CreatePlan(ctx, task, contextInfo)
   │
   │ ① llm.Call(plannerSystemPrompt, "Task: ...")
   │ ② parsePlanJSON  (剥 ```json 围栏 + Unmarshal + 默认 Status=pending)
   │ ③ Plan.ID = "plan_<ns>", Version=1, CreatedAt=now
   │ ④ ValidateDAG  (重复 ID / 自依赖 / 未知依赖 / 环)  ← 任一失败立即 return
   │ ⑤ PlanEvaluator.Evaluate (4 维 → Overall)
   │ ⑥ if quality.ShouldImprove (<0.7): improvePlan
   │     │ ⑥a llm.Call(improvementSystemPrompt, plan+report)
   │     │ ⑥b 再 ValidateDAG + 再 Evaluate；若 Overall 没提升 → 弃用
   │ ⑦ if quality.Feasibility == 0.0: return error "plan is infeasible"
   ▼
3. metrics.PlannerPlansCreated.Inc()
   o.persistPlan(taskID, plan)  ← store.SavePlan(JSON)
   ▼
4. 选择执行后端：
   │ if o.supervisor != nil && planHasParallelism(plan):
   │   → executePlanWithSupervisor  (见 22_multiagent)
   │ else:
   │   → planner.Executor.Execute
   ▼
5. Executor.Execute (≤ maxRevision+1 轮)
   │
   │ for revision := 0..2:
   │   levels := TopologicalSort(plan.Steps)
   │   failures := []
   │   for each level:
   │     failures += executeLevel(ctx, plan, level, outputs)
   │   if len(failures) == 0: success break
   │   if revision < maxRevision:
   │     plan = planner.RevisePlan(ctx, plan, failureSummary)
   ▼
6. o.persistPlan(taskID, execResult.Plan)  ← 最终态写回
   metrics.PlannerStepsTotal.WithLabel(action, status).Inc()  per step
   if !success: metrics.PlannerRevisionTotal.Inc()
   ▼
7. return models.ChatResponse{
       State:   completed/failed,
       Message: formatPlanResult(execResult),  ← markdown 进度表
   }
```

### 2.5.2 同层并发：`executeLevel`

```
executeLevel(level, outputs):
  toRun := []
  for each step in level:
    skip if step.Status ∈ {completed, skipped}     ← 上一轮 revision 已完成
    if any dep.Status != completed:
      updateStepStatus(step, skipped, "dep not met")
      continue
    toRun.append(step)

  if maxParallelism > 0:
    sem := make(chan struct{}, maxParallelism)

  wg := sync.WaitGroup{}
  mu := sync.Mutex{}                              ← 保护 outputs + failures
  failures := []

  for each step in toRun:
    wg.Add(1)
    go func(s):
      defer wg.Done()
      sem <- struct{}{} (if bounded)
      defer (<-sem)

      updateStepStatus(s, running)
      start := time.Now()
      output, err := stepExec(ctx, s)              ★ orchestrator.executePlanStep
      dur := time.Since(start)

      mu.Lock()
      if err != nil:
        updateStepStatus(s, failed, err.Error())
        failures.append(s)
      else:
        updateStepStatus(s, completed, output)
        outputs[s.ID] = output
      step.Duration = dur
      mu.Unlock()

  wg.Wait()
  return failures
```

**⚠️ 已知问题**（与 `09_orchestrator §10` 同源）：`wg.Wait()` 不 select 在 ctx.Done 上。`ctx` 取消后 stepExec 内部如果不响应 ctx，整层会跑完才返回。属 P0。

### 2.5.3 HierarchicalPlanner 数据流（未接线，列此供完整性）

```
HierarchicalPlanner.Execute(goal, contextInfo):
  subGoals = decomposer.Decompose(goal)            ← LLM 拆 2-4 个
  tracker.Track(subGoals)
  for each sg (按 Priority 顺序):
    tracker.MarkStarted(sg.ID)
    plan = planner.CreatePlan(sg.Description, contextInfo)
    execResult = executor.Execute(ctx, plan)
    if execResult.Success:
      tracker.MarkCompleted(sg.ID)
    else:
      needsReplan := tracker.MarkFailed(sg.ID)     ← consecutive >= 3 时返回 true
      if needsReplan:
        replanSubGoal(sg, contextInfo):            ← 用 "(Retry) {desc} — try different approach" 重新 CreatePlan + Execute
  result.Progress = tracker.Progress()             ← completed/total
  result.Success = (progress >= 1.0)
```

---

## 3. 核心数据模型

### 3.1 `Plan` 与 `Step`（planner.go:20-53）

```go
type Plan struct {
    ID        string     `json:"id"`           // "plan_<unix-ns>" 自动生成
    Goal      string     `json:"goal"`         // 由 LLM 在 JSON 里填
    Steps     []Step     `json:"steps"`
    Reasoning string     `json:"reasoning"`    // LLM 的思考过程，供人读
    CreatedAt time.Time  `json:"created_at"`
    RevisedAt *time.Time `json:"revised_at,omitempty"`
    Version   int        `json:"version"`      // 每 Revise/improve +1
}

type Step struct {
    ID                string          `json:"id"`             // LLM 命名："step_1"/"install-deps"
    Action            string          `json:"action"`         // == tool 名（read_file/edit_file/...）
    Description       string          `json:"description"`    // 人可读
    Parameters        json.RawMessage `json:"parameters,omitempty"` // 工具参数（raw JSON）
    DependsOn         []string        `json:"depends_on,omitempty"`
    Status            StepStatus      // pending/running/completed/failed/skipped
    Output            string          // 完成后塞工具返回内容
    Error             string          // 失败时塞 err.Error()
    Duration          time.Duration   // 执行耗时（Executor 写入）
    ReasoningRequired bool            // 占位字段，目前未使用
}
```

#### 关键约定

- **`Step.ID` 由 LLM 命名而非 UUID**——LLM 在 `DependsOn` 字段里引用，用自然名（`install-deps`、`run-tests`）比 UUID 稳定得多。
- **`Step.Action == tool 名**——这是 planner 与 tools.Registry 的胶水约定。`executePlanStep` (planner_bridge.go:236) 直接把 `step.Action` 作为 `ToolCall.Name` 传给 orchestrator。Planner 系统提示 (planner.go:84) 把允许的 action 列表硬编码进去（17 个 builtin/skill 工具）。
- **`Step.Parameters` 是 `json.RawMessage`**——不在 planner 里反序列化，直接透传给 `executeTool`，由各 builtin/skill 的 schema 校验。空 parameters 在 bridge 里被改写为 `{}` 避免 nil 崩。
- **`StepStatus.Skipped`**：仅在"依赖失败"时触发；不参与 `Plan.IsComplete()` 判断（completed/skipped 都算"通过"）。

### 3.2 `Planner`（planner.go:63-82）

```go
type Planner struct {
    llm       LLMCaller     // 注入；不依赖 *llm.Client 具体类型
    evaluator *PlanEvaluator // 自动构造（17 个默认合法 action）
    logger    *zap.Logger
}
```

`NewPlanner` 内部硬编码 `defaultActions`（planner.go:71-76）—— 与 `plannerSystemPrompt` 的 actions 列表是**两份**字符串。**改了一份要同步改另一份**，否则 LLM 生成合法 action，Evaluator 却判 unknown。属低优 P2，但坑过人。

### 3.3 `Executor`（executor.go:65-71）

```go
type Executor struct {
    planner        *Planner       // 失败时调 RevisePlan
    stepExec       StepExecutor   // 必填，由外部注入
    maxRevision    int            // 默认 2
    maxParallelism int            // 默认 4；orchestrator AttachPlanner 改成 2
    logger         *zap.Logger
}
```

`orchestrator.AttachPlanner` (planner_bridge.go:90) 主动把 `MaxParallelism` 从默认 4 调到 **2** —— 因为每个 step 可能开 sandbox 容器或 LLM 子调用，2 个并发已经能给宿主机不小压力。

### 3.4 `PlanEvaluator` & `PlanQuality`（evaluator.go:9-30）

```go
type PlanQuality struct {
    Completeness float64  // 关键词覆盖率 + 是否有 verification 步
    Feasibility  float64  // Action 合法性 + DAG 是否仍 valid
    Efficiency   float64  // 冗余步数 + 是否 >20 步
    Robustness   float64  // 有无 fallback/retry 关键词 + 依赖链层数
    Overall      float64  // 0.35 C + 0.35 F + 0.15 E + 0.15 R
    Weaknesses   []string // 具体问题列表
}
```

权重把 Completeness + Feasibility 设为 70%，效率与鲁棒性合计 30%——意图是"先保证能跑通，再说漂不漂亮"。

### 3.5 `SubGoal` & `HierarchicalResult`（goal_decomposer.go:13-20, hierarchical.go:33-39）

```go
type SubGoal struct {
    ID, ParentID, Description string
    Priority   int      // LLM 给的执行优先级
    Plan       *Plan    // CreatePlan 后填回
    Status     StepStatus
}

type HierarchicalResult struct {
    Goal     string
    SubGoals []SubGoal
    Results  []*ExecutionResult
    Success  bool
    Progress float64    // tracker.Progress() == completed/total
}
```

---

## 4. CreatePlan：从任务到 Plan

### 4.1 主流程（planner.go:101-160）

```
CreatePlan(ctx, task, contextInfo):
  ① 拼 userPrompt = "Task: <task>\n\nContext:\n<contextInfo>"
  ② raw = llm.Call(plannerSystemPrompt, userPrompt)
  ③ plan = parsePlanJSON(raw)
  ④ plan.ID = "plan_<UnixNano>"
     plan.CreatedAt = now
     plan.Version = 1
  ⑤ ValidateDAG(plan.Steps)
     ├─ duplicate ID → error
     ├─ unknown DependsOn → error
     ├─ self-dependency → error
     └─ cycle (via TopologicalSort) → error
  ⑥ if evaluator != nil:
       quality = evaluator.Evaluate(plan, task)
       if quality.ShouldImprove (Overall < 0.7):
         improved = improvePlan(plan, task, quality)
         if improved != nil: plan = improved
         elif quality.Feasibility == 0.0: return error "infeasible"
       if quality.Feasibility == 0.0: return error "infeasible"
  ⑦ return plan
```

### 4.2 `plannerSystemPrompt`（planner.go:84-99）

要点：

1. **严格 JSON schema**：让 LLM 直出 `{"goal": ..., "reasoning": ..., "steps": [...]}`；
2. **Step 粒度约束**：1 step = 1 次工具调用；
3. **依赖显式声明**：`"depends_on": ["A","B"]`；
4. **保守原则**：minimal but complete，宁少勿多；
5. **Action 白名单**：17 个工具名硬编码在 prompt 里（read_file/write_file/edit_file/execute_code/search_code/run_tests/think/patch_file/apply_diff/list_files/create_directory/run_workspace_cmd/shell_exec/goto_definition/find_references/hover_info/rename_symbol）。

### 4.3 `parsePlanJSON`（planner.go:410-436）

容忍 LLM 三种常见输出：

- 裸 JSON：直接 `json.Unmarshal`；
- ``` ```json ... ``` ```：剥前缀 + 后缀；
- ``` ``` ... ``` ```：剥通用代码围栏。

之后给所有 step 默认 `Status = pending`（防 LLM 漏字段）。

> ⚠️ 旧版文档曾声称 "对 `depends_on: null` 自动归一为 `[]`"——**实际代码没有此处理**。如果 LLM 输出 `"depends_on": null`，`json.Unmarshal` 会留 nil 切片，下游 `range` 没事，但要小心未来加 `len(nil) == 0` 之外的检查时炸出来。

### 4.4 `improvePlan` 二次润色（planner.go:170-210）

```
improvePlan(plan, goal, quality):
  prompt = original goal + plan(JSON) + quality.FormatReport
  raw = llm.Call(improvementSystemPrompt, prompt)
  improved = parsePlanJSON(raw)
  improved.ID/CreatedAt 继承自原 plan
  improved.Version = plan.Version + 1
  improved.RevisedAt = now
  ValidateDAG(improved.Steps)
  newQuality = Evaluate(improved, goal)
  if newQuality.Overall <= quality.Overall:
    return nil   ← 没提升就当没改
  return improved
```

**为什么改完还要重 Evaluate**？因为 LLM 偶尔会自信地"改"出一个更差的 plan。代码不信 LLM，用同一份评分器 gate 一道。这是把 "信号-修复" 闭环留在 plan 自己包内的关键设计。

---

## 5. ValidateDAG（planner.go:256-277）

四种非法情形一次性挡掉：

| 检查 | 实现 | 报错信息 |
|---|---|---|
| 重复 step ID | `ids := map[string]bool{}` 扫一遍 | `"duplicate step ID: <id>"` |
| Self-dependency | `dep == s.ID` | `"step %q depends on itself"` |
| 未知依赖 | `dep ∉ ids` | `"step %q depends on unknown step %q"` |
| 环 | 调用 `TopologicalSort`，processed != len(steps) | `"cycle detected in step dependencies"` |

**Validate 与 TopoSort 互相依赖**——其实 Validate 把 TopoSort 作为环检测器再跑一次，重复但便宜（O(N+E)）。换来 API 简单：上层只调 `ValidateDAG` 就知道能不能跑。

---

## 6. TopologicalSort：按层 Kahn 算法（planner.go:281-339）

```
inDegree[id] = len(step.DependsOn)
dependents[dep] = list of steps that depend on dep

queue := { id : inDegree[id] == 0 }
sort.Strings(queue)     ← 关键：deterministic ordering

levels := [][]Step{}
processed := 0
while queue not empty:
  level := []Step{ stepMap[id] for id in queue }
  nextQueue := []
  for each id in queue:
    processed++
    for each dependent in dependents[id]:
      inDegree[dependent]--
      if inDegree[dependent] == 0:
        nextQueue.append(dependent)
  levels.append(level)
  sort.Strings(nextQueue)   ← 同样保证顺序确定
  queue = nextQueue

if processed != len(steps):
  return ErrCycle
return levels
```

**两次 `sort.Strings` 是重点**：planner 不要求 step 顺序有意义，但同 plan 多次执行必须得到完全一致的层划分（便于 debugging + checkpoint diff + 单测稳定）。

输出形如：

```
levels = [
  ["fetch-code", "read-config"],   // 第 0 层：无依赖，可并发
  ["parse-ast"],                   // 依赖 fetch-code
  ["check-lint", "run-tests"],     // 都依赖 parse-ast，可并发
  ["summary"],                     // 依赖 lint + tests
]
```

---

## 7. Executor.Execute：分层并发执行（executor.go:102-156）

### 7.1 主循环

```
Execute(ctx, plan):
  for revision := 0..maxRevision (=2):
    levels := TopologicalSort(plan.Steps)
    failures := []
    for each level in levels:
      failures += executeLevel(ctx, plan, level, result.StepOutputs)
    if len(failures) == 0:
      result.Success = true
      result.Summary = "Plan completed successfully: <N> steps executed"
      return result
    result.FailedStepIDs += [step.ID for step in failures]
    if revision < maxRevision:
      plan = planner.RevisePlan(ctx, plan, buildFailureSummary(failures))
      result.Plan = plan
  result.Success = false
  result.Summary = "Plan failed after <maxRevision> revision(s): <N> steps failed"
  return result
```

### 7.2 `ExecutionResult`（executor.go:93-99）

```go
type ExecutionResult struct {
    Plan          *Plan
    Success       bool
    StepOutputs   map[string]string  // step.ID → output
    FailedStepIDs []string
    Summary       string             // 给用户看的摘要
}
```

> ⚠️ 旧版文档曾列出 `DurationMs/CompletedCount/RevisionCount` 字段——**代码里没有这些**。前端要进度统计只能 from `len(plan.CompletedStepIDs())` / `len(plan.FailedSteps())` 自己算。

### 7.3 `executeLevel` 细节（executor.go:159-238）

四件事：

1. **跳过已完成**：上一轮 revision 跑过的 step `Status == completed/skipped` 直接 continue（保护成本）。
2. **依赖未达 → skipped**：上层 dep 不是 completed 就 mark skipped（**注意：不是 pending → 也不是 failed**，是独立的 skipped 状态）。
3. **信号量限流**：`maxParallelism > 0` 才创建 sem channel，`<= 0` 视为"unlimited"（测试场景用）。
4. **每个 step goroutine**：
   - 改状态 → 跑 stepExec → 拿结果 → 锁 mu 写 outputs / failures / step.Duration
   - **没有 panic recover**！stepExec 内部 panic 会让整个 process 退出。属 P1。

### 7.4 `buildFailureSummary`（executor.go:266-272）

```
for f in failures:
  sb.WriteString("- Step %q (%s): %s\n", f.ID, f.Action, f.Error)
```

简单字符串拼接，喂给 `RevisePlan` 的 user prompt。

---

## 8. RevisePlan：失败后的二次规划（planner.go:212-251）

```
RevisePlan(plan, failureSummary):
  prompt = plan(JSON) + failureSummary
  raw = llm.Call(revisionSystemPrompt, prompt)
  revised = parsePlanJSON(raw)
  revised.ID = plan.ID
  revised.CreatedAt = plan.CreatedAt
  revised.RevisedAt = now
  revised.Version = plan.Version + 1
  ValidateDAG(revised.Steps)
  return revised
```

`revisionSystemPrompt` (planner.go:212) 比 `plannerSystemPrompt` 多两条约束：

1. **"Keep completed steps as-is"**：明示 LLM 不要重跑已完成 step；
2. **"Fix or replace failed steps. Add new steps if the failure reveals missing work."**：允许扩计划，但前提是错误信号支持。

> ⚠️ **隐患**：RevisePlan 没有强制 LLM 把已 completed step 复制进 revised plan。如果 LLM 漏了某个 completed step，下次 ValidateDAG 时 `step_3 depends on step_1` 会因为 step_1 不在新 plan 而失败。当前测试覆盖了"严格遵守"情形，对"LLM 偷懒"情形未做兜底。属 P1。

---

## 9. PlanEvaluator：4 维质量评分（evaluator.go）

| 维度 | 函数 | 信号 | 惩罚力度 |
|---|---|---|---|
| **Completeness** | `checkCompleteness` (L59) | 关键词覆盖率 < 50% → Completeness = coverage；无 verification step 且总步数 > 2 → ×0.9 | 高 |
| **Feasibility** | `checkFeasibility` (L98) | Unknown action 超过半数 → 直接 0.0 (plan rejected)；少量 → 按比例扣；DAG 失败 → 0.0 | 致命 |
| **Efficiency** | `checkEfficiency` (L121) | 同 (action + 描述前缀) 重复 → 按比例扣；> 20 step → ×0.8 | 低 |
| **Robustness** | `checkRobustness` (L148) | 总步数 > 5 且无 "fallback/retry/if fail" → 0.7；依赖链 > 5 层 → ×0.9 | 中 |
| **Overall** | `q.Overall = 0.35C + 0.35F + 0.15E + 0.15R` | 加权 | — |

`ShouldImprove`（L211）阈值 0.7 是经验值；`improvePlan` 通过后还要确认 `newQuality.Overall > quality.Overall` 才接受。

### 9.1 `extractKeywords` 的弱点

只过滤 26 个英文 stopword（the/a/and/...），**中文完全不过滤**。"把所有 Go 文件" 会把"把"、"所有"、"文件" 都当关键词，覆盖率失真。中文场景 Completeness 维度可信度低。属 P2。

---

## 10. 与 Orchestrator 的桥接（`planner_bridge.go`）

### 10.1 LLM Adapter（L44-66）

```go
type llmCallerAdapter struct { client *llm.Client }

func (a *llmCallerAdapter) Call(ctx, systemPrompt, userPrompt) (string, error) {
    resp, err := a.client.ChatCompletion(ctx, &llm.ChatRequest{
        Messages: []models.Message{
            {Role: RoleSystem, Content: systemPrompt},
            {Role: RoleUser,   Content: userPrompt},
        },
        Temperature: 0.2,    // 低温保证 JSON 稳定
    })
    return resp.Content, nil
}
```

**为什么温度固定 0.2？** Planner JSON 模式禁不起 temperature 1.0 的发散；0.2 既能允许 LLM 在多个合法 plan 中挑一个，又不至于编造未知 action。

### 10.2 `AttachPlanner`（L83-92）

```go
func (o *Orchestrator) AttachPlanner(p *planner.Planner) {
    if p == nil { return }
    exec := planner.NewExecutor(p, o.executePlanStep, o.logger)
    exec.SetMaxParallelism(2)             // ★ 默认 4 → 调到 2
    o.planner = &plannerComponents{planner: p, executor: exec}
}
```

**为什么延后注入？** Planner 依赖 `LLMCaller`（= orchestrator 自己持有的 `llm.Client`），而 orchestrator 构造期 planner 未必已实例化。延后注入避免循环构造。

### 10.3 `MaybeUsePlanner`（L108-178）

返回三元组 `(*ChatResponse, bool, error)` 区分三种情况：

- `(nil, false, nil)` → fall through，orchestrator 继续 ReAct
- `(*ChatResponse, true, nil)` → planner 成功跑完
- `(nil, false, err)` → planner 跑了但**严重失败**（执行器层错误，不是 plan 内单 step 失败）

**重要：CreatePlan 失败不是严重错误**（L132-140）。它会 fall through 到 ReAct，让 ReAct 兜底。这是 graceful degradation 设计。

### 10.4 `executePlanStep`（L236-260）

```go
func (o *Orchestrator) executePlanStep(ctx, step planner.Step) (string, error) {
    args := step.Parameters
    if len(args) == 0 {
        args = json.RawMessage(`{}`)   // 防 nil 崩
    }
    tc := models.ToolCall{
        ID:   step.ID,
        Name: step.Action,
        Args: args,
    }
    result, err := o.executeTool(ctx, tc)    // ★ 复用 09_orchestrator §5 三级分派
    if err != nil:
        return "", err
    if result != nil && result.IsError:
        return result.Content, errors.New("tool reported error: " + truncate(result.Content, 200))
    if result == nil:
        return "", nil
    return result.Content, nil
}
```

**关键**：Planner **完全复用** ReAct 循环里的 `executeTool` 分派器，意味着所有 builtin/skill/MCP 工具对 Planner 自动可用。新增工具到 `tools.Registry` 后无需改 planner 即可生效。

### 10.5 `formatPlanResult` 用户态渲染（L191-208）

```markdown
## ✅ Plan completed

Plan completed successfully: 5 steps executed

### Steps
- ✅ **fetch-code** (`read_file`): Read auth.go to understand current structure
- ✅ **parse-ast** (`search_code`): Find all log.Print usages
- ⏭ **early-return** (`think`): (skipped because dependency failed)
- ❌ **migrate-call** (`edit_file`): Replace log.Print → slog.Info
    - error: file is read-only
```

前端直接 markdown 渲染成带 emoji 的进度表。

### 10.6 `persistPlan`（L264-276）

Plan **创建后写一次**、**执行完写一次**（更新最终态）。写库失败仅 log warning 不阻塞主流程——`store.SavePlan` 不可用时 Planner 仍能跑，只是 crash 后无法恢复。

---

## 11. P0 / P1 / P2 风险清单

| 级别 | 项 | 位置 | 说明 |
|---|---|---|---|
| **P0** | `wg.Wait()` 不响应 ctx 取消 | executor.go:236 | 同 09_orchestrator P0，ctx 取消后已 in-flight 的 stepExec 不被打断 |
| **P0** | step goroutine 无 panic recover | executor.go:199-233 | stepExec 内 panic 会让 process 退出 |
| **P0** | `Step.Action` 双源真相 | planner.go:71-76 + planner.go:89 | `defaultActions` 与 system prompt 的 actions 列表是两份字符串，改了一份不改另一份会导致 LLM 生成合法 action 但 Evaluator 判 unknown |
| **P1** | `HierarchicalPlanner` 未接线 | planner_bridge.go:108 | `MaybeUsePlanner` 没有 `if NeedsHierarchical → executeHierarchical` 分支；测试覆盖但生产路径用不到 |
| **P1** | `RevisePlan` 不强校验 completed step 保留 | planner.go:221-251 | LLM 漏写已 completed step 会导致 DAG 校验失败；缺兜底 merge 逻辑 |
| **P1** | `improvePlan` 不限重试 | planner.go:170-210 | 理论上 improvePlan 内不会递归，但若未来调用方循环调用没有上限 |
| **P1** | `planHasParallelism` / `executePlanWithSupervisor` 未在本文档外有公开协议 | planner_bridge.go:148 | 多智能体分支静默接管，前端无法预判 |
| **P2** | `extractKeywords` 不处理中文 | evaluator.go:174 | 中文场景 Completeness 维度评分失真 |
| **P2** | plan 写库失败仅 log warning | planner_bridge.go:273 | crash 后用户无法看到执行历史；建议改为 metrics 计数 + retry |
| **P2** | `improvementSystemPrompt` 和 `revisionSystemPrompt` 内容相近但分开维护 | planner.go:162, 212 | 易漂移；考虑模板化 |

---

## 12. 设计权衡与设计哲学

| 抉择 | 动机 |
|---|---|
| **层级拓扑排序**（`[][]Step`）而非线性拓扑 | 同层 step 并发成为自然结构，不需要执行期再判"这俩能并行吗" |
| **Step.ID 由 LLM 自命名**（不是 UUID） | LLM 在 DependsOn 里引用，自然名稳定；UUID 会污染 prompt 上下文 |
| **maxRevision=2** | 三次失败多半是任务不可行；保护 token 成本 |
| **同层并发用信号量限流**（不用 errgroup 默认） | sandbox 容器有资源上限；并发度可调更可控 |
| **依赖失败 → Skip 而非 Fail** | 区分"主动失败"与"被牵连"，便于人工审阅定位根因 |
| **Executor 只持 `StepExecutor` 闭包** | planner 包不依赖 sandbox/mcp/skill，零循环 import |
| **CreatePlan 后立即 Evaluate + improve** | LLM 自评易自满；用代码做一致性 gate 更刚性更便宜 |
| **PlanEvaluator 用加权和**（C/F/E/R = 0.35/0.35/0.15/0.15） | 先保证能跑通再说漂亮；feasibility 等同 completeness 权重 |
| **不在 planner 内做 dry-run / lint** | 只校验 DAG 结构；业务逻辑交给 StepExecutor，边界清晰 |
| **`NeedsPlanning` 是关键词启发式** | 预筛本身就是省钱，再 LLM 调用自相矛盾 |
| **Hierarchical 用 `ProgressTracker` 计连续失败** | 区分"偶发失败重试"和"系统性失败放弃" |
| **Plan 用 `o.store` 持久化而非 Redis** | Plan 是审计资料，需长期保存；Redis 用于热路径 session |
| **LLMCaller 接口比 *llm.Client 窄** | 测试 mock 极简；planner 包不知道 streaming/tool calling 存在 |
| **planner 包不导出 main 工具列表** | system prompt 内的 17 个 action 名是硬编码"约定"；与 tools.Registry 解耦——同步靠人 |

---

## 13. micro_plan.go：与本章独立的"小计划"机制（一定要分清）

`orchestrator/micro_plan.go`（39 行）**不是** DAG Planner，但容易混淆，特此说明：

```go
// micro_plan.go:9
const (
    microPlanTriggerStep = 3   // 从第 3 步开始
    microPlanInterval    = 6   // 每 6 步一次
)
```

每隔 6 步在 ReAct 循环里插一条 system 消息，让 LLM 自述：

1. 截止目前完成了啥（1 句话）
2. 接下来 2-3 个动作 + 理由
3. 怎么判断收工

**与 DAG Planner 区别**：

| 对比项 | DAG Planner（本章） | MicroPlan |
|---|---|---|
| 形式 | LLM 一次出完整 DAG | LLM 在步骤间自述短期规划 |
| 数据结构 | `*Plan` + `[]Step` | 仅一条临时 system message |
| 触发 | `NeedsPlanning(task) >= 5` | 每 6 步固定插入 |
| 执行 | Executor 调度 | 不执行，只是给 LLM 提示 |
| 用途 | 替代 ReAct | **增强** ReAct（防 LLM 偏题） |

MicroPlan 与 DAG Planner 可以**同时启用**——ReAct 走流程，MicroPlan 周期性提醒目标；DAG Planner 是另一条腿，二选一。

---

## 14. 后续演进

- [ ] **接线 HierarchicalPlanner**：`MaybeUsePlanner` 加入 `if NeedsHierarchical → executeHierarchical` 分支，让 score >= 12 的任务走子目标分解
- [ ] **Plan 预览 + 人工确认**：生成 plan 后 SSE 推到前端，用户 approve/edit 后再执行；现状是创建即执行
- [ ] **Plan checkpoint & resume**：crash 后从 store 恢复未完成 step；目前重启即丢
- [ ] **Step Retry 单步重试**：当前单 step 失败直接 Failed；应支持 `MaxRetries` + 指数退避
- [ ] **Plan 模板库**：常见任务（"新建 FastAPI 项目"）缓存模板，跳过 LLM
- [ ] **Output 大对象引用**：Step 间可能传大 JSON/代码，目前全塞在 `outputs map[string]string` 里；加 ObjectStore 引用替代
- [ ] **观测**：补充 Prometheus metrics —— `planner_plan_evaluator_score{dimension=}`、`planner_improve_attempts_total`、`planner_step_duration_seconds`
- [ ] **`Step.Parameters` 实参注入**：现在 step.Parameters 是创建时的静态值；增加 `${step_1.output}` 之类引用让后续 step 用上前置 step 输出
- [ ] **条件分支**：DAG 不支持 `if A.output contains "fail" then run B`；考虑加 conditional step 类型
- [ ] **Sub-plan 递归**：复杂 step 可以 spawn 子 plan；让 plan 有分形结构
- [ ] **Action 单一来源**：把 `defaultActions` 列表和 system prompt 的 actions 合并到一个常量 + go:generate 注入 prompt，杜绝双源真相
- [ ] **panic recover**：`executeLevel` 每个 goroutine 加 `defer recover` + log + mark step failed
- [ ] **ctx 响应**：`wg.Wait` 改为 select on `ctx.Done()` 路径，把取消从 orchestrator 传到 step 层

---

## 15. 设计教训

1. **"少调一次 LLM" 不一定省成本**——CreatePlan 一次 + Evaluate 一次 + improvePlan 一次的代价已经接近 3 次 ReAct；Planner 的真正收益是**并发执行 + 可视化**，不是节省 LLM 调用。

2. **Plan 是契约不是过程**——一旦 Plan 落到 store，前端看到的就是这份。如果中途偷偷修改 Plan 让用户惊讶，会比"plan 失败"更让人困惑。所有 mutation 走 `Version++` + `RevisedAt` 留痕。

3. **`StepExecutor` 闭包是关键解耦**——planner 包 0 个外部业务依赖。如果哪天换执行后端（云函数？K8s Job？），只需注入新 stepExec，planner 包本身不动。

4. **DAG 结构性校验 vs 业务可执行性是两件事**——`ValidateDAG` 只能保证拓扑正确；某个 step 的 action 是否真存在于 tools.Registry、parameters 是否符合该 tool 的 schema —— planner 一无所知。这件事推到 `executeTool` 在执行期才报错。**好处**是 planner 不需要扫工具表；**坏处**是 LLM 编造 action 名时只在执行时才发现。`PlanEvaluator.checkFeasibility` 弥补了一半（白名单 17 个 action），但 17 个之外的 dynamic skill 名仍走不过这道检查。

5. **失败修订要给 LLM 看"已经做了啥"**——`revisionSystemPrompt` 反复强调 "keep completed steps as-is"。否则 LLM 会重新规划整个任务（包括重跑 install-deps、重新克隆仓库），把时间和钱白白花掉。

6. **`MicroPlan` 与 `Planner` 是两套机制不要合并**——他们解决不同问题（防偏题 vs 批量执行），强行合并会让 ReAct 路径凭空多 6 步一次的 LLM 调用。保持独立、可选、互不干扰是更好的设计。

7. **质量评分用启发式不用 LLM**——LLM 自评 plan 几乎总说"很好"。代码评分（关键词覆盖、动作合法性、冗余检测）更刚性，且**完全免费**。在能用启发式的地方拒绝再叫一次 LLM 是工程纪律。

---

下一篇：[`11_temporal.md`](11_temporal.md) —— Temporal 工作流：把 Orchestrator 的长任务迁到 Workflow Runtime，获得持久化状态机、崩溃自愈、可重放执行。注意 `main.go` 的 `initTemporalWorker` **目前是占位**，工作流代码已就位但 Worker 未启动。
