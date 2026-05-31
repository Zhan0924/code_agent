# 11 · Temporal `internal/temporal` + `cmd/agent/temporal_adapter.go` + `orchestrator/temporal_bridge.go`

> **代码骨架（2026-05 真实代码）**
>
> | 文件 | 行数 | 角色 |
> |---|---|---|
> | `internal/temporal/workflows.go` | 257 | `AgentTaskWorkflow` 主工作流 + 输入/输出/Activity 结果类型 |
> | `internal/temporal/activities.go` | 208 | 三个 Activity：`ParseIntentActivity`/`SecurityCheckActivity`/`ExecuteTaskActivity` |
> | `internal/temporal/doc.go` | 71 | 包级文档（保留为参考；某些 API 描述如 `workflow.Await` 与真实实现不符——见 §6.2） |
> | `cmd/agent/main.go` L680-791 | 110 | `startTemporalWorker`：Dial → RegisterWorkflow → RegisterActivity → Worker.Start |
> | `cmd/agent/temporal_adapter.go` | 46 | `temporalHITLAdapter`：把 SDK client 适配成 orchestrator 期望的 `TemporalClient` 接口 |
> | `internal/orchestrator/temporal_bridge.go` | 86 | Orchestrator 端：`TemporalClient` 接口 + `ContextWithSkipHITL` 防递归 + `suspendForApprovalTemporal`/`HandleApprovalTemporal` |
>
> **现状澄清（与早期文档不同）**：Temporal Worker **已经接线**。`main.go:688` 当 `cfg.Temporal.Host != ""` 时调 `startTemporalWorker`，Dial 成功后注册 workflow + activities 并启动 worker；Dial 失败 → log warning + fall through，HITL 走进程内通道。**所以 Temporal 路径已就绪，是否启用由 config 决定**。

---

## 1. 模块定位

**"把'需要人审 2 小时'这种活，从'goroutine 占资源等到天荒地老'变成'状态写库挂起、收到信号再 replay 跑下去'。"**

Temporal 在本系统的职责很窄：**只承担 HITL 高危任务的工作流**。Plan/ReAct/RAG/Sandbox 这些都不走 Temporal——它们是进程内状态机，奉行 graceful-degradation。Temporal 只接管"需要持久挂起 + 状态恢复"的部分。

### 1.1 与 in-process HITL 的二选一

```
                ┌─ o.temporalClient == nil ─→ suspendForApprovalInProcess
suspendForApproval                              · approvalCh map[taskID]chan
                │                               · 30 min 硬编码超时
                │                               · 进程重启 = 用户重新点
                │
                └─ o.temporalClient != nil ──→ suspendForApprovalTemporal
                                                · StartHITLWorkflow
                                                · 工作流持久化，重启不丢
                                                · 30 min Selector Timer
```

代码切换在 `orchestrator/orchestrator.go:640-649`。**优先用 Temporal**，失败才回退进程内（`suspendForApprovalTemporal` 内 StartHITLWorkflow 出错时直接 fall back，见 `temporal_bridge.go:40-42`）。

### 1.2 必要场景（什么时候 Temporal 不是过度工程）

| 场景 | 是否需要 Temporal |
|---|---|
| "运行 ls / cat 这种命令" | ❌ 进程内通道即可 |
| "kubectl apply 生产环境" | ✅ 人工审批可能跨班次 |
| "把 staging DB 同步到 prod" | ✅ DBA 可能要几小时 review SQL |
| "发布 PR 到 main" | ✅ Reviewer 可能下班，第二天才点 approve |
| "重启 Docker 容器" | ⚠️ 进程内也行，但用 Temporal 能在 audit log 看到完整时间线 |

### 1.3 与 Planner 的关系（必须分清）

| 子系统 | 持久化 | 适合场景 |
|---|---|---|
| **Planner**（10_planner） | Plan 写 `o.store` 但 Executor 是进程内 goroutine | 1-20 分钟批量任务，无 HITL |
| **Temporal**（本章） | 整个 Workflow 状态由 Temporal Server 持久化，跨进程 replay | HITL 长挂起、需崩溃恢复 |

Planner 用 Temporal 做底是**未来的事**（见 `_principles.go` 的 RFC），目前 Planner 不依赖 Temporal。

---

## 1.5 核心设计问题

### Q1：为什么 Workflow 函数必须是确定性的？

Temporal Server 把每次 `workflow.ExecuteActivity`、`workflow.NewTimer`、`workflow.GetSignalChannel` 等 SDK 调用写入 **Event History**。Worker 进程 crash 后，**任何**新 Worker 都能从 history replay 重建 workflow 局部变量与执行位置——前提是**每次 replay 跑完同样的代码路径**。

→ 在 workflow 函数里**禁止**：
- `time.Now()`（用 `workflow.Now(ctx)`）
- `rand.*`（用 `workflow.SideEffect`）
- 直接 IO、HTTP、文件操作（必须丢给 Activity）
- 启动 `go` routine（用 `workflow.Go`）
- 迭代 `map`（顺序未定义，违反确定性）
- `time.Sleep` / `time.After`（用 `workflow.NewTimer`）

违反任一条 → replay 决策与原 history 不一致 → workflow 直接 `NonDeterminismError` fail。

### Q2：为什么 HITL 在 Temporal 下是 0 资源占用？

