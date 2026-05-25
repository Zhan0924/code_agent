# 11 · Temporal 工作流 `internal/temporal`

> 代码：
> - `workflows.go` (173) — `AgentTaskWorkflow`：三阶段 DAG（解析意图 → 安全检查 → 执行），含 Signal 唤醒
> - `activities.go` (104) — `Activities`：ParseIntent / SecurityCheck / ExecuteTask 三个 Activity 桥接到 orchestrator
>
> 依赖：`go.temporal.io/sdk/workflow` + `go.temporal.io/sdk/activity`
>
> **当前状态**：Workflow/Activity 骨架**已实装**，但 `cmd/agent/main.go:341-360` 的 Worker 注册段**整段被注释**。这意味着：
> - Agent 当前跑在**纯内存 Orchestrator**路径（见 `09_orchestrator`）；
> - Temporal 代码是**预埋的"紧急可用逃生舱"**，生产切换步骤见 §9。

---

## 1. 模块定位

**"把一次 chat 请求升格成可持久化、可回放、可手工介入的状态机。"**

Orchestrator 的 ReAct 循环虽然能干活，但**进程一挂就全丢**：

- LLM 调到一半 → 进程 OOM → 用户只看到 500；
- 敏感操作 HITL 挂起 → pod 被 K8s rescheduler 调走 → 授权 channel 消失；
- 连续跑 30 分钟的大迁移任务 → 部署变更 → 半途而废。

Temporal 的价值正好对应：

| 痛点 | Temporal 怎么解 |
|---|---|
| 进程崩溃丢状态 | Workflow 每一步状态都落 **Temporal Server** (Cassandra/PG)，重启自动续跑 |
| HITL 挂起不能无限等 | 用 `workflow.Await` + `Signal` 天然支持**数天级**挂起，内存/连接 0 占用 |
| Activity 调 LLM 超时失败 | SDK 内建**指数退避重试** + 自定义 RetryPolicy |
| 想看任务跑到哪一步 | Temporal Web UI 给每个 workflow 实例**可视化 timeline** |
| 业务改了要重跑历史任务 | **History replay** 可以用新代码回放旧 workflow |

但这些好处**都有代价**：

- 运维复杂度陡增（Temporal Cluster + Worker 部署）；
- Workflow 代码有严格的**确定性约束**（不能直接 time.Now / rand / goroutine）；
- 调试门槛陡增。

所以本仓库的策略是：**先把骨架写好**，当项目跨过 "运行时间 > 5 分钟的任务占比 > 30%" 这条线，一键切入生产。

---

## 1.5 核心设计问题

### 何时值得引入 Temporal？

**信号**（都出现才值得）：
- 有任务需要**挂起等外部事件**（HITL 审批、外部 webhook、定时触发）
- 挂起时长**分钟级以上**（< 秒级用 channel 就够）
- 挂起期间**不能占用 worker 资源**（挂 1000 个任务不能吃 1000 个 goroutine）
- 需要**崩溃恢复**（重启后从断点续跑）

没这些信号的任务（如 Planner DAG 1-20 分钟）就用进程内实现。

### Workflow 的确定性约束为什么值得

Temporal 的卖点是"任意时刻重放 workflow 代码复现状态"。代价是 workflow
代码不能做：`time.Now()`, `rand`, 直接 IO, `go func()`, map 遍历。
看起来很严，但这份严格恰好保证了"服务重启后任务不丢" 这个核心承诺。

**经验法则**：workflow 代码只做两件事——decision（条件分支 + 等待）
和 orchestration（调 activity + 收集结果）。真正的计算 / IO / 时间 /
随机都丢到 activity 里。

### 单 workflow 还是多 workflow？

本系统选**单个 `AgentTaskWorkflow`** 处理所有 chat 任务。原因：
- HITL 模式对所有 intent 一致（signal + resume）
- Activity 粒度已经区分了 parse intent / security check / execute
- 多 workflow 会让 Temporal UI 变得混乱（难追踪）

未来如果 intent 之间差异巨大（如实时监控 workflow vs 批量迁移 workflow），
再拆成多个 workflow。

---

## 2. 依赖架构

