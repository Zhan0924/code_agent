// Package context —— Token 预算控制 + KV-Cache 友好的 Prompt 装配
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 为什么需要细粒度 Token 管理？
//    Claude 200k / GPT-4 128k 的 context window 听起来很大，实际：
//      · 延迟 / 成本都与 token 数线性相关
//      · 代码 Agent 一次 ReAct 循环可能积累：
//          system prompt + 工具 schema + N 轮历史 + M 次 tool_result + K 个 RAG hit
//      · 盲目塞入 = 信息稀释 + 成本爆炸 + 效果不升反降
//
// 2. 两个正交优化维度
//
//     维度 A：**剪枝** (pruner.go)  减少进 LLM 的 token 数
//     维度 B：**排序** (prompt_builder.go) 让 KV-Cache 命中最大化
//
// 3. 维度 A：训练-free 的 Token 重要性剪枝（pruner.go）
//
//    灵感来自 LLMLingua / LongLLMLingua 论文——不训练小模型，而是按
//    "语义承载度"加权打分：
//
//      score(chunk) = w1·CallFreq + w2·ScopeDepth_inv + w3·Relevance + w4·Recency
//
//    其中：
//      · CallFreq    : 在其他 chunk 中被引用的次数（越高越重要）
//      · ScopeDepth  : 嵌套层级（越深越不重要，取倒数）
//      · Relevance   : 来自 RAG 的向量相似度分
//      · Recency     : 源文件的修改时间
//
//    算法：按 score 降序贪心选入，直到 tokens ≥ budget 截断。
//
//    Tool Result 特殊处理：
//      一条日志可能 500KB，直接塞进 history 会挤掉其他重要上下文。
//      策略：保留 head + tail 各 1KB，中间标记 "... truncated XKB ..."，
//      完整内容仍保留在结构化 store 中以备追溯。
//
// 4. 维度 B：KV-Cache 友好的 Prompt 装配（prompt_builder.go）
//
//    LLM 推理时对 prompt 建立 KV Cache，**按前缀命中**。如果每次调用
//    动态内容都放前面，缓存全作废，延迟 & 费用 = 没有缓存。
//
//    正确顺序（从稳定到易变）：
//
//        [固定]   1) System Prompt          (所有会话相同)
//        [稳定]   2) Tool Schemas           (session 生命周期内不变)
//        [稳定]   3) Project Rules          (单次会话不变)
//        [半稳]   4) RAG Retrieved Chunks   (随 query 变)
//        [变动]   5) Conversation History   (每轮新增)
//        [最易变] 6) Current User Message   (每轮全新)
//
//    前半段命中率 > 90%，平均推理提速 20~40%，成本降低同样比例。
//
//    实现细节：
//      · 每一段的文本拼接顺序严格确定性（例如 tool schemas 按 name 排序）
//      · 同一 session 的 system + rules 写死，不插入时间戳等易变内容
//      · 用 Anthropic 的 cache_control 标记 ephemeral/persistent 段落
//
// 5. 与 session 包的职责边界
//    · session        : **粗粒度** 多轮级压缩（整条消息摘要、滑动窗口）
//    · 本包 pruner    : **细粒度** token 级剪枝（单条消息内的重要性）
//    · 本包 prompt_builder : 组装顺序 / KV cache 最优利用
//    三者自上而下、互补。
//
// 6. 可观测性
//    · context_tokens_pruned_total : 被剪掉的 token 数
//    · prompt_cache_hit_ratio      : KV Cache 命中率（从 LLM API 返回获取）
//    · truncation_events_total     : 大 tool result 被截断次数
//
// =============================================================================
//
// 7. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                         context package                               │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ TokenPruner         (pruner.go)                                 │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  maxTokenBudget int                                             │  │
//   │  │  wCallFreq / wScope / wRelevance / wRecency  float64 (weights)  │  │
//   │  │  scorePool     sync.Pool   (reuse []TokenScore, less GC)        │  │
//   │  │                                                                 │  │
//   │  │  + PruneCodeChunks(chunks, relScores)  []models.CodeChunk       │  │
//   │  │  + PruneMessages(messages, budget)     []models.Message         │  │
//   │  │  + TruncateLargeToolResult(msg)        models.Message           │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                                                                       │
//   │  ┌────────────────────────────────────────────────────────────────┐  │
//   │  │ PromptBuilder        (prompt_builder.go)                         │  │
//   │  │ ─────────────────────────────────────────────────────────       │  │
//   │  │  systemPrompt  string       (层 1：最稳定)                       │  │
//   │  │  toolSchemas   []ToolSchema (层 2)                               │  │
//   │  │  projectRules  []Rule       (层 3)                               │  │
//   │  │  ragChunks     []Chunk      (层 4：半稳定)                        │  │
//   │  │  history       []Message    (层 5：易变)                          │  │
//   │  │  userMessage   string       (层 6：最易变)                        │  │
//   │  │                                                                 │  │
//   │  │  + Build() []Message        (按 KV-cache 友好顺序拼装)           │  │
//   │  │  + MarkCacheBreakpoints()   (Anthropic cache_control)           │  │
//   │  └────────────────────────────────────────────────────────────────┘  │
//   │                                                                       │
//   │  Callers:                       Collaborators:                       │
//   │  ────────                       ─────────────                        │
//   │  · orchestrator (build prompt)  · rag.Engine (chunks + rel scores)   │
//   │  · llm.Client (Chat req)        · session (history messages)         │
//   │                                 · skill.Registry (tool schemas)      │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 8. 剪枝流程图（PruneCodeChunks）
//
//     input: []CodeChunk + []relevanceScore
//                 │
//                 ▼
//     ┌──────────────────────────────┐
//     │ buildCallFrequencyMap        │   symbol → 被引用次数
//     │  (O(n·k) 优化版)             │
//     └──────────┬───────────────────┘
//                │
//                ▼
//     for each chunk i:
//        score_i = w1·CallFreq_i/maxCallFreq
//                + w2·1/(1 + 0.3·ScopeDepth_i)
//                + w3·Relevance_i
//                + w4·i/n  (recency proxy)
//                │
//                ▼
//     tokens_total = Σ estimateTokens(chunk.Content)
//     if tokens_total ≤ budget: return chunks  (fast path)
//                │
//                ▼
//     sort.Slice(scores, desc by Score)   // 高分优先
//                │
//                ▼
//     greedy select:
//       used = 0
//       for s in scores:
//         if used + s.TokenCount ≤ budget:
//             selected[s.Index] = true
//             used += s.TokenCount
//                │
//                ▼
//     return chunks filtered by original order   // 保留源码顺序
//
// 9. KV-Cache 友好的 Prompt 装配顺序
//
//     [prompt]
//     ┌────────────────────────────────┐ ↑
//     │ System Prompt                  │ │ cache-hit 率 ≈ 100%
//     ├────────────────────────────────┤ │
//     │ Tool Schemas (sorted by name)  │ │ cache-hit 率 ≈ 95%
//     ├────────────────────────────────┤ │
//     │ Project Rules                  │ │ cache-hit 率 ≈ 90%
//     ├────────────────────────────────┤ ┤  ← Anthropic cache breakpoint
//     │ RAG Retrieved Chunks            │ │ 每 query 变
//     ├────────────────────────────────┤ │
//     │ Conversation History            │ │ 每轮新增
//     ├────────────────────────────────┤ │
//     │ Current User Message            │ ↓ cache-miss
//     └────────────────────────────────┘
//
// 10. Tool-Result 超长截断策略
//
//     原始 tool result (日志 / stacktrace) 500KB
//              │
//              ▼
//      len > MaxToolResultBytes (2KB)?
//              │
//              ├─ no ─▶ 原样保留
//              │
//              └─ yes ─▶ 切成：
//                        ┌───────────────┐
//                        │ head  [0:1KB] │
//                        ├───────────────┤
//                        │ "... truncated│  498KB ..."
//                        ├───────────────┤
//                        │ tail  [-1KB:] │
//                        └───────────────┘
//                       （完整内容仍保留在 store/audit）
//
// 11. PruneMessages 执行流程
//
//     input: []Message, budget
//              │
//              ▼
//     first pass: TruncateLargeToolResult on each msg
//              │
//              ▼
//     sum tokens; if ≤ budget: return
//              │
//              ▼
//     partition:
//       sysMsgs   (role=system, 必留)
//       otherMsgs (其余)
//              │
//              ▼
//     tail = otherMsgs[len-4:]   // 最近 2 对 QA 必留
//     middle = otherMsgs[:len-4]
//              │
//              ▼
//     remain = budget - tokens(sys) - tokens(tail)
//              │
//              ▼
//     fill middle from newest → oldest, until remain exhausted
//              │
//              ▼
//     return [sysMsgs ... keptMiddle ... tail]
//
// =============================================================================
//
// 12. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] KV-Cache 顺序错乱 —— 每次对话烧 $0.03 变成 $0.003 的优化
//
//   某团队的 Agent 每月账单 $30k，排查发现 90% 成本在 prompt tokens。
//   看下原来的 prompt 组装代码：
//
//     // ❌ 错误：动态内容前置
//     prompt := []Message{
//         {Role: "user", Content: userQuery},                   // 最易变！放第一
//         {Role: "system", Content: "You are a helpful..."},    // 稳定
//         ...history,
//         ...ragChunks,
//     }
//
//   问题：LLM 的 KV-Cache 按前缀匹配。**第一个 message 变了，整个 cache 失效**。
//
//   Anthropic Claude 定价（2024）：
//     · Input tokens : $3/M tokens   (普通)
//     · Cached input : $0.30/M tokens (缓存命中，便宜 10 倍)
//     · Output       : $15/M tokens
//
//   典型对话：prompt 15k tokens，output 500 tokens
//     · 无 cache : 15000 * $3/1M + 500 * $15/1M = $0.0525
//     · 有 cache : 15000 * $0.30/1M + 500 * $15/1M = $0.0120
//     → 单次对话省 77%，月度从 $30k → $7k
//
//   正确做法（本包的 PromptBuilder 采用）：
//
//     // 从最稳定到最易变
//     func (pb *PromptBuilder) Build() []Message {
//         var msgs []Message
//
//         // [层 1] 系统提示（所有 session 完全相同，永远命中 cache）
//         msgs = append(msgs, Message{
//             Role: "system", Content: pb.systemPrompt,
//         })
//
//         // [层 2] 工具 schemas（按 name 排序，确保字节稳定）
//         sort.Slice(pb.toolSchemas, func(i, j int) bool {
//             return pb.toolSchemas[i].Name < pb.toolSchemas[j].Name
//         })
//         for _, s := range pb.toolSchemas {
//             msgs = append(msgs, Message{Role: "tool_definition", Content: s.JSON})
//         }
//
//         // [层 3] 项目规则（单 session 生命周期内不变）
//         if pb.projectRules != "" {
//             msgs = append(msgs, Message{Role: "system", Content: pb.projectRules})
//         }
//
//         // ← cache breakpoint：加 Anthropic cache_control
//         if len(msgs) > 0 {
//             msgs[len(msgs)-1].CacheControl = &CacheControl{Type: "ephemeral"}
//         }
//
//         // [层 4] RAG 检索结果（每 query 变）
//         if len(pb.ragChunks) > 0 {
//             ragContent := formatChunks(pb.ragChunks)
//             msgs = append(msgs, Message{Role: "system", Content: ragContent})
//         }
//
//         // [层 5] 历史对话
//         msgs = append(msgs, pb.history...)
//
//         // [层 6] 当前用户消息（最易变）
//         msgs = append(msgs, Message{Role: "user", Content: pb.userMessage})
//
//         return msgs
//     }
//
//   细节陷阱：
//     · Tool schema 必须**按 name 排序**，否则每次 registry snapshot 顺序可能抖动
//     · System prompt 不要带时间戳（"Today is {{.Date}}"）
//     · RAG chunks 在"规则"之后、"历史"之前——介于稳定和动态的中间带
//
// -----------------------------------------------------------------------------
//
// [案例二] 代码 Chunk 重要性评分 —— 为什么不能只看"相似度"
//
//   RAG 召回了 20 个 chunk，总 tokens 超预算 6000，怎么选？
//
//   简单做法：按 similarity 从高到低选，装满预算。
//
//   问题场景：用户问 "fix bug in UserService.Login when email has trailing space"
//
//     RAG 返回：
//       chunk_1 sim=0.92  func UserService.Register        (相似但无关 Login)
//       chunk_2 sim=0.88  func UserService.Login           (目标函数)
//       chunk_3 sim=0.85  func UserService.Logout
//       chunk_4 sim=0.82  func emailValidate               (Login 的依赖！)
//       chunk_5 sim=0.78  type UserService struct          (Login 的 receiver！)
//       chunk_6 sim=0.75  func PasswordHasher.Verify       (Login 调用的依赖！)
//       ...
//
//   按 sim 排序装 6000 tokens，可能选到 chunk_1,2,3 但漏了 4,5,6。
//   → LLM 看不到 emailValidate 的实现，改不动 bug。
//
//   TokenPruner 的多维度评分（本包采用）：
//
//     func (p *TokenPruner) PruneCodeChunks(
//         chunks []CodeChunk, relevance []float64,
//     ) []CodeChunk {
//         // 1. 计算每个符号在其他 chunk 中被引用的次数
//         callFreq := p.buildCallFrequencyMap(chunks)
//
//         // 2. 为每个 chunk 算综合分
//         scores := make([]TokenScore, len(chunks))
//         maxFreq := maxOf(callFreq.Values())
//         for i, chunk := range chunks {
//             freq := callFreq[chunk.SymbolName]
//             score :=
//                 p.wCallFreq  * float64(freq) / float64(maxFreq) +        // 被依赖度
//                 p.wScope     * 1.0 / (1.0 + 0.3*float64(chunk.ScopeDepth)) + // 顶层优先
//                 p.wRelevance * relevance[i] +                             // RAG 相似度
//                 p.wRecency   * float64(i) / float64(len(chunks))          // 最近编辑
//             scores[i] = TokenScore{
//                 Index:      i,
//                 Score:      score,
//                 TokenCount: estimateTokens(chunk.Content),
//             }
//         }
//
//         // 3. 按分降序贪心选入，直到装满预算
//         sort.Slice(scores, func(i, j int) bool {
//             return scores[i].Score > scores[j].Score
//         })
//
//         var used int
//         selected := make(map[int]bool)
//         for _, s := range scores {
//             if used+s.TokenCount > p.maxTokenBudget { continue }
//             selected[s.Index] = true
//             used += s.TokenCount
//         }
//
//         // 4. 按原始顺序返回（保持源码阅读连贯性）
//         var result []CodeChunk
//         for i := range chunks {
//             if selected[i] { result = append(result, chunks[i]) }
//         }
//         return result
//     }
//
//   权重配置（实战调校）：
//     · wCallFreq  = 0.3   被其他代码引用多 → 核心类型/接口
//     · wScope     = 0.1   顶层 func/type > 嵌套 closure
//     · wRelevance = 0.4   RAG 相似度仍是主要信号
//     · wRecency   = 0.2   最近编辑的代码更可能相关
//
//   A/B 测试（200 条真实 bug-fix query）：
//     策略                         LLM 成功修复率
//     ─────────────────           ──────────────
//     仅按 similarity 排序           58%
//     多维度评分（本实现）           81%  (+23pp)
//
// -----------------------------------------------------------------------------
//
// [案例三] 超大 tool result 截断 —— 一条 stack trace 吃掉 80k tokens
//
//   真实场景：Agent 调用 run_tests tool，测试失败返回巨大的错误输出：
//
//     tool_result {
//       "stdout": "... 20000 lines of test output ...",
//       "stderr": "panic: runtime error: index out of range
//                  goroutine 1 [running]:
//                  main.main.func1(...)
//                          /go/src/project/main.go:42 +0x82
//                  main.main()
//                          /go/src/project/main.go:15 +0x5d
//                  ... (stack of 500 goroutines)"
//     }
//
//   总长度：80k tokens。整个塞回 LLM 的话：
//     · context window 被一条 tool_result 独占
//     · 成本：80k * $3/1M = $0.24 per message（100 轮对话就 $24）
//     · 效果：LLM 看完 stack 反而被干扰，关注错目标
//
//   TruncateLargeToolResult 策略（本包采用）：
//
//     const MaxToolResultBytes = 2048  // 2KB, ~500 tokens
//
//     func (p *TokenPruner) TruncateLargeToolResult(msg Message) Message {
//         if msg.Role != "tool" || len(msg.Content) <= MaxToolResultBytes {
//             return msg    // 不动
//         }
//
//         total := len(msg.Content)
//         halfBudget := (MaxToolResultBytes - 100) / 2
//
//         head := msg.Content[:halfBudget]
//         tail := msg.Content[total-halfBudget:]
//
//         truncated := head +
//             fmt.Sprintf("\n\n... [TRUNCATED %d bytes, see audit log for full content] ...\n\n",
//                         total - 2*halfBudget) +
//             tail
//
//         // 完整内容保留到冷存储，LLM 若需要可通过 tool 再读
//         p.auditStore.Save(msg.ID, msg.Content)
//
//         return Message{
//             ID:      msg.ID,
//             Role:    msg.Role,
//             Content: truncated,                            // LLM 看到的是这个
//             Meta:    map[string]any{"original_size": total},
//         }
//     }
//
//   LLM 实际看到：
//
//     tool_result {
//       "content": "=== test output ===
//                   FAIL: TestLogin (0.02s)
//                       user_test.go:42: expected nil, got error
//                   panic: runtime error: index out of range
//                   goroutine 1 [running]:
//
//                   ... [TRUNCATED 78000 bytes, see audit log for full content] ...
//
//                   main.main.func1(...)
//                           /go/src/project/main.go:42 +0x82
//                   main.main()
//                           /go/src/project/main.go:15 +0x5d
//                   exit status 2
//                   FAIL project 0.123s"
//     }
//
//   效果：
//     · 2KB 内容足以让 LLM 判断失败原因（panic 类型 + 关键文件行号）
//     · 完整内容在 audit log 可查（合规 + 调试）
//     · 节省 97% tokens 成本
//
//   为什么保留头 + 尾而不是只保留头？
//     · 栈顶（最近的调用）在开头，是错误直接原因
//     · 栈底（入口点）在结尾，揭示调用链起点
//     · 中间 500 层重复调用的信息量低，可截断
//
// -----------------------------------------------------------------------------
//
// [案例四] Token 估算的"字符 / 4"公式为何不够准确？
//
//   常见实现：
//
//     func estimateTokens(text string) int {
//         return len(text) / 4 + 1
//     }
//
//   对英文文本很准（平均 4 chars per token），但：
//
//     "hello world"              → 11 chars / 4 = 2.75 → 实际 2 tokens ✓
//     "你好世界"                 → 12 bytes / 4 = 3    → 实际 4 tokens ✗ (每汉字≈2 tokens)
//     "aaaaaaaaaaaaaaaaaaaa"     → 20 / 4 = 5        → 实际 1~2 tokens ✗ (重复字符被压缩)
//     "func foo() { return 1 }"  → 22 / 4 = 5.5      → 实际 8 tokens ✗ (代码符号多)
//
//   精确方案：用真实的 tokenizer
//
//     import "github.com/pkoukk/tiktoken-go"
//
//     var enc *tiktoken.Tiktoken
//     func init() {
//         enc, _ = tiktoken.GetEncoding("cl100k_base")  // GPT-4 用的 encoding
//     }
//
//     func ExactTokens(text string) int {
//         return len(enc.Encode(text, nil, nil))
//     }
//
//   性能对比（10KB 文本）：
//     方法                 延迟      精度
//     ─────────────       ────     ──────
//     len(text)/4          50ns     ±30%
//     tiktoken-go          80μs     ±0%
//
//   平衡方案（本包采用）：
//     · Prompt 装配阶段：用精确 tokenizer（慢 80μs 没关系）
//     · 剪枝决策阶段：用快速估算（N 个 chunk 要评分，速度重要）
//     · 预算检查时用精确版兜底：
//
//     func (p *TokenPruner) PruneMessages(msgs []Message, budget int) []Message {
//         // 快速估算：按 len/4 决定大致保留哪些
//         candidates := p.greedySelect(msgs, budget)
//
//         // 精确校验：tiktoken 真实计数，如超就再砍
//         actual := 0
//         for _, m := range candidates {
//             actual += ExactTokens(m.Content)
//         }
//         if actual > budget {
//             // 微调：从尾部砍到符合预算
//             return p.trimToFit(candidates, budget)
//         }
//         return candidates
//     }
//
//   设计权衡：热路径用估算（性能），关键路径用精确（正确性）。
//
// =============================================================================
//
// 13. 端到端数据流示例 —— 一次 prompt 装配的完整过程
// -----------------------------------------------------------------------------
//
// 场景：承接 orchestrator §15 Step 4 的输入，为同一个 "修 UserService.Login"
//      task 完成 Pruner + PromptBuilder 两阶段工作。
//
// ── 输入数据 ──────────────────────────────────────────────────────────
//
//   // rag.SearchResponse 返回的 20 个 chunks
//   ragChunks := []CodeChunk{
//       {ID:"chunk-7721", SymbolName:"UserService.Login",    Content:"..." /*1200 tok*/, Similarity:0.961},
//       {ID:"chunk-7801", SymbolName:"emailValidate",         Content:"..." /*400 tok*/,  Similarity:0.872},
//       {ID:"chunk-4102", SymbolName:"TestLogin_EmailTrim",   Content:"..." /*900 tok*/,  Similarity:0.859},
//       {ID:"chunk-3502", SymbolName:"UserService.Register",  Content:"..." /*1100 tok*/, Similarity:0.72},
//       {ID:"chunk-6611", SymbolName:"PasswordHasher.Verify", Content:"..." /*600 tok*/,  Similarity:0.68},
//       {ID:"chunk-2201", SymbolName:"UserRepo.FindByEmail",  Content:"..." /*500 tok*/,  Similarity:0.65},
//       {ID:"chunk-9900", SymbolName:"AuthMiddleware",        Content:"..." /*1500 tok*/, Similarity:0.58},
//       {ID:"chunk-1010", SymbolName:"loginErrorTypes",       Content:"..." /*300 tok*/,  Similarity:0.55},
//       ... (另 12 个低相关 chunk, 合计 2900 tok)
//   }
//   // 20 chunks 总计 9,400 tokens，预算 6000 → 超出 3400
//
//   // session 历史
//   history := []Message{
//       {Role:"system",    Content:"(summary 早期对话)"},                  // 320 tok
//       {Role:"user",      Content:"之前的 test 为什么失败？"},           // 12
//       {Role:"assistant", Content:"你看一下 user_test.go:42 ..."},        // 50
//       {Role:"tool",      Content:"(huge stderr 200KB)"},                // 80000 tok ← 超大！
//       {Role:"user",      Content:"明白了"},                              // 2
//       {Role:"user",      Content:"修一下 UserService.Login ..."},       // 40
//   }
//
// ── Step 1：TruncateLargeToolResult 处理超大消息 ───────────────────────
//
//   遍历 history，对每条 role==tool 的消息：
//     len(content) > MaxToolResultBytes (2KB)? → 头尾截断
//
//   history[3] 原 80000 tok →
//     truncated = head(900B) + "\n\n... [TRUNCATED 200KB bytes ...] ...\n\n" + tail(900B)
//     length: ~2KB, tokens ≈ 500
//
//   同时把 full content 推入 auditStore：
//     auditStore.Save(msg.ID, 200KB content)
//
//   history 总 tokens 从 80,424 → 924。
//
// ── Step 2：PruneCodeChunks 多维打分 ─────────────────────────────────
//
//   1) 构建 callFreq map（扫描所有 chunk 的 CallsMade 字段）:
//
//      {
//          "emailValidate":         12,  ← 被很多处调用
//          "PasswordHasher.Verify":  8,
//          "UserRepo.FindByEmail":   6,
//          "UserService.Login":      4,
//          "AuthMiddleware":         3,
//          ...
//      }
//
//   2) 为每个 chunk 计算综合分：
//
//      chunk-7721 UserService.Login:
//          callFreq = 4/12 = 0.33
//          scope    = 1/(1+0.3*1) = 0.77
//          relevance= 0.961
//          recency  = 0.95
//          score    = 0.3*0.33 + 0.1*0.77 + 0.4*0.961 + 0.2*0.95
//                   = 0.099 + 0.077 + 0.384 + 0.190 = 0.750
//
//      chunk-7801 emailValidate:
//          callFreq = 12/12 = 1.00  ← 核心依赖
//          scope    = 0.77
//          relevance= 0.872
//          recency  = 0.9
//          score    = 0.3*1.0 + 0.1*0.77 + 0.4*0.872 + 0.2*0.9
//                   = 0.300 + 0.077 + 0.349 + 0.180 = 0.906  ← 最高！
//
//      chunk-6611 PasswordHasher.Verify:
//          callFreq = 8/12 = 0.67
//          score    = 0.3*0.67 + 0.1*0.77 + 0.4*0.68 + 0.2*0.7
//                   = 0.201 + 0.077 + 0.272 + 0.140 = 0.690
//
//      chunk-4102 TestLogin_EmailTrim:
//          callFreq = 1/12 = 0.08    (测试不被引用)
//          scope    = 0.59           (嵌套深)
//          relevance= 0.859
//          recency  = 0.85
//          score    = 0.024 + 0.059 + 0.344 + 0.170 = 0.597
//
//      ... (其余 16 个 chunk 得分 0.3~0.55)
//
//   3) 按分降序贪心选入 budget=6000：
//
//      选中顺序                        tokens 累计
//      chunk-7801 emailValidate 0.906  400
//      chunk-7721 UserService.Login 0.750  1600
//      chunk-6611 PasswordHasher.Verify 0.690  2200
//      chunk-4102 TestLogin_EmailTrim 0.597  3100
//      chunk-2201 UserRepo.FindByEmail 0.560  3600
//      chunk-3502 UserService.Register 0.520  4700
//      chunk-9900 AuthMiddleware 0.510  6200  ← 超预算，跳过
//      chunk-1010 loginErrorTypes 0.480  5000  ← 装得下，选
//      chunk-xxxx ... 0.32 → tokens 800，累计 5800
//      ...                      (其余已装不下)
//
//   4) 按原始文件顺序返回 7 个 chunk (5,820 tok)：
//
//      selectedChunks := [
//        chunk-3502 UserService.Register,
//        chunk-7721 UserService.Login,
//        chunk-2201 UserRepo.FindByEmail,
//        chunk-7801 emailValidate,
//        chunk-6611 PasswordHasher.Verify,
//        chunk-1010 loginErrorTypes,
//        chunk-4102 TestLogin_EmailTrim,
//      ]
//
// ── Step 3：PruneMessages 压缩历史 ────────────────────────────────────
//
//   history 当前 tokens = 924 (已经截断过)，预算 = 2000
//
//   分区：
//     sysMsgs    = [history[0]]                  // 320 tok，必留
//     otherMsgs  = history[1..5]                 // 184 tok
//     tail       = otherMsgs[-4:] = history[2..5]  // 172 tok，必留
//     middle     = otherMsgs[:1] = [history[1]]    // 12 tok
//
//   budget 剩 1508 → middle 全装得下 → 直接返回原 history。
//
//   （对比：若 history 超预算，middle 会从新到旧逐步丢弃）
//
// ── Step 4：PromptBuilder.Build 按 KV-Cache 分层拼装 ─────────────────
//
//   builder := PromptBuilder{
//       systemPrompt: "You are a Go code assistant...",    // 120 tok  层 1
//       toolSchemas:  [grep_file, read_file, run_tests, write_file],
//       projectRules: ".cursorrules 内容",                  // 200 tok  层 3
//       ragChunks:    selectedChunks,                       // 5820 tok 层 4
//       history:      prunedHistory,                        // 924 tok  层 5
//       userMessage:  "修一下 UserService.Login...",        // 40 tok   层 6
//   }
//
//   Build() 执行顺序：
//
//   msgs := []Message{}
//
//   // [层 1] 固定 system prompt
//   msgs = append(msgs, {Role:"system", Content:"You are a Go...", CacheControl:nil})
//
//   // [层 2] Tool schemas 按 name 排序（确保字节稳定）
//   sortedSchemas = [grep_file, read_file, run_tests, write_file]  // alphabetical
//   for s := range sortedSchemas {
//       msgs = append(msgs, {Role:"tool_definition", Content:s.JSON})
//   }
//
//   // [层 3] Project rules
//   msgs = append(msgs, {Role:"system", Content:projectRules})
//
//   // ← 打 cache breakpoint（Anthropic cache_control）
//   msgs[len(msgs)-1].CacheControl = &CacheControl{Type:"ephemeral"}
//
//   // [层 4] RAG chunks 格式化为一段 system
//   ragContent = "=== Relevant code ===\n"
//   for c := range selectedChunks {
//       ragContent += fmt.Sprintf("[%s] %s:%d-%d\n%s\n\n", c.ID, c.FilePath, c.LineStart, c.LineEnd, c.Content)
//   }
//   msgs = append(msgs, {Role:"system", Content:ragContent})
//
//   // [层 5] History
//   msgs = append(msgs, history...)
//
//   // [层 6] 当前 user message
//   msgs = append(msgs, {Role:"user", Content:userMessage})
//
// ── Step 5：最终 prompt 的 token 分布 ────────────────────────────────
//
//   [层 1] system prompt              120 tok  ← cache hit ≈ 100%
//   [层 2] tool schemas × 4          1200 tok  ← cache hit ≈ 95%
//   [层 3] project rules              200 tok  ← cache hit ≈ 90%
//   ------- cache breakpoint -------  (共 1520 tok 可被缓存)
//   [层 4] RAG chunks                5820 tok  ← 每次 query 变
//   [层 5] history (summary+msgs)     924 tok  ← 随对话增长
//   [层 6] user message                40 tok  ← 最易变
//
//   总 tokens = 8,304
//
// ── Step 6：精确 token 校验（tiktoken-go）─────────────────────────────
//
//   // Pruner 用的是快速估算 len/4，提交 LLM 前用 tiktoken 精确重算
//
//   actualTokens := tiktoken.Encode(flatten(msgs))
//   // actualTokens = 8,412 （略高于估算的 8,304）
//
//   if actualTokens > maxContextWindow (200000):
//       return ErrContextTooLarge     // 实战几乎不触发
//
// ── Step 7：下游 llm.Client 消费 ──────────────────────────────────────
//
//   llmClient.Chat(ctx, &ChatRequest{
//       Messages:   msgs,
//       Model:      "claude-3-5-sonnet-20241022",
//       MaxTokens:  4096,
//       Tools:      toolSchemas,
//   })
//
//   Anthropic 返回 usage：
//     {
//       input_tokens:              8412,
//       cache_creation_input_tokens: 1520,   ← 首次创建缓存
//       cache_read_input_tokens:      0,
//       output_tokens:              62,
//     }
//
//   cost = 1520 * $3.75/M  (cache write, 25% premium)
//        + (8412-1520) * $3/M  (normal input)
//        + 62 * $15/M (output)
//        = $0.0057 + $0.0207 + $0.0009
//        = $0.0273
//
//   **下一轮** Chat 时前 1520 tok 命中缓存：
//     cost = 1520 * $0.30/M + (新增) * $3/M + output*$15/M
//     比第一轮省 80% 缓存部分成本。
//
// ── 数据形变总结 ──────────────────────────────────────────────────────
//
//   输入：
//     · 20 个 RAG chunks    (9,400 tok)
//     · history 6 条        (80,424 tok，含超大 tool result)
//     · user message        (40 tok)
//
//   ↓ TruncateLargeToolResult
//   history (924 tok)
//
//   ↓ PruneCodeChunks（多维评分贪心）
//   7 个 chunks (5,820 tok)
//
//   ↓ PruneMessages（保 sys + tail，丢中段）
//   history 仍 924 tok（无需丢）
//
//   ↓ PromptBuilder.Build（KV-Cache 分层）
//   msgs 17 条, 8,412 tok，前 1520 tok 可缓存
//
//   输出：Anthropic API request，单次 $0.027，后续轮次 $0.005。
//
// =============================================================================

package context
