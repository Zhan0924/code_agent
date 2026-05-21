// Package temporal 集成 Temporal Workflow 引擎，提供高可用的长任务编排与 Human-in-the-Loop。
//
// # 为什么用 Temporal
//
// Agent 常面临**长耗时 + 需挂起恢复**的场景：
//
//   - 部署任务需要人工审批（可能等几分钟到几天）
//   - 代码编辑+测试+构建流水线（含重试、分支逻辑）
//   - 多步骤外部集成（GitHub PR 创建 → Review → Merge）
//
// 若用内存态 Goroutine + channel：
//   - Agent 重启 → 状态全丢
//   - 挂起期间占 goroutine/FD → 成本高
//   - 无重试策略、无可视化
//
// Temporal 把**Workflow 状态持久化到数据库**，节点迁移/重启不丢；等待信号时
// 0 Worker 资源；原生支持重试/超时/并行/子工作流。
//
// # 架构
//
//	┌──────────────┐  StartWorkflow    ┌──────────────────┐
//	│ Orchestrator │ ────────────────▶ │ Temporal Server  │
//	└──────┬───────┘                    │ (frontend/matching│
//	       │                            │  history/worker) │
//	       │ SignalWorkflow             └────────┬─────────┘
//	       ▼                                     │
//	   /tasks/approve                            ▼
//	       │                             ┌───────────────┐
//	       └──── Signal("approval") ────▶│  Worker (Go)  │←── Activities:
//	                                     │  (本进程内)    │   validateManifest
//	                                     └───────────────┘   execKubectl ...
//
// # 核心概念
//
//	Workflow   —— 确定性代码，描述业务流程（代码必须可重放）
//	Activity   —— 真正产生副作用的函数（调外部 API、写文件、跑命令）
//	Signal     —— 外部向运行中 Workflow 发送异步消息（HITL 就靠它）
//	Query      —— 只读询问 Workflow 当前状态
//
// # HITL Workflow 典型结构
//
//	func DeployFlow(ctx workflow.Context, params DeployParams) error {
//	    workflow.ExecuteActivity(ctx, validateManifest, params).Get(...)
//	    workflow.ExecuteActivity(ctx, diffCluster, params).Get(...)
//
//	    var approval ApprovalResponse
//	    ok := workflow.Await(ctx, func() bool { return approvalReceived })
//	    if !ok || !approval.Approved {
//	        return ErrRejected
//	    }
//
//	    workflow.ExecuteActivity(ctx, applyKubectl, params).Get(...)
//	    return nil
//	}
//
// # 关键文件
//
//	workflows.go   —— Workflow 定义（DeployFlow、EditFlow 等）
//	activities.go  —— Activity 实现（实际执行命令、调 API、审计）
//
// # 重试策略
//
// 每个 Activity 默认带 RetryPolicy：
//   - InitialInterval: 1s
//   - BackoffCoefficient: 2.0
//   - MaxInterval: 60s
//   - MaximumAttempts: 3
//   - NonRetryableErrorTypes: [ValidationError, PermissionDeniedError]
//
// 详见 docs/architecture/11_temporal.md。
package temporal
