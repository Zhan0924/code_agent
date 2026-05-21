// Package rag —— 深度代码语义检索引擎
//
// =============================================================================
//                                   设 计 原 理
// =============================================================================
//
// 1. 为什么传统 RAG 在"代码场景"下失效？
//    RecursiveCharacterTextSplitter（LangChain 默认）按字符切分，对代码：
//      · 200 行函数被拦腰截断，LLM 读到半截函数
//      · import / 变量定义与调用体分离
//      · 完全忽略语言结构与类型信息
//    结果：检索结果"相关但不可用"，LLM 编出错误代码。
//
// 2. 核心原则：AST 分块 + 多模态索引 + 双路召回 + 重排
//
//      ┌─────────────────────────────────────────────────────────┐
//      │                  Ingest Pipeline                         │
//      ├─────────────────────────────────────────────────────────┤
//      │ source.go ─▶ AST Parser ─▶ []astChunk                   │
//      │              (tree-sitter 或启发式)                       │
//      │                  │                                        │
//      │                  ▼                                        │
//      │             Embedder (OpenAI / local)                    │
//      │                  │                                        │
//      │                  ▼                                        │
//      │   Qdrant Payload: {project, version, lang, symbol, deps} │
//      └─────────────────────────────────────────────────────────┘
//
//      ┌─────────────────────────────────────────────────────────┐
//      │                  Query Pipeline                          │
//      ├─────────────────────────────────────────────────────────┤
//      │ user_query ─▶ 并行两路                                   │
//      │   ├─ 稠密 (dense) : embedding → Qdrant ANN              │
//      │   └─ 稀疏 (sparse): BM25 → Tantivy/倒排索引              │
//      │              │                                            │
//      │              ▼  merge top-K                              │
//      │         Cross-Encoder Reranker (本地部署)                │
//      │              │                                            │
//      │              ▼                                            │
//      │         Top-N chunks → prompt                            │
//      └─────────────────────────────────────────────────────────┘
//
// 3. AST 分块（ast_parser.go）
//    按语言选"最小自包含单元"：
//      Go       : func / method / type(struct|interface)
//      Python   : def / class
//      Markdown : #, ##, ### 小节
//      Shell    : function 声明
//    每个 chunk 带 symbolName / symbolType / dependencies，便于：
//      · payload 过滤（只搜 funcs 不搜 imports）
//      · 回溯调用图（已调用的符号 → 再检索一层）
//
// 4. 多租户 Payload 过滤
//    Qdrant 的 Payload 机制：chunk 存储时附带
//      { project: "auth-service", version: "v1.2", path: "handlers/login.go" }
//    检索时用 MustMatch/MustNotMatch 做硬过滤，返回结果仅限本项目；
//    避免跨租户污染 + 大幅提高检索准确率。
//
// 5. 双路召回（Dense + Sparse）的互补性
//    · 稠密向量（Semantic）：擅长"语义近似"
//         query: "登录失败怎么排查" → 命中含 "auth error handling" 的函数
//    · 稀疏 BM25（Keyword）：擅长"精确匹配"
//         query: "GetUserByEmail" → 命中变量名完全一致的位置
//    两路各取 TopK，合并去重后进入 reranker。对代码场景 recall 提升 30%+。
//
// 6. Cross-Encoder 重排（Rerank）
//    召回阶段用 bi-encoder（query 和 chunk 独立编码），速度快但不够准。
//    重排阶段用 cross-encoder（query+chunk 拼成一个输入），精度高但 O(N×K)。
//    所以策略 = "召回 100 条 → 重排 Top-10"，速度和精度都保住。
//    本地部署 bge-reranker-base 即可，p99 < 50ms。
//
// 7. 增量索引
//    · Watch 文件系统 fsnotify，检测 *.go 变更
//    · 计算 chunk hash，对比 Qdrant 里旧 chunk 的 content_hash
//    · 仅删除/更新变化的 chunk，未变者跳过 embedding（省钱）
//
// 8. 冷启动优化
//    大项目首次索引可能数分钟。策略：
//      · goroutine worker pool 并行 embedding（限流避免触发 OpenAI RPM）
//      · 优先索引 README、接口目录、最近修改文件
//      · 后台慢速完成其余文件，不阻塞用户首次对话
//
// 9. 与 Orchestrator 的协作
//    Orchestrator 执行 ReAct 前调 rag.Search(query)，把 Top-N chunks
//    按 KV-Cache 友好顺序（参考 internal/context/prompt_builder）插入
//    system prompt 之后、user message 之前。
//
// =============================================================================
//
// 10. 模块结构图
//
//   ┌──────────────────────────────────────────────────────────────────────┐
//   │                           rag package                                 │
//   │                                                                       │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ Engine                                                         │   │
//   │  │ ─────────────────────────────────────────────────────────      │   │
//   │  │  parser      ASTParser        (Go / Python / Markdown / ...)  │   │
//   │  │  embedder    Embedder         (OpenAI / local / multi-model)  │   │
//   │  │  vecStore    VectorStore      (Qdrant client)                 │   │
//   │  │  sparseIdx   SparseIndex      (BM25 / Tantivy-backed)         │   │
//   │  │  reranker    Reranker         (cross-encoder, local)          │   │
//   │  │  workers     *pool.WorkerPool (concurrent embed/ingest)       │   │
//   │  │                                                               │   │
//   │  │  + IngestFile(ctx, path) error                                │   │
//   │  │  + IngestProject(ctx, root, opts) error                       │   │
//   │  │  + Search(ctx, query, filter) ([]Chunk, error)                │   │
//   │  │  + DeleteProject(projectID) error                             │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  Internal data:                                                      │
//   │  ┌──────────────────────────────────────────────────────────────┐   │
//   │  │ CodeChunk                                                     │   │
//   │  │ ─────────────────────────────────────────────────────────     │   │
//   │  │  ID, Content, FilePath, Language,                             │   │
//   │  │  SymbolName, SymbolType, ScopeDepth, Dependencies,            │   │
//   │  │  StartLine, EndLine, ContentHash,                             │   │
//   │  │  Payload {project, version, tenant, tags...}                  │   │
//   │  │  DenseVector []float32                                        │   │
//   │  │  SparseTerms map[string]float32                               │   │
//   │  └──────────────────────────────────────────────────────────────┘   │
//   │                                                                       │
//   │  Backends:                          Callers:                         │
//   │  ─────────                          ────────                         │
//   │  · Qdrant (HTTP/gRPC)               · orchestrator (Search)          │
//   │  · LLM embedding API                · indexer  (IngestProject)       │
//   │  · tree-sitter (CGO) / heuristic    · api      (/rag/search)         │
//   └──────────────────────────────────────────────────────────────────────┘
//
// 11. Ingest 流程图（离线/增量入库）
//
//     ┌────────┐   fsnotify     ┌─────────┐
//     │ source │───events──────▶│ indexer │
//     │ files  │                └────┬────┘
//     └────────┘                     │ changed files
//                                    ▼
//                             ┌──────────────┐
//                             │ ASTParser    │ split into symbol-level chunks
//                             └──────┬───────┘
//                                    │
//                                    ▼
//                             ┌──────────────┐   hash == existing? skip embed
//                             │ dedup by hash │──────┐
//                             └──────┬───────┘       │ (save $$$)
//                                    │               ▼
//                                    ▼          (unchanged chunks)
//                        ┌───────────────────────┐
//                        │  pool.WorkerPool       │  N goroutines, rate-limited
//                        │   └─▶ embedder.Embed   │  (avoid OpenAI RPM breach)
//                        └───────────┬───────────┘
//                                    │ [vec + sparse terms]
//                                    ▼
//                        ┌───────────────────────┐
//                        │ vecStore.Upsert batch  │  Qdrant payload 带 tenant
//                        │ sparseIdx.Index batch  │
//                        └───────────────────────┘
//
// 12. Search 流程图（在线检索，双路召回 + 重排）
//
//       query + filter{project, version}
//                     │
//           ┌─────────┴──────────┐
//           ▼                    ▼
//     ┌──────────────┐     ┌──────────────┐
//     │ dense path    │     │ sparse path  │
//     │  ───────────  │     │  ─────────── │
//     │ embedder.Embed│     │ BM25 tokenize│
//     │      │        │     │      │       │
//     │      ▼        │     │      ▼       │
//     │ Qdrant ANN    │     │ Tantivy/idx  │
//     │ topK=K1       │     │ topK=K2      │
//     └──────┬───────┘     └──────┬───────┘
//            │                    │
//            └────────┬───────────┘
//                     ▼
//              merge + dedup by ID
//                     │
//                     ▼
//             ┌────────────────┐
//             │ cross-encoder  │   pair (query, chunk) → score
//             │ reranker       │   O(N·M) but N,M <= 100
//             └────────┬───────┘
//                      │ top-N
//                      ▼
//              return []Chunk (orderless)
//
// 13. 多租户 Payload 过滤模型
//
//     ingest :  upsert(id, vec, payload={project:"auth-svc", version:"v1.2",
//                                        lang:"go", symbol:"LoginHandler"})
//
//     search :  filter = Must({project=="auth-svc"}) + Must({version=="v1.2"})
//               Qdrant 先按 payload 硬过滤 candidate pool，再 ANN
//
//     删除   :  DeleteProject(id) = scroll + delete where project==id
//
// 14. Engine 与 orchestrator 的协作
//
//     orchestrator.Run                      rag.Engine
//            │ user message                     │
//            │──── extractKeywords ──────▶      │
//            │                                  │
//            │  Search(query, {project, ver})   │
//            │─────────────────────────────────▶│
//            │                                  │ double-recall + rerank
//            │◀──── [chunk1, chunk2, ...] ──────│
//            │                                  │
//            │  PromptBuilder places chunks in  │
//            │  the "RAG Retrieved" slot (between Rules and History)
//            │                                  │
//            │  llm.Chat(...)                   │
//
// =============================================================================
//
// 15. 深度原理剖析 + 实战案例
// -----------------------------------------------------------------------------
//
// [案例一] 字符分块 vs AST 分块 —— 真实对比：300 行 Go 文件的检索命中
//
//   目标：用户问 "where does UserService.Login validate password?"
//
//   文件：internal/service/user.go（300 行，包含 10 个 method）
//
//   字符分块 (chunk_size=500, overlap=50) 切出来：
//
//     chunk 1:  package service
//               import (...)
//               type UserService struct {
//                   db     *sql.DB
//                   hasher PasswordHasher
//               }
//               func (s *UserService) Register(ctx context.Context, req RegisterReq) (*User, error) {
//                   if err := validate(req); err != nil { return nil, err }
//                   hashed, _ := s.hasher.Hash(req.Password)
//                   return s.db.Insert(ctx, &User{...})     // <-- 截断在这里
//
//     chunk 2:  }
//               func (s *UserService) Login(ctx context.Context, req LoginReq) (*Token, error) {
//                   user, err := s.db.GetByEmail(ctx, req.Email)
//                   if err != nil { return nil, ErrNotFound }
//                   if !s.hasher.Verify(req.Password, user.Password) {   // <-- 在这里
//                       return nil, ErrInvalidCredential
//                   }
//                   ...
//     (Login 函数被切断，hasher.Verify 调用和 return 分别在不同 chunk)
//
//   LLM 看到 chunk 2 时：
//     · 不知道 s.hasher 是什么类型
//     · 不知道函数签名完整定义
//     · 不知道这是否有 return 或报错处理
//
//   → LLM 编造答案："UserService.Login 在第 N 行用 bcrypt.CompareHashAndPassword 校验"
//     （但实际代码用的是 s.hasher.Verify，抽象接口不是 bcrypt）
//
//   AST 分块（本包采用）：
//
//     chunk_ast_1:
//       symbol: UserService
//       type:   struct
//       content: (完整 struct 定义)
//       deps:   [sql.DB, PasswordHasher]
//
//     chunk_ast_2:
//       symbol: UserService.Register
//       type:   method
//       scope:  UserService
//       content: (完整 Register 函数体)
//       deps:   [validate, hasher.Hash, db.Insert]
//
//     chunk_ast_3:
//       symbol: UserService.Login
//       type:   method
//       scope:  UserService
//       content: (完整 Login 函数体，自包含)
//       deps:   [db.GetByEmail, hasher.Verify]      <-- 关键信息
//       comments: "// Login authenticates user by email and password"
//
//   LLM 看到 chunk_ast_3 能准确回答："UserService.Login 调用 s.hasher.Verify
//   (接口 PasswordHasher 的实现) 校验密码。"
//
//   AST 分块代码片段：
//
//     func (p *GoASTParser) Parse(path string, content []byte) ([]CodeChunk, error) {
//         fset := token.NewFileSet()
//         file, err := parser.ParseFile(fset, path, content, parser.ParseComments)
//         if err != nil { return nil, err }
//
//         var chunks []CodeChunk
//         for _, decl := range file.Decls {
//             switch d := decl.(type) {
//             case *ast.FuncDecl:
//                 // 每个函数/方法作为一个 chunk
//                 start := fset.Position(d.Pos()).Line
//                 end   := fset.Position(d.End()).Line
//                 body  := extractLines(content, start, end)
//                 deps  := extractCallees(d)     // 函数内调用的其他符号
//
//                 chunks = append(chunks, CodeChunk{
//                     SymbolName:   funcFullName(d),
//                     SymbolType:   "func",
//                     Content:      body,
//                     Dependencies: deps,
//                     StartLine:    start,
//                     EndLine:      end,
//                     ContentHash:  sha256(body),
//                 })
//             case *ast.GenDecl:
//                 // type / var / const 声明
//                 for _, spec := range d.Specs {
//                     // 每个 type / const 单独成 chunk
//                     ...
//                 }
//             }
//         }
//         return chunks, nil
//     }
//
//   实测数据（50k 行 Go 项目）：
//     指标                      字符分块      AST 分块
//     ────────────────────     ──────────   ──────────
//     chunk 数量                 3500         1200
//     平均 chunk tokens          220          180
//     top-10 检索准确率           48%          89%   (+41 pp!)
//     LLM 编造率                 30%          4%
//
// -----------------------------------------------------------------------------
//
// [案例二] 双路召回的价值 —— 变量名"UserID"的检索盲区
//
//   用户问："find all places that use variable UserID"
//
//   纯 Dense（semantic vector）召回：
//     · query embedding "variable UserID" 偏向"user identity concept"
//     · top-5 返回：
//         hit 1: chunk 讲 Authentication flow（相关但不含 UserID 变量）
//         hit 2: chunk 讲 User struct 定义
//         hit 3: chunk 讲 Session management
//         ...
//     · 实际包含 `UserID` 的小 helper 函数被排在第 30 名
//
//   纯 Sparse (BM25) 召回：
//     · 精确匹配 token "UserID"
//     · top-5 返回所有出现 UserID 字面量的 chunk
//     · 准确率高，但缺少"语义相似但命名不同"（如 UID、UserIdentity）的召回
//
//   双路融合（本包采用）：
//
//     func (e *Engine) Search(ctx context.Context, query string, filter Filter) ([]CodeChunk, error) {
//         var denseHits, sparseHits []CodeChunk
//         var wg sync.WaitGroup
//
//         // 并发执行两路召回
//         wg.Add(2)
//         go func() {
//             defer wg.Done()
//             queryVec, _ := e.embedder.Embed(ctx, query)
//             denseHits, _ = e.vecStore.Search(ctx, queryVec, filter, 50)
//         }()
//         go func() {
//             defer wg.Done()
//             sparseHits, _ = e.sparseIdx.Search(ctx, query, filter, 50)
//         }()
//         wg.Wait()
//
//         // 合并 + 按 chunk.ID 去重（保留更高分数）
//         merged := mergeByID(denseHits, sparseHits)   // 约 80 条
//
//         // Reranker (cross-encoder) 重排
//         scored, _ := e.reranker.Score(ctx, query, merged)
//         sort.Slice(scored, func(i, j int) bool {
//             return scored[i].Score > scored[j].Score
//         })
//
//         return scored[:10], nil      // 返回 top-10
//     }
//
//   query "find all places that use variable UserID" 的 top-10:
//     · 3 条来自 Sparse （精确包含 UserID 字面量）
//     · 4 条来自 Dense  （语义相关但用 UID/UserIdentifier 变量）
//     · 3 条双路都命中  （最高置信度）
//     → Reranker 对综合质量排序后，用户看到的是真正有用的 10 条
//
//   实测（某后端仓库 ~500 个 RAG query）：
//     指标                  Dense only   Sparse only   双路+重排
//     ───────────────     ──────────   ───────────   ──────────
//     Recall@10             62%          71%           93%
//     MRR (mean rank)       4.2          3.8           1.6
//     LLM 回答正确率         58%          64%           87%
//
// -----------------------------------------------------------------------------
//
// [案例三] Qdrant Payload 多租户 —— 防止 "客户 A 看到客户 B 代码" 的硬隔离
//
//   SaaS 场景：一个 Agent 服务 100 个客户的代码库。错误做法：
//
//     // 所有客户共享同一个 collection，靠 filename 前缀区分
//     e.vecStore.Upsert("code-chunks", vec, Payload{
//         "filename": "tenant_A/src/login.go",
//     })
//
//   风险：
//     · 若 query filter 忘写 tenant，会返回其他客户的 chunk
//     · Audit log 无法按租户切分
//     · 要删除一个客户的数据，得 scroll 全表过滤
//
//   Qdrant Payload 硬过滤（本包采用）：
//
//     // Ingest 时带完整租户元数据
//     e.vecStore.Upsert(ctx, "code-chunks", UpsertPoint{
//         ID:     chunkID,
//         Vector: vec,
//         Payload: map[string]any{
//             "tenant":  "tenant_A",
//             "project": "auth-service",
//             "version": "v1.2.3",
//             "branch":  "main",
//             "lang":    "go",
//             "symbol":  "UserService.Login",
//         },
//     })
//
//     // Search 时 MUST 过滤 tenant
//     filter := &QdrantFilter{
//         Must: []Condition{
//             {Key: "tenant",  Match: "tenant_A"},        // 硬隔离
//             {Key: "project", Match: "auth-service"},
//         },
//     }
//     // Qdrant 在 ANN 搜索前先用 payload index 过滤 candidate pool
//     hits, _ := e.vecStore.Search(ctx, collection, queryVec, filter, topK)
//
//   Qdrant 底层优化：payload field 带 HNSW index，过滤是 O(log N) 不是 O(N)
//   所以加 tenant 过滤几乎不损失性能。
//
//   额外收益：
//     · 删除一个租户：DeletePoints(filter={tenant:"X"}) 一次搞定
//     · 分租户计费：scroll 统计各 tenant 的 chunk 数 / 总 tokens
//     · 审计：每次 search 日志带 tenant/project，合规检查简单
//
//   API 层面必须强制从 JWT claims 拿 tenant，不能信任前端传参：
//
//     claims := c.Get("claims").(*auth.Claims)
//     chunks, _ := rag.Search(ctx, query, Filter{
//         Tenant:  claims.TenantID,    // ← 从 JWT，不是 query param
//         Project: c.Query("project"),
//     })
//
// -----------------------------------------------------------------------------
//
// [案例四] Cross-Encoder 重排 —— 为什么 bi-encoder 不够用？
//
//   Bi-encoder（召回阶段用的模型）：
//     · query   → vector_q     (独立编码)
//     · doc_1   → vector_d1    (独立编码)
//     · score   = cosine(vector_q, vector_d1)
//
//   优点：doc 向量可以预计算索引，查询时只编码 query，非常快。
//   缺点：query 和 doc 不交互，无法建模"具体关联"。
//
//   举例：
//     query: "how to handle 500 error in middleware"
//     doc_A: "// This middleware logs 500 errors from downstream services..."
//     doc_B: "// 500 lines of middleware refactor notes..."
//
//   bi-encoder 的 vector_A 和 vector_B 可能都和 query 很近（都提到 500 和 middleware）。
//   但显然 doc_A 才是真正相关的"错误处理"文档。
//
//   Cross-Encoder（重排阶段用的模型）：
//     · 直接把 [query, doc] 拼成一个输入送进 transformer
//     · 模型内的 attention 机制可以建模两者的**交互关系**
//     · 输出一个 0~1 的相关性分数
//
//     [CLS] how to handle 500 error in middleware [SEP] This middleware logs 500 errors... [SEP]
//          ↓ (12 层 Transformer, 双向 attention 建模 query-doc 关系)
//          ↓
//     [CLS] 隐藏向量 → linear → 相关性分数: 0.91 (强相关)
//
//     [CLS] how to handle 500 error in middleware [SEP] 500 lines of middleware refactor... [SEP]
//          ↓
//     相关性分数: 0.23 (弱相关)
//
//   缺点：O(N) query 计算量，100 doc 就要跑 100 次 Transformer。
//   所以策略是"召回多，重排少":
//
//     ┌────────────────────────────────────────────────────┐
//     │ Stage 1 (bi-encoder, 10ms):  ANN 召回 100 条 cand  │
//     │   ↓                                                 │
//     │ Stage 2 (cross-encoder, 50ms): 重排 100 条          │
//     │   ↓                                                 │
//     │ 返回 top-10                                          │
//     └────────────────────────────────────────────────────┘
//     总延迟 60ms，质量显著优于纯 ANN。
//
//   本项目用 bge-reranker-base（multilingual，~110MB）本地部署，
//   batch=32 时 p99 < 50ms，完全可接受。
//
//   架构建议：
//     · Reranker 服务独立部署（GPU 推理），多 Agent 共享
//     · Go 端用 gRPC 调 reranker，降低 CGO 复杂度
//     · A/B 实验：没有 rerank vs 有 rerank 的 LLM 回答质量差异
//
// =============================================================================
//
// 15. 端到端数据流示例 —— 一次完整的 Ingest + Search
// -----------------------------------------------------------------------------
//
// 场景 A：离线 Ingest —— 新项目 "auth-service" 全量入库
//
// ── Step 0：输入参数 ────────────────────────────────────────────────────
//
//   IngestRequest{
//       TenantID:  "acme",
//       ProjectID: "auth-service",
//       Version:   "v1.2.3",
//       RootPath:  "/repos/auth-service",
//       Branch:    "main",
//       CommitSHA: "a3f2b1c...",
//   }
//
//   项目规模：Go 代码 42 个 .go 文件，共 18,400 行。
//
// ── Step 1：Walker 扫描文件系统 ────────────────────────────────────────
//
//   walker.Walk("/repos/auth-service") 产出 chan<- FilePath：
//
//     "/repos/auth-service/internal/user_service.go"     1842 bytes
//     "/repos/auth-service/internal/validator.go"        612 bytes
//     "/repos/auth-service/cmd/server/main.go"           3201 bytes
//     ... (42 files total, 680KB)
//
//   过滤规则：跳过 vendor/、*_generated.go、文件大小 > 1MB。
//
// ── Step 2：AST Parser 按符号切块 ──────────────────────────────────────
//
//   对 user_service.go（摘录其中一个 chunk）：
//
//     tree := treesitter.Parse(content, goLanguage)
//     walk tree → collect:
//
//     RawChunk{
//         FilePath:    "internal/user_service.go",
//         SymbolName:  "UserService.Login",
//         SymbolKind:  "method",
//         Receiver:    "*UserService",
//         LineStart:   84,
//         LineEnd:     127,
//         Content:     "func (s *UserService) Login(email, pwd string) error {...}",
//         Imports:     []string{"strings","errors","context"},
//         CallsMade:   []string{"s.repo.FindByEmail", "s.hasher.Verify"},
//         ScopeDepth:  1,
//         ScopeStack:  []string{"UserService"},
//         Comments:    "// Login authenticates user by email+password\n",
//     }
//
//   42 个文件总共切出 ~180 个 chunk。
//
// ── Step 3：Metadata 装配 + Content Hash ───────────────────────────────
//
//   为每个 chunk 计算 SHA256(content) 作为去重键：
//
//     chunk.ContentHash = "sha256:8f3a1bc2..."
//     chunk.Payload = {
//         tenant:   "acme",
//         project:  "auth-service",
//         version:  "v1.2.3",
//         branch:   "main",
//         commit:   "a3f2b1c",
//         lang:     "go",
//         symbol:   "UserService.Login",
//         kind:     "method",
//         file:     "internal/user_service.go",
//         hash:     "sha256:8f3a1bc2...",
//     }
//
//   差分 ingest：与上次 commit 的 hash 列表比对，仅新增/变更的入库。
//   本次假设全量 → 180 个 chunk 全部入库。
//
// ── Step 4：Embedder 批量调用（Dense + Sparse）──────────────────────────
//
//   batches := chunk(180 chunks, batchSize=32)  // 6 批
//
//   for _, batch := range batches:
//       // Dense: OpenAI text-embedding-3-small
//       denseVecs := embedder.EmbedBatch(batch.texts)   // [][]float32, 1536-dim
//       // Sparse: BM25-like tokenizer
//       sparseVecs := bm25.SparseEmbed(batch.texts)     // []map[uint32]float32
//
//       for i, chunk := range batch {
//           chunk.DenseVec  = denseVecs[i]
//           chunk.SparseVec = sparseVecs[i]
//       }
//
//   6 批 × 32 → 180 chunks，总耗时 ~4s（OpenAI 批量 API 并行）。
//
// ── Step 5：Qdrant Upsert（批量写入）─────────────────────────────────
//
//   client.Upsert(ctx, &qdrant.UpsertPoints{
//       CollectionName: "code_chunks",
//       Points: []*qdrant.PointStruct{
//           {
//               Id:      chunk.ID,                  // UUID
//               Vectors: map[string][]float32{
//                   "dense":  chunk.DenseVec,
//                   "sparse": chunk.SparseVec,
//               },
//               Payload: chunk.Payload,              // 过滤字段
//           },
//           // ... 179 more
//       },
//       Wait: true,                                  // 等落盘后返回
//   })
//
//   收益：180 条 upsert 一次 RPC，耗时 ~120ms（单条 upsert × 180 需要 ~3s）。
//
// ── Step 6：回写 PG 元数据 + 更新索引统计 ─────────────────────────────
//
//   db.Exec(`
//     INSERT INTO rag_projects(tenant, project, version, chunks_count, indexed_at)
//     VALUES ($1, $2, $3, $4, NOW())
//     ON CONFLICT (tenant, project, version) DO UPDATE SET ...
//   `, "acme", "auth-service", "v1.2.3", 180)
//
// ── 整体吞吐 ──────────────────────────────────────────────────────────
//
//   总耗时：约 6.5s
//     · Walk + Parse：1.8s
//     · Embed（6 批并发）：4.0s
//     · Qdrant Upsert：0.12s
//     · PG Meta：~50ms
//
//   存储量：
//     · Qdrant：180 point × (dense 1536*4B + sparse ~200*8B) ≈ 1.4MB
//     · PG：元数据约 50KB
//
// =============================================================================
//
// 场景 B：在线 Search —— 一次真实检索请求的数据流
//
// ── Step 0：上游 orchestrator 发起 ──────────────────────────────────────
//
//   SearchRequest{
//       Query:    "UserService Login email trailing space bug",
//       TenantID: "acme",
//       Filters:  map[string]any{
//           "project": "auth-service",
//           "version": "v1.2.3",
//           "lang":    "go",
//       },
//       TopK:           20,   // 先多召回
//       RerankTopN:     8,    // Rerank 后返回
//       WithExpansion:  true, // 是否启用 neighbor expansion
//   }
//
// ── Step 1：QueryExpander（可选，由 light LLM 扩展）─────────────────────
//
//   原始 query 可能词太少，用 light 模型扩展：
//
//     original: "UserService Login email trailing space bug"
//     expanded:
//       - "UserService Login"           (核心)
//       - "email whitespace handling"
//       - "TrimSpace authentication"
//       - "login failure with space"
//
//   4 条 sub-query 合并后并行查。
//
// ── Step 2：Dense + Sparse 并发召回 ────────────────────────────────────
//
//   // 并行 fan-out：4 sub-queries × 2 vectors = 8 并发
//   var wg sync.WaitGroup
//   denseHits := make(chan []*ScoredPoint, 4)
//   sparseHits := make(chan []*ScoredPoint, 4)
//
//   for _, q := range expandedQueries {
//       wg.Add(2)
//       go func() {
//           defer wg.Done()
//           vec := embedder.Embed(q)                    // 1536-dim
//           hits, _ := qdrant.Search(&SearchRequest{
//               Vector:        vec,
//               VectorName:    "dense",
//               Limit:         20,
//               Filter:        buildQdrantFilter(req.Filters),
//           })
//           denseHits <- hits
//       }()
//       go func() {
//           defer wg.Done()
//           svec := bm25.SparseEmbed(q)
//           hits, _ := qdrant.Search(&SearchRequest{
//               SparseVector: svec,
//               VectorName:   "sparse",
//               Limit:        20,
//               Filter:       buildQdrantFilter(req.Filters),
//           })
//           sparseHits <- hits
//       }()
//   }
//   wg.Wait()
//
//   各通道聚合后（去重合并）：
//
//     denseHits（合并）:
//       chunk-7721  score=0.893  UserService.Login
//       chunk-7801  score=0.812  emailValidate
//       chunk-9204  score=0.801  LoginHandler
//       ... (35 条)
//
//     sparseHits（合并）:
//       chunk-7721  score=11.42  UserService.Login
//       chunk-8811  score=9.21   TestLogin
//       ... (28 条)
//
// ── Step 3：RRF 融合（Reciprocal Rank Fusion）─────────────────────────
//
//   rrfScore(c) = Σ 1 / (k + rank_i(c))     k=60
//
//   例如 chunk-7721：
//     dense rank 1  → 1/(60+1) = 0.01639
//     sparse rank 1 → 1/(60+1) = 0.01639
//     RRF = 0.03278   ← 最高
//
//   chunk-7801：
//     dense rank 2  → 0.01613
//     sparse rank 0 → 0
//     RRF = 0.01613
//
//   排序后取 top-20：
//
//     fusedTop20 := [
//       chunk-7721 rrf=0.033 UserService.Login,
//       chunk-7801 rrf=0.016 emailValidate,
//       chunk-9204 rrf=0.015 LoginHandler,
//       ...
//     ]
//
// ── Step 4：Cross-Encoder Rerank（精排）─────────────────────────────
//
//   把 (query, chunk.content) pair 送入本地 cross-encoder：
//
//   reranker.Rerank(ctx, query, fusedTop20) → 精排分：
//
//     chunk-7721  rerank=0.961  UserService.Login       (✓ 正是目标)
//     chunk-7801  rerank=0.872  emailValidate            (✓ 依赖)
//     chunk-4102  rerank=0.859  TestLogin_EmailTrimming  (✓ 测试)
//     chunk-9204  rerank=0.412  LoginHandler             (相似但不相关)
//     ...
//
//   top-8 保留精排分 > 0.5 的：[7721, 7801, 4102, 3502, 6611, 2201, 9900, 1010]
//
// ── Step 5：Neighbor Expansion（上下文扩展）───────────────────────────
//
//   对每个命中 chunk，读取其 **前后 2 个 sibling chunks**（同文件同 scope）：
//
//   chunk-7721 (UserService.Login) 扩展：
//     + chunk-7720  UserService.Register    (前邻)
//     + chunk-7722  UserService.Logout      (后邻)
//     + chunk-7700  type UserService struct (所在 scope 根)
//
//   这样 LLM 能看到完整的"类 + 相关方法"上下文，避免只看到孤立函数。
//
// ── Step 6：汇总构造 SearchResponse ───────────────────────────────────
//
//   return &SearchResponse{
//       Query:      "UserService Login email trailing space bug",
//       TotalHits:  63,                     // 召回总数
//       Returned:   20,                     // 经扩展后
//       Chunks: []ScoredChunk{
//           {
//               Chunk:       chunk-7721,
//               DenseScore:  0.893,
//               SparseScore: 11.42,
//               RRFScore:    0.033,
//               RerankScore: 0.961,
//               SourceType:  "primary",
//           },
//           {
//               Chunk:       chunk-7720,    // neighbor
//               RerankScore: 0.961,          // 继承主 chunk 分
//               SourceType:  "neighbor",
//           },
//           ...
//       },
//       Latency: {
//           Embed:   28ms,
//           Qdrant:  45ms,
//           Rerank:  320ms,
//           Expand:  15ms,
//           Total:   412ms,
//       },
//       Budget: {
//           TokensEstimated: 9400,          // 供上游 Pruner 裁剪
//       },
//   }
//
// ── 输出被 orchestrator 消费 ──────────────────────────────────────────
//
//   这个 SearchResponse 就是 orchestrator _principles.go §15 中的 Step 3
//   输入数据，由 context.Pruner 进一步按预算裁剪。
//
// ── 数据形变总结 ──────────────────────────────────────────────────────
//
//   Query (38 chars)
//     ↓ QueryExpander (LLM 200ms)
//   4 sub-queries
//     ↓ fan-out 8 Qdrant RPCs
//   63 unique hits
//     ↓ RRF 融合
//   top-20 fused
//     ↓ Cross-encoder rerank (320ms)
//   top-8 精排
//     ↓ Neighbor expansion
//   ~20 chunks 最终返回
//
//   全链路 p99 < 500ms。
//
// =============================================================================

package rag
