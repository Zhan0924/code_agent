// Package temporal provides Temporal workflow and activity definitions for
// managing long-running, stateful agent tasks with HITL (Human-in-the-Loop) support.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么需要 Temporal】
//
//	ReAct / Planner 都是"进程内状态机"。高危任务（kubectl apply 生产、
//	删数据库）要求：
//	  · **有序恢复**：服务重启后能从断点继续，不用用户重新操作；
//	  · **HITL 等待**：挂起等待人工审批 2 小时不能占 CPU/内存；
//	  · **分布式执行**：跨 Pod 调度 activity，单点故障不影响任务推进；
//	  · **可观测**：Temporal UI 能看到每个任务的完整状态机。
//	普通 goroutine 做不到。Temporal 是这类需求的成熟方案。
//
// 【Workflow 的确定性约束】
//
//	Temporal 会在任意时刻 replay workflow 代码（从 event history 重建状态）。
//	→ workflow 函数里**禁止**：time.Now()、rand.Read、直接 IO、goroutine
//	  math、非确定性 map 遍历。所有这些都要通过 `workflow.X()` API 拿"replay
//	  友好"版本，或者丢给 activity。违反会导致 replay 不一致，Workflow
//	  直接 fail。
//
// 【HITL 模式】
//
//	workflow.Await(ctx, func() bool { return signal 到来 })：Worker 不消耗
//	CPU，Temporal Server 管理挂起状态。前端点"批准"→ 发 signal → Workflow
//	被重新调度到任意 Worker 继续跑。关键收益：1000 个待审批任务 = 0 资源占用。
//
// 【Retry 策略】
//
//	activityOpts 里配 MaximumAttempts=3，失败指数退避。注意这个 retry 和
//	LLM Client 的 gobreaker 是不同层——
//	  · gobreaker: **熔断** — 连续失败后拒绝新请求
//	  · Temporal retry: **重试** — 单个 activity 失败后重来
//	两者互补。
//
// ============================================================================
package temporal

