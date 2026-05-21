// Package temporal —— 有状态工作流编排（Temporal + HITL）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 问题：Agent 的长任务与人工审批场景
//    · 部署任务可能等几分钟到几小时的人工批准
//    · 代码重构→测试→提交 PR 链路 = 10+ 步骤 + 失败重试
//    · 外部集成（GitHub PR → Review → Merge）中间状态不确定
//    若用内存态 goroutine + channel：
//      · Agent 重启 → 状态全丢
//      · 挂起期间占用 goroutine 和 FD → 成本高
//      · 无重试策略、无可视化、无审计
//    必须把状态持久化到"工作流引擎"。
//
// 2. Temporal 是什么？
//    Uber 开源的分布式工作流引擎：
//      · 把 Workflow 函数当"可持久化 goroutine"
//      · 每次 workflow.ExecuteActivity / workflow.Await 会写入 Event History
//      · Worker 崩溃后，在另一台机器上 replay 历史恢复到断点
//      · 挂起等 Signal 时 Worker 资源占用 = 0（由 Server 维持状态）
//
// 3. 三大核心概念
//
//     Workflow   —— **确定性**业务编排代码（禁用 time.Now, rand, http.Get）
//     Activity   —— 真正产生副作用的函数（调 LLM、写文件、kubectl）
//     Signal     —— 外部向运行中 Workflow 发送异步消息（HITL 就靠它）
//     Query      —— 只读询问 Workflow 当前状态（用于前端显示进度）
//
// 4. HITL（Human-in-the-Loop）的优雅实现
//
//     ┌──────────────────────────────┐
//     │ AgentTaskWorkflow             │
//     ├──────────────────────────────┤
//     │ (1) ParseIntentActivity       │   ← LLM 拆解用户意图
//     │ (2) SecurityCheckActivity     │   ← 命中敏感规则 → RequiresApproval
//     │ (3) if RequiresApproval {     │
//     │       selector.AddReceive(    │
//     │         approvalCh, ...)      │   ← 等用户点"同意"
//     │       selector.AddFuture(     │
//     │         NewTimer(30min))      │   ← 兜底超时
//     │       selector.Select(ctx)    │   ← 挂起期间 Worker 0 占用
//     │     }                          │
//     │ (4) ExecuteTaskActivity       │   ← 真正执行
//     └──────────────────────────────┘
//
//    关键点：**workflow.NewTimer / GetSignalChannel 不消耗 goroutine**，
//    挂起状态由 Temporal Server 的数据库承载。
//
// 5. 为什么 Workflow 必须确定性？
//    · replay 机制要求：给同样的 Event History，代码执行路径必须完全一致
//    · 禁用的非确定源：time.Now() / rand / http.Get / map 迭代顺序 /
//                    原生 channel / goroutine
//    · 替代：workflow.Now / workflow.SideEffect / workflow.ExecuteActivity /
//            workflow.GetSignalChannel
//    · 有副作用的逻辑全部放到 Activity 里（Activity 可以非确定性）。
//
// 6. 重试策略
//    每个 Activity 默认带 RetryPolicy：
//      InitialInterval    : 1s
//      BackoffCoefficient : 2.0
//      MaximumInterval    : 60s
//      MaximumAttempts    : 3
//      NonRetryableErrorTypes : [ValidationError, PermissionDeniedError]
//    业务方可以覆盖：例如 deploy 失败允许重试 10 次。
//
// 7. 信号路由（Signal Routing）
//    前端点"批准" → POST /tasks/:id/approve
//      ↓
//    API handler 调 temporal.Client.SignalWorkflow(
//      workflowID="task-xxx",
//      signalName="approval-signal",
//      arg=ApprovalResponse{Approved:true}
//    )
//      ↓
//    Temporal Server 把 Signal 写入 WorkflowExecution
//      ↓
//    任意 Worker 被调度 replay 到 selector.Select 处
//      ↓
//    approvalCh.Receive 读出 signal，继续执行
//
// 8. 可观测性
//    · temporal-web UI 显示每个 Workflow 的 event timeline
//    · 每个 Activity 的输入/输出/重试次数/耗时
//    · 失败任务一键重跑（ResetWorkflow）
//
// 9. 与其他模块的关系
//    · orchestrator 判断"是否需要长任务"→ 启动本包 workflow
//    · skill.Registry Invoke 高危 skill 时返回 ErrNeedApproval → 触发 HITL
//    · api /tasks/:id/approve 是 Signal 的入口
//
// =============================================================================
//
// 10. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                        temporal package                               │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Client (wrapper on go.temporal.io/sdk/client)                   │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  cli       tclient.Client                                       │  │
//   │  │  taskQueue string                                               │  │
//   │  │                                                                 │  │
//   │  │  + StartAgentTask(ctx, req) (runID, workflowID, error)          │  │
//   │  │  + SignalApproval(id, payload) error                            │  │
//   │  │  + QueryStatus(id) (*TaskStatus, error)                         │  │
//   │  │  + CancelTask(id, reason) error                                 │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                        │                                              │
//   │                        ▼                                              │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Worker                                                          │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  RegisterWorkflow(AgentTaskWorkflow)                            │  │
//   │  │  RegisterActivity(ParseIntent / SecurityCheck / Execute / ...) │  │
//   │  │  Run() blocks until ctx.Done                                   │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                                                                       │
//   │  Workflows (determinism required):     Activities (side effects OK): │
//   │  ────────────────────────────────      ───────────────────────────── │
//   │  · AgentTaskWorkflow                   · ParseIntentActivity         │
//   │      signals: approval-signal          · SecurityCheckActivity       │
//   │      queries: "status"                 · ExecuteTaskActivity         │
//   │                                        · SummarizeActivity           │
//   │                                                                       │
//   │  Backing store: Temporal Server (cassandra / postgres) +              │
//   │                 event history (replay-able log)                       │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 11. AgentTaskWorkflow 流程图
//
//        user request                   api            Temporal
//              │                          │                │
//              │  POST /agent/task        │                │
//              │─────────────────────────▶│ StartWorkflow  │
//              │                          │───────────────▶│  persist history [started]
//              │                          │◀─ workflowID ──│
//              │◀───── 202 Accepted ──────│                │
//              │                          │                │
//    (workflow loop, durable):           │
//     ┌────────────────────────────────────────────────┐
//     │ 1. ParseIntentActivity                          │
//     │       └─▶ LLM classify intent, risk             │
//     │                                                  │
//     │ 2. SecurityCheckActivity                        │
//     │       └─▶ match deny rules, high-risk tools     │
//     │                                                  │
//     │ 3. if RequiresApproval:                         │
//     │       selector.AddReceive(approvalCh)           │
//     │       selector.AddFuture(NewTimer(30m))         │
//     │       selector.Select(ctx)                      │
//     │        ╔═══════════════════════════════════════╗│
//     │        ║  WORKER PARKED                        ║│  ← 0 goroutine used
//     │        ║  (state persisted in Temporal DB)     ║│
//     │        ╚═══════════════════════════════════════╝│
//     │                                                  │
//     │ 4. ExecuteTaskActivity(retryPolicy)             │
//     │       └─▶ call orchestrator/skill/sandbox       │
//     │                                                  │
//     │ 5. return TaskResult{status, output, metrics}   │
//     └────────────────────────────────────────────────┘
//
// 12. HITL（Human-in-the-Loop）信号路由时序
//
//     user UI          api                temporal.Client    Temporal Server     Worker
//       │ Approve       │                      │                   │                │
//       ├──────────────▶│                      │                   │                │
//       │               │ SignalWorkflow(id,   │                   │                │
//       │               │ "approval-signal",   │                   │                │
//       │               │ {Approved:true})     │                   │                │
//       │               │─────────────────────▶│──── signal ──────▶│                │
//       │               │                      │                   │                │
//       │               │                      │                   │ (dispatch task)│
//       │               │                      │                   │───────────────▶│
//       │               │                      │                   │                │ replay history
//       │               │                      │                   │                │ hit selector.Select
//       │               │                      │                   │                │ approvalCh.Receive → resume
//       │               │                      │                   │                │ run ExecuteActivity
//       │◀──────── 200 OK ─────────────────────│                   │                │
//
// 13. Activity 重试策略（RetryPolicy）
//
//     ┌────────────────────────────────────────────────────────┐
//     │ Attempt 1  ──fail──▶ wait 1s  (+jitter)                │
//     │ Attempt 2  ──fail──▶ wait 2s                            │
//     │ Attempt 3  ──fail──▶ give up (return error to workflow)│
//     │                                                         │
//     │ NonRetryableErrorTypes: [ValidationError, PermDenied]  │
//     │ 业务方可覆盖：e.g. deploy → MaximumAttempts=10          │
//     └────────────────────────────────────────────────────────┘
//
// 14. 为什么 Workflow 必须确定性？
//
//     replay 的本质：给定同一 Event History，重放代码必须走出相同的路径。
//
//     禁用 (non-deterministic)       正确替代 (deterministic via SDK)
//     ─────────────────────          ──────────────────────────────
//     time.Now()                →   workflow.Now(ctx)
//     rand.*                    →   workflow.SideEffect / workflow.NewRandom
//     http.Get / db.Query       →   workflow.ExecuteActivity(...)
//     map iteration             →   sort.Slice(keys) 后遍历
//     goroutine / chan          →   workflow.Go / workflow.NewChannel
//     time.Sleep                →   workflow.Sleep
//
// =============================================================================
//
// 15. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] Workflow 确定性的血泪教训 —— 一行 time.Now() 引发的灾难
//
//   某团队写了个"自动部署 Workflow"，5 步骤：拉代码 → 编译 → 测试 → 部署 → 通知。
//   原始代码：
//
//     func DeployWorkflow(ctx workflow.Context, req DeployReq) error {
//         startTime := time.Now()                      // ❌ 非确定性
//         workflow.ExecuteActivity(ctx, PullCode, req.Repo).Get(ctx, nil)
//         workflow.ExecuteActivity(ctx, Build, req).Get(ctx, nil)
//
//         if time.Since(startTime) > 10*time.Minute { // ❌ 基于 time.Now
//             return errors.New("build too slow")
//         }
//
//         workflow.ExecuteActivity(ctx, Deploy, req).Get(ctx, nil)
//         return nil
//     }
//
//   正常情况下这个 workflow 工作良好。某天 worker 在 Deploy 步骤 OOM 重启。
//   Temporal 开始 replay：
//
//     第一次执行（T0）：
//       startTime = T0
//       PullCode done at T0+1min
//       Build done at T0+3min
//       time.Since(T0) = 3min ✓
//       开始 Deploy...worker crash
//
//     Replay（T1，worker 重启后）：
//       startTime = T1   (←! T1 != T0，与历史不一致)
//       PullCode done (from history, instant)
//       Build done (from history, instant)
//       time.Since(T1) = 0min ✓
//       继续 Deploy...
//
//   看起来结果一样？不！Temporal 的 determinism checker 发现：
//     · 第一次产生 command: `time.Since > 10min check passed`
//     · Replay 产生 command: 在同一个位置，上下文不同
//     · 抛出 "Non-deterministic workflow execution detected"
//     · Workflow 卡死，不能前进也不能重试
//
//   更严重的案例：if time.Now().Hour() < 6 { doA() } else { doB() }
//   历史：T0 = 3am → 走 A 分支；replay 时 T1 = 8am → 走 B 分支 → 历史对不上。
//   整个 workflow 的 event sequence 会错乱，数据可能丢失。
//
//   正确做法：
//
//     func DeployWorkflow(ctx workflow.Context, req DeployReq) error {
//         startTime := workflow.Now(ctx)                // ✅ replay 时返回历史时间
//
//         workflow.ExecuteActivity(ctx, PullCode, req.Repo).Get(ctx, nil)
//         workflow.ExecuteActivity(ctx, Build, req).Get(ctx, nil)
//
//         elapsed := workflow.Now(ctx).Sub(startTime)   // ✅ 确定性
//         if elapsed > 10*time.Minute {
//             return errors.New("build too slow")
//         }
//
//         workflow.ExecuteActivity(ctx, Deploy, req).Get(ctx, nil)
//         return nil
//     }
//
//   记住 3 个"绝对不能在 Workflow 中出现"的写法：
//
//     ┌──────────────────────────┬───────────────────────────────────────┐
//     │ 错误                      │ 正确替代                              │
//     ├──────────────────────────┼───────────────────────────────────────┤
//     │ time.Now()                │ workflow.Now(ctx)                     │
//     │ rand.Int()                │ workflow.SideEffect(ctx, func() any {…}) │
//     │ uuid.New()                │ workflow.SideEffect(ctx, uuid.New)    │
//     │ http.Get, db.Query        │ workflow.ExecuteActivity(ctx, ...)    │
//     │ go func() {...}()         │ workflow.Go(ctx, func(wctx) {...})    │
//     │ time.Sleep(3*time.Second) │ workflow.Sleep(ctx, 3*time.Second)    │
//     │ for k, v := range map {}  │ sort.Strings(keys); for _, k := keys  │
//     └──────────────────────────┴───────────────────────────────────────┘
//
// -----------------------------------------------------------------------------
//
// [案例二] HITL 信号挂起的魔法 —— Worker 0 占用等待 1 小时
//
//   传统"人工审批"实现（基于内存 channel）：
//
//     func waitApproval(taskID string) bool {
//         ch := approvalChs[taskID]
//         select {
//         case result := <-ch:          // 阻塞等用户点按钮
//             return result
//         case <-time.After(1 * time.Hour):
//             return false
//         }
//     }
//
//   问题：
//     · 1 个审批中的任务 = 1 个常驻 goroutine + 1 个内存 channel
//     · 1000 个同时等待 = 1000 个 goroutine（内存 ≈ 8GB for stacks）
//     · Agent 重启 → 所有审批状态丢失 → 用户点了批准但任务已死
//
//   Temporal 的魔法实现：
//
//     func AgentTaskWorkflow(ctx workflow.Context, req Request) (*Result, error) {
//         // 先执行前置步骤
//         var plan Plan
//         workflow.ExecuteActivity(ctx, ParseIntentActivity, req).Get(ctx, &plan)
//
//         var riskResp RiskCheckResult
//         workflow.ExecuteActivity(ctx, SecurityCheckActivity, plan).Get(ctx, &riskResp)
//
//         // 如果需要审批，挂起等信号
//         if riskResp.RequiresApproval {
//             approvalCh := workflow.GetSignalChannel(ctx, "approval-signal")
//
//             var approval ApprovalResponse
//             selector := workflow.NewSelector(ctx)
//             selector.AddReceive(approvalCh, func(c workflow.ReceiveChannel, more bool) {
//                 c.Receive(ctx, &approval)
//             })
//             selector.AddFuture(workflow.NewTimer(ctx, 30*time.Minute), func(f workflow.Future) {
//                 approval = ApprovalResponse{Approved: false, Reason: "timeout"}
//             })
//
//             // 魔法时刻：这里阻塞等待
//             // 但 Worker 不实际占资源，状态持久化到 Temporal DB
//             selector.Select(ctx)
//
//             if !approval.Approved {
//                 return &Result{Status: "rejected"}, nil
//             }
//         }
//
//         var result Result
//         workflow.ExecuteActivity(ctx, ExecuteTaskActivity, plan).Get(ctx, &result)
//         return &result, nil
//     }
//
//   真实行为观察（Prometheus 指标）：
//
//     场景：1000 个任务同时等待人工审批，平均等待 30min
//
//       指标                   传统 goroutine 方案    Temporal 方案
//       ─────────────────     ──────────────────    ──────────────
//       goroutine 数            1000                    ~20 (空闲 worker)
//       内存占用                ~8GB                    ~200MB
//       Agent 可重启            ❌ 丢失全部审批         ✅ 透明续跑
//       审批超时                Bug-prone timer         workflow.NewTimer
//       审计回溯                要自己写日志            自动 event history
//
//   底层原理：selector.Select 执行时，workflow 把当前状态（变量值、下一步指令）
//   序列化后写入 Temporal DB，worker 线程立即释放。收到 Signal 时，
//   Temporal 从任意 worker 恢复 workflow，反序列化状态，继续执行。
//
//   通过"状态持久化 + 事件重放"模拟出"永远阻塞但零开销"的协程效果。
//
// -----------------------------------------------------------------------------
//
// [案例三] Activity 重试策略 —— 幂等性 vs 重试次数的陷阱
//
//   某 Agent 的 "git commit + push" Activity：
//
//     func GitCommitPushActivity(ctx context.Context, req CommitReq) error {
//         exec.Command("git", "add", ".").Run()
//         exec.Command("git", "commit", "-m", req.Message).Run()
//         exec.Command("git", "push", "origin", req.Branch).Run()
//         return nil
//     }
//
//   默认 RetryPolicy 是 MaximumAttempts=3。某次网络抖动：
//
//     Attempt 1 : add ✓, commit ✓, push 失败 (network error) → 返回 err
//     Attempt 2 : add ✓, commit ✓ (又创建一个同内容的 commit!), push ✓
//     Attempt 3 : 未执行
//
//   结果：git 仓库里出现两个相同内容的 commit，审计人员懵了。
//
//   根因：**Activity 必须幂等**，否则重试会造成副作用翻倍。
//
//   修复方案 A：让 Activity 本身幂等
//
//     func GitCommitPushActivity(ctx context.Context, req CommitReq) error {
//         // 检查当前是否已经有同样的 commit
//         out, _ := exec.Command("git", "log", "-1", "--format=%s").Output()
//         if strings.TrimSpace(string(out)) == req.Message {
//             // 已经 commit 过，跳过到 push
//         } else {
//             exec.Command("git", "add", ".").Run()
//             exec.Command("git", "commit", "-m", req.Message).Run()
//         }
//         return exec.Command("git", "push", "origin", req.Branch).Run()
//     }
//
//   修复方案 B：拆分 Activity（推荐）
//
//     workflow.ExecuteActivity(ctx, GitAddActivity, req).Get(ctx, nil)
//     workflow.ExecuteActivity(ctx, GitCommitActivity, req).Get(ctx, &commitSHA)
//     // 标记 commit Activity 不可重试（因为每次都会创建新 commit）
//     opts := workflow.ActivityOptions{
//         RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
//     }
//
//     // Push 可以幂等，放心重试
//     pushCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
//         RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 5},
//     })
//     workflow.ExecuteActivity(pushCtx, GitPushActivity, commitSHA).Get(ctx, nil)
//
//   重试策略黄金法则：
//     1. 判断 Activity 是否**天然幂等**（GET、PUT、DELETE、创建唯一资源）
//     2. 非幂等 → MaximumAttempts=1 或在 Activity 内加去重逻辑
//     3. 幂等 → MaximumAttempts=5~10，InitialInterval=1s，
//                NonRetryableErrorTypes=[PermissionDenied, ValidationError]
//     4. 对外部 API 调用，考虑对方的 rate limit，MaximumInterval<=60s
//
// -----------------------------------------------------------------------------
//
// [案例四] Query 与 Signal 的职责边界 —— 别把 Query 当同步 RPC
//
//   新手错误：把 Query 当作"让 Workflow 做事"的同步 RPC
//
//     // ❌ 在 Query handler 里修改 workflow state
//     workflow.SetQueryHandler(ctx, "approve", func() error {
//         approved = true            // 改变 workflow 状态
//         return nil
//     })
//
//   Query 规范要求：
//     · Query 是**只读的**，不得产生副作用
//     · Query 在任意 worker 上通过 replay 得到当前状态
//     · 如果 Query handler 修改状态，每次 Query 都会导致"replay 历史不一致"
//
//   正确用法：
//
//     // ✅ Query 只读
//     workflow.SetQueryHandler(ctx, "status", func() (TaskStatus, error) {
//         return TaskStatus{
//             Step:        currentStep,        // 当前执行到第几步
//             Progress:    progress,            // 进度百分比
//             LastUpdated: lastUpdated,
//         }, nil
//     })
//
//     // ✅ Signal 用于"让 workflow 做事"
//     approvalCh := workflow.GetSignalChannel(ctx, "approval-signal")
//     var approval ApprovalResponse
//     approvalCh.Receive(ctx, &approval)
//
//   API 设计：
//     GET  /tasks/:id/status       → temporal.QueryWorkflow("status") (立即返回快照)
//     POST /tasks/:id/approve      → temporal.SignalWorkflow("approval-signal", req)
//     POST /tasks/:id/cancel       → temporal.CancelWorkflow(id, reason)
//
//   前端轮询 status 查询进度，一旦拿到 step="awaiting_approval"，
//   弹出审批对话框；用户点批准 → signal 过去；workflow 恢复执行。
//
// =============================================================================
//
// 14. 端到端数据流示例 —— 高危 kubectl apply 从挂起到批准
// -----------------------------------------------------------------------------
//
// 场景：Agent 接到 "把 api-service 新镜像部署到 prod" 指令，触发 HITL 审批。
//
// ── Step 0：orchestrator 判定需要审批 ─────────────────────────────────
//
//   LLM 输出 ToolCall{Name:"kubectl_apply", ...}
//   securityGuard.Scan → RiskHigh（kubectl + prod namespace）
//
//   orchestrator.requireApproval：
//     taskID := "task-deploy-9f3a1b"
//     temporal.StartAgentTask(ctx, &TaskRequest{
//         TaskID:    taskID,
//         Type:      "hitl_deploy",
//         SessionID: "sess-8f3a1b",
//         UserID:    "u-42",
//         Payload: map[string]any{
//             "cluster":   "prod-k8s",
//             "namespace": "default",
//             "manifest":  "apiVersion: apps/v1\nkind: Deployment\n...",
//             "image":     "acme/api:v2.3.7",
//         },
//         ApprovalRequired: true,
//         Approvers:        []string{"sre-team"},
//     })
//
// ── Step 1：Temporal Client.StartWorkflow ──────────────────────────────
//
//   wf, err := tc.StartWorkflow(ctx, client.StartWorkflowOptions{
//       ID:        "wf-" + taskID,
//       TaskQueue: "agent-tasks",
//   }, AgentTaskWorkflow, req)
//
//   → Temporal 集群（gRPC）：
//       StartWorkflowExecutionRequest {
//           Namespace:  "code-agent",
//           WorkflowId: "wf-task-deploy-9f3a1b",
//           WorkflowType: "AgentTaskWorkflow",
//           TaskQueue:  "agent-tasks",
//           Input:      [encoded TaskRequest],
//       }
//
//   → 集群持久化到 Cassandra/PG：
//       events.append(WorkflowExecutionStarted{
//           workflow_id, run_id, input, start_time,
//       })
//
//   立即返回 WorkflowRun{ID, RunID}，orchestrator 马上把 taskID 返前端：
//
//     HTTP 200 {
//       "status": "awaiting_approval",
//       "task_id": "task-deploy-9f3a1b",
//       "approval_url": "/tasks/task-deploy-9f3a1b/approve"
//     }
//
// ── Step 2：Worker Pick Up Workflow Task ────────────────────────────────
//
//   Worker Pool（Agent Pod 内的 goroutine）长轮询 agent-tasks 队列：
//     PollWorkflowTaskQueueRequest → Temporal 返回 WorkflowTask
//
//   Worker 执行 AgentTaskWorkflow 函数（从 Event History 重放）：
//
//     func AgentTaskWorkflow(ctx workflow.Context, req TaskRequest) (*TaskResult, error) {
//         ao := workflow.ActivityOptions{
//             StartToCloseTimeout: 5 * time.Minute,
//             RetryPolicy: &temporal.RetryPolicy{
//                 InitialInterval:    time.Second,
//                 BackoffCoefficient: 2.0,
//                 MaximumInterval:    30 * time.Second,
//                 MaximumAttempts:    3,
//             },
//         }
//         ctx = workflow.WithActivityOptions(ctx, ao)
//
//         // Step 2a: 记录审批请求
//         if err := workflow.ExecuteActivity(ctx, AuditRequestActivity, req).Get(ctx, nil); err != nil {
//             return nil, err
//         }
//
//         // Step 2b: 需要审批 → 挂起等 Signal
//         if req.ApprovalRequired {
//             approval := ApprovalDecision{}
//             selector := workflow.NewSelector(ctx)
//
//             // Signal channel
//             approvalCh := workflow.GetSignalChannel(ctx, "approval")
//             selector.AddReceive(approvalCh, func(c workflow.ReceiveChannel, _ bool) {
//                 c.Receive(ctx, &approval)
//             })
//
//             // 24h 超时
//             timerFuture := workflow.NewTimer(ctx, 24*time.Hour)
//             selector.AddFuture(timerFuture, func(workflow.Future) {
//                 approval = ApprovalDecision{Approved:false, Reason:"timeout"}
//             })
//
//             selector.Select(ctx)      // ← 阻塞在这！
//
//             if !approval.Approved {
//                 return &TaskResult{Status:"rejected", Reason:approval.Reason}, nil
//             }
//         }
//
//         // Step 2c: 审批通过 → 实际执行
//         var result TaskResult
//         if err := workflow.ExecuteActivity(ctx, ExecuteKubectlActivity, req.Payload).Get(ctx, &result); err != nil {
//             // 活动失败 → Compensation
//             workflow.ExecuteActivity(ctx, RollbackActivity, req.Payload).Get(ctx, nil)
//             return nil, err
//         }
//
//         return &result, nil
//     }
//
// ── Step 3：Workflow 挂起状态持久化 ────────────────────────────────────
//
//   selector.Select(ctx) 导致 Workflow 进入 blocked 状态。Temporal 生成事件：
//
//     WorkflowTaskCompleted
//     TimerStarted { duration: 24h }
//     WorkflowExecutionSignaled ← 等待此事件
//
//   Worker 线程立即释放（不阻塞 Go goroutine）。
//   Workflow 状态存在 Cassandra 里。即使 Agent Pod 全部重启，状态也无损。
//
// ── Step 4：审批 UI 查询 pending 任务 ──────────────────────────────────
//
//   SRE 打开面板：
//     GET /api/tasks?status=pending&approver=sre-team
//
//   后端查询 Temporal：
//     tc.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
//         Namespace: "code-agent",
//         Query:     "WorkflowType='AgentTaskWorkflow' AND ExecutionStatus='Running' AND CustomTag='awaiting_approval'",
//     })
//
//   或用 Workflow Query（不影响状态）：
//     resp, _ := tc.QueryWorkflow(ctx, wfID, "", "getStatus")
//     // resp → "awaiting_approval"
//
// ── Step 5：SRE 批准 → Signal ──────────────────────────────────────────
//
//   POST /api/tasks/task-deploy-9f3a1b/approve
//     {"approved": true, "reason": "reviewed, LGTM"}
//
//   后端：
//     auth.Verify(approver)     // 必须 sre-team 角色
//     tc.SignalWorkflow(ctx, "wf-task-deploy-9f3a1b", "", "approval",
//         ApprovalDecision{
//             Approved: true,
//             ApproverID: "sre-alice",
//             Reason:    "reviewed, LGTM",
//             Timestamp: time.Now(),
//         })
//
//   → Temporal 集群：
//     SignalWorkflowExecutionRequest {
//         WorkflowId: "wf-task-deploy-9f3a1b",
//         SignalName: "approval",
//         Input:      [encoded ApprovalDecision],
//     }
//
//   → Cassandra append event:
//     WorkflowExecutionSignaled { signal_name: "approval", input: ... }
//
//   → 集群调度 Worker 继续处理该 workflow
//
// ── Step 6：Worker 重放 Event History 继续执行 ─────────────────────────
//
//   Worker 收到 WorkflowTask，含 history 到当前事件：
//
//     1. WorkflowExecutionStarted
//     2. WorkflowTaskScheduled
//     3. WorkflowTaskStarted
//     4. WorkflowTaskCompleted
//     5. ActivityTaskScheduled (AuditRequestActivity)
//     6. ActivityTaskCompleted
//     7. WorkflowTaskScheduled
//     8. WorkflowTaskStarted
//     9. WorkflowTaskCompleted
//    10. TimerStarted (24h)
//    11. WorkflowExecutionSignaled  ← 新事件！
//    12. WorkflowTaskScheduled      ← 本次任务
//    13. WorkflowTaskStarted
//
//   Worker 从头重放函数：
//     · 到 selector.Select 时，这次 signal 已存在，立即 Receive 赋值 approval
//     · 继续往下执行 ExecuteKubectlActivity
//
//   注意 workflow 代码的确定性约束：
//     · 不能用 time.Now()，要用 workflow.Now(ctx)
//     · 不能用 rand.Intn，要用 workflow.SideEffect(ctx, func() any { ... })
//     · 否则重放时值不一致会报 NonDeterministicError
//
// ── Step 7：ExecuteKubectlActivity 真实执行 ───────────────────────────
//
//   Activity 运行在普通 goroutine（非 workflow 上下文，可以做任意事）：
//
//   func ExecuteKubectlActivity(ctx context.Context, payload map[string]any) (*TaskResult, error) {
//       // Idempotency: 活动可能被 Temporal 重试，所以要幂等
//       if alreadyApplied(payload["image"]) {
//           return &TaskResult{Status:"already_applied"}, nil
//       }
//
//       // 写入 tmp manifest
//       tmpfile := writeManifest(payload["manifest"])
//       defer os.Remove(tmpfile)
//
//       // kubectl apply
//       cmd := exec.CommandContext(ctx, "kubectl",
//           "--kubeconfig", kubeconfigFor("prod-k8s"),
//           "apply", "-f", tmpfile,
//           "--record",
//       )
//       stdout, stderr := runWithHeartbeat(ctx, cmd)   // 期间 activity.RecordHeartbeat(ctx, "applying...")
//
//       if cmd.ProcessState.ExitCode() != 0 {
//           return nil, temporal.NewApplicationError("kubectl failed: " + stderr, "kubectl_error")
//       }
//
//       return &TaskResult{
//           Status:  "applied",
//           Output:  stdout,
//           Image:   payload["image"].(string),
//       }, nil
//   }
//
//   耗时 ~45s（kubectl apply + rollout status）。期间每 10s heartbeat：
//     activity.RecordHeartbeat(ctx, fmt.Sprintf("deployment progress: %s", currentStatus))
//
//   前端轮询 workflow Query 或 subscribe WebSocket：
//     "status": "applying", "progress": "3/5 pods ready"
//
// ── Step 8：成功完成 → Workflow 结束 ──────────────────────────────────
//
//   Activity 返回 TaskResult{Status:"applied"}
//   Workflow 函数 return → Temporal 写入：
//
//     WorkflowExecutionCompleted { result: [encoded TaskResult] }
//
//   orchestrator 通过 signal/webhook 收到完成通知：
//     session.AddMessage({
//         role: "assistant",
//         content: "✅ Deployment 'api-service:v2.3.7' applied successfully.",
//     })
//
// ── 失败 + Compensation 分支 ──────────────────────────────────────────
//
//   如果 ExecuteKubectlActivity 失败（kubectl error）：
//     Temporal 按 RetryPolicy 重试 3 次，仍失败 → Activity 返回 error
//     Workflow 函数继续执行 Compensation：
//
//     workflow.ExecuteActivity(ctx, RollbackActivity, RollbackInput{
//         Image:    previousImage,
//         Manifest: payload["manifest"],
//     }).Get(ctx, nil)
//
//     return &TaskResult{Status:"failed_rolled_back", Error:err.Error()}, nil
//
// ── 24h 超时分支 ───────────────────────────────────────────────────────
//
//   没人审批，24h 后 timer fire：
//     approval = ApprovalDecision{Approved:false, Reason:"timeout"}
//     Workflow 返回 {Status:"rejected_by_timeout"}
//     orchestrator 通知用户任务过期
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   [Agent 判定高危]
//   TaskRequest{type:hitl_deploy, payload:{image,manifest}}
//       ↓ StartWorkflow
//   Temporal Cluster：持久化 WorkflowExecutionStarted 事件
//       ↓ Worker poll + run
//   AgentTaskWorkflow 执行到 selector.Select → 挂起
//       ↓ 状态持久化到 Cassandra
//       ↓ (前端展示"等待审批")
//
//   [SRE 审批]
//   POST /approve → tc.SignalWorkflow
//       ↓ Temporal append WorkflowExecutionSignaled 事件
//       ↓ Worker 拉新 task，重放 history 到 Signal 点
//   selector.Receive(approval) → 继续执行
//       ↓ ExecuteKubectlActivity（heartbeat 实时进度）
//   kubectl apply 成功 → TaskResult{applied}
//       ↓ Workflow complete
//   结果通知 orchestrator + 写 session
//
//   [失败保护]
//   kubectl 失败 → retry 3x → Compensation Activity 回滚
//   24h 无响应 → Timer fire → Status=rejected_by_timeout
//
//   关键指标：
//     · 挂起期间 Worker 资源 0 占用（状态在 Temporal 存储）
//     · Agent Pod 重启不影响 pending workflow（Event History 可恢复）
//     · HITL 审批链路完整审计（每个 Signal 落日志 → 合规）
//     · 活动幂等 + 可补偿 → 故障恢复零数据遗留
//
// =============================================================================

package temporal
