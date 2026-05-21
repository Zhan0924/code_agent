// Package orchestrator 是 Code Agent 的"大脑"，实现了基于状态机的任务编排器。
//
// # 职责
//
// orchestrator 包负责把用户的自然语言请求转换为一次或多次具体的行动（tool call / sandbox exec /
// MCP RPC），并根据中间结果决定下一步是继续推理、请求 LLM、还是挂起等待人工授权。它是 API 层
// 与底层能力块（LLM / RAG / Sandbox / MCP / Tools / Skill）之间的桥梁。
//
// # 核心循环 (ReAct)
//
// 每一次 /chat 请求最终都进入以下循环：
//
//	┌────────────────────────┐
//	│  1. Parse Intent       │  ← 通过 LLM 或缓存识别任务类型
//	├────────────────────────┤
//	│  2. Build Prompt       │  ← context.PromptBuilder 装配 sys+rag+history+tools
//	├────────────────────────┤
//	│  3. LLM Chat           │  ← llm.Client (Circuit Breaker / Fallback)
//	├────────────────────────┤
//	│  4. Dispatch           │  ← 命中 tool_calls 则分发到 Sandbox/MCP/File/Git
//	├────────────────────────┤
//	│  5. Evaluate           │  ← FailureTracker / AutoTestRunner 判定是否终止
//	└─────┬─────────┬────────┘
//	      │         │
//	      │ 继续    │ 完成 / 失败 / 人工审批
//	      └→ (回到 2)
//
// 循环上限由 getMaxSteps 根据 TaskIntent 自适应（问答 10 步、诊断 25 步、编码 50 步）。
//
// # Human-in-the-Loop (HITL)
//
// 当命中敏感规则库（regex 匹配 kubectl/DROP DATABASE/rm -rf 等）时，Orchestrator 将：
//  1. 通过 store 持久化 TaskApprovalPending 状态；
//  2. 在 approvalCh 中注册 chan models.ApprovalResponse；
//  3. 将当前 Goroutine 挂起（select on chan / context.Done）；
//  4. 通过 SSE/WS 通知前端；
//  5. 等待 POST /tasks/{id}/approve → Approve() → chan 收到 {ok:true|false}；
//  6. 恢复执行或终止并审计。
//
// HITL 超时兜底：24h 未决自动 reject。参见 models.Task 状态机。
//
// # 关键子组件
//
//	EditEngine       —— 精准行级代码编辑（unique-match + backup + lint）
//	AutoTestRunner   —— TDD 自检环：改代码后自动跑测试
//	FailureTracker   —— 连续失败计数，达阈值强制终止，防死循环
//	MessagePruner    —— 历史消息压缩，避免 token 溢出
//	PlannerBridge    —— 可选的 DAG 规划器桥接点
//	ProjectRules     —— 项目级强制规则（如禁 git push --force）
//
// # 并发与可观测
//
//   - approvalCh map 由 approvalMu RWMutex 保护，支持高并发审批；
//   - intentCache 通过 intentCacheMu 实现读多写少的 LRU；
//   - 所有关键路径发射 Prometheus 指标（见 metrics 包）与 OpenTelemetry Span。
//
// # 典型调用
//
//	orch := orchestrator.New(llmClient, sessionMgr, ragEngine, sandbox, mcp, promptBuilder, cfg)
//	resp, err := orch.ProcessMessage(ctx, sessionID, userMsg)
//
// 详见 docs/architecture/09_orchestrator.md。
package orchestrator