```go
// workflows.go:172-187 (简化)
selector := workflow.NewSelector(ctx)
selector.AddReceive(approvalCh, ...)
selector.AddFuture(timerFuture, ...)
selector.Select(ctx)   // ★ 这里"挂起"
```

`Select(ctx)` 在 Temporal Worker 看是**yielding**——Worker 把 workflow goroutine 让出，整个 workflow 状态由 Server 持久化。挂起期间：

- Worker 进程没有任何 goroutine 在等这个 workflow（**0 内存、0 CPU**）；
- Worker Pod 可以被 K8s 重启 / 缩容 / 滚动；
- 信号到达时 Server 选**任意**有空闲 worker poll 的进程，replay 历史到 Select 处，把信号塞进去继续跑。

这是 Temporal 的杀手 feature。1000 个 pending workflow = 0 worker 资源（不算 Server 自己存数据的开销）。

### Q3：为什么 `ParseIntentActivity` 跟 `SecurityCheckActivity` 也是 Activity？

`ParseIntentActivity` 调 LLM（明显有副作用）→ 必须 Activity。

`SecurityCheckActivity` 是纯正则匹配，**理论上**可以放 workflow 里。但实际上：

- 正则 `Compile` 是确定性的，但 `regexp` 包内部分支可能版本相关；
- Activity 化后可独立配 retry（虽然正则不需要重试）；
- 让 workflow 代码尽量简短，把所有"决策"包装成 activity 的清晰输入输出。

这是 Temporal 社区惯例：**workflow 像主控制器，activity 是单个步骤**。

### Q4：为什么 Activity 失败重试 3 次，BackoffCoefficient 2.0？

```go
// workflows.go:108-114
RetryPolicy: &temporal.RetryPolicy{
    InitialInterval:    time.Second,
    BackoffCoefficient: 2.0,
    MaximumInterval:    time.Minute,
    MaximumAttempts:    3,
}
```

3 次重试覆盖大多数**瞬时失败**（网络抖动、LLM rate limit）；超过 3 次基本是**配置/逻辑错**，再重试只是浪费。BackoffCoefficient=2.0 是指数退避标配；MaximumInterval=60s 把"指数爆炸"压在 1 分钟以内。

注意：**Temporal Activity Retry 与 LLM Client gobreaker 是不同层**——
- `gobreaker`：**熔断**——连续 N 次失败后**拒绝**新请求；
- Temporal Retry：**重试**——单次 Activity 失败后**重试**。

两者互补：熔断保护下游，重试容忍瞬时。

### Q5：为什么 `ContextWithSkipHITL` 必须存在？

Workflow → ExecuteTaskActivity → orchestrator.ProcessMessage → containsSensitiveContent 检查 → **触发新的 HITL** ❌

如果不阻止，会形成无限递归"workflow 提交审批 → 审批通过 → 再开一个 workflow 审批"。

`temporal_bridge.go:16` 用 ctx value 注入 `skipHITL=true` 标记，`orchestrator.go:834` 的 `if !skipHITL(ctx) && ...` 短路。

### Q6：30 分钟硬编码超时是否合理？

```go
// workflows.go:168
timerFuture := workflow.NewTimer(timerCtx, 30*time.Minute)
```

- 短于 30 分钟：reviewer 没空看完上下文；
- 长于 30 分钟：用户已经离开，等更久没意义；
- 改成 config：可以，但**不建议**——Temporal Workflow 一旦运行是不能"在线改 timer 时长"的，新值只对**新启动的 workflow**生效。简单常量反而更可预测。

---

## 2. 依赖架构

```
┌──────────────────────────────────────────────────────────────────────┐
│ cmd/agent/main.go                                                    │
│   if cfg.Temporal.Host != "":                                        │
│     temporalCli, temporalWorker = startTemporalWorker(...)           │
│       │                                                              │
│       ├─ temporalclient.Dial(HostPort, Namespace)                    │
│       ├─ worker.New(cli, queue, {})                                  │
│       ├─ worker.RegisterWorkflow(AgentTaskWorkflow)                  │
│       ├─ worker.RegisterActivity(Activities{orch, secCfg, logger})   │
│       └─ worker.Start()  ← 非阻塞，自起 polling goroutines           │
│                                                                      │
│   orch.SetTemporalClient(&temporalHITLAdapter{client, queue})        │
└────────┬─────────────────────────────────────────────────────────────┘
         │
         ▼
┌──────────────────────────────────────────────────────────────────────┐
│ Orchestrator                                                         │
│   suspendForApproval:                                                │
│     if o.temporalClient != nil:                                      │
│       return suspendForApprovalTemporal(task)                        │
│     else:                                                            │
│       return suspendForApprovalInProcess(task)                       │
└────────┬─────────────────────────────────────────────────────────────┘
         │ (Temporal path)
         ▼
┌──────────────────────────────────────────────────────────────────────┐
│ temporalHITLAdapter (cmd/agent/temporal_adapter.go)                  │
│   StartHITLWorkflow(taskID, sessionID, userMessage):                 │
│     workflowID = "hitl-" + taskID                                    │
│     cli.ExecuteWorkflow(opts{ID, TaskQueue}, AgentTaskWorkflow, ...) │
│   SignalApproval(workflowID, approved, comment):                     │
│     cli.SignalWorkflow(workflowID, "", "approval-signal", payload)   │
└────────┬─────────────────────────────────────────────────────────────┘
         │
         ▼
┌──────────────────────────────────────────────────────────────────────┐
│ AgentTaskWorkflow (internal/temporal/workflows.go)                   │
│   ① ParseIntentActivity     ──┐                                     │
│   ② SecurityCheckActivity   ──┤ each: Retry 3x, StartToClose 5min   │
│   ③ if RequiresApproval:                                            │
│         Selector { approvalCh / timer 30min }.Select(ctx)            │
│         → approved? proceed : Cancelled                              │
│   ④ ExecuteTaskActivity      ──┘                                     │
└────────┬─────────────────────────────────────────────────────────────┘
         │
         ▼
┌──────────────────────────────────────────────────────────────────────┐
│ Activities (internal/temporal/activities.go)                         │
│   · ParseIntentActivity:                                             │
│       LLM classify → fallback keyword (deploy/conversation)          │
│   · SecurityCheckActivity:                                           │
│       预编译 regex × cfg.Security.SensitivePatterns                  │
│       + IntentDeploy → RequiresApproval=true, RiskLevel=critical     │
│   · ExecuteTaskActivity:                                             │
│       execCtx = ContextWithSkipHITL(ctx)                             │
│       orch.ProcessMessage(execCtx, ...)  ← ★ 这里递归回 orchestrator │
└──────────────────────────────────────────────────────────────────────┘
```

