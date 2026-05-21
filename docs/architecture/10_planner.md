# 10 · Planner `internal/planner`

> 代码：
> - `planner.go` (347) — `Plan / Step / Planner`：从自然语言任务 → JSON 计划 + DAG 校验 + 拓扑排序 + 失败修订
> - `executor.go` (323) — `Executor`：按 DAG 层级执行 Step，同层并行、层间串行，失败后触发 `RevisePlan`
> - 测试：`planner_test.go` (533) — 完整覆盖 DAG 校验 / 拓扑排序 / 重试修订 / 并发上限
>
> 外部消费者：
> - `internal/orchestrator/planner_bridge.go` — Orchestrator 在"长任务 + 高复杂度"意图下切换到 Planner 路径

---

## 1. 模块定位

**"ReAct 的替身：把 LLM 调工具的决策权从'每步问一次'改为'开头问一次'。"**

场景对比：

| 场景 | ReAct 合适吗 | Planner 合适吗 |
|---|---|---|
| 用户问 "这段代码为啥报错？" | ✅ 边试边改 | ❌ 没有静态计划可下 |
| 用户说 "把项目里所有 `log.Print` 换成 `slog.Info`" | ❌ 50 次 LLM 浪费 | ✅ 一次生成 N 条 edit 计划 |
| "跑一下测试再告诉我结果" | ✅ 一步完事 | ❌ 杀鸡用牛刀 |
| "新建一个 FastAPI 项目 + 3 个路由 + pytest 配置" | ⚠️ 容易跑偏 | ✅ 结构化 plan 可 review |

Planner 的价值：

- **少 LLM 调用**：一次出 10 步 > 10 次 ReAct，成本/延迟都下降；
- **可视化 + 可审核**：前端可以先让用户看 plan.md，确认后再跑；
- **并行加速**：DAG 同层的 step（无依赖）能并发执行；
- **失败修订**：部分 step 失败不会整个放弃，`RevisePlan` 让 LLM 基于剩余状态重做未完成部分。

---

## 1.5 核心设计问题

### Planner vs ReAct：什么场景切到 Planner？

ReAct 每步问 LLM "下一步"，但有些场景**步骤是可预先列出来的**：
- "给所有 .go 文件加版权头" — 清晰的 N 个文件 × 相同操作
- "把 go-redis v8 升级到 v9" — 可先分析出依赖链，再批量 migrate

这类任务用 ReAct 会：
1. 浪费 LLM 调用（每步都问）
2. 容易半路跑偏（第 15 步忘了目标）
3. 无法并发（第 N 步串行等）

Planner 的价值：**先 LLM 一次产出 DAG，然后 Go 代码调度执行**。
后续 N 步不需要 LLM 参与，可并行。

### 为什么是 DAG 而不是线性列表？

DAG 的边表达"A 完成 B 才能开始"。独立分支可以**并发执行**。
例："升级依赖"：
```
      ┌── 改 go.mod ──→ go mod tidy ──→ go test ──┐
      │                                            │
start ├── 更新 import ────────────────────────────→─ done
      │                                            │
      └── 改文档 ────────────────────────────────→─┘
```
三个分支并行，收敛到 done。线性列表做不到。

### checkpoint / resume 的必要

Planner 任务可能跑 10+ 分钟。服务器重启或被 K8s rolling update 过一次
就前功尽弃不可接受。Executor 每完成一个 step 就把状态写 Redis/Postgres，
重启后从 checkpoint 续跑。

### 为什么不用 Temporal 做 Planner？

Temporal 是"极重"的选择：Temporal Server（Cassandra/PG 后端）+ Worker
进程 + 额外网络跳。对 `planner` 这种"1-20 分钟任务"过度工程。

Temporal 的价值在 **HITL 挂起几小时不消耗资源**——planner 场景没这个
需求。所以 Planner 是**进程内 DAG 执行 + 外部 checkpoint 持久化**的
折中。

---

## 2. 依赖架构