import (
	"fmt"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// AgentTaskInput is the input for the AgentTask workflow.
type AgentTaskInput struct {
	SessionID   string `json:"session_id"`
	UserMessage string `json:"user_message"`
	TaskID      string `json:"task_id"`
}

// AgentTaskOutput is the output of the AgentTask workflow.
type AgentTaskOutput struct {
	TaskID  string           `json:"task_id"`
	State   models.TaskState `json:"state"`
	Message string           `json:"message"`
}

// ApprovalSignal is the signal type for human approval/rejection.
const ApprovalSignal = "approval-signal"

// AgentTaskWorkflow is the main Temporal workflow for processing agent tasks.
// It supports suspension for HITL approval and automatic timeout.
//
// ┌──────────────────────────────────────────────────────────────────────────┐
// │                     Agent 长任务编排的标准骨架                            │
// ├──────────────────────────────────────────────────────────────────────────┤
// │ (1) ParseIntent   : Activity —— 调 LLM 拆解用户意图                      │
// │ (2) SecurityCheck : Activity —— 命中敏感规则则 RequiresApproval=true     │
// │ (3) HITL 等待审批 : 本 Workflow 就地挂起，用 Selector 监听 Signal + Timer │
// │        · Signal("approval-signal") → 收到人类批准/拒绝                    │
// │        · Timer(30min)              → 超时自动拒绝，防止"永久挂起"          │
// │ (4) Execute       : Activity —— 真正执行（沙箱、kubectl 等）             │
// └──────────────────────────────────────────────────────────────────────────┘
//
// 关键原理：**Workflow 函数必须是确定性的**
//   - 不能直接用 time.Now()、rand、http.Get、map 迭代顺序等非确定源；
//   - 所有副作用都要通过 workflow.ExecuteActivity 委派给 Activity；
//   - Temporal Server 会把每次调用写入 Event History；Worker 崩溃后，
//     在另一台机器上"重放"历史，**只要历史一致，执行路径就一致**，从而
//     无缝续跑。这就是 Temporal 能做到"Agent 挂起几小时不占资源"的秘诀。
//
// HITL 的 O(0) 成本：
//
//	· workflow.Await / NewSelector.Select(ctx) 这类调用在等待期间，
//	  Worker 完全释放 goroutine，Temporal Server 管理挂起状态；
//	· 前端 2 小时后才点"同意"，中间 Worker 资源占用 = 0；
//	· 信号到达时 Server 把 Workflow 重新调度到任意 Worker 继续跑。
func AgentTaskWorkflow(ctx workflow.Context, input AgentTaskInput) (*AgentTaskOutput, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("AgentTask workflow started",
		"task_id", input.TaskID,
		"session_id", input.SessionID,
	)

	// Configure activity options with retries
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

	// Step 1: Parse intent
	var intentResult IntentResult
	err := workflow.ExecuteActivity(ctx, ParseIntentActivity, input).Get(ctx, &intentResult)
	if err != nil {
		return &AgentTaskOutput{
			TaskID:  input.TaskID,
			State:   models.TaskStateFailed,
			Message: fmt.Sprintf("Intent parsing failed: %v", err),
		}, nil
	}

	// Step 2: Security check
	var securityResult SecurityCheckResult
	err = workflow.ExecuteActivity(ctx, SecurityCheckActivity, SecurityCheckInput{
		TaskID:      input.TaskID,
		UserMessage: input.UserMessage,
		Intent:      intentResult.Intent,
	}).Get(ctx, &securityResult)
	if err != nil {
		return nil, err
	}

	// Step 3: If sensitive, await human approval (HITL)
	//
	// 这是整个系统最能体现 Temporal 价值的地方：下面几行代码等效于
	// "把 Workflow 挂起等人批准"，而不消耗任何 Worker CPU/内存。
	// 架构图：
	//     ┌───── Signal("approval") ─────┐        ┌────────────┐
	//     │                               ├──────▶│  Continue  │
	//     │         Selector.Select(ctx)  │       └────────────┘
	//     │                               │        ┌────────────┐
	//     └───── Timer(30min)   ──────────┤──────▶│  TimeOut   │
	//                                              └────────────┘
	// 挂起期间：Worker goroutine 会 yield，整个 workflow 状态由 Temporal
	// 服务端的数据库持久化。哪怕 Worker Pod 此时被 K8s 驱逐，新 Pod 起来
	// 后 Temporal 会自动 replay Event History 恢复到此处继续等待。
	if securityResult.RequiresApproval {
		logger.Info("Task requires human approval, suspending workflow",
			"task_id", input.TaskID,
		)

		// GetSignalChannel 返回一个"信号通道"，与 Go 原生 chan 语义相似
		// 但由 Temporal 持久化保证 exactly-once 交付。外部通过
		// client.SignalWorkflow(workflowID, "approval-signal", payload) 推送。
		approvalCh := workflow.GetSignalChannel(ctx, ApprovalSignal)
		var approval models.ApprovalResponse

		// workflow.NewTimer 是 Temporal 原生"可挂起定时器"。注意：**不能用
		// time.After / time.Sleep**，那是非确定性的，会破坏 replay。
		// WithCancel 配对是为了在收到 Signal 后提前取消 Timer，避免资源浪费。
		timerCtx, cancelTimer := workflow.WithCancel(ctx)
		timerFuture := workflow.NewTimer(timerCtx, 30*time.Minute)

		// Selector 是 Temporal 版的 select 语句，支持对 Future/Channel 的
		// 多路监听。任一路径触发，Selector.Select 就返回。
		selector := workflow.NewSelector(ctx)

		var approved bool
		selector.AddReceive(approvalCh, func(ch workflow.ReceiveChannel, more bool) {
			ch.Receive(ctx, &approval)
			approved = approval.Approved
			cancelTimer()
		})

		selector.AddFuture(timerFuture, func(f workflow.Future) {
			// Timeout - treat as rejection
			approved = false
			logger.Warn("Approval timed out", "task_id", input.TaskID)
		})

		selector.Select(ctx)

		if !approved {
			return &AgentTaskOutput{
				TaskID:  input.TaskID,
				State:   models.TaskStateCancelled,
				Message: "Operation cancelled: approval denied or timed out",
			}, nil
		}

		logger.Info("Task approved, resuming execution", "task_id", input.TaskID)
	}

	// Step 4: Execute the task
	var execResult ExecutionResult
	err = workflow.ExecuteActivity(ctx, ExecuteTaskActivity, ExecuteTaskInput{
		TaskID:      input.TaskID,
		SessionID:   input.SessionID,
		UserMessage: input.UserMessage,
		Intent:      intentResult.Intent,
	}).Get(ctx, &execResult)
	if err != nil {
		return &AgentTaskOutput{
			TaskID:  input.TaskID,
			State:   models.TaskStateFailed,
			Message: fmt.Sprintf("Execution failed: %v", err),
		}, nil
	}

	return &AgentTaskOutput{
		TaskID:  input.TaskID,
		State:   models.TaskStateCompleted,
		Message: execResult.Output,
	}, nil
}

// ─── Activity Input/Output Types ──────────────────────────────────────────────

// IntentResult holds the result of intent classification.
type IntentResult struct {
	Intent models.TaskIntent `json:"intent"`
}

// SecurityCheckInput is the input for the security check activity.
type SecurityCheckInput struct {
	TaskID      string            `json:"task_id"`
	UserMessage string            `json:"user_message"`
	Intent      models.TaskIntent `json:"intent"`
}

// SecurityCheckResult holds the result of the security check.
type SecurityCheckResult struct {
	RequiresApproval bool   `json:"requires_approval"`
	RiskLevel        string `json:"risk_level"`
	Reason           string `json:"reason"`
}

// ExecuteTaskInput is the input for the task execution activity.
type ExecuteTaskInput struct {
	TaskID      string            `json:"task_id"`
	SessionID   string            `json:"session_id"`
	UserMessage string            `json:"user_message"`
	Intent      models.TaskIntent `json:"intent"`
}

// ExecutionResult holds the result of task execution.
type ExecutionResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code,omitempty"`
}