**关键反向依赖**：Activity → `orchestrator.ProcessMessage`，所以 `Activities` 结构体持有 `*orchestrator.Orchestrator`（activities.go:48）。这条边让 Temporal 路径上的执行和进程内路径走**同一个 reactLoop**——一致性比独立性更重要。

---

## 2.5 数据流总览

### 2.5.1 标准高危请求（含 HITL）端到端

```
1. POST /api/v1/chat → orchestrator.ProcessMessage(ctx, sessionID, "kubectl apply prod/...")
2. classifyIntent → IntentDeploy
3. containsSensitiveContent → true   (匹配 sensitive_patterns)
4. suspendForApproval (orchestrator.go:640)
   │
   │ o.temporalClient != nil → suspendForApprovalTemporal (temporal_bridge.go:37)
   ▼
5. temporalHITLAdapter.StartHITLWorkflow:
     workflowID = "hitl-<taskID>"
     cli.ExecuteWorkflow(opts, AgentTaskWorkflow, AgentTaskInput{...})
   ▼
6. AgentTaskWorkflow 开始执行 (Temporal Server 调度到任意 Worker):
     ① ParseIntentActivity → IntentDeploy
        · Activity-level retry 3 次，每次 StartToClose 5min
     ② SecurityCheckActivity → RequiresApproval=true, RiskLevel="critical"
     ③ 进入 Selector 等待:
        approvalCh = workflow.GetSignalChannel(ctx, "approval-signal")
        timerFuture = workflow.NewTimer(ctx, 30*time.Minute)
        selector.Select(ctx)   ★ 0 worker 资源挂起
   ▼
7. 同步返回前端 (suspendForApprovalTemporal):
     ChatResponse{
       State: Suspended,
       Message: "⚠️ This operation requires approval...",
       Approval: &ApprovalRequest{TaskID, RiskLevel="high", Action, Details},
     }
   ▼
8. 用户在前端点 "Approve":
     POST /api/v1/chat/approval { task_id, approved: true, comment: "" }
   ▼
9. handler → orchestrator.HandleApproval (orchestrator.go:715)
     o.temporalClient != nil → HandleApprovalTemporal (temporal_bridge.go:67)
   ▼
10. temporalHITLAdapter.SignalApproval:
      cli.SignalWorkflow("hitl-<taskID>", "", "approval-signal",
                         {Approved:true, Comment:""})
   ▼
11. AgentTaskWorkflow Selector 收到信号:
      · cancelTimer() 立即取消 30min Timer
      · approved = true
      · 跳出 Selector
   ▼
12. ExecuteTaskActivity:
      execCtx = ContextWithSkipHITL(ctx)
      resp = orch.ProcessMessage(execCtx, sessionID, userMessage)
      → 这次不再触发 HITL（skipHITL=true 短路）
      → 走 reactLoop，调 sandbox 跑 kubectl apply
      → return ExecutionResult{Output: resp.Message}
   ▼
13. Workflow return AgentTaskOutput{
      State: Completed,
      Message: <execution output>,
    }
   ▼
14. (可选) 前端 query 任务状态 or 接 SSE 拿结果
```

### 2.5.2 拒绝路径

```
8'. 用户点 "Reject":
     POST /api/v1/chat/approval { task_id, approved: false, comment: "Wrong env" }
10'. SignalApproval(approved=false, comment="Wrong env")
11'. Selector 接收 ApprovalResponse{Approved:false}
12'. approved == false → return Cancelled output
13'. Workflow return AgentTaskOutput{
       State: Cancelled,
       Message: "Operation cancelled: approval denied or timed out",
     }
```

### 2.5.3 超时路径

```
8''. 30 分钟无响应:
11''. timerFuture 触发 → approved=false (默认值) → Cancelled
     · metrics.HITLApprovalTotal{outcome=timeout}.Inc()
13''. 输出同上 Cancelled
```

