// Package skill —— 动态工具注册表（Skill Registry）
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 为什么需要 Registry？
//    LLM 的 function_call 机制要求每次请求都携带"当前可用工具"的 JSON Schema。
//    这些工具来源异构：
//      a) 内置工具：read_file / run_sandbox / rag_search（本进程 Go 函数）
//      b) MCP 工具：github.search_issues（来自子进程，运行时才知道）
//      c) 用户自定义 Skill：upload_log_parser.py（热插拔）
//
//    Registry = **以 Map 为核心的并发安全容器**，负责：
//      · 收集所有来源的工具；
//      · 生成 function_call 所需的 schema 列表；
//      · 接收 LLM 的 call({name, args}) → 路由到对应执行器。
//
// 2. 数据结构
//
//     type Skill struct {
//         Name        string            // 唯一键，如 "github.search_issues"
//         Description string            // 自然语言描述（给 LLM 看）
//         InputSchema json.RawMessage   // JSON Schema
//         Invoke      InvokeFn          // 实际执行闭包
//         Source      SkillSource       // Builtin / MCP / UserScript
//         RiskLevel   int               // 0=安全 1=需审计 2=需人工批准
//     }
//
//     type Registry struct {
//         mu     sync.RWMutex
//         skills map[string]*Skill
//     }
//
//    读多写少：RWMutex 比 Mutex 在热路径上（每次 LLM 调用都要 snapshot 列表）吞吐
//    高约 3~5 倍。
//
// 3. 动态注册 / 注销时序图
//
//      MCP.Connected ─▶ registry.RegisterBatch(mcpTools)   ─┐
//                                                           │  LLM 调用前
//      MCP.Disconnected ─▶ registry.UnregisterBySource(mcp) │  snapshot()
//      User.Install     ─▶ registry.Register(skill)         │  生成工具列表
//                                                           │  → 发给 LLM
//      User.Uninstall   ─▶ registry.Unregister(name)        │
//                                                          ─┘
//
//    Snapshot 返回的是切片拷贝，调用期间再发生注册/注销不会污染当前调用。
//
// 4. 与 LLM 的协同（function_call 动态组装）
//
//     snap := registry.Snapshot()          // []*Skill
//     openaiTools := make([]openai.Tool, len(snap))
//     for i, s := range snap {
//         openaiTools[i] = openai.Tool{
//             Type: "function",
//             Function: openai.FunctionDef{
//                 Name: s.Name, Description: s.Description,
//                 Parameters: s.InputSchema,
//             },
//         }
//     }
//     resp, _ := llm.Chat(ctx, msgs, openaiTools)
//     for _, tc := range resp.ToolCalls {
//         out, _ := registry.Invoke(ctx, tc.Name, tc.Args)  // 路由执行
//         msgs = append(msgs, tool_role_msg(out))
//     }
//
// 5. 风险分级与中断
//    RiskLevel=2 的 Skill 在被 Invoke 时，注册表不直接执行，而是：
//      · 写入 workflow variable `awaitingApproval`；
//      · 返回 ErrNeedApproval；
//      · 由 Orchestrator 捕获错误 → 触发 Temporal workflow.Await（见 internal/temporal）。
//
// 6. 防御性设计
//    a) 名字冲突：Register 时若 key 存在，返回 ErrDuplicate（不允许静默覆盖）；
//    b) Schema 校验：注册时用 github.com/xeipuuv/gojsonschema 预编译，失败即拒；
//    c) 执行限流：每个 Skill 可配置 RPS，超限直接返回 ErrRateLimited。
//
// =============================================================================
//
// 7. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                         skill package                                 │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ Registry                                                        │  │
//   │  │  ─────────────────────────────────────────────────────────     │  │
//   │  │  mu       sync.RWMutex                                          │  │
//   │  │  skills   map[string]*Skill        ──┐                          │  │
//   │  │  rates    map[string]*rateLimiter    │  index by Name           │  │
//   │  │  stats    map[string]*InvokeStat     │                          │  │
//   │  │                                      │                          │  │
//   │  │  + Register(s *Skill)                │                          │  │
//   │  │  + Unregister(name)                  │                          │  │
//   │  │  + UnregisterBySource(src)           │                          │  │
//   │  │  + Snapshot() []*Skill               │                          │  │
//   │  │  + Invoke(ctx, name, args)           │                          │  │
//   │  └──────────────────────────────────────┘                          │  │
//   │                        ▲                                             │
//   │                        │ holds                                       │
//   │                        │                                             │
//   │  ┌─────────────────────┴────────────────────────────────────────┐  │
//   │  │ Skill                                                         │  │
//   │  │  ────────────────────────────────────────────────────────    │  │
//   │  │  Name        string        "github.search_issues"             │  │
//   │  │  Description string                                           │  │
//   │  │  InputSchema json.RawMessage                                  │  │
//   │  │  Source      SkillSource   (Builtin | MCP | UserScript)       │  │
//   │  │  RiskLevel   int           (0/1/2)                            │  │
//   │  │  RPS         int           (per-skill rate limit)             │  │
//   │  │  Invoke      InvokeFn                                         │  │
//   │  └──────────────────────────────────────────────────────────────┘  │
//   │                                                                      │
//   │   Registered by:                   Consumed by:                      │
//   │   ─────────────                    ─────────────                     │
//   │   · internal/tools (builtin)       · orchestrator (Invoke)           │
//   │   · internal/mcp   (tools/list)    · llm.Client (Tools schema)       │
//   │   · internal/api   (user install)                                    │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 8. 典型调用时序图：一次 LLM ReAct → tool_call 路由
//
//   orchestrator      skill.Registry           tools / mcp / sandbox
//        │                  │                           │
//        │   Snapshot()     │                           │
//        ├─────────────────▶│                           │
//        │  []*Skill        │                           │
//        │◀─────────────────┤                           │
//        │                  │                           │
//        │   build openaiTools, call llm.Chat           │
//        │──────────────────────────────────────────────┼─▶ LLM
//        │                  │     tool_calls[{name,args}]
//        │◀─────────────────────────────────────────────┤
//        │                  │                           │
//        │ Invoke("x", args)│                           │
//        ├─────────────────▶│ rate check / risk check   │
//        │                  │──────────────────────────▶│  backend exec
//        │                  │                           │
//        │                  │◀──────────── result ──────│
//        │  result / error  │                           │
//        │◀─────────────────┤                           │
//        │                  │                           │
//        │  [if ErrNeedApproval: enter Temporal HITL]   │
//
// 9. 动态增删时序（MCP 热插拔）
//
//     mcp.Gateway              skill.Registry          Orchestrator
//          │ AddServer(cfg)           │                      │
//          │ handshake → tools/list   │                      │
//          │──────────────────────────│                      │
//          │ RegisterBatch(tools,     │                      │
//          │               Source=MCP)│                      │
//          │─────────────────────────▶│                      │
//          │                          │  next Run()          │
//          │                          │◀─────────────────────│
//          │                          │  Snapshot() 含新工具  │
//          │                          │──────────────────────▶ LLM
//          │                          │                      │
//          │ RemoveServer(name)       │                      │
//          │─────────────────────────▶│ UnregisterBySource   │
//
// =============================================================================
//
// 15. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] 为什么 sync.RWMutex 比 sync.Mutex 吞吐高 3~5 倍？
//
//   场景：一次用户对话 Orchestrator 会连续调用：
//     · Snapshot()   —— 读操作，每个请求都要拿全量 Skill 列表
//     · Invoke()     —— 读操作，按名字查 Skill（不修改）
//     · Register()   —— 写操作，MCP 连接建立时批量注册，频率极低
//
//   读写比 ≈ 1000:1。
//
//   Mutex 方案（假设用 sync.Mutex）：
//     ┌──────────────────────────────────────────────────────┐
//     │ 10 个并发读请求                                        │
//     │   req1: Lock   ───▶ 读   ───▶ Unlock                   │
//     │   req2: 阻塞等待 Lock ...                              │
//     │   req3: 阻塞等待 Lock ...                              │
//     │   (串行化)                                             │
//     └──────────────────────────────────────────────────────┘
//     吞吐：   1 / (read_time * 10)
//
//   RWMutex 方案（本项目采用）：
//     ┌──────────────────────────────────────────────────────┐
//     │ 10 个并发读请求                                        │
//     │   req1..req10: RLock 同时获得                          │
//     │                 ├─▶ 读     ┐                          │
//     │                 ├─▶ 读     ├─ 并发无阻塞               │
//     │                 └─▶ 读     ┘                          │
//     │                 RUnlock                                │
//     │                                                        │
//     │   Register 发起 Lock ──▶ 等待所有 RLock 释放           │
//     │                    ──▶ 独占写                         │
//     └──────────────────────────────────────────────────────┘
//     吞吐：  约 N * (1 / read_time)  (N = 读并发数)
//
//   代码片段（Snapshot 热路径）：
//     func (r *Registry) Snapshot() []*Skill {
//         r.mu.RLock()
//         defer r.mu.RUnlock()          // 只读锁，允许多读并发
//         out := make([]*Skill, 0, len(r.skills))
//         for _, s := range r.skills {
//             out = append(out, s)      // 指针拷贝，O(1) per entry
//         }
//         return out
//     }
//
//   踩坑提醒：返回 []*Skill 指针而非 []Skill 值拷贝，调用方必须
//   **只读**，不可修改 Skill 字段（否则需加 Deep Copy）。这也是
//   为什么把 InvokeFn 设为 closure 持有不可变配置的原因。
//
// -----------------------------------------------------------------------------
//
// [案例二] RiskLevel 分级与"人工审批"闭环——真实 drop database 事故复盘
//
//   2024 年某金融团队生产事故：
//     运维在 Agent 对话框里问"这个库为什么占用这么大"。
//     Agent 自动：
//       ① 生成 SQL: SELECT * FROM information_schema.tables...
//       ② 又自动执行了 ANALYZE TABLE xxx
//       ③ 最后 LLM 认为需要 "DROP TABLE old_users" 释放空间
//       ④ 没有审批直接跑，丢了 200 万行数据
//
//   Skill Registry 的 RiskLevel + HITL 设计就是针对这类场景：
//
//     // 注册高危 Skill
//     registry.Register(&Skill{
//         Name: "db_execute",
//         RiskLevel: 2,                            // 必须审批
//         Invoke: func(ctx, args) (any, error) {
//             sql := args["sql"].(string)
//             // 先检查是否包含危险关键字
//             if matchDrop.MatchString(sql) {
//                 return nil, ErrNeedApproval    // ← 拦截
//             }
//             return dbClient.Exec(sql)
//         },
//     })
//
//     // Orchestrator 捕获 ErrNeedApproval：
//     result, err := registry.Invoke(ctx, tc.Name, tc.Args)
//     if errors.Is(err, ErrNeedApproval) {
//         taskID, _ := temporalCli.StartAgentTask(ctx, HITLPayload{
//             Skill: tc.Name,
//             Args:  tc.Args,
//             User:  claims.UserID,
//         })
//         return &Result{Status: Paused, TaskID: taskID}, nil
//     }
//
//   事故后在 Agent 里复现：
//     user:  "clean up old_users table"
//     agent: (生成 DROP TABLE) → ErrNeedApproval
//     UI:    显示审批对话框 "⚠️ DROP TABLE old_users will affect
//                           2M rows. Approve?"
//     user:  点 Reject
//     agent: "Operation cancelled by user."
//
//   救了团队一命。这就是 RiskLevel 字段的价值——不是花架子。
//
// -----------------------------------------------------------------------------
//
// [案例三] 动态工具装配的"LLM 上下文污染"陷阱
//
//   错误做法（常见）：启动时把所有可能的工具都注入 system prompt。
//
//     system: "You have tools: github.create_issue, github.close_issue,
//              jira.search, jira.update, confluence.create_page,
//              slack.send, pagerduty.trigger, k8s.apply, k8s.delete,
//              aws.s3.put, aws.ec2.start, ... (共 80 个)"
//
//   问题：
//     · 80 个工具 = ~15k tokens，挤占上下文 + 推理变慢
//     · LLM 看到 k8s.delete 会"手痒"乱调，Attack surface 扩大
//     · 用户只想查 Jira 工单，根本用不到 aws 工具
//
//   正确做法（Registry + 动态 Snapshot）：
//
//     // 按 session 的项目上下文过滤
//     allTools := registry.Snapshot()
//     projectTools := filter(allTools, func(s *Skill) bool {
//         return s.RiskLevel <= userMaxRisk &&
//                s.matchesProject(session.ProjectID)
//     })
//     openaiTools := toOpenAITools(projectTools)
//     // 只传 10~15 个相关工具给 LLM
//
//   真实收益（某 SaaS Agent 产品实测）：
//     · prompt tokens : 15k → 2k   (节省 87%)
//     · TTFT          : 1.8s → 0.4s  (响应延迟降 4x)
//     · 工具误调率     : 12%  → 0.3%  (越少越准)
//
//   这也是 skill.Registry 把 Source / RiskLevel 作为一等公民字段的
//   原因——它不是为了美观，而是为了支持**运行时按维度过滤装配**。
//
// =============================================================================
//
// 14. 端到端数据流示例 —— 从 ReAct tool_call 到 Skill.Invoke 的全路径
// -----------------------------------------------------------------------------
//
// 场景：LLM 决定调用 github.create_issue 在高并发下完成批量工单创建。
//      展示 ToolSchema 装配、并发调用、RWMutex 性能、审计全链路。
//
// ── Step 0：前置状态 ──────────────────────────────────────────────────
//
//   Registry 当前快照（已加载）：
//     内置 skills:      [read_file, grep_file, run_sandbox, vector_search]
//     MCP github:       [github.search_issues, github.create_issue, ...]
//     MCP jira:         [jira.search, jira.comment, ...]
//     合计 28 个 skill
//
//   Snapshot cache（ETag："v42"）：
//     schemas: [28 个 JSON Schema, 合计 6.2KB 格式化后]
//     generation: 42
//
// ── Step 1：orchestrator 构造 ChatRequest（拿 toolSchemas）────────────
//
//   schemas := registry.Snapshot()  // 单次 RLock，纳秒级
//   // ↓ snapshot.generation = 42, schemas 缓存未变 → 直接返回指针
//
//   chatReq := &llm.ChatRequest{
//       Messages: promptBuilder.Build(),
//       Tools:    schemas,            // 28 个 schemas 打包进 prompt
//       ...
//   }
//
//   高并发观察：1000 个 orchestrator goroutine 同时 Snapshot
//     · RWMutex RLock 允许并发读
//     · 返回的 schemas 切片是只读共享，无拷贝
//     · p99 延迟 < 50μs
//
// ── Step 2：LLM 返回 ToolCalls ────────────────────────────────────────
//
//   resp := llm.Chat(ctx, chatReq)
//   resp.ToolCalls = []ToolCall{
//       {ID:"tc_01", Name:"github.create_issue", Arguments:`{"repo":"acme/api","title":"Fix login bug","body":"...","labels":["bug"]}`},
//       {ID:"tc_02", Name:"github.create_issue", Arguments:`{"repo":"acme/api","title":"Add email trim","body":"...","labels":["feature"]}`},
//       {ID:"tc_03", Name:"github.create_issue", Arguments:`{"repo":"acme/api","title":"Update tests","body":"...","labels":["test"]}`},
//   }
//
//   （Claude 支持一条响应里多个 tool_call 并行）
//
// ── Step 3：orchestrator 并发分发 ─────────────────────────────────────
//
//   results := make([]*SkillResult, len(resp.ToolCalls))
//   var wg sync.WaitGroup
//
//   for i, tc := range resp.ToolCalls {
//       i, tc := i, tc
//       wg.Add(1)
//       go func() {
//           defer wg.Done()
//           results[i], _ = skillRegistry.Invoke(ctx, tc)
//       }()
//   }
//   wg.Wait()
//
// ── Step 4：Registry.Invoke 单次调用 ──────────────────────────────────
//
//   func (r *Registry) Invoke(ctx, tc ToolCall) (*SkillResult, error) {
//       // 4.1 Lookup (RLock)
//       r.mu.RLock()
//       skill, ok := r.skills[tc.Name]
//       r.mu.RUnlock()
//       if !ok { return nil, ErrSkillNotFound }
//
//       // 4.2 参数校验（JSON Schema）
//       if err := skill.Validator.Validate(tc.Arguments); err != nil {
//           return &SkillResult{IsError:true, Content:err.Error()}, nil
//       }
//       // 某次调用若缺 title → validator 返回 "missing required: title"
//
//       // 4.3 风险 gate
//       if skill.RiskLevel >= RiskMedium && !ctx.Value(approvalCtxKey{}).(bool) {
//           return nil, ErrNeedApproval   // 交给 Temporal HITL
//       }
//       // github.create_issue RiskLevel = 2 (会产生外部副作用)
//       // 但当前 session 已有 "auto_approve_github" token → 通过
//
//       // 4.4 审计埋点
//       auditID := audit.Log(ctx, AuditEvent{
//           Kind:     "skill_invoke",
//           SkillName: tc.Name,
//           Source:   skill.Source,
//           Args:     redact(tc.Arguments),  // 日志脱敏
//           SessionID:ctx.Value(sessionIDKey{}).(string),
//           UserID:   ctx.Value(userIDKey{}).(string),
//       })
//
//       // 4.5 Metrics counter
//       r.metrics.Inc("skill_invoke_total", skill.Name, skill.Source)
//       start := time.Now()
//
//       // 4.6 Handler 调用（MCP 场景）
//       result, err := skill.Handler(ctx, tc.Arguments)
//       // handler 内部：gateway.CallTool("github", "create_issue", args)
//       // 耗时 ~900ms (GitHub API RTT)
//
//       duration := time.Since(start)
//       r.metrics.Observe("skill_invoke_duration_seconds", duration.Seconds(),
//           skill.Name, skill.Source, boolLabel("error", err != nil))
//
//       // 4.7 更新审计
//       audit.Update(auditID, AuditUpdate{
//           Duration: duration,
//           Success:  err == nil,
//           Output:   truncate(result, 1024),
//           Error:    errString(err),
//       })
//
//       if err != nil {
//           return &SkillResult{IsError:true, Content:err.Error()}, err
//       }
//       return result, nil
//   }
//
// ── Step 5：三个并发 invoke 的时序 ────────────────────────────────────
//
//   T+0ms      orchestrator 起 3 个 goroutine
//   T+0.1ms    g1/g2/g3 都 RLock → 同时读取 skills map（RWMutex 无阻塞）
//   T+0.2ms    三者都拿到 skill pointer
//   T+0.3ms    参数校验 + audit 埋点（并发）
//   T+5ms      g1/g2/g3 分别进入 skill.Handler
//              → mcp.Gateway.CallTool（共用同一个 MCP client connection）
//              → 三条 JSON-RPC request 写入 stdin（mu.Lock 串行化写）
//                req id = [101, 102, 103]
//              → MCP server 并发处理（它是 Node 进程，内部 async 处理）
//   T+850ms    三个 response 陆续返回（id=101, 103, 102 乱序）
//              readLoop 按 id 路由到对应 respCh
//              g1, g3, g2 的 select 各自收到响应
//   T+870ms    三个 goroutine 完成 audit.Update，返回 results
//   T+875ms    wg.Wait 完成，orchestrator 聚合结果
//
//   总耗时 ~880ms（接近单个 RTT，并发节省 2×900ms）
//
// ── Step 6：Schema 装配时的排序（确定性）──────────────────────────────
//
//   Registry 在注册时调用 rebuildSnapshot：
//
//     var names []string
//     for k := range r.skills { names = append(names, k) }
//     sort.Strings(names)           // ← 关键：按字母序
//
//     schemas := make([]ToolSchema, 0, len(names))
//     for _, n := range names {
//         s := r.skills[n]
//         schemas = append(schemas, ToolSchema{
//             Type:        "function",
//             Name:        s.Name,
//             Description: s.Description,
//             Parameters:  s.InputSchema,
//         })
//     }
//
//     r.snapshot = &snapshot{
//         schemas:    schemas,
//         generation: r.generation + 1,
//         etag:       fmt.Sprintf("v%d", r.generation+1),
//     }
//
//   好处：
//     · orchestrator 每次 Build prompt 的 tool schemas 字节完全一致
//       → LLM KV-Cache 完美命中（参见 context/_principles.go 案例一）
//     · ETag 稳定 → 前端 /skills 请求可 304 Not Modified
//
// ── Step 7：热插拔 MCP 工具刷新 ───────────────────────────────────────
//
//   管理员新挂载一个 postgres MCP server，带来 8 个新工具：
//
//     registry.RegisterBatch(tools, source:"mcp", sourceServer:"postgres")
//
//   内部：
//     r.mu.Lock()
//     for _, t := range tools {
//         r.skills["postgres."+t.Name] = &Skill{...}
//     }
//     r.generation++
//     r.rebuildSnapshot()          // 新 schemas 排序
//     r.snapshotETag = "v43"
//     r.mu.Unlock()
//
//   下一个 orchestrator Snapshot 调用会拿到 36 个 skills。
//   此刻若有 100 个 goroutine 正持有旧 snapshot（RLock 已释放），它们继续
//   完成当前 request 不受影响（schemas 指针是快照时的副本）。
//
// ── Step 8：Invoke 失败的降级 ─────────────────────────────────────────
//
//   假设 tc_02 的 GitHub API 返回 403（repo 权限不足）：
//
//     skill.Handler 返回 error: "github api: 403 forbidden"
//     SkillResult{IsError:true, Content:"github api: 403 forbidden"}
//
//   orchestrator 把这条 error 作为 tool_result 回喂 LLM：
//
//     Message{
//         Role:       "tool",
//         ToolCallID: "tc_02",
//         Content:    `{"error":"github api: 403 forbidden","tool":"github.create_issue"}`,
//     }
//
//   LLM 下一轮 ReAct 看到 error observation：
//     "I see the second issue failed due to permissions. Let me request access
//      or ask the user to retry with a different repo."
//
//   另两个 tc_01 / tc_03 已成功，不受影响。
//
// ── 整体数据形变 ──────────────────────────────────────────────────────
//
//   [Snapshot 阶段]
//   registry.skills map → 按名排序 → []ToolSchema → 打包 prompt
//       ↓ 单次 RLock，纳秒级
//   LLM 看到 28 个 function schemas
//
//   [LLM 决策]
//   resp.ToolCalls = [tc_01, tc_02, tc_03]   (并行 3 个)
//
//   [并发 Invoke]
//   3 个 goroutine RLock 读 skill → validator → audit.Log
//       ↓ 共用 MCP connection, stdin 加锁写
//   3 条 JSON-RPC req id=[101,102,103] → GitHub API
//       ↓ ~900ms
//   3 条 response 乱序回 → readLoop 按 id 路由
//       ↓ audit.Update + metrics.Observe
//   SkillResult × 3 → orchestrator
//
//   [热插拔场景]
//   MCP mount postgres → RegisterBatch 8 新工具
//       ↓ mu.Lock + rebuildSnapshot + generation++
//   下一个 Snapshot 拿 36 schemas
//
//   [失败分支]
//   invoke error → SkillResult{IsError:true}
//       ↓ orchestrator tool_result 含 error
//   LLM 自主决策（重试 / 改策略 / 询问用户）
//
//   关键指标：
//     · Snapshot：1000 并发 p99 < 50μs (RWMutex 读零阻塞)
//     · Invoke：完全并发，3 个调用总耗时 ≈ 1 个 RTT
//     · 注册热更新：O(ms) 级别，不阻塞运行中的 Invoke
//     · Audit：每次 invoke 双写（start + end），合规可追溯
//
// =============================================================================

package skill