```
┌──────────────── HTTP API ────────────────┐
│  /chat → (current) Orchestrator.Process  │  ← 默认路径
│                                           │
│         (future) client.StartWorkflow─┐   │  ← 切换后的路径
└───────────────────────────────────────┼──┘
                                        ▼
                        ┌──────────────────────────┐
                        │     Temporal Server      │
                        │  (state store + scheduler│
                        │   Cassandra/PostgreSQL)  │
                        └────────────┬─────────────┘
                                     │ gRPC
                                     ▼
                         ┌────────────────────────┐
                         │    Temporal Worker     │
                         │ (本进程的一部分 goroutine)│
                         │                        │
                         │  Workflow: AgentTask  ─┼──▶ workflow.Context (非真正 ctx)
                         │  Activity: Parse/Sec/Exec │     ↓
                         └────────────────────────┘     orchestrator methods
                                                        (真 ctx.Context)
```

**关键边界**：

- `workflow.Context` **不是** `context.Context`；它没有 deadline，反而有"时间回放"语义；
- `activity.Context` **是**真正的 context.Context；Activity 里可以自由调 SDK / 做 I/O；
- 所有副作用（DB、LLM、文件）**只能**发生在 Activity 内。Workflow 代码纯函数式。

---

## 2.5 数据流总览

### 2.5.1 Workflow 完整生命周期

```text
┌───────────────────────────────────────────────────────────────┐
│ HTTP Handler: POST /api/v1/chat                               │
│  cfg.Temporal.Enabled == true?                                │
│  YES → 走 Temporal 路径                                       │
│  NO  → 直接调 orchestrator (in-memory 模式)                   │
└──────────────────────────┬────────────────────────────────────┘
                           │ (Temporal 路径)
                           ▼
┌───────────────────────────────────────────────────────────────┐
│ temporal.Client.ExecuteWorkflow(                               │
│   "AgentTaskWorkflow",                                        │
│   AgentTaskInput{SessionID, Message, UserID})                 │
└──────────────────────────┬────────────────────────────────────┘
                           │
                           ▼
┌───────────────────────────────────────────────────────────────┐
│ 【Temporal Server】 调度任务到 Worker                          │
└──────────────────────────┬────────────────────────────────────┘
                           │
                           ▼
┌═══════════════════════════════════════════════════════════════┐
║ AgentTaskWorkflow (确定性代码, 无副作用)                       ║
║                                                               ║
║  Stage 1: ParseIntentActivity                                ║
║    → classifyIntent(msg) 关键词匹配                          ║
║    → RetryPolicy: max 3次, backoff 1s                        ║
║    → 返回 IntentResult{Intent}                               ║
║                                                               ║
║  Stage 2: SecurityCheckActivity                              ║
║    → 检查 intent 是否需要人工审批                             ║
║    → 返回 SecurityCheckResult{NeedsApproval, Reason}         ║
║                                                               ║
║  ┌─── NeedsApproval? ───────────────────────────────────┐    ║
║  │                                                       │    ║
║  │ YES:                                                  │    ║
║  │   ┌─────────────────────────────────────────────┐    │    ║
║  │   │ workflow.Await(ApprovalSignal, 24h timeout) │    │    ║
║  │   │   → 推送 approval_request 事件到前端 (SSE) │    │    ║
║  │   │   → 等待外部 Signal                         │    │    ║
║  │   └───────────────────┬─────────────────────────┘    │    ║
║  │                       │                               │    ║
║  │          ┌────────────┴────────────┐                  │    ║
║  │          ▼                         ▼                  │    ║
║  │   ┌───────────┐           ┌──────────────┐           │    ║
║  │   │ Approved  │           │ Rejected /   │           │    ║
║  │   │ → 继续   │           │ Timeout      │           │    ║
║  │   └─────┬─────┘           │ → 终止 workflow│          │    ║
║  │         │                 └──────────────┘           │    ║
║  │         ▼                                            │    ║
║  │ NO:     │                                            │    ║
║  └─────────┼────────────────────────────────────────────┘    ║
║            │                                                  ║
║            ▼                                                  ║
║  Stage 3: ExecuteTaskActivity                                ║
║    → orchestrator.ProcessMessage(ctx, req)                   ║
║    → RetryPolicy: max 2次, timeout 300s                      ║
║    → 返回 AgentTaskOutput{Response, Status}                  ║
║                                                               ║
╚═══════════════════════════════════════════════════════════════╝
                           │
                           ▼
┌───────────────────────────────────────────────────────────────┐
│ HTTP 轮询 / WebSocket: 获取 workflow 结果                     │
│  → client.QueryWorkflow / GetWorkflowResult                  │
│  → 返回给前端                                                │
└───────────────────────────────────────────────────────────────┘
```