### 2.5.4 Worker 重启路径（Temporal 的杀手锏）

```
A. Workflow 在 Selector.Select 处挂起
B. Worker Pod 被 K8s 重启或缩容
C. Temporal Server 把 history 重新分配给另一个 Worker
D. 新 Worker poll 到这个 workflow:
   · 从 Event History replay 所有已发生事件
   · ParseIntent 已 done → 直接拿 history 里的结果（不重跑）
   · SecurityCheck 已 done → 直接拿结果
   · Selector 还没 done → 继续等
E. 信号到达 → 正常继续走 ExecuteTaskActivity
```

**关键**：步骤 D 中"已完成的 activity 不会重跑"——history 里记着输出，replay 时直接复用。这就是 Temporal 与"普通 retry library" 的根本区别。

---

## 3. 核心数据模型

### 3.1 Workflow IO（workflows.go:54-66）

```go
type AgentTaskInput struct {
    SessionID   string `json:"session_id"`
    UserMessage string `json:"user_message"`
    TaskID      string `json:"task_id"`
}

type AgentTaskOutput struct {
    TaskID  string           `json:"task_id"`
    State   models.TaskState `json:"state"`     // Completed / Failed / Cancelled
    Message string           `json:"message"`
}

const ApprovalSignal = "approval-signal"
```

Signal payload 用 `models.ApprovalResponse{Approved bool, Comment string}`（workflows.go:162-179）。

### 3.2 Activity IO 类型（workflows.go:223-256）

```go
type IntentResult struct {
    Intent models.TaskIntent  // deploy / conversation / ...
}

type SecurityCheckInput struct {
    TaskID, UserMessage string
    Intent              models.TaskIntent
}

type SecurityCheckResult struct {
    RequiresApproval bool
    RiskLevel        string  // "low" / "high" / "critical"
    Reason           string
}

type ExecuteTaskInput struct {
    TaskID, SessionID, UserMessage string
    Intent                          models.TaskIntent
}

type ExecutionResult struct {
    Output   string
    ExitCode int   // 占位字段，目前 ExecuteTaskActivity 不设
}
```

### 3.3 `Activities` 结构（activities.go:47-78）

```go
type Activities struct {
    Orchestrator *orchestrator.Orchestrator   // ★ 反向依赖：Activity 调回 orch
    SecurityCfg  *config.SecurityConfig
    LLMClient    LLMClient                    // Optional：LLM intent 分类
    Logger       *zap.Logger
    sensitivePatterns []*regexp.Regexp        // 预编译，避免每次重新 Compile
}
```

`NewActivities` 把 `secCfg.SensitivePatterns` 全部 `regexp.Compile("(?i)" + pattern)` 一次预编译；非法 pattern → log warning + skip。

### 3.4 `TemporalClient` 接口（temporal_bridge.go:26-29）

```go
type TemporalClient interface {
    StartHITLWorkflow(ctx, taskID, sessionID, userMessage string) (workflowID string, err error)
    SignalApproval(ctx, workflowID string, approved bool, comment string) error
}
```

只暴露 2 个方法是刻意的：**让 orchestrator 不依赖 Temporal SDK 类型**。`temporalHITLAdapter`（cmd/agent/temporal_adapter.go）才知道 `temporalclient.Client`。这样：

- Orchestrator 包不 import `go.temporal.io/sdk`；
- 测试时可以注入 fake adapter；
- 未来换 workflow runtime（比如 Cadence/Argo）只改 adapter。

---

## 4. `startTemporalWorker`：进程启动接线（main.go:740-791）

```go
func startTemporalWorker(cfg, secCfg, orch, logger) (Client, Worker) {
    ns := cfg.Namespace; if "" → DefaultNamespace
    queue := cfg.TaskQueue; if "" → "agent-tasks"
    
    cli, err := temporalclient.Dial({HostPort, Namespace})
    if err: return nil, nil   ← 优雅降级
    
    w := temporalworker.New(cli, queue, {})
    w.RegisterWorkflow(AgentTaskWorkflow)
    activities := temporalpkg.NewActivities(orch, secCfg, logger)
    w.RegisterActivity(activities)   ← 一次性注册所有方法
    
    if err := w.Start(); err != nil:
        cli.Close()
        return nil, nil
    
    return cli, w
}
```

### 4.1 关键设计

| 决策 | 动机 |
|---|---|
| Dial 失败 → 返回 (nil, nil) 而非 fatal | Temporal 是可选依赖；HTTP 服务必须保活 |
| `w.Start()` 非阻塞 | 让 main 继续起 HTTP server，worker 在后台 poll task queue |
| `defer temporalWorker.Stop()` (main.go:700) | Graceful shutdown：worker.Stop() 默认 drain 1 min 等 in-flight activity 完成 |
| `RegisterActivity(activities)` 整个 struct | Temporal SDK 反射所有 public 方法注册为 activity；不需要逐个手注 |

### 4.2 注册的 activity 名

通过反射，注册的 activity 名是 `*Activities`'s exported method 名：

- `ParseIntentActivity`
- `SecurityCheckActivity`
- `ExecuteTaskActivity`

Workflow 里通过 `workflow.ExecuteActivity(ctx, ParseIntentActivity, ...)` 引用——`ParseIntentActivity` 这个**值**（不是 *Activities 的方法）是怎么解析的？