```
┌─ Orchestrator.ProcessMessage ─┐
│                                │
│  MaybeUsePlanner() ─────────┐  │
└─────────────────────────────┼──┘
                              ▼
                 ┌────────────────────────┐
                 │     Planner            │
                 │  CreatePlan(task)      │  ← LLM 出 JSON
                 │  ValidateDAG(steps)    │
                 │  TopologicalSort(...)  │
                 │  RevisePlan(plan, fail)│  ← 失败后重做
                 └────────────┬───────────┘
                              │
                              ▼
                 ┌────────────────────────┐
                 │     Executor           │
                 │  Execute(ctx, plan)    │  ★ 按层执行
                 │  executeLevel(...)     │  ← WaitGroup 并发
                 └────────────┬───────────┘
                              │ per-step
                              ▼
                        StepExecutor
                  (orchestrator.executePlanStep)
                  委托回 executeTool → sandbox/edit/search
```

外部 `StepExecutor` 注入函数签名：

```go
type StepExecutor func(ctx context.Context, step Step) (output string, err error)
```

Planner 自己**不执行**任何工具；执行权由 orchestrator 注入的 closure 持有 —— 典型的「控制反转」。

---

## 2.5 数据流总览

```text
┌─────────────────────────────────────────────────────────────┐
│ orchestrator.MaybeUsePlanner(task)                           │
│   NeedsPlanning() + EstimateComplexity() → 决定是否走 Plan  │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Planner.CreatePlan(ctx, task)                                │
│   构造 plannerSystemPrompt + 用户消息                        │
│   → 【LLM API】 生成 JSON Plan                              │
│   → parsePlanJSON (容错: 提取 ```json 块)                    │
└──────────────────────────┬──────────────────────────────────┘
                           │ (*Plan: steps + dependencies)
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ ValidateDAG(plan)                                            │
│   ① 检查重复 step ID                                        │
│   ② 检查未知依赖引用                                         │
│   ③ DFS 环检测                                              │
│   失败 → 返回错误，不执行                                    │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ TopologicalSort(plan) → [][]Step (按层分组)                   │
│   Layer 0: 无依赖的 steps (可并发)                           │
│   Layer 1: 依赖 Layer 0 的 steps                            │
│   Layer 2: ...                                              │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ Executor.Execute(ctx, plan)                                  │
│   for each level:                                           │
│     executeLevel(steps, semaphore=N)                         │
│                                                              │
│   ┌─────────────────────────────────────────────────────┐   │
│   │ per step (并发, WaitGroup):                          │   │
│   │   检查 deps 全 completed → skip if any failed       │   │
│   │   注入上游 step outputs 到 args                     │   │
│   │   StepExecutor(ctx, step) ──▶ orchestrator.         │   │
│   │     executeTool (sandbox/edit/search)                │   │
│   │   step.Status = completed / failed                  │   │
│   └─────────────────────────────────────────────────────┘   │
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
     ┌──────────────┐        ┌─────────────────────────┐
     │ 全部成功      │        │ 有 step 失败            │
     │ → format     │        │ → RevisePlan (max 2次)  │
     │   Result 返回│        │   LLM 重新规划保留已完成 │
     └──────────────┘        │   → 重新 Validate+Sort  │
                             │   → 重新 Execute        │
                             └─────────────────────────┘
```

---

## 3. 核心数据模型

### 3.1 `Plan` & `Step`

```go
// planner.go:20-42
type Plan struct {
    ID        string    `json:"id"`
    UserTask  string    `json:"user_task"`
    Steps     []Step    `json:"steps"`
    CreatedAt time.Time `json:"created_at"`
    Revision  int       `json:"revision"`       // 每 Revise 一次 +1
}