### 2.5.2 外部审批信号流

```text
┌───────────────────┐
│ 前端: 用户点击    │
│ "Approve" / "Reject"
└─────────┬─────────┘
          │
          ▼
┌───────────────────────────────────────────────────────────────┐
│ POST /api/v1/tasks/:id/approve  {approved: true/false}        │
└──────────────────────────┬────────────────────────────────────┘
                           │
                           ▼
┌───────────────────────────────────────────────────────────────┐
│ temporal.Client.SignalWorkflow(                                │
│   workflowID, "approval_signal",                              │
│   ApprovalResponse{Approved, Reason})                         │
└──────────────────────────┬────────────────────────────────────┘
                           │
                           ▼
┌───────────────────────────────────────────────────────────────┐
│ Workflow 内部:                                                 │
│  workflow.Await 收到 Signal → 解除阻塞                        │
│  approved=true  → 继续 Stage 3                               │
│  approved=false → workflow 返回 Rejected 状态                │
└───────────────────────────────────────────────────────────────┘
```

---

## 3. 数据结构

```go
// workflows.go:15-27
type AgentTaskInput struct {
    TaskID    string `json:"task_id"`
    SessionID string `json:"session_id"`
    UserInput string `json:"user_input"`
    UserID    string `json:"user_id"`
}

type AgentTaskOutput struct {
    TaskID    string `json:"task_id"`
    Content   string `json:"content"`
    State     string `json:"state"`       // "completed" / "suspended" / "failed"
    Error     string `json:"error,omitempty"`
    Metadata  map[string]any `json:"metadata,omitempty"`
}

// workflows.go:29
const ApprovalSignal = "approval-signal"
```

三类中间产物（各自独立的结构体，便于 Activity 签名清晰）：

```go
type IntentResult struct {
    Intent     string
    Confidence float64
}
type SecurityCheckInput   struct { UserInput string; Intent string }
type SecurityCheckResult  struct { IsSafe bool; Reason string; RiskLevel string }
type ExecuteTaskInput     struct { TaskID, SessionID, UserInput, Intent string }
type ExecutionResult      struct { Content string; State string; Error string }
```

---

## 4. ★ 主 Workflow `AgentTaskWorkflow` (L33-141)

```go
// workflows.go:33
func AgentTaskWorkflow(ctx workflow.Context, input AgentTaskInput) (*AgentTaskOutput, error)
```

展开为三段式 + 可选 HITL 分支：

```
┌─ Stage 1: ParseIntentActivity ──────────────┐
│   retry: 3次, per-attempt=30s                 │
│   输出 IntentResult                            │
└───────────────┬──────────────────────────────┘
                │
┌───────────────▼──────────────────────────────┐
│ Stage 2: SecurityCheckActivity               │
│   输出 {IsSafe, RiskLevel, Reason}             │
└───────────────┬──────────────────────────────┘
                │
         ┌──────┴──────┐
         │             │
      IsSafe        NOT IsSafe
         │             │
         │             ▼
         │   ┌────────────────────────────┐
         │   │  workflow.Await(ctx,       │
         │   │     ApprovalSignal)        │  ← 挂起，天然持久
         │   │  信号里带 approve=true/false │
         │   └─────┬──────────────────────┘
         │         │
         │     approved?
         │   ┌─────┴─────┐
         │  no          yes
         │   │           │
         │   └──▶ return State:denied
         │                │
         ▼                ▼
┌──────────────────────────────────────────────┐
│ Stage 3: ExecuteTaskActivity                 │
│   retry: 2次, per-attempt=30min               │
│   输出 ExecutionResult                         │
└───────────────┬──────────────────────────────┘
                │
                ▼
         return AgentTaskOutput
```

### 4.1 Signal 机制详解