```go
// workflows.go:201-207
var (
    ParseIntentActivity   = (*Activities).ParseIntentActivity
    SecurityCheckActivity = (*Activities).SecurityCheckActivity
    ExecuteTaskActivity   = (*Activities).ExecuteTaskActivity
)
```

这是 Go method value 语法——把 method 写成 first-class 函数引用。Temporal SDK 调用时通过函数名（反射 `runtime.FuncForPC`）匹配已注册的方法。

---

## 5. `AgentTaskWorkflow`：主工作流（workflows.go:98-221）

四阶段流水线：

```
①  ParseIntentActivity     ─┐
②  SecurityCheckActivity   ─┤  每个：StartToClose=5min, Retry=3x, Backoff=2.0
③  (if RequiresApproval)    │
     Selector { signal, timer }.Select(ctx)
④  ExecuteTaskActivity     ─┘
```

### 5.1 Activity 选项（workflows.go:106-115）

```go
activityOpts := workflow.ActivityOptions{
    StartToCloseTimeout: 5 * time.Minute,
    RetryPolicy: &temporal.RetryPolicy{
        InitialInterval:    time.Second,
        BackoffCoefficient: 2.0,
        MaximumInterval:    time.Minute,
        MaximumAttempts:    3,
    },
}
ctx = workflow.WithActivityOptions(ctx, activityOpts)
```

**所有三个 activity 共用同一个 RetryPolicy** —— 简单粗暴；理想情况下 `ExecuteTaskActivity` 应该独立配置（它可能跑 10 分钟，5min StartToClose 太紧）。属 P1。

### 5.2 步骤一：ParseIntentActivity

```go
err := workflow.ExecuteActivity(ctx, ParseIntentActivity, input).Get(ctx, &intentResult)
```

`Get(ctx, &out)` 是 Temporal 的"同步等待 future"，但**这里的"同步"是 workflow 层概念**——underlying yield + resume 由 Server 管。

失败处理：直接 return `AgentTaskOutput{State: Failed, Message: "Intent parsing failed: ..."}`，**不 return err**。为什么？让 workflow **正常结束**（写 history、入库）而不是抛 workflow-level 错误：

- workflow error 会触发 `workflow.RetryPolicy`（如果有）重试 workflow 本体；
- 业务错误（intent 解析失败）已经经过 activity retry 3 次了，再退一层 workflow retry 没意义；
- 用 `State: Failed` 告诉前端"你的任务结束了，结局是失败"。

### 5.3 步骤二：SecurityCheckActivity

不区分 `nil, err` 返回：失败的话 workflow 直接 `return nil, err` 让 Temporal Server 记录 workflow 失败（因为这是**系统**错误，不是业务错误）。

### 5.4 步骤三：HITL Selector（workflows.go:153-198）

```go
if securityResult.RequiresApproval {
    approvalCh := workflow.GetSignalChannel(ctx, ApprovalSignal)
    var approval models.ApprovalResponse
    
    timerCtx, cancelTimer := workflow.WithCancel(ctx)
    timerFuture := workflow.NewTimer(timerCtx, 30*time.Minute)
    
    selector := workflow.NewSelector(ctx)
    var approved bool
    
    selector.AddReceive(approvalCh, func(ch, more) {
        ch.Receive(ctx, &approval)
        approved = approval.Approved
        cancelTimer()                        // ★ 收到信号立即取消 timer
    })
    selector.AddFuture(timerFuture, func(f) {
        approved = false                     // 超时 = 拒绝
        logger.Warn("Approval timed out")
    })
    
    selector.Select(ctx)                     // ★ 挂起，0 worker 资源
    
    if !approved {
        return Cancelled output
    }
}
```

#### 关键点

- **`workflow.GetSignalChannel`** 返回的"通道"语义类似 Go chan，但由 Temporal 持久化保证 exactly-once 交付；
- **`workflow.WithCancel + NewTimer + cancelTimer()`** 配对，是为了"收到信号后立即停止 timer 计费"——Server 不会在 timer 触发后还推一个 expire 事件；
- **`Selector` 不是 Go select**——它支持 future、channel、orphan future 等多种异步源；
- **`Select(ctx)` 是一次性 dispatch**——一旦命中一个 handler，Selector 就完成，后续 Select 调用要重新构造。

### 5.5 步骤四：ExecuteTaskActivity（活儿在这里干）

```go
execResult: ExecutionResult
err := workflow.ExecuteActivity(ctx, ExecuteTaskActivity, ExecuteTaskInput{...}).Get(ctx, &execResult)
```

Activity 内部（activities.go:181-199）：

```go
execCtx := orchestrator.ContextWithSkipHITL(ctx)
resp, err := a.Orchestrator.ProcessMessage(execCtx, input.SessionID, input.UserMessage)
return &ExecutionResult{Output: resp.Message}, nil
```

**调回 orchestrator** —— 这是设计的精髓：Temporal 不取代 reactLoop，**只是把"挂起等审批"那一段套上持久化**。一旦审批过了，executor 还是走 reactLoop（complete with tools.Registry 三级分派、speculative cache、context compaction、etc）。

---

## 6. Activities 内幕

### 6.1 `ParseIntentActivity`（activities.go:81-109）