type Step struct {
    ID          string   `json:"id"`             // 用户定义，如 "edit-auth.go"
    Description string   `json:"description"`    // 给人看
    Tool        string   `json:"tool"`           // 调什么工具，例如 "edit_file"
    Args        map[string]any `json:"args"`     // 工具参数
    DependsOn   []string `json:"depends_on"`     // 前置 step IDs
    Status      StepStatus `json:"status"`
    Output      string   `json:"output,omitempty"`
    Error       string   `json:"error,omitempty"`
    StartedAt   *time.Time `json:"started_at,omitempty"`
    FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type StepStatus string
const (
    StatusPending   StepStatus = "pending"
    StatusRunning   StepStatus = "running"
    StatusCompleted StepStatus = "completed"
    StatusFailed    StepStatus = "failed"
    StatusSkipped   StepStatus = "skipped"   // 依赖失败 → skip
)
```

**Step.ID 由 LLM 起名** 而非自动 UUID，原因：LLM 需要在 `DependsOn` 里引用；用"自然名字"（`install-deps`, `run-tests`）比 UUID 对 LLM 友好得多，生成更稳定。

### 3.2 `Planner` & `LLMCaller` 接口

```go
// planner.go:57-70
type LLMCaller interface {
    ChatCompletion(ctx context.Context, req *llm.ChatRequest) (*llm.ChatResponse, error)
}

type Planner struct {
    llm    LLMCaller
    logger *zap.Logger
}

func NewPlanner(llm LLMCaller, logger *zap.Logger) *Planner
```

用**自定义 interface 而非直接依赖 `*llm.Client`**：

- 测试时 mock 极简（见 `planner_test.go`）；
- 未来换 LLM 后端（例如用一个专门的 "planning model" fine-tune 版）不用改上层代码。

---

## 4. 从任务到 Plan：`CreatePlan`

```go
// planner.go:90-121
CreatePlan(ctx, task, contextInfo) (*Plan, error):
  messages := [
    {role: system, content: plannerSystemPrompt},
    {role: user,   content: task + "\n\nContext:\n" + contextInfo},
  ]

  resp, _ := llm.ChatCompletion({Messages: messages, JSONMode: true})

  plan := parsePlanJSON(resp.Content)        # 提取 {"steps":[...]}
  plan.ID = uuid
  plan.UserTask = task
  plan.CreatedAt = now
  plan.Revision = 0

  if err := ValidateDAG(plan.Steps); err: return nil, err
  return plan
```

### 4.1 `plannerSystemPrompt` (L72) 的精髓

给 LLM 四件事：

1. **严格 JSON schema**：让 LLM 直接出 `{"steps": [...]}`；
2. **Step 粒度约束**：一个 step = 一次工具调用，不要"做三件事"合并成一步；
3. **依赖显式声明**：同时依赖 A,B 的 step 要写 `"depends_on": ["A","B"]`；
4. **保守原则**：不确定的任务**不要硬拆**，宁可生成 5 步也别幻觉 20 步。

### 4.2 `parsePlanJSON` (L321)

容忍 LLM 两种常见输出：

- 裸 JSON：直接 `json.Unmarshal`；
- 被 ```json ... ``` 包裹：正则剥壳后再解析。

对 `depends_on: null` 自动归一为 `[]`，免得下游判空。

---

## 5. ★ DAG 校验 `ValidateDAG` (L167-190)

```
ValidateDAG(steps):
  1. build idSet := { step.ID for step in steps }     # 查重
     if 重复 ID: return ErrDuplicateStepID

  2. for each step:
       for dep := range step.DependsOn:
         if dep not in idSet: return ErrUnknownDependency
         if dep == step.ID:   return ErrSelfDependency

  3. cycleDetect via DFS:
     color[id] = white/gray/black
     for each node:
       if color == white: dfs(node)
       if touch a GRAY node: return ErrCycle
```

为什么**必须拦截环**？`Executor` 用拓扑层次并发执行，如果有环会**永远排不出层**（死锁）。宁可 `CreatePlan` 直接失败重做，也不让 Executor 卡住。

---

## 6. 拓扑排序 `TopologicalSort` (L192-253)

核心变体：**按层输出** 而非一维顺序。

```
TopologicalSort(steps) [][]Step:
  inDegree := { id: len(step.DependsOn) }
  level0   := { id for id, deg in inDegree if deg == 0 }
  levels   := [[level0]]

  while level != empty:
    nextLevel := []
    for each node in current level:
      for each dependent of node:
        inDegree[dep] -= 1
        if inDegree[dep] == 0: nextLevel.append(dep)
    levels.append(nextLevel)

  if totalVisited != len(steps): return ErrCycle  # 兜底
  return levels, nil
```

输出形如：

```
levels = [
    ["fetch-code", "read-config"],       // 第 0 层：无依赖，可并发
    ["parse-ast"],                       // 依赖 fetch-code
    ["check-lint", "run-tests"],         // 都依赖 parse-ast，可并发
    ["summary"],                         // 依赖 check-lint + run-tests
]
```

每一层**可无脑并发**，层间必须串行。

---

## 7. ★ 执行器 `Executor.Execute` (executor.go:63-118)

```
Execute(ctx, plan) (*ExecutionResult, error):
  levels, _ := TopologicalSort(plan.Steps)
  outputs   := map[stepID]string{}          # 前置步骤输出，后续可引用

  for levelIdx, level := range levels:
    failed := executeLevel(ctx, plan, level, outputs)   # §8

    if len(failed) > 0:
      # 收集失败信息 + 请 LLM 修订
      summary   := buildFailureSummary(failed)
      revPlan, err := planner.RevisePlan(ctx, plan, summary)
      if err: return partial result with error
      if revPlan.Revision > 2: break        # 防止无限修订
      plan = revPlan; restart outer loop    # 从新 levels 开始

  # 结尾汇总
  return &ExecutionResult{
    Plan: plan,
    Success: plan.IsComplete(),
    DurationMs: ...,
    CompletedCount: len(plan.CompletedStepIDs()),
    FailedCount:    len(plan.FailedSteps()),
  }
```

### 7.1 ExecutionResult (L54-62)

```go
type ExecutionResult struct {
    Plan         *Plan
    Success      bool
    DurationMs   int64
    CompletedCount int
    FailedCount    int
    RevisionCount  int
}
```

前端拿到这个就能直接渲染进度条和失败步骤表。

---

## 8. 同层并发 `executeLevel` (L120-202)

```
executeLevel(ctx, plan, level, outputs):
  sem     := make(chan struct{}, maxParallelism)   # 信号量限流
  wg      := sync.WaitGroup{}
  mu      := sync.Mutex{}                          # 保护 outputs / failed
  failed  := []Step{}

  for _, step := range level:
    # 跳过：上层依赖失败
    deps := step.DependsOn
    if any dep status == failed:
      updateStepStatus(plan, step.ID, Skipped, "", "dep failed")
      continue

    sem <- struct{}{}       # 占位 (阻塞直到有空闲槽)
    wg.Add(1)
    go func(s Step):
      defer wg.Done()
      defer func(){ <-sem }()

      updateStepStatus(plan, s.ID, Running, "", "")

      # 将前置输出注入 args (支持 LLM 引用上一步的产出)
      injectOutputs(s.Args, outputs)

      output, err := stepExecutor(ctx, s)           # ★ 外部注入的闭包
      mu.Lock()
      if err:
        updateStepStatus(plan, s.ID, Failed, "", err.Error())
        failed = append(failed, s)
      else:
        updateStepStatus(plan, s.ID, Completed, output, "")
        outputs[s.ID] = output
      mu.Unlock()
    (step)

  wg.Wait()
  return failed
```

### 8.1 `SetMaxParallelism` (L49)

```go
executor.SetMaxParallelism(4)   // 默认通常 = runtime.NumCPU()
```

为什么要限流？因为 StepExecutor 背后是 orchestrator → sandbox，每个 step 可能起一个容器。**不限流一层 20 个 step 同时起容器**，宿主机 OOM 分分钟。

### 8.2 依赖失败的级联 skip

只要某 step 的 `DependsOn` 里**有一个**是 `Failed`，本 step 直接置 `Skipped` 不执行。失败不扩散到整个 plan，但会**剪枝**依赖子树。

---

## 9. 失败修订 `RevisePlan` (planner.go:132-166)

```
RevisePlan(ctx, plan, failureSummary):
  messages := [
    {system: revisionSystemPrompt},
    {user: "original task: " + plan.UserTask +
           "\ncompleted: " + plan.CompletedStepIDs() +
           "\nfailures: " + failureSummary},
  ]
  resp := llm.ChatCompletion(...)
  newPlan := parsePlanJSON(resp.Content)

  # 保留已完成的 step 状态（不要让 LLM 重新跑 "install-deps"）
  newPlan.Steps = merge(newPlan.Steps, plan.CompletedSteps())
  newPlan.ID       = plan.ID
  newPlan.UserTask = plan.UserTask
  newPlan.Revision = plan.Revision + 1

  ValidateDAG(newPlan.Steps)
  return newPlan
```

### 9.1 `revisionSystemPrompt` (L123)

比 `plannerSystemPrompt` 多强调两件事：

1. **不要重跑已完成的 step**（根据 completed step IDs 做增量计划）；
2. **先讲清为什么失败**，再给修订的 steps —— 便于 debug 时 human 审阅 plan 文件。

### 9.2 修订次数上限

Executor 里硬编码：**最多 2 次修订**。再失败就 return partial result + error。

动机：

- 三次重做仍失败多半是任务本身不可行（环境缺 tool、权限、外部服务挂了）；
- 保护钱包：每次 revision = 1 次 LLM 调用 + N 次 step 执行。

---

## 10. 何时用 Planner？`NeedsPlanning` + `EstimateComplexity`

两个启发式函数（executor.go:239, 305）：

```go
EstimateComplexity(userMessage):
  score := 0
  if 包含 "all files" / "entire project" / "批量":     score += 3
  if 包含 "migrate" / "refactor" / "rename":          score += 2
  if 文本长度 > 200:                                   score += 1
  if 同时出现 "create" + "install" + "test":          score += 2
  if 包含 "for each" / "每个":                         score += 2
  return score

NeedsPlanning(userMessage):
  return EstimateComplexity(userMessage) >= 3
```

`orchestrator.MaybeUsePlanner` 调 `NeedsPlanning` 做**预筛**，避免"帮我看下这个函数"这种请求也走 Planner。

---

## 11. 与 Orchestrator 的桥接（`planner_bridge.go`）

### 11.1 延后注入

```go
// orchestrator.AttachPlanner(p *planner.Planner)
o.planner = &plannerComponents{
    planner:  p,
    executor: planner.NewExecutor(p, o.executePlanStep, logger),
}
```

**为什么延后**？planner 依赖 `LLMCaller`（= orchestrator 自己持有的 llm.Client），而 orchestrator 构造期 planner 未必已实例化。延后注入避免循环构造。

### 11.2 StepExecutor 闭包

```go
// planner_bridge.go:190 executePlanStep
func (o *Orchestrator) executePlanStep(ctx, step planner.Step) (string, error) {
    tc := models.ToolCall{
        ID:   step.ID,
        Name: step.Tool,
        Args: json.Marshal(step.Args),
    }
    result, err := o.executeTool(ctx, tc)       # ★ 复用 §9 的分派器
    if err: return "", err
    if result.IsError: return "", errors.New(result.Content)
    return result.Content, nil
}
```

Planner **完全复用** ReAct 循环里的 `executeTool` 分派器，意味着所有 builtin/skill/MCP 工具对 Planner 自动可用。

### 11.3 结果回显 `formatPlanResult`

```go
// planner_bridge.go:145
formatPlanResult(r *ExecutionResult) string:
  lines := ["## Plan Execution"]
  for level, steps := range r.Plan.StepsByLevel():
    lines.append("### Level %d")
    for _, step := range steps:
      icon := stepStatusIcon(step.Status)       # ✅ ❌ ⏭️ 🔄
      lines.append(icon + " " + step.ID + ": " + truncateLine(step.Output, 120))
  lines.append("\nCompleted: %d / %d, Duration: %dms")
  return join(lines, "\n")
```

前端拿到这个直接 markdown 渲染成带 emoji 的进度表。

---

## 12. 设计权衡

| 抉择 | 动机 |
|---|---|
| **层级拓扑排序** 而非线性拓扑 | 让同层 step 并发成为自然结构，而不是事后判断"这两个能并行吗" |
| Step.ID 由 **LLM 自命名** | UUID 对 LLM 不友好，会污染 DependsOn 字段；自然名生成更稳 |
| **最多 2 次修订** | 保护成本 + 多数情况再改也是在原地打转 |
| 同层并发用**信号量限流** 不是 errgroup 默认 | Sandbox 容器有资源上限；让并发度可调 |
| 依赖失败 → Skip 而非 Fail | 区分"主动失败"与"被牵连"，便于人工审阅定位根因 |
| Executor 只收 `StepExecutor` 闭包 | 彻底解耦：planner 包不知道 sandbox / mcp / skill 存在 |
| CreatePlan 用 `JSONMode: true` | OpenAI/Anthropic 都支持，避免 LLM 夹带自然语言 |
| `RevisePlan` 保留 completed step | 增量思维：不重做已经花钱跑过的东西 |
| 不在 planner 内做 **dry-run/lint 校验** | 只校验 DAG 结构；业务逻辑交给 StepExecutor（边界清晰） |
| `NeedsPlanning` 是**简单关键词启发式** 而非另一次 LLM 调用 | 预筛本身就是省钱，再调 LLM 自相矛盾 |

---

## 13. 后续演进

- [ ] **Plan 持久化**：目前 plan 是进程内结构，crash 就丢；放到 Temporal Workflow 的 state 里（见 `11_temporal`）；
- [ ] **Plan 预览 + 人工确认**：生成后 return suspended + plan.json，前端让用户 edit/approve 再执行；
- [ ] **Step Retry**：目前单 step 失败直接进 Failed；应该支持 `MaxRetries` 和指数退避；
- [ ] **Plan 模板库**：常见任务（"新建 FastAPI 项目"）缓存模板，跳过 LLM；
- [ ] **Output 大对象引用**：Step 间可能传大 JSON/代码，目前全塞在 outputs map 里；加个 ObjectStore；
- [ ] **观测**：给 Planner 加 Prometheus metrics（`planner_plan_steps`, `planner_revisions_total`）；
- [ ] **Cost-aware 合并**：近似 Step（两次小 edit 在同文件）合成一个 edit_file 多 block 调用，省 LLM tokens；
- [ ] **条件分支 Step**：当前只支持 DAG，不支持 `if A failed then run B else C`；
- [ ] **Sub-plan 递归**：复杂 step 可以 spawn 子 plan；让 plan 有分形结构。

---

## 12. 实现剖析与改进方向

### Executor 的调度循环

```text
while pendingSteps > 0:
    ready := stepsWithAllDepsDone()
    if len(ready) == 0:
        break  # 有环或卡住（理论不该发生）

    # 并发执行 ready 层的所有 step
    parallelRun(ready, concurrency=N)

    for step := range ready:
        if step.failed:
            checkpoint()
            cancel all remaining
            return error

        markDone(step.id)
        removeFromDeps(step.id)
```

### Pros
- ✅ 独立分支并发跑，总延迟 = 最长关键路径
- ✅ Checkpoint 让重启续跑，长任务不丢
- ✅ 失败后不盲目继续（cancel）

### Cons
- ⚠️ 并发度硬编码 N=5（没根据 step 类型动态调）
- ⚠️ 重试策略粗糙（全失败就停）
- ⚠️ Plan 本身由 LLM 生成，没有静态校验（循环依赖 / 不存在的 tool）

### 改进方向
- **P0** — Plan validation：`cycleDetect()` 拒绝有环的 DAG
- **P1** — 按 step.Tool 类型调并发度（IO 密集 N=20，CPU 密集 N=2）
- **P1** — 单步重试（瞬时错误）+ 降级（失败多次后跳过继续）
- **P2** — Plan 的可视化 + 实时进度推到 SSE

---

下一篇：`11_temporal.md` —— Temporal 工作流：把 Orchestrator 的长任务迁到 Workflow Runtime，获得持久化状态机、崩溃自愈、可重放的执行。