```go
// Inside workflow (伪代码):
if !secResult.IsSafe {
    var approval ApprovalPayload
    signalCh := workflow.GetSignalChannel(ctx, ApprovalSignal)

    // 最多等 24 小时
    timerCtx, cancel := workflow.WithTimeout(ctx, 24*time.Hour)
    defer cancel()

    selector := workflow.NewSelector(ctx)
    selector.AddReceive(signalCh, func(c workflow.ReceiveChannel, _ bool) {
        c.Receive(ctx, &approval)
    })
    selector.AddFuture(workflow.NewTimer(timerCtx, 24*time.Hour), func(f workflow.Future){
        approval = ApprovalPayload{Approved: false, Reason: "timeout"}
    })
    selector.Select(ctx)
    ...
}
```

**外部唤醒**（从 HTTP handler）：

```go
client.SignalWorkflow(ctx,
    workflowID,              // = task.ID
    "",                      // runID: "" 表示当前 run
    ApprovalSignal,
    ApprovalPayload{Approved: true, Comment: "LGTM"},
)
```

这替代了 orchestrator 的 `approvalCh map`，**零内存占用**：挂起期间 worker 可以被 K8s kill，重启后 Temporal 自动 replay workflow 到 Await 那一行。

### 4.2 RetryPolicy 的默认值

```go
// activities.go:100-104 (伪)
var (
    ParseIntentRetry = &temporal.RetryPolicy{
        InitialInterval:    time.Second,
        BackoffCoefficient: 2.0,
        MaximumInterval:    30 * time.Second,
        MaximumAttempts:    3,
    }
    ExecuteRetry = &temporal.RetryPolicy{
        MaximumAttempts: 2,  // 执行可能有副作用，不要乱重试
    }
)
```

**为什么 Execute 只重试 2 次**？因为 `ExecuteTaskActivity` 背后是 `orchestrator.ProcessMessage`，里面已经有自己的 LLM 重试 + 工具重试。外层再放大重试次数 = 指数爆炸。

---

## 5. Activities 桥接 `activities.go`

```go
type Activities struct {
    orch   *orchestrator.Orchestrator
    secCfg *config.SecurityConfig
    logger *zap.Logger
}

NewActivities(orch, secCfg, logger) *Activities
```

### 5.1 `ParseIntentActivity` (L72)

```go
func (a *Activities) ParseIntentActivity(ctx, input AgentTaskInput) (*IntentResult, error) {
    intent := classifyIntent(input.UserMessage) // 关键词匹配: deploy/部署/发布 → IntentDeploy
    return &IntentResult{Intent: intent}, nil
}
```

基于关键词的 intent 分类器 —— 匹配 deploy/部署/发布/上线等关键词时返回 `IntentDeploy`，触发 SecurityCheckActivity 的审批流程。

### 5.2 `SecurityCheckActivity` (L53)

```go
func (a *Activities) SecurityCheckActivity(ctx, input SecurityCheckInput) (*SecurityCheckResult, error) {
    for _, pattern := range a.secCfg.SensitivePatterns {
        re, _ := regexp.Compile(pattern)
        if re.MatchString(input.UserInput) {
            return &SecurityCheckResult{
                IsSafe: false,
                Reason: "matched pattern: " + pattern,
                RiskLevel: "HIGH",
            }, nil
        }
    }
    return &SecurityCheckResult{IsSafe: true, RiskLevel: "LOW"}, nil
}
```

和 orchestrator 的 `containsSensitiveContent` 语义一致，但**独立实现** —— 因为 orchestrator 的 sensitiveRules 是内部字段，从 Activity 访问需要更多耦合。副本在这里可接受。

### 5.3 `ExecuteTaskActivity` (L82)

```go
func (a *Activities) ExecuteTaskActivity(ctx, input ExecuteTaskInput) (*ExecutionResult, error) {
    resp, err := a.orch.ProcessMessage(ctx, input.SessionID, input.UserInput)
    if err != nil {
        return &ExecutionResult{State: "failed", Error: err.Error()}, err
    }
    return &ExecutionResult{
        Content: resp.Content,
        State:   string(resp.State),
    }, nil
}
```

**把 orchestrator 的 30 分钟 ReAct 包装成一个 Activity** —— 工作流层面看起来就是"调一次方法"，实际 Activity 内部自主运转。

**注意**：这里 ReAct 的中间状态（messages 数组、失败计数）**不会**被 Temporal 持久化。要真正 crash-safe，需要在 ReAct 里**更细粒度地**把每个 tool_call 拆成独立 Activity。当前版本是**中间方案**——关键敏感节点（HITL）走 Signal，其他仍然是单体 Activity。见 §8 演进方向。