```
ParseIntentActivity:
  if a.LLMClient != nil:
    intent = classifyIntentWithLLM(message)
    if err: log warn, fallback to keyword
  else:
    intent = classifyIntent(message)
  return IntentResult{Intent: intent}
```

`classifyIntentWithLLM` 用 `MaxTokens=10` 强制极短输出：

```
"Classify the user's intent as either "deploy" or "conversation".
User message: {msg}
Respond with ONLY one word: "deploy" or "conversation"."
```

`classifyIntent`（keyword fallback）只识别 9 个中英文 deploy 关键词：deploy, 部署, 发布, 上线, rollout, release to prod, push to production, ship it, go live。**其他全部归 conversation** —— 这是粗分类，细 intent（code-edit / debugging）由 orchestrator 自己再分。

> ⚠️ **LLM 失败 fallback 是 Activity 内决策，不被 Temporal Retry 看到**。如果你想 "LLM 失败时 Activity 也算失败让 Temporal 重试"，需要改成 `return nil, err`。当前实现选择"无论 LLM 怎样都返回 intent"是合理的（intent 分类不应该 block workflow），但要在 metric 里区分。

### 6.2 `SecurityCheckActivity`（activities.go:153-179）

```
SecurityCheckActivity:
  result = {RequiresApproval: false, RiskLevel: "low"}
  for re in a.sensitivePatterns:
    if re.MatchString(userMessage):
      result.RequiresApproval = true
      result.RiskLevel = "high"
      result.Reason = "Matched sensitive pattern: " + re.String()
      break
  if input.Intent == IntentDeploy:
    result.RequiresApproval = true
    result.RiskLevel = "critical"
    result.Reason = "Deployment operation requires approval"
  return result
```

**正则失败、Intent=Deploy 时不并存比较**——后者覆盖前者的 reason，但保留 `RequiresApproval=true`。要看具体哪条匹配只能看 reason 字段。

> ⚠️ `doc.go:43-53` 的 `workflow.Await` 代码示例**与真实实现不符**——真实实现用 `Selector`，而不是 `Await`。Await 也是 Temporal SDK 提供的 API（更简洁），但当前代码选了 Selector（更通用，支持多路监听）。`doc.go` 那个 snippet 是 RFC 残留，不要照抄。

### 6.3 `ExecuteTaskActivity`（activities.go:181-199）

```go
execCtx := orchestrator.ContextWithSkipHITL(ctx)
resp, err := a.Orchestrator.ProcessMessage(execCtx, input.SessionID, input.UserMessage)
if err: return nil, err
return &ExecutionResult{Output: resp.Message}, nil
```

**关键：ctx 来自 Temporal Activity Context**——不是 workflow context。Activity ctx 可以正常用 `time.Now()` / 任意 IO，因为它就是普通 Go 调用，不参与 replay。

但要注意：`ProcessMessage` 内部如果**长跑 > 5 分钟**（StartToCloseTimeout）会被 Temporal 强制中断（cancel ctx）。orchestrator 是否响应 ctx cancel？详见 `09_orchestrator §10`——**部分响应**（in-flight tool 不响应）。属 P0 风险。

---

## 7. `temporal_bridge.go`：Orchestrator 端集成

### 7.1 `ContextWithSkipHITL`（L11-23）

唯一的目的：防止 `ExecuteTaskActivity → ProcessMessage → containsSensitiveContent → suspendForApproval` 死循环。

```go
type ctxKeySkipHITL struct{}
func ContextWithSkipHITL(ctx) context.Context { ... }
func skipHITL(ctx) bool { ... }
```

`orchestrator.go:834` 处的 short-circuit：
```go
if !skipHITL(ctx) && (containsSensitiveContent || intent == IntentDeploy) {
    // → suspendForApproval
}
```

### 7.2 `suspendForApprovalTemporal`（L37-64）

```
suspendForApprovalTemporal(task):
  wfID, err := temporalClient.StartHITLWorkflow(task.ID, sessionID, userInput)
  if err:
    log warn "fallback to in-process"
    return suspendForApprovalInProcess(ctx, task)    ★ graceful degradation
  
  return ChatResponse{
    State: Suspended,
    Approval: {TaskID, SessionID, Action, RiskLevel:"high", Details, RequestedAt},
    Message: "⚠️ ...",
  }
```

**关键**：Temporal 不可达不影响主流程——退到 in-process HITL（30 min channel 等待），用户体验降级但能用。

### 7.3 `HandleApprovalTemporal`（L67-85）

```
HandleApprovalTemporal(resp):
  wfID := "hitl-" + resp.TaskID                    ★ ID 约定，必须和 StartHITL 一致
  temporalClient.SignalApproval(wfID, approved, comment)
  return ChatResponse{
    State: approved ? Executing : Cancelled,
    Message: approved ? "Operation approved..." : "Operation cancelled by user.",
  }
```

> ⚠️ **ID 约定耦合**：`"hitl-" + taskID` 这个前缀在 `temporal_adapter.go:18` 和 `temporal_bridge.go:68` 各写一遍。改一份不改另一份 → SignalWorkflow 找不到 workflow。建议抽常量。属 P2。

---

## 8. `temporalHITLAdapter`：SDK ↔ orchestrator 胶水（temporal_adapter.go）

短短 46 行做两件事：

