// Package orchestrator —— Agent 中枢编排（ReAct 循环 + 工具分派 + 失败兜底）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 定位：Agent 的"大脑干"
//    用户消息 → Orchestrator 是**唯一入口**，负责把一次对话拆成：
//      · 意图识别  (planner_bridge)
//      · 上下文装配 (context / rag / session)
//      · LLM 决策  (llm.Client + Router)
//      · 工具执行  (skill.Registry / mcp / sandbox / tools)
//      · 结果回写  (session + metrics + audit)
//    并在必要时启动 Temporal Workflow 承接长任务和人工审批。
//
// 2. 为什么选 ReAct 架构？
//    纯 LLM "一轮推理一次性输出"无法胜任多步骤代码任务。
//    ReAct = **Reason + Act** 循环：
//
//         ┌───────────────────────────────┐
//         │   Thought                     │  ← LLM 输出推理
//         │      ↓                        │
//         │   Action (tool_call)          │  ← 选中一个工具及参数
//         │      ↓                        │
//         │   Observation (tool_result)   │  ← 执行结果回喂 LLM
//         │      ↓                        │
//         │   (loop until Final Answer)   │
//         └───────────────────────────────┘
//
//    核心由 orchestrator.Run() 的 for-loop 实现，最多 N 轮（防失控）。
//
// 3. 关键子职责
//
//      orchestrator.go        : 主 ReAct 循环 + tool_call 分派
//      planner_bridge.go      : 把"用户消息"解析为 intent + complexity
//                               驱动 llm.Router 选模型
//      edit_engine.go         : LLM 输出的 diff/patch 安全应用到文件系统
//      auto_test_runner.go    : 修完代码后自动跑单测并回传结果
//      file_tools.go / git_*  : 通用工具集合（read/write/grep/git diff）
//      project_rules.go       : 加载 .cursorrules / AGENT.md 等规则文件
//      failure_tracker.go     : 记录每步失败原因；超阈值触发"停手"策略
//      message_pruner.go      : 调用 context.Pruner 做消息级裁剪
//
// 4. 工具分派（Tool Dispatch）
//    LLM 返回 tool_calls 时，Orchestrator 按名字路由：
//      · 内置工具  → tools 包的 Go 函数
//      · MCP 工具 → mcp.Gateway.CallTool
//      · Sandbox  → sandbox.Manager.Execute
//      · 高危工具  → 返回 ErrNeedApproval → 转入 Temporal HITL
//    所有分派统一走 skill.Registry.Invoke，保证 LLM 面对的是统一模型。
//
// 5. 失败兜底（failure_tracker.go）
//    LLM 会出现"一直调同一个工具""在错误参数里循环"等失控场景。
//    策略：
//      · 连续 3 次同名工具失败 → 强制插入 system msg 提示换方法
//      · 连续 5 次失败 → 退出 ReAct，返回给用户 "Agent 已尝试 N 次未果"
//      · 工具参数结构错误（JSON Schema 失败）→ 把错误消息回喂 LLM 自纠
//
// 6. 自测循环（auto_test_runner.go）
//    修代码的任务最终往往需要 "run tests" 验证：
//      · 检测项目类型 (go/npm/pytest)
//      · 跑对应命令，截 stdout/stderr
//      · 有失败 → 把失败堆栈作为 observation 回喂 LLM 继续改
//      · 全绿 → 标任务成功
//    这让 Agent 具备"自修复"闭环。
//
// 7. 规则注入（project_rules.go）
//    扫描项目根下 .cursorrules / AGENT.md / CONTRIBUTING.md，
//    拼入 system prompt 的 "Project Rules" 段（KV-cache 稳定区）。
//    保证 Agent 遵守该项目的 coding style / 提交规范 / 安全红线。
//
// 8. 与外围模块的协作图
//
//                 ┌──────────┐
//     ┌──▶ skill  │ registry │ ◀─┐
//     │           └──────────┘   │
//     │                          │
//     │   Orchestrator.Run()     │
//     │   ┌──────────────────┐   │
//     ├──▶│   ReAct Loop     │◀──┤
//     │   └──────────────────┘   │
//     │        │   ▲             │
//     │        ▼   │             │
//     │    ┌──────────┐         ┌────────────┐
//     ├──▶ │ llm.Client│         │ temporal   │
//     │    └──────────┘         │ HITL workf.│
//     │                          └────────────┘
//     ├──▶ session / context / rag
//     ├──▶ sandbox / tools / mcp
//     └──▶ metrics / audit / tracing
//
// 9. 可中断性与可恢复性
//    · 每轮 ReAct 入口检查 ctx.Err()，前端断开立即停止烧 LLM token
//    · 长任务不在本包阻塞，fork 给 temporal workflow，Orchestrator 立即返回 taskID
//    · 下次 resume 可由 API 触发 SignalWorkflow，流程无损续跑
//
// =============================================================================
//
// 10. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                       orchestrator package                            │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ Orchestrator  (orchestrator.go)                               │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  llm         *llm.Client                                       │   │
//   │  │  router      *llm.Router                                       │   │
//   │  │  skills      *skill.Registry                                   │   │
//   │  │  session     *session.Manager                                  │   │
//   │  │  rag         *rag.Engine                                       │   │
//   │  │  sandbox     *sandbox.Manager                                  │   │
//   │  │  mcp         *mcp.Gateway                                      │   │
//   │  │  pruner      *context.TokenPruner                              │   │
//   │  │  promptBuild *context.PromptBuilder                            │   │
//   │  │  temporal    *temporal.Client                                  │   │
//   │  │  failures    *FailureTracker                                   │   │
//   │  │  rules       *ProjectRules                                     │   │
//   │  │                                                                │   │
//   │  │  + Run(ctx, sid, userMsg)  (*Result, error)                    │   │
//   │  │  + RunStream(ctx, sid, userMsg, out)  error                    │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  Supporting pieces:                                                  │
//   │  ──────────────────                                                  │
//   │  · planner_bridge.go    : intent/complexity classifier              │
//   │  · edit_engine.go       : LLM diff → fs apply with rollback         │
//   │  · auto_test_runner.go  : detect project type → run tests           │
//   │  · failure_tracker.go   : dedup loops / force break                 │
//   │  · message_pruner.go    : bridge to context.Pruner                  │
//   │  · project_rules.go     : load .cursorrules / AGENT.md              │
//   │  · file_tools.go / git_tools.go : builtin tool implementations      │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 11. 主流程图：Orchestrator.Run
//
//        user message
//             │
//             ▼
//     ┌─────────────────────────────────────┐
//     │ 1. session.GetMessages(sid, budget) │  ← history + summary
//     └─────────────┬───────────────────────┘
//                   ▼
//     ┌─────────────────────────────────────┐
//     │ 2. planner_bridge.Classify(msg)     │  → intent, complexity
//     └─────────────┬───────────────────────┘
//                   ▼
//     ┌─────────────────────────────────────┐
//     │ 3. rag.Search(msg, {project})       │  → chunks (top-N after rerank)
//     └─────────────┬───────────────────────┘
//                   ▼
//     ┌─────────────────────────────────────┐
//     │ 4. context.Pruner.PruneCodeChunks    │  → budget-fit chunks
//     │    context.Pruner.PruneMessages      │
//     └─────────────┬───────────────────────┘
//                   ▼
//     ┌─────────────────────────────────────┐
//     │ 5. PromptBuilder.Build()             │  KV-cache friendly order
//     └─────────────┬───────────────────────┘
//                   ▼
//     ┌─────────────────────────────────────┐
//     │ 6. router.Route → llm.Chat(req)      │  → assistant msg / tool_calls
//     └─────────────┬───────────────────────┘
//                   ▼
//         has tool_calls?
//          ├─ no  → return finalAnswer
//          │
//          └─ yes → dispatch loop (see §12)
//
// 12. ReAct 循环 & 工具分派流程图
//
//     ┌───────────────────────────────────────────────────────────────┐
//     │ for iter := 0; iter < MaxIterations; iter++ {                  │
//     │                                                                │
//     │   resp, _ := llm.Chat(prompt + history + toolResults)          │
//     │                                                                │
//     │   if resp.stopReason == "end_turn" || !resp.tool_calls:        │
//     │       break                 // 拿到 Final Answer                │
//     │                                                                │
//     │   for tc in resp.tool_calls {                                  │
//     │       skill := skills.Get(tc.name)                             │
//     │                                                                │
//     │       ┌─────────────────────────────────────┐                  │
//     │       │ switch on source / risk:            │                  │
//     │       │ ┌─────────────────────────────────┐ │                  │
//     │       │ │ Source=Builtin  → tools.*       │ │                  │
//     │       │ │ Source=MCP      → mcp.CallTool  │ │                  │
//     │       │ │ Name="run_sandbox" → sandbox.Exec│ │                  │
//     │       │ │ RiskLevel>=2    → return        │ │                  │
//     │       │ │   ErrNeedApproval → temporal... │ │                  │
//     │       │ └─────────────────────────────────┘ │                  │
//     │       └─────────────────────────────────────┘                  │
//     │                                                                │
//     │       result := skill.Invoke(ctx, args)                        │
//     │       failures.Record(skill.Name, result.err)                  │
//     │       if failures.TooManyFor(skill.Name, 3):                   │
//     │           inject system("try a different approach")            │
//     │                                                                │
//     │       history += tool_msg(tc.id, result)                       │
//     │   }                                                            │
//     │ }                                                              │
//     └───────────────────────────────────────────────────────────────┘
//
// 13. HITL 分支：遇到高危工具时
//
//   Orchestrator       skill.Registry        temporal.Client        UI
//         │  Invoke("kubectl_apply") │               │                │
//         │─────────────────────────▶│               │                │
//         │◀── ErrNeedApproval ──────│               │                │
//         │                          │               │                │
//         │  StartAgentTask(HITLPayload)             │                │
//         │──────────────────────────────────────────▶│ persist       │
//         │◀── {taskID, status=awaiting_approval} ───│                │
//         │                          │               │                │
//         │  return "Awaiting Approval" to user (Result.Status=Paused)│
//         │──────────────────────────────────────────────────────────▶│
//         │                                                           │
//   (user clicks approve)                                             │
//         ◀────── POST /tasks/{id}/approve ───────────────────────────│
//         │  SignalApproval(id, {Approved:true})                       │
//         │─────────────────────────▶ (see temporal._principles §12)   │
//         │                          workflow 重放 → ExecuteActivity   │
//         │                          调回 Orchestrator.resume          │
//         │◀── Result.final ─────────│                                 │
//
// 14. 自修复闭环（auto_test_runner 介入）
//
//     edit_engine.Apply(diff)      → 写入文件 + git stash 备份
//             │
//             ▼
//     auto_test_runner.Detect()    → go test / npm test / pytest ...
//             │
//             ▼
//     runTests(ctx) → stdout/stderr/exit
//             │
//       ┌─────┴──────┐
//       │            │
//     exit=0        exit≠0
//       │            │
//       ▼            ▼
//     commit       inject observation = stderr
//                 → 再跑一轮 ReAct，LLM 读失败堆栈继续改
//                 → 直到绿 / 超出修复预算 (maxFixAttempts=3)
//
// 15. FailureTracker 状态示意
//
//     name       → count  lastErr
//     read_file     2     "ENOENT: /x/y"     → warn
//     run_sandbox   3     "exit 137 OOM"     → inject advice
//     git_commit    5     "signing failed"   → BREAK 退出
//
//     退出条件：
//       · 任一工具连续失败 ≥ 3：注入 system 建议换策略
//       · 累计失败 ≥ 5：结束 ReAct 返回 "已尝试多次未果"
//       · 同一 tool_call 的 {name, args} 重复两次以上：立即 break（死循环守卫）
//
// =============================================================================
//
// 14. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] ReAct 死循环的血泪史 —— "Thought-Action-Thought-Action..." 不停
//
//   某团队上线 Agent 后，第一天就被 CFO 点名：单个用户一次对话烧了 $47。
//   日志还原：
//
//     Turn 1 : Thought "need to find user record"
//              Action grep_file(pattern="user", path="/")
//              Obs    "No results"
//     Turn 2 : Thought "maybe wrong path, search again"
//              Action grep_file(pattern="user", path="/src")
//              Obs    "No results"
//     Turn 3 : Thought "try different pattern"
//              Action grep_file(pattern="User", path="/")
//              Obs    "No results"
//     ...
//     Turn 87: Thought "try other spelling"
//              Action grep_file(pattern="USER", path="/")
//              Obs    "No results"
//     Turn 88: Thought "try fuzzy search"
//              Action grep_file(pattern="usr", path="/")
//              Obs    "No results"
//     ... (LLM 不会自己说"找不到，放弃"，只会继续尝试)
//
//   根因：LLM 天然乐观，不会主动停下。没有外部终止条件就会无限循环。
//
//   四重保护（本包 ReActLoop 设计）：
//
//     const (
//         MaxIterations = 10     // 硬上限：超过 10 轮直接停
//         MaxTokens     = 100000 // token 预算：超过就停
//         MaxDuration   = 5*time.Minute // 时间预算
//         MaxSameAction = 3      // 同一 tool+args 重复 N 次就停
//     )
//
//     func (l *ReActLoop) Run(ctx context.Context, req Request) (*Response, error) {
//         ctx, cancel := context.WithTimeout(ctx, MaxDuration)
//         defer cancel()
//
//         actionHistory := map[string]int{}     // 防重复
//         totalTokens := 0
//
//         for iter := 0; iter < MaxIterations; iter++ {
//             // 检查 1: ctx 超时（MaxDuration）
//             if ctx.Err() != nil {
//                 return nil, fmt.Errorf("react timeout: %w", ctx.Err())
//             }
//
//             // 检查 2: token 预算
//             if totalTokens >= MaxTokens {
//                 return nil, fmt.Errorf("token budget exceeded")
//             }
//
//             // 调 LLM
//             resp, err := l.llm.Chat(ctx, req)
//             if err != nil { return nil, err }
//             totalTokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
//
//             // 检查 3: LLM 直接给出最终答案
//             if len(resp.ToolCalls) == 0 {
//                 return &Response{Content: resp.Content}, nil
//             }
//
//             // 检查 4: 同 action 重复
//             for _, tc := range resp.ToolCalls {
//                 key := tc.Name + string(tc.Arguments)   // 规范化后作为 key
//                 actionHistory[key]++
//                 if actionHistory[key] >= MaxSameAction {
//                     l.logger.Warn("same action repeated",
//                         zap.String("action", key),
//                         zap.Int("count", actionHistory[key]))
//                     // 主动注入"不要再重复"的系统消息
//                     req.Messages = append(req.Messages, Message{
//                         Role: "system",
//                         Content: fmt.Sprintf(
//                             "You have called %s with same args %d times. "+
//                             "Stop retrying and give final answer based on what you know.",
//                             tc.Name, actionHistory[key]),
//                     })
//                 }
//             }
//
//             // 执行工具
//             for _, tc := range resp.ToolCalls {
//                 result, err := l.skillRegistry.Invoke(ctx, tc.Name, tc.Arguments)
//                 req.Messages = append(req.Messages, formatToolResult(tc.ID, result, err))
//             }
//         }
//
//         return nil, fmt.Errorf("max iterations (%d) reached", MaxIterations)
//     }
//
//   运行效果（对比同一个"找 user 文件"的 query）：
//     · 修复前：87 轮，$47，最终报"无法找到"
//     · 修复后：第 4 轮自动停止，$0.12，返回"已尝试 N 次搜索无结果"
//
//   教训：**LLM 是强力发动机，但没有刹车必翻车**。外部硬约束不可省。
//
// -----------------------------------------------------------------------------
//
// [案例二] HITL 拦截器的真实工作流 —— DROP TABLE 时刻
//
//   生产事故复盘：工程师让 Agent 清理"脏数据"。
//
//     user: "Our staging db has some test users, please clean them up"
//
//     Agent 的思考链：
//       1. Thought: "I need to find test users first"
//       2. Action: db_query(sql="SELECT * FROM users WHERE email LIKE '%@test.%'")
//       3. Obs: "returned 342 rows"
//       4. Thought: "To delete efficiently, I'll drop and recreate the test data"
//       5. Action: db_execute(sql="DROP TABLE users; ...")    ← 💀
//
//   如果没有拦截，这个 SQL 会直接执行。
//
//   orchestrator 的 HITL 拦截机制（本包设计）：
//
//     func (o *Orchestrator) handleToolCall(ctx context.Context, tc ToolCall) (*ToolResult, error) {
//         skill, ok := o.skillRegistry.Get(tc.Name)
//         if !ok { return nil, fmt.Errorf("unknown skill: %s", tc.Name) }
//
//         // 第 1 层：静态规则（快速拦截明显高危的参数）
//         if risk := o.securityGuard.Scan(tc.Name, tc.Arguments); risk.Level >= RiskHigh {
//             // 不直接执行，转交 Temporal workflow 等审批
//             return o.requireApproval(ctx, tc, risk)
//         }
//
//         // 第 2 层：skill 自带 RiskLevel 判断
//         if skill.RiskLevel >= 2 {
//             return o.requireApproval(ctx, tc, RiskInfo{
//                 Level:   RiskHigh,
//                 Reason:  fmt.Sprintf("%s marked as high risk skill", skill.Name),
//             })
//         }
//
//         // 安全，直接执行
//         return skill.Invoke(ctx, tc.Arguments)
//     }
//
//     func (o *Orchestrator) requireApproval(ctx context.Context, tc ToolCall, risk RiskInfo) (*ToolResult, error) {
//         // 启动 Temporal workflow 挂起等审批
//         taskID := uuid.New().String()
//         _, err := o.temporal.StartAgentTask(ctx, TaskRequest{
//             TaskID:     taskID,
//             Type:       "hitl_approval",
//             SessionID:  ctx.Value("session_id").(string),
//             ToolCall:   tc,
//             RiskInfo:   risk,
//             ApprovalRequired: true,
//         })
//         if err != nil { return nil, err }
//
//         // 立即返回给前端：任务进入审批状态（异步）
//         return &ToolResult{
//             Status: "paused",
//             Content: fmt.Sprintf(
//                 "High-risk action %s requires approval (task %s). "+
//                 "Check the UI to approve or reject.", tc.Name, taskID),
//         }, nil
//     }
//
//   SecurityGuard 的规则（本包示例）：
//
//     var highRiskRules = []Rule{
//         {Tool: "db_execute", Pattern: regexp.MustCompile(`(?i)\b(drop|truncate)\s+(table|database)\b`)},
//         {Tool: "shell_exec", Pattern: regexp.MustCompile(`rm\s+-rf\s+/`)},
//         {Tool: "kubectl",    Pattern: regexp.MustCompile(`\b(delete|destroy)\b`)},
//         {Tool: "git",        Pattern: regexp.MustCompile(`push\s+--force|reset\s+--hard`)},
//     }
//
//     func (g *SecurityGuard) Scan(toolName string, args json.RawMessage) RiskInfo {
//         argsStr := string(args)
//         for _, rule := range highRiskRules {
//             if rule.Tool != toolName && rule.Tool != "*" { continue }
//             if rule.Pattern.MatchString(argsStr) {
//                 return RiskInfo{
//                     Level: RiskHigh,
//                     Reason: fmt.Sprintf("matched rule: %s", rule.Pattern),
//                 }
//             }
//         }
//         return RiskInfo{Level: RiskSafe}
//     }
//
//   实际拦截效果：
//     · Agent 调 `db_execute("DROP TABLE users")` → SecurityGuard 命中
//     · 不执行，返回 "paused" 给前端
//     · UI 弹出审批对话框："⚠️ 危险操作！DROP TABLE users 会影响 342 行，是否继续？"
//     · 工程师一看：不对！我要的是 DELETE，不是 DROP。点击 Reject。
//     · Agent 收到拒绝信号：修正思路 → 重新生成 DELETE ... WHERE ...
//
//   救命了。这就是 HITL 拦截的价值。
//
// -----------------------------------------------------------------------------
//
// [案例三] Intent Parser 的"过度拟合"问题
//
//   初版 IntentParser 用 JSON Schema + Function Calling 硬约束：
//
//     intent_schema := {
//         type: "object",
//         properties: {
//             intent:     {enum: ["qa", "code_edit", "deploy", "debug", "plan"]},
//             complexity: {type: "integer", min: 1, max: 10},
//             ...
//         },
//         required: ["intent", "complexity"]
//     }
//
//   看起来严谨。但生产发现问题：
//
//     user: "this is weird, it works on my machine but prod keeps crashing"
//     LLM intent parser 被迫选一个：
//       intent="debug"   ← 选了这个
//       complexity=7
//
//     实际 intent 应该是 debug+conversation 混合（先讨论再定位）。
//     强制单一 intent 导致后续 Router 走重模型 + RAG，实际用户只想聊两句。
//
//   改进：Soft Intent + 置信度
//
//     type IntentResult struct {
//         Primary       string          // 首选 intent
//         Secondary     []string        // 次要 intent（多个可能的分支）
//         Confidence    float64         // 对 Primary 的置信度
//         Complexity    int
//         RequiresRAG   bool
//         RequiresTools []string        // 预测需要的 tool
//     }
//
//     // Prompt 里允许"不确定"
//     systemPrompt := `
//     Classify the user intent. If ambiguous, set primary + secondary options
//     and a lower confidence.
//
//     Examples:
//     - "how to use X" → primary=qa, conf=0.95
//     - "it doesn't work" → primary=debug, secondary=[qa], conf=0.6
//     - "hi, thanks for the fix yesterday" → primary=conversation, conf=0.9
//     `
//
//     func (p *IntentParser) Parse(ctx context.Context, userMsg string, history []Message) (*IntentResult, error) {
//         resp, err := p.llm.Chat(ctx, &ChatRequest{
//             Model: p.lightModel,       // 用轻量级模型，~200ms
//             Messages: p.buildPrompt(userMsg, history),
//             Tools:    []Tool{intentClassifierTool},
//             ToolChoice: "intent_classify",  // 强制调用分类工具
//         })
//         if err != nil {
//             // Fallback：失败时使用规则
//             return p.ruleBased(userMsg), nil
//         }
//
//         var result IntentResult
//         json.Unmarshal(resp.ToolCalls[0].Arguments, &result)
//
//         // 低置信度时补一步"澄清"
//         if result.Confidence < 0.5 && result.Primary == "debug" {
//             result.RequiresClarification = true
//         }
//
//         return &result, nil
//     }
//
//   下游 Router 按置信度动态决策：
//
//     route := llm.Route(
//         intent:     result.Primary,
//         complexity: result.Complexity,
//         fallbackIntents: result.Secondary,   // 如果模型不 sure，Router 可以退到混合策略
//     )
//
//   效果：
//     · 用户不满意"答非所问"的情况减少 60%
//     · Router 在不确定场景更倾向用 Medium 而非 Heavy，成本省 15%
//     · UI 在 RequiresClarification=true 时显示"请补充细节：xxx"，交互体验更好
//
// -----------------------------------------------------------------------------
//
// [案例四] 错误处理的"自修复"循环 —— 让 Agent 自己爬起来
//
//   初版 orchestrator 遇到 tool 执行失败直接返回 error 给用户：
//
//     result, err := skill.Invoke(...)
//     if err != nil {
//         return nil, err   // 失败即退出
//     }
//
//   用户看到：
//     user: "deploy the api service"
//     agent: "Error: kubectl connection refused"
//     user: "why? retry?"
//     agent: (从头开始思考)
//
//   这对用户的体验是"Agent 太弱"。
//
//   改进：**Error as Observation，让 LLM 自己决定怎么办**（本包采用）
//
//     func (o *Orchestrator) executeToolCall(ctx context.Context, tc ToolCall) Message {
//         result, err := o.skillRegistry.Invoke(ctx, tc.Name, tc.Arguments)
//         if err != nil {
//             // 不 return err，而是作为 observation 返回给 LLM
//             return Message{
//                 Role:       "tool",
//                 ToolCallID: tc.ID,
//                 Content: fmt.Sprintf(
//                     "ERROR: %s failed with: %v\n"+
//                     "Context: %s\n"+
//                     "You may:\n"+
//                     "- Retry with different arguments\n"+
//                     "- Try alternative tools\n"+
//                     "- Explain the limitation to the user",
//                     tc.Name, err, extractErrorContext(err)),
//             }
//         }
//         return formatSuccess(tc.ID, result)
//     }
//
//   现在 LLM 看到错误后会继续思考：
//
//     Turn 1 : Action kubectl apply -f api.yaml
//              Obs    "ERROR: kubectl connection refused. Context: cluster unreachable"
//     Turn 2 : Thought "kubectl can't connect. Maybe I should check cluster status first."
//              Action kubectl cluster-info
//              Obs    "ERROR: Unable to connect to the server: no route to host"
//     Turn 3 : Thought "Network issue. Let me check if kube-config is correct."
//              Action read_file(path="~/.kube/config")
//              Obs    (config content)
//     Turn 4 : Thought "The server URL is pointing to old cluster. I should tell the user."
//              FinalAnswer: "Deployment failed because ~/.kube/config still points to the
//                            deprecated cluster (api.old-k8s.example.com). Please update
//                            your kubeconfig with 'aws eks update-kubeconfig --name prod'
//                            and try again."
//
//   实测（500 次包含各类错误的 task）：
//
//     指标                    旧版（直接 error）    新版（self-heal）
//     ────────────────────   ──────────────────  ─────────────────
//     最终成功率               31%                  74%
//     返回有用诊断的比例       20%                  88%
//     平均轮次                 2.1                  4.3
//     token 成本增加           基线                 +30%（可接受）
//
//   权衡：多花一点 token，换来 2x+ 的成功率。好 Agent 的关键是**鲁棒性**
//   不是"一步到位"，而是"遇挫不折，自我修复"。
//
//   这个思路其实 mimic 了人类工程师调 bug 的方式：报错 → 观察 → 假设 → 验证 → 再试。
//
// =============================================================================
//
// 15. 端到端数据流示例 —— 跟一条真实请求穿越整个模块
// -----------------------------------------------------------------------------
//
// 场景：工程师对着 Agent 说 "修一下 UserService.Login 里邮箱首尾有空格会
//      报错的 bug，修完跑测试确认绿"。以下追踪这条消息在 orchestrator 中
//      **每一步数据的实际形态**。
//
// ── Step 0：API 层封装入口请求 ──────────────────────────────────────────
//
//   orchestrator.RunRequest{
//       SessionID:   "sess-8f3a1b",
//       UserID:      "u-42",
//       TenantID:    "acme",
//       Message:     "修一下 UserService.Login 里邮箱首尾有空格会报错的 bug，修完跑测试确认绿",
//       StreamOut:   <-chan StreamEvent,   // SSE 通道
//   }
//
// ── Step 1：session.GetMessages 拉取历史 ──────────────────────────────────
//
//   已有对话 12 轮，含一段早期摘要。session.Manager 返回：
//
//   history := []Message{
//       {Role:"system",    Content:"(summary) 用户在调试 auth-service，熟悉 Go..."},
//       {Role:"user",      Content:"之前的 test 为什么失败？"},
//       {Role:"assistant", Content:"你看一下 user_test.go:42 ..."},
//       {Role:"user",      Content:"明白了"},
//       {Role:"user",      Content:"修一下 UserService.Login 里邮箱首尾有空格会报错的 bug..."},
//   }
//   // 总 tokens = 1840
//
// ── Step 2：planner_bridge.Classify 做意图识别 ──────────────────────────
//
//   intentParser 用 Light 模型（Haiku）做 ~200ms 调用：
//
//   intent := &IntentResult{
//       Primary:      "code_edit",
//       Secondary:    []string{"debug", "test"},
//       Confidence:   0.91,
//       Complexity:   6,                     // 涉及多文件，中等
//       RequiresRAG:  true,                  // 要检索 UserService 相关代码
//       RequiresTools:[]string{"read_file","write_file","run_tests"},
//   }
//
// ── Step 3：rag.Search 召回相关代码 ────────────────────────────────────
//
//   RAG query: "UserService Login email trailing space bug"
//   tenant filter: {tenant:"acme", project:"auth-service"}
//
//   返回 20 个 chunk，举其中 3 个：
//
//   chunks := []CodeChunk{
//       {
//           ID:         "chunk-7721",
//           SymbolName: "UserService.Login",
//           FilePath:   "internal/auth/user_service.go",
//           LineStart:  84,
//           LineEnd:    127,
//           Content:    "func (s *UserService) Login(email, pwd string) ...",
//           Similarity: 0.893,
//           ScopeDepth: 1,
//       },
//       {
//           ID:         "chunk-7801",
//           SymbolName: "emailValidate",
//           FilePath:   "internal/auth/validator.go",
//           Similarity: 0.812,
//           ScopeDepth: 1,
//       },
//       {
//           ID:         "chunk-4102",
//           SymbolName: "TestLogin_EmailTrimming",
//           FilePath:   "internal/auth/user_service_test.go",
//           Similarity: 0.774,
//           ScopeDepth: 2,
//       },
//       // ... 17 more
//   }
//   // 20 chunks 合计 9,400 tokens，超过 6k 预算
//
// ── Step 4：context.Pruner 多维评分 + 贪心剪枝 ─────────────────────────
//
//   为每个 chunk 计算综合分：
//
//   scores[0]: UserService.Login
//      callFreq=8  scope=1  sim=0.893  recency=0.95
//      = 0.3*1.0 + 0.1*0.77 + 0.4*0.893 + 0.2*0.95 = 0.92  ← 最高
//   scores[1]: emailValidate
//      = 0.3*0.75 + 0.1*0.77 + 0.4*0.812 + 0.2*0.9 = 0.81
//   scores[2]: TestLogin_EmailTrimming
//      = 0.3*0.12 + 0.1*0.59 + 0.4*0.774 + 0.2*0.85 = 0.57
//
//   按 score 降序贪心装入，直到累计 ≤ 6k tokens：
//
//   selectedChunks := []CodeChunk{ chunk-7721, chunk-7801, chunk-4102, ..., } // 7 个
//   // 合计 5,820 tokens
//
// ── Step 5：PromptBuilder.Build 按 KV-cache 友好顺序拼装 ───────────────
//
//   prompt := []Message{
//       // [层1] 稳定系统提示（cache hit ≈100%）
//       {Role:"system", Content:"You are a Go code assistant..."},
//
//       // [层2] Tool schemas（按 name 排序）
//       {Role:"tool_definition", Content:"{name:'grep_file',...}"},
//       {Role:"tool_definition", Content:"{name:'read_file',...}"},
//       {Role:"tool_definition", Content:"{name:'run_tests',...}"},
//       {Role:"tool_definition", Content:"{name:'write_file',...}"},
//
//       // [层3] 项目规则（.cursorrules）
//       {Role:"system", Content:"Project rules: use gofmt, wrap errors with %w...",
//        CacheControl:&CacheControl{Type:"ephemeral"}},  // ← cache breakpoint
//
//       // [层4] RAG chunks（易变）
//       {Role:"system", Content:"=== Relevant code ===\n[chunk-7721] user_service.go:84-127\nfunc (s *UserService) Login ..."},
//
//       // [层5] 历史（含早期 summary）
//       history[0..4]...,
//
//       // [层6] 当前 user message
//       {Role:"user", Content:"修一下 UserService.Login 里邮箱首尾..."},
//   }
//   // 总 tokens ≈ 7,200
//
// ── Step 6：llm.Router 选型 ────────────────────────────────────────────
//
//   router.Route(intent="code_edit", complexity=6, msgCount=17) →
//
//   route := ModelRoute{
//       Tier:           ModelHeavy,              // code_edit + complexity≥4
//       Provider:       "anthropic",
//       Model:          "claude-3-5-sonnet-20241022",
//       MaxOutputTokens:4096,
//       Reason:         "code_exec_medium",
//   }
//
// ── Step 7：llm.Client.Chat (第 1 轮) ──────────────────────────────────
//
//   调 Anthropic，~3s 返回：
//
//   resp := &ChatResponse{
//       Content: "",
//       ToolCalls: []ToolCall{
//           {
//               ID:   "tc_01A",
//               Name: "read_file",
//               Arguments: json.RawMessage(`{"path":"internal/auth/user_service.go","start":80,"end":130}`),
//           },
//       },
//       Usage: Usage{InputTokens:7200, OutputTokens:62},
//       StopReason: "tool_use",
//   }
//
// ── Step 8：分派工具调用 → skill.Registry.Invoke ─────────────────────
//
//   orch.handleToolCall(ctx, tc)：
//     → securityGuard.Scan("read_file", args) → RiskInfo{Level:RiskSafe}
//     → skills.Get("read_file") → *Skill{RiskLevel:0, Handler:...}
//     → skill.Handler.Invoke(ctx, args)
//
//   返回：
//
//   result := &ToolResult{
//       ID:      "tc_01A",
//       Content: "func (s *UserService) Login(email, pwd string) error {\n    user, err := s.repo.FindByEmail(email)\n    if err != nil { return err }\n    ...",
//       IsError: false,
//       Metadata:{"bytes":1820, "lines":44},
//   }
//   // 写入 history：{Role:"tool", ToolCallID:"tc_01A", Content: result.Content}
//
// ── Step 9：ReAct 第 2 轮 —— LLM 决定修改 ──────────────────────────────
//
//   prompt 追加 tool_result 后再次 Chat：
//
//   resp2 := &ChatResponse{
//       Content: "找到问题，email 没有 trim。",
//       ToolCalls: []ToolCall{
//           {ID:"tc_02A", Name:"write_file", Arguments:json.RawMessage(`{
//               "path":"internal/auth/user_service.go",
//               "diff":"@@ -85 +85,2 @@\n-    user, err := s.repo.FindByEmail(email)\n+    email = strings.TrimSpace(email)\n+    user, err := s.repo.FindByEmail(email)"
//           }`)},
//       },
//   }
//
//   → handleToolCall 分派给 edit_engine.Apply：
//     1. git stash 备份
//     2. 校验 diff 可 apply
//     3. 写文件
//     4. gofmt 回写
//     5. 返回 {"status":"applied", "lines_changed":1}
//
// ── Step 10：ReAct 第 3 轮 —— 跑测试 ──────────────────────────────────
//
//   resp3.ToolCalls[0] = ToolCall{
//       Name: "run_tests",
//       Arguments: `{"package":"./internal/auth/..."}`,
//   }
//
//   → auto_test_runner.Run：
//     ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
//     cmd = exec.CommandContext(ctx, "go", "test", "./internal/auth/...")
//     stdout = "ok   auth  0.342s"
//     exitCode = 0
//
//   tool_result := "=== test output ===\nok internal/auth 0.342s\nPASS"
//
// ── Step 11：ReAct 第 4 轮 —— 最终答案 ────────────────────────────────
//
//   resp4 := &ChatResponse{
//       Content: "已修复 UserService.Login：在查询数据库前调用 strings.TrimSpace 去除邮箱首尾空格。测试全绿。",
//       ToolCalls: nil,
//       StopReason: "end_turn",
//   }
//   // 没有 tool_calls → ReAct 退出
//
// ── Step 12：session.AddMessage 持久化回写 ────────────────────────────
//
//   session.AddMessage(sid, assistantMsg)  // Lua 原子 append
//   → Redis EVAL 原子：
//       RPUSH sess:msg:{sess-8f3a1b}:2  msg_json
//       HSET  sess:meta:{sess-8f3a1b}   tokens 11420  updated_at ...
//       EXPIRE sess:meta:{sess-8f3a1b}  86400
//
//   tokens 超过 summary_threshold=4000 → 触发异步摘要 goroutine
//
// ── Step 13：Metrics + Audit ───────────────────────────────────────────
//
//   metrics.Record: {
//       agent_iterations_total:         4,
//       llm_tokens_total:               {tier:heavy, in:29800, out:520},
//       llm_cost_usd_total:             {tier:heavy, cost:$0.1014},
//       tool_invocations_total:         {name:read_file=1, write_file=1, run_tests=1},
//       agent_latency_seconds_bucket:   {p50:8.4},
//   }
//
//   audit.Log: {
//       session_id:"sess-8f3a1b", user:"u-42", tenant:"acme",
//       action:"code_edit",
//       files_modified:["internal/auth/user_service.go"],
//       tests_passed:true,
//       cost_usd:0.1014,
//   }
//
// ── Step 14：返回给 API 层 ────────────────────────────────────────────
//
//   return &RunResult{
//       Status:  "completed",
//       Content: "已修复 UserService.Login：...",
//       Iterations: 4,
//       Changes: []FileChange{
//           {Path:"internal/auth/user_service.go", LinesAdded:2, LinesRemoved:1},
//       },
//       Metrics: {cost:"$0.10", latency:"8.4s", tokens:30320},
//   }, nil
//
// ── 整体数据形变总结 ──────────────────────────────────────────────────
//
//   输入：自然语言 ~30 字
//       ↓ classify
//   IntentResult{Primary:"code_edit", Complexity:6}
//       ↓ RAG 检索
//   20 个 CodeChunk (9,400 tok)
//       ↓ Pruner 剪枝
//   7 个 CodeChunk (5,820 tok)
//       ↓ Builder 装配
//   prompt 17 条 msg (7,200 tok)
//       ↓ ReAct × 4 轮
//   3 次 ToolCall + 3 次 ToolResult → 1 个 FinalAnswer
//       ↓ 持久化
//   Session +1 msg, 文件 +2 / -1 行, 测试绿
//       ↓ 输出
//   RunResult{ content, changes[1], cost:$0.10 }
//
//   整个过程：4 轮 LLM，8.4s，$0.10，用户零介入。
//
// =============================================================================

package orchestrator