---

## 6. Worker 注册（当前状态：已接入）

> ⚠️ **2026-05 更新（P0 #17 修复）**：此节曾描述 worker 处于"注释中"的
> 占位状态。实际上 `cmd/agent/main.go` 原来的 `initTemporalWorker` 函数
> 只打 log 不做事——文档与代码双双误导读者。现在的代码已真实接入
> Temporal SDK，`main.go:345-385` 的 `startTemporalWorker` 函数如下：

```go
// startTemporalWorker dials Temporal, registers workflow + activities,
// starts worker. Returns (client, worker) for main to defer cleanup.
func startTemporalWorker(
    cfg *config.TemporalConfig,
    secCfg *config.SecurityConfig,
    orch *orchestrator.Orchestrator,
    logger *zap.Logger,
) (temporalclient.Client, temporalworker.Worker) {
    ns := cfg.Namespace
    if ns == "" {
        ns = temporalclient.DefaultNamespace
    }
    queue := cfg.TaskQueue
    if queue == "" {
        queue = "agent-tasks"
    }

    cli, err := temporalclient.Dial(temporalclient.Options{
        HostPort:  cfg.Host,
        Namespace: ns,
    })
    if err != nil {
        logger.Warn("temporal dial failed — HITL workflow path disabled", ...)
        return nil, nil   // fail-safe：HTTP 主路径继续工作
    }

    w := temporalworker.New(cli, queue, temporalworker.Options{})
    w.RegisterWorkflow(temporalpkg.AgentTaskWorkflow)
    activities := temporalpkg.NewActivities(orch, secCfg, logger)
    w.RegisterActivity(activities)

    if err := w.Start(); err != nil {   // non-blocking
        logger.Warn("temporal worker failed to start", zap.Error(err))
        cli.Close()
        return nil, nil
    }
    logger.Info("temporal worker started", zap.String("task_queue", queue))
    return cli, w
}
```

**Main 的接线**：
```go
var temporalCli  temporalclient.Client
var temporalWorker temporalworker.Worker
if cfg.Temporal.Host != "" {
    temporalCli, temporalWorker = startTemporalWorker(&cfg.Temporal, &cfg.Security, orch, logger)
    if temporalCli != nil    { defer temporalCli.Close() }
    if temporalWorker != nil { defer temporalWorker.Stop() }
}
```

**关键设计点**：

1. **Fail-safe** — Dial 失败不 Fatal；HTTP 主路径（非 HITL）必须独立可用。
2. **非阻塞 Start** — `w.Start()` 立刻返回，内部开轮询 goroutine；不占用 main。
3. **Defer 顺序** — 先 `worker.Stop()` 让 in-flight activities drain，再 `client.Close()`。
4. **Activities 构造** — `NewActivities(orch, secCfg, logger)` 注入 orchestrator
   引用，Activity 内部委派给 orchestrator 的现成方法。

**启用条件**：`config.yaml` 里设置非空的 `temporal.host`：
```yaml
temporal:
  host: "temporal:7233"
  namespace: "code-agent"
  task_queue: "agent-tasks"
  workflow_timeout: 30m
  activity_timeout: 5m
```

**日志验证**：
```
# 成功
"msg":"initializing Temporal worker","host":"temporal:7233"
"msg":"temporal worker started","task_queue":"agent-tasks"

# 失败（Temporal 服务不可达）
"msg":"temporal dial failed — HITL workflow path disabled","error":"..."
```