```go
func (a *temporalHITLAdapter) StartHITLWorkflow(ctx, taskID, sessionID, userMessage) (string, error) {
    workflowID := "hitl-" + taskID
    opts := temporalclient.StartWorkflowOptions{
        ID:        workflowID,        // 用 taskID 派生，便于 SignalWorkflow 找到
        TaskQueue: a.queue,           // "agent-tasks" 默认
    }
    input := temporalpkg.AgentTaskInput{SessionID, UserMessage, TaskID}
    we, err := a.client.ExecuteWorkflow(ctx, opts, AgentTaskWorkflow, input)
    if err: return "", fmt.Errorf("start HITL workflow: %w", err)
    return we.GetID(), nil
}

func (a *temporalHITLAdapter) SignalApproval(ctx, workflowID, approved, comment) error {
    return a.client.SignalWorkflow(ctx, workflowID, "" /*runID*/, "approval-signal", payload)
}
```

`SignalWorkflow` 的第 3 个参数（runID）传 `""` —— Temporal 自动找最新一次 run。这意味着如果同一个 workflowID 重复 ExecuteWorkflow（不该发生但理论上可能），信号会发到最新那个。

---

## 9. Worker 生命周期与配置

### 9.1 `TemporalConfig`（config/types）

```yaml
temporal:
  host: "localhost:7233"        # 不填 → 不启 worker
  namespace: ""                 # 默认 "default"
  task_queue: ""                # 默认 "agent-tasks"
```

### 9.2 Worker 启动顺序（main.go）

```
1. orch (Orchestrator) 已构造
2. apiServer 已构造
3. startTemporalWorker → 拿到 cli, w
4. orch.SetTemporalClient(adapter)    ← 注入回 orch
5. httpServer 启动
6. ... (运行期)
7. SIGTERM
8. httpServer.Shutdown(ShutdownTimeout)
9. defer rdb.Close()
10. defer temporalWorker.Stop()       ← 最后才停 worker, drain 1 min 默认
11. defer temporalCli.Close()
```

**关键顺序**：先停 HTTP 防新请求 → 再停 worker drain 在跑的 activity → 最后关闭 client。这保证 graceful shutdown 中已审批通过的任务能跑完。

---

## 10. P0 / P1 / P2 风险清单

| 级别 | 项 | 位置 | 说明 |
|---|---|---|---|
| **P0** | `StartToClose=5min` 对 `ExecuteTaskActivity` 太紧 | workflows.go:107 | ExecuteTaskActivity 内部走 reactLoop，可能 > 5 分钟；超时后 ctx cancel，orchestrator 不全响应 → 数据可能半执行 |
| **P0** | `"hitl-" + taskID` 前缀双写 | temporal_adapter.go:18 + temporal_bridge.go:68 | ID 约定耦合，改一份不改另一份 → SignalWorkflow 失败 |
| **P0** | Activity 内 LLM 调用无 timeout 控制 | activities.go:111-131 | classifyIntentWithLLM 没用 `ctx` 的 deadline；ctx 来自 activity 由 StartToClose 限制 5min，但 LLM client 应该 detect deadline 主动 cancel |
| **P1** | `RegisterActivity(activities)` 把私有 helper 也注册 | main.go:779 | Temporal SDK 通过反射注册所有 exported method；当前 `Activities` 只有 3 个 exported method，但加新方法时要小心 |
| **P1** | 三个 Activity 共用同一个 RetryPolicy | workflows.go:108 | ExecuteTask 失败重试 3 次会变成 3 次 sandbox 命令重跑，可能产生副作用（创建多个文件、发多次邮件） |
| **P1** | Signal payload `struct{Approved bool, Comment string}` 与 `models.ApprovalResponse` 字段重叠但不复用 | temporal_adapter.go:38 | 改 ApprovalResponse 字段时容易忘记同步 |
| **P1** | Workflow 失败后没有死信处理 | workflows.go (全文) | NonDeterminismError 会让 workflow 进入 failed state；当前没有自动通知用户的路径 |
| **P1** | `cancelTimer()` 在 Selector handler 调，但若 Selector 没命中 approval 分支（先命中 timer），cancelTimer 不会被调用——其实 Timer 已触发，调不调 cancel 等价。但代码读起来容易让人误解 | workflows.go:178 | 注释一下 |
| **P2** | `doc.go` 的 `workflow.Await` 代码示例与真实 `Selector` 实现不符 | doc.go:43 | 容易误导阅读者 |
| **P2** | Worker.Stop 的默认 1 min drain 不可调 | main.go:700 | 长任务还在跑 worker stop 会强制中断 |
| **P2** | `cfg.Temporal.Namespace` 默认 `temporalclient.DefaultNamespace`（"default"） | main.go:752-755 | 多租户场景需显式配 |

---

## 11. 设计权衡与设计哲学

| 抉择 | 动机 |
|---|---|
| Temporal 是**可选**依赖，dial 失败 fall through 进程内 | HTTP 服务必须保活；Temporal 价值是"长寿命挂起"，短任务用进程内通道一样 |
| Workflow 函数严格确定性（用 Temporal SDK API） | replay 一致性是 Temporal 的根基，违反 → workflow 直接 fail |
| Activity 失败 3 次重试，BackoffCoefficient 2.0 | 覆盖瞬时失败但不至于雪崩 |
| `TemporalClient` 接口只 2 个方法 | Orchestrator 包不 import Temporal SDK，便于换 runtime |
| `temporalHITLAdapter` 在 `cmd/agent/`（不在 internal/temporal） | 让 internal 包不依赖 client；adapter 是组装层 |
| `ContextWithSkipHITL` 用 ctx value 而非函数参数 | 避开侵入式 API 改动；signal 传播自动跟随 ctx |
| `"hitl-" + taskID` 作 workflowID | 便于按 taskID 反查；taskID 已是 UUID 唯一 |
| Activity StartToClose=5min，所有 activity 共用 | 简单；大任务的应对策略是 SubWorkflow 而非长 Activity |
| Selector 而非 Await | 支持多路监听（signal + timer），后续可扩展（如 cancel signal） |
| Signal payload 用匿名 struct 而非 `models.ApprovalResponse` | 减少 cmd/agent 对 internal/models 的耦合（但带来重复字段问题，见 P1） |
| ExecuteTaskActivity 调回 orchestrator | 不让 Temporal 成为"第二套 reactLoop"——一致性优先 |
| 30 分钟超时硬编码 | 业务上"够长"但"不无限"；动态配会带来 workflow 版本兼容噩梦 |
| Worker registered 在 main.go 进程内，不是独立部署 | 简化运维：一个二进制，一份 deploy；Temporal 不强制 worker 独立部署 |

---

## 12. 后续演进

- [ ] **细分 Activity 超时**：`ExecuteTaskActivity` 应该有独立的 StartToClose（如 30min）和 retry 策略（如 1 次，避免副作用）
- [ ] **Activity 内 LLM 调用响应 ctx.Deadline**：让 `classifyIntentWithLLM` 在 ctx deadline 触发前主动 cancel
- [ ] **Cancel Signal**：增加 `cancel-signal`，让用户能在审批前主动撤销任务
- [ ] **Query Handler**：暴露 `workflow.SetQueryHandler` 让前端轮询 task 进度而非等 SSE
- [ ] **Workflow Versioning**：用 `workflow.GetVersion` 管理 workflow 代码变更，旧 workflow replay 走旧路径
- [ ] **Child Workflow**：把 `ExecuteTaskActivity` 改成 child workflow，让大任务也有 replay 能力（当前 Activity 内 panic 会丢上下文）
- [ ] **死信队列**：Workflow 进 failed state 后自动通知用户 + 写 audit log
- [ ] **Workflow ID 常量化**：抽 `func HITLWorkflowID(taskID) string` 消除 `"hitl-" + taskID` 双写
- [ ] **`ApprovalSignal` payload 类型抽 `models` 包**：消除 `cmd/agent/temporal_adapter.go` 与 `internal/temporal/workflows.go` 之间的字段重复
- [ ] **多 Worker 部署**：Worker 拆出 K8s Deployment 独立 scale；当前与 HTTP 服务共部署
- [ ] **`doc.go` 修复**：把 `workflow.Await` 示例改成 Selector 真实实现 / 或干脆删掉示例代码

---

## 13. 设计教训

1. **Temporal 不取代核心循环**——`ExecuteTaskActivity` 调回 `orchestrator.ProcessMessage` 是关键。如果 Temporal Workflow 自己实现"调 LLM + 调工具 + 看 RAG"的全流程，会变成第二套 reactLoop，维护成本翻倍且行为漂移。正确做法：Temporal 只负责"挂起 + 持久化 + 恢复"，业务逻辑留在 orchestrator。

2. **Workflow 代码可读性 > 简洁**——`Selector + WithCancel + NewTimer` 比 `workflow.Await` 啰嗦，但能扩展（加 cancel signal、加 progress query）。三方提交者第一次读 workflow 代码必须能 follow 控制流。

3. **"Activity 共用 retry policy"是个坑**——`SecurityCheckActivity` 是纯函数重试无害；`ExecuteTaskActivity` 重试会重跑 sandbox 命令产生副作用。一旦发现重试导致脏数据，再加细分 retry 已经晚了。**新加 Activity 必默认 retry=1 + 显式 review 是否安全**。

4. **`ContextWithSkipHITL` 是必须的，但易忘记**——任何"workflow → orchestrator"的回调都要 skipHITL，否则递归。新功能加这类回调时**必须**在 ctx 上注入标记。

5. **Graceful degradation > 强制依赖**——Temporal 不可达时回退进程内通道，业务功能仍能用。这种"主路径 + 降级路径"双备份在企业部署里非常有价值——降级路径的存在让"是否启用 Temporal"成为零风险决策。

6. **Workflow ID 是契约**——`"hitl-" + taskID` 这种约定**必须有单一来源**。代码两处各写一遍是技术债，建议立即收敛。一旦多人维护，这种约定会成为最容易漂移的 bug 源头。

7. **`doc.go` 是 RFC 残留**——文档与代码不同步是 Temporal 集成的常见症状（因为 Temporal SDK API 多、写法多）。docstring 用伪代码而非真实代码会随时间漂移；要么完全用真实代码片段，要么干脆指向 `workflows.go` 行号让读者去看。

---

下一篇：[`12_hitl.md`](12_hitl.md) —— HITL 流程总章：把 in-process 通道 + Temporal Workflow 两条 HITL 路径放在一起对比，讲清"什么时候降级、什么时候必须 Temporal"。