**回归详情**：见 [22_recent_improvements.md § C.3](22_recent_improvements.md#c3-temporal-worker-实际是-no-op-p0-17)。

---

## 7. 确定性约束（Workflow 代码的"地雷"）

Workflow 代码**不是普通 Go**，有若干禁忌：

| ❌ 不能用 | ✅ 替代 |
|---|---|
| `time.Now()` | `workflow.Now(ctx)` |
| `time.Sleep(d)` | `workflow.Sleep(ctx, d)` |
| `rand.Int()` | `workflow.SideEffect(ctx, func(ctx){ return rand.Int() })` |
| `go func(){...}()` | `workflow.Go(ctx, func(ctx){...})` |
| `context.Background()` | 直接用传入的 `workflow.Context` |
| 直接读取文件 / HTTP | 全部包成 Activity |
| map 遍历随便顺序 | 要稳定顺序（Go 1.12+ map 遍历乱序 → 必须用 sorted keys） |

**为什么这么严**？Temporal 靠 **history replay** 重建状态：worker 重启后，workflow 代码会被"从头跑一遍"，每遇到 Activity/Sleep/SideEffect 就从 history 读取上次结果。任何非确定性都会让 replay 产生与原始不同的决策，导致状态损坏。

**维护 tip**：用 `// +build !workflowcheck` 或 go-linter 插件 [`workflowcheck`](https://github.com/temporalio/sdk-go/tree/master/contrib/tools/workflowcheck) CI 扫描此类违规。

---

## 8. 双模共存：HTTP 路径如何切换

上线 Temporal 后，`/chat` 路由的伪代码：

```go
func ChatHandler(c *gin.Context) {
    var req ChatRequest
    c.BindJSON(&req)

    if cfg.Temporal.Enabled {
        // Workflow 模式
        wfID := req.TaskID  // 用户提供或服务端生成
        wfRun, err := temporalClient.ExecuteWorkflow(ctx,
            client.StartWorkflowOptions{
                ID: wfID,
                TaskQueue: cfg.Temporal.TaskQueue,
                WorkflowExecutionTimeout: 2 * time.Hour,
            },
            temporalpkg.AgentTaskWorkflow,
            temporalpkg.AgentTaskInput{...},
        )
        if err != nil { c.JSON(500, ...); return }

        var out temporalpkg.AgentTaskOutput
        if err := wfRun.Get(ctx, &out); err != nil {
            c.JSON(500, gin.H{"error": err.Error()}); return
        }
        c.JSON(200, out)
    } else {
        // 内存模式（当前）
        resp, err := orch.ProcessMessage(ctx, req.SessionID, req.Message)
        c.JSON(200, resp)
    }
}
```

`/chat/approve` 对应：

```go
if cfg.Temporal.Enabled {
    temporalClient.SignalWorkflow(ctx, taskID, "",
        temporalpkg.ApprovalSignal,
        ApprovalPayload{Approved: req.Approved, Comment: req.Comment})
} else {
    orch.HandleApproval(ctx, req)   // 走 approvalCh map
}
```

两套逻辑**并存**的好处是：渐进迁移，灰度放量，出问题一键 flag 回退。

---

## 9. 上线 Checklist

从"当前（注释）"到"生产 Temporal"：

- [ ] **基础设施**：部署 Temporal Cluster（单节点 `temporalio/auto-setup` 也可先跑），配 Cassandra 或 PostgreSQL 作持久层；
- [ ] **config.yaml**：新增 `temporal.enabled / host_port / namespace / task_queue / workflow_timeout`；
- [ ] **config.go**：`TemporalConfig` struct + validation；
- [ ] **go.mod**：`go get go.temporal.io/sdk@latest`；
- [ ] **main.go**：取消注释 Worker 注册段；增加 client Close 到 graceful shutdown；
- [ ] **api/handlers.go**：按 §8 的双模 if/else 改写 `/chat` 和 `/chat/approve`；
- [x] **ParseIntentActivity**：已实现关键词分类器（deploy/部署/发布 → IntentDeploy）；
- [ ] **观测**：接 Temporal 的 OpenTelemetry interceptor，metrics 进 Prometheus；
- [ ] **灰度**：`cfg.Temporal.RolloutPercent = 10`，按 session_id 哈希选路；
- [ ] **回放测试**：用 `replayer.ReplayWorkflowHistory()` 拿一条生产 history 回放，CI 里跑一次；
- [ ] **workflowcheck linter** 接入 CI，防止非确定性代码偷偷加入 Workflow。

---

## 10. 设计权衡

| 抉择 | 动机 |
|---|---|
| **当前注释 Temporal Worker** | 早期流量小，内存 Orchestrator 更简单；骨架预留以便后续切换 |
| `AgentTaskWorkflow` 三阶段拆分（Parse/Sec/Exec） | 每段独立重试策略；failure 边界清晰；Temporal Web UI 看得到每步状态 |
| HITL 用 **Signal + Await** 而非轮询 | 天生持久，worker 随便重启；等待几小时 ~ 几天都 0 成本 |
| Activity 直接复用 orchestrator 方法 | 零重复代码；两种运行模式共享相同业务逻辑 |
| ExecuteTaskActivity **包大粒度** | 当前折中：HITL 在 workflow 层精细，其他靠 orchestrator 内部鲁棒性 |
| 确定性约束由 **workflowcheck CI** 守护 | 人肉 review 靠不住；workflow 代码太 subtle |
| Workflow ID = Task ID | 用户能按 taskID 查 Web UI 追溯；幂等 start |
| RetryPolicy 对**每类 Activity 独立配置** | LLM 可以 retry，迁移数据库**不能** retry |
| Activity 里**独立实现** SecurityCheck 的正则循环 | orchestrator 内部字段私有；副本可接受 |
| 预留 **双模 if cfg.Temporal.Enabled** 切换 | 灰度上线，retreat 一个 flag 的距离 |

---

## 11. 后续演进

- [ ] **ReAct 每一步拆 Activity**：把 "LLM call" 和 "tool exec" 都做成独立 Activity，真正 crash-resume；代价：Workflow 代码复杂度 +3×；
- [ ] **Child Workflow 并行执行**：Planner 的多 Step 可以 spawn Child Workflow，借 Temporal 天生 parallelism；
- [ ] **Search Attributes**：给每个 Workflow 打上 `user_id / intent / risk_level` 标签，Web UI 支持按属性过滤；
- [ ] **Cron Workflow**：基于 Temporal 调度的定时任务（每日索引刷新、每周 RAG 重编码）；
- [ ] **Versioning 策略**：Workflow 改逻辑时用 `workflow.GetVersion` patch，保证线上实例不被破坏；
- [ ] **Saga 回滚**：长流程中某步失败时自动跑补偿 activity（例如 git revert）；
- [ ] **Advanced Visibility**：接 Elasticsearch，按任意字段模糊搜 Workflow；
- [ ] **Metrics dashboards**：`temporal_workflow_completed_total / temporal_activity_latency_ms`，Grafana 标配。

---

## 11. 实现剖析与改进方向

### Workflow 执行的实际时序（HITL 审批场景）

```text
User → POST /chat {"deploy to prod"}
  │
  ▼
orchestrator → DetectIntent → IntentDeploy
  │
  ▼
TemporalClient.StartWorkflow(AgentTaskWorkflow, input)
  │
  ▼
Worker pulls task queue →
  ActivityExecute(ParseIntentActivity)
  ActivityExecute(SecurityCheckActivity)
    → result.RequiresApproval = true
  │
  ▼
workflow.Await(ctx, signalReceived)
  │ 挂起，Worker 释放 goroutine，Temporal Server 保存状态
  │ [此刻占用资源 = 0]
  │
  ▼
（管理员在前端点批准）
  │
POST /tasks/:id/approve
  │
  ▼
TemporalClient.SignalWorkflow(taskID, "approval-signal", payload)
  │
  ▼
Server 重新调度 workflow 到某 Worker
  │ workflow 从 Await 返回
  │
  ▼
ActivityExecute(ExecuteTaskActivity)  ← 真正执行 orchestrator
  │
  ▼
return AgentTaskOutput
```

### Pros
- ✅ 挂起零资源占用（核心卖点）
- ✅ 崩溃恢复（worker 挂重启后 workflow 从断点续跑）
- ✅ 可视化 UI（Temporal Web UI 看每个 workflow 的 event history）

### Cons
- ⚠️ 部署复杂（Temporal Server + PG / Cassandra + Worker）
- ⚠️ Workflow 代码确定性约束严格（调试不便）
- ⚠️ 学习曲线陡
- ⚠️ 和 orchestrator 双存的数据模型（Task 既在 Temporal 也在 session）

### 改进方向
- **P0** — Temporal Dial 失败后定时重试（目前仅启动一次）
- **P1** — workflow 增加 ReAct 中间状态的 checkpoint（更细粒度 replay）
- **P1** — Temporal 指标接 Prometheus（workflow 数、成功率、挂起时长）
- **P2** — workflow cancelation 路径（用户主动取消 workflow）

---

下一篇：`12_session.md` —— Session 管理器 + 摘要器：Redis 多轮对话状态、超阈值自动 summarization。
