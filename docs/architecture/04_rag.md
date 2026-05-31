# 04 · RAG 引擎 `internal/rag`

> 代码（共 2491 行，9 个文件）：
> - `engine.go` (283) — 核心编排：索引 / 双路召回 / 重排
> - `qdrant_store.go` (343) — Qdrant gRPC 客户端，实现 `VectorStore`
> - `ast_native.go` (209) + `ast_parser.go` (239) — Go/Python/通用 AST 解析
> - `markdown_parser.go` (417) — Markdown/Shell 文档感知切分
> - `embedder.go` (107) — 远程 OpenAI 兼容 embedding
> - `local_embedder.go` (129) — 本地 hash-based fallback embedding
> - `reranker.go` (141) — 远程 cross-encoder 重排服务
> - `reconnect.go` (99) — Qdrant/embedding 断连后自恢复
>
> 测试：`ast_native_test.go` (145) + `markdown_parser_test.go` (379)

---

## 1. 模块定位

**"给 Agent 一个懂代码的长期记忆。"**

不是把代码当成普通文本塞进向量库，而是：

1. **语义切分**：沿 AST 边界切，一个函数就是一个 chunk（不再在 `if` 语句中间硬断开）；
2. **双路召回**：语义向量（dense）+ 精确匹配（BM25/sparse）并行跑，结果合并；
3. **交叉重排**：用本地 cross-encoder 二次打分，把 "看起来像但其实不相关" 的项踢掉；
4. **租户隔离**：payload 里记 `project/version/language`，检索时作为硬过滤，避免跨项目串扰；
5. **优雅降级**：远程 embedder 挂了就用本地 hash embedder，reranker 挂了就用原始分数 —— 服务永不全白。

---

## 1.5 设计哲学：代码 RAG 的独特挑战

通用文档 RAG（LlamaIndex / LangChain 默认方案）直接套到代码上效果很差。
本节列出代码 RAG **不同于文本 RAG** 的 5 个本质差异，以及本系统的应对。

### D1 — 切块单位：**函数**，不是段落

**文本 RAG**：按字符数 sliding window（默认 512 tokens + 64 overlap）。

**代码 RAG 的问题**：一个 80 行函数从 40 行被切开 → 前半段缺 return
类型、后半段缺函数签名。LLM 拿到后二义性极高。

**对策**：AST 切块（[§4](#4-ast-切块)）。Go 用 `go/parser`，Python 用
缩进启发式，Markdown 用 heading，Shell 用函数关键字。每块一个顶层
FuncDecl/GenDecl/Class，附 leading comment。

### D2 — 召回线索：**标识符**，不是语义

**文本**：用户问"公司 2024 年营收"，embedding 能找到"Annual revenue was..."
（语义近）。

**代码**：用户问"JWT 解析在哪"，embedding 能找到 `decodeJWT` 或
`parseToken`——但也可能找到 `parseInt`（都是 parse），或 `decodeAES`
（都是 decode）。**标识符完全匹配**比"语义近"更信号强。

**对策**：双路召回。Dense（embedding）抓语义相近；Sparse（BM25）抓
标识符、函数名、错误字符串。Reranker 对合并结果做精排。

### D3 — camelCase / snake_case 分词

**文本 tokenizer**：空格拆词。

**代码问题**：标识符 `NewHTTPClient` 在文本 tokenizer 里是一个 token。
用户查"http client" 永远 match 不到——除非做特殊拆分。

**对策**：`bm25.go:splitCamel` 同时保留原词和子词：
```
"NewHTTPClient" → ["NewHTTPClient", "New", "HTTP", "Client"]
```
保留原词 → 精确查 `NewHTTPClient` 仍命中；
拆成子词 → 查 `http client` 也命中 HTTP 和 Client。

### D4 — 多租户硬过滤

**文本 RAG**：租户间文档完全隔离，不同 index / 不同 collection。

**代码场景**：多项目共享 Agent，同一 collection 里可能有 `project=auth`
和 `project=billing` 两类 chunk。查询 auth 项目时必须**硬过滤**掉
billing——否则 billing 的 `parseToken` 污染 auth 的检索结果。

**对策**：Qdrant payload filter：
```go
Filter{Must: [{Key: "project", Match: req.ProjectName}]}
```
**必**而不是 should——不允许跨项目召回。

### D5 — 增量更新与稀疏索引

**文本场景**：全量重建一次，索引半天不变。

**代码场景**：一次 IDE 保存就可能让几个 chunk 失效。dense 索引可以
逐条 upsert 到 Qdrant；**sparse 索引是进程内的**（BM25Index），需要
增量维护以保持同步。

**对策**（已实现）：
- Dense：upsert 即生效（Qdrant 原生支持）
- Sparse：`AddChunks`/`RemoveChunks` 增量更新，`Upsert`/`Delete` 后立即同步
- 首次查询时从 Qdrant 全量 scroll 构建 BM25 索引，后续增量维护
- 文件 watcher 集成：文件变更自动触发增量索引（可选，通过 `watch_enabled` 配置）

### 核心决策树

```
是否需要查代码？
 │
 ├── 否（查文档 / commit / PR） → 沿用通用 RAG
 │
 └── 是 →
      │
      ├── 查询含明确标识符？
      │    ├── 是 → Sparse（BM25）优先，权重高
      │    └── 否 → Dense 优先
      │
      ├── 跨项目 / 多租户？
      │    └── 是 → payload filter 必加
      │
      └── 需要 top-K 精度？
           ├── K ≤ 3 → 必须 rerank
           └── K ≥ 30 → 双路 + rerank 才能稳
```

---

## 2. 依赖架构

```
            ┌─────────── orchestrator / indexer ───────────┐
            │ .IndexCode()   .Retrieve()                   │
            └────────────────────┬─────────────────────────┘
                                 ▼
                      ┌───────────────────┐
                      │   rag.Engine       │ ← 编排器
                      │  cfg.TopK/TopN/…   │
                      └──┬───────┬─────┬──┘
             ┌───────────┘       │     └─────────────┐
             ▼                   ▼                   ▼
       Embedder             VectorStore          Reranker
       (interface)          (interface)          (interface)
        │                     │                     │
        │                     │                     │
   ┌────┴─────┐         ┌─────┴─────┐         ┌─────┴─────┐
   │ OpenAI   │         │ Qdrant    │         │ APIReranker│
   │ Embedder │         │ Store     │         │ (cross-enc)│
   └──────────┘         │ (gRPC)    │         └────────────┘
   │ Local    │         └───────────┘
   │ Hash     │                ▲
   │ Embedder │                │
   └──────────┘                │
                         ┌─────┴──────┐
                         │ parseCode  │
                         │ Chunks()   │
                         │  - Go AST  │
                         │  - Python  │
                         │  - MD/sh   │
                         │  - Generic │
                         └────────────┘
```

---

## 2.5 数据流总览

下图将本模块各链路串成一张端到端视图，各步骤详见后续对应章节。

### 2.5.1 索引链路 (IndexCode)

```text
┌──────────────────────────────────────────┐
│ indexer.IndexRepository / API 上传新文件  │
└──────────────────────┬───────────────────┘
                       │ (filePath, content)
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ rag.Engine.ProcessFile(filePath, content)                     │
└──────────────────────┬──────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ AST Parser 路由 (按语言选择):                                │
│   .go   → Go AST (go/parser, 函数级切块)                    │
│   .py   → Python indentation parser                         │
│   .md   → Heading splitter                                  │
│   .sh   → Function block splitter                           │
│   其他  → Generic (行数窗口切块, 150行/块)                   │
└──────────────────────┬──────────────────────────────────────┘
                       │ ([]CodeChunk: content + metadata)
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ Build Embedding Text (per chunk):                            │
│   "File: {path}\nLanguage: {lang}\n{symbols}\n{content}"    │
│   → 给 embedder 提供足够上下文                               │
└──────────────────────┬──────────────────────────────────────┘
                       │ ([]string: embedding texts)
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ Embedder.Embed(texts) → 【OpenAI /embeddings API】          │
│  batch size=10, 维度=1536 (DashScope text-embedding-v4)       │
│  fallback: LocalHashEmbedder (无网络时降级,低精度)            │
└──────────────────────┬──────────────────────────────────────┘
                       │ ([][]float32: vectors)
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ BM25 Indexer 同步更新:                                       │
│   tokenize(content) → camelCase/snake_case split            │
│   计算 TF → 更新 IDF → 存入内存倒排索引                     │
└──────────────────────┬──────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ 【Qdrant】 Upsert                                            │
│  ID: UUID5(namespace, filePath + "#" + chunkIdx)            │
│  Vector: dense embedding                                    │
│  Payload: {path, language, symbols, start_line, end_line}   │
│  → 确定性 UUID 保证重复索引幂等                              │
└─────────────────────────────────────────────────────────────┘
```

### 2.5.2 检索链路 (Retrieve)

```text
┌─────────────────────────┐
│ orchestrator.reactLoop  │
│ rag.Engine.Retrieve(    │
│   query, topK=10)       │
└────────────┬────────────┘
             │
             ▼
┌─────────────────────────────────────────────────────────────┐
│ Query Preprocessing:                                         │
│  normalize + extract keywords                                │
└────────────┬────────────────────────────────────────────────┘
             │
      ┌──────┴──────────────────────────────┐
      │ (并行执行)                           │
      ▼                                     ▼
┌──────────────────────────┐  ┌──────────────────────────────┐
│ Dense Path:              │  │ Sparse Path (BM25):          │
│  Embedder.Embed(query)   │  │  tokenize(query)             │
│  → 【Qdrant】cosine     │  │  → 内存倒排索引              │
│    search, limit=topK*2 │  │  → TF-IDF score              │
│  → []ScoredPoint        │  │  → top-K heap                │
└────────────┬─────────────┘  └──────────────┬───────────────┘
             │                               │
             └───────────────┬───────────────┘
                             ▼
┌─────────────────────────────────────────────────────────────┐
│ RRF (Reciprocal Rank Fusion):                                │
│  score(d) = Σ 1/(k + rank_i(d))  for each retriever         │
│  去重 (by chunk ID) + 合并分数                               │
└────────────────────────────┬────────────────────────────────┘
                             │ (merged top-N candidates)
                             ▼
┌─────────────────────────────────────────────────────────────┐
│ Reranker (可选, CrossEncoder):                               │
│  对 top-N pairs (query, chunk.content) 打精排分              │
│  → 重排序 → 返回 top-K (K ≤ N)                              │
│  未配置时 → 直接用 RRF 分数排序                              │
└────────────────────────────┬────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────┐
│ []RetrievalResult{Chunk, Score, FilePath, Lines}             │
│ → 返回 orchestrator → 注入 PromptBuilder Code Context 区    │
└─────────────────────────────────────────────────────────────┘
```

---

三个核心 interface 在 `engine.go` 顶部定义（整个包的依赖倒置中心）：

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type VectorStore interface {
    Upsert(ctx, chunks []models.CodeChunk) error
    SearchDense(ctx, vector []float32, topK int, filters map[string]string) ([]RetrievalResult, error)
    SearchSparse(ctx, query string, topK int, filters map[string]string) ([]RetrievalResult, error)
    Delete(ctx, ids []string) error
    Close() error
}

type Reranker interface {
    Rerank(ctx, query string, results []RetrievalResult, topN int) ([]RetrievalResult, error)
}
```

**任意实现可 swap**：测试里拿 in-memory fake；生产里远程 Qdrant + OpenAI embedding + 本地 cross-encoder。

---

## 3. 索引链路 `IndexCode()`

```
输入：filePath, language, content, metadata
  │
  ▼
┌───────────────────────────────────────────┐
│ 1. parseCodeChunks()                      │
│    - 按语言选解析器：Go/Py/MD/Sh/Generic   │
│    - 产出 []models.CodeChunk              │
│    - 每个 chunk 带 Symbol{Name,Type}、    │
│      StartLine/EndLine、Dependencies      │
└─────┬─────────────────────────────────────┘
      │
      ▼  len == 0? warn 并返回（空文件/注释文件）
┌───────────────────────────────────────────┐
│ 2. buildEmbeddingText(chunk)              │
│    "<type>:<symbol>\n<content>"           │
│    刻意把 symbol name 拼进 text，          │
│    让 dense embedding 天然对 symbol        │
│    也有语义感知                           │
└─────┬─────────────────────────────────────┘
      │
      ▼
┌───────────────────────────────────────────┐
│ 3. embedder.Embed(ctx, texts) → vectors   │
│    批量一次调用（成本最优）                 │
└─────┬─────────────────────────────────────┘
      │
      ▼  chunks[i].Embedding = vectors[i]
┌───────────────────────────────────────────┐
│ 4. store.Upsert(ctx, chunks)              │
│    - Qdrant payload 带上全部 metadata     │
│    - ID = deterministicUUID(filePath+L+R) │
│      保证同 chunk 重复入库是 overwrite     │
└───────────────────────────────────────────┘
```

### 3.1 `deterministicUUID` — 重入索引的幂等性保证

```go
// 位置：qdrant_store.go:22
func deterministicUUID(key string) string {
    h := sha1.Sum([]byte(key))
    return uuid.FromBytes(h[:16]).String()   // 同样 key → 同样 UUID
}
```

好处：当你 reindex 一个修改过的文件，旧 chunk 自动被同 ID 的新 chunk 覆盖；不会出现"旧+新"两份在库里打架。

---

## 4. AST 解析器

不同语言用不同策略，一律输出统一的 `astChunk`：

```go
type astChunk struct {
    Content      string
    StartLine    int
    EndLine      int
    SymbolName   string
    SymbolType   string // "func", "type", "class", "method", "section"…
    ScopeDepth   int    // 用于 context/pruner 做粒度裁剪
    Dependencies []string
}
```

### 4.1 `ast_native.go` — Go 原生 AST (首选)

Go 文件走 **标准库 `go/parser`**，最稳：

```go
parseGoCodeNative(content string) []astChunk {
    fset := token.NewFileSet()
    file, err := parser.ParseFile(fset, "", content, parser.ParseComments)
    // → 遍历 Decls:
    //     FuncDecl → extractFuncChunk
    //     GenDecl (type/const/var) → extractTypeChunk / extractValueChunk
}
```

重点：

- `extractCallDeps(body *ast.BlockStmt)` 分析函数体，抓出**函数调用的 selector**（`http.Get`, `db.Query` 等）→ 写入 `Dependencies` 字段；
- 能正确处理接收者 → `SymbolName = "UserService.Save"`；
- 会 skip `init()`、空函数体（降低噪声）。

### 4.2 `ast_parser.go` — Python/Fallback 轻量解析

用**正则 + 状态机**，不依赖 tree-sitter（编译复杂、MCP 场景部署麻烦）：

- `parsePythonCode` — 识别 `def`/`async def`/`class`，按缩进抓作用域；
- `parseGenericCode` — 空括号匹配，识别 `{ ... }` 块；C/Java/Rust/JS 都可对付；
- `extractGoDependencies` — 在无 AST 可用的场景下，用词法 fallback；
- `isGoKeyword` / `isValidShellIdentifier` — 避免把关键词当符号。

缺点：对复杂 Python（装饰器链、嵌套类）有边界情况，但稳、可调。

### 4.3 `markdown_parser.go` — 文档/Shell

这是被低估的关键文件（417 行）：

- `splitMarkdownSections` 按 ATX 标题 (`#`, `##`, …) 切分，每段独立成 chunk；
- `buildHeadingPath` 为每段生成 "Guide > Install > Linux" 的面包屑，塞进 `SymbolName`；
- `mergeSmallSections(sections, minChars)` 合并过短段落（< 200 字符），避免被 embedder 当噪声；
- `parseShellScript` 识别 `function foo() { ... }` 和 `foo() { ... }` 两种语法，抽取 shell 函数。

**为什么独立做？** 因为技术文档（README、架构设计、API 规范）是 Agent RAG 最常被问的 60%，远比代码检索频繁。

### 4.4 解析路由（`parseCodeChunks`）

```
language:
  "go"       → parseGoCodeNative
  "python"   → parsePythonCode
  "markdown" → parseMarkdown
  "shell"    → parseShellScript
  otherwise  → parseGenericCode
```

---

## 5. 检索链路 `Retrieve()`

```
输入：query, filters(map[string]string)
  │
  ├── 1. embedder.Embed(ctx, [query]) → queryVec
  │                                     │
  │    ┌────────────────────────────────┴──────────────────┐
  │    │                                                   │
  │    ▼                                                   ▼
  │  goroutine A:                              goroutine B:
  │  store.SearchDense(queryVec, topK, f)      store.SearchSparse(query, topK, f)
  │    │                                                   │
  │    │  ┌──── chan (cap=2) ──────────────────────────────┘
  │    └─►│
  │       ▼
  │   收集 2 条结果：
  │   - 失败则 log.Warn 并跳过（不中断）
  │   - 给每条打 Source = "dense" | "sparse"
  │
  ├── 2. deduplicateResults()        ← 同 Chunk.ID 保留高分那条
  │
  ├── 3. if cfg.RerankEnabled && reranker != nil:
  │        reranked = reranker.Rerank(ctx, query, all, topN)
  │        成功 → return
  │        失败 → log.Warn，fallthrough 使用原始分数
  │
  └── 4. sort by Score desc → trim to topN → return
```

### 5.1 双路召回的必要性

| 场景                     | Dense 向量 | Sparse BM25  |
|--------------------------|-----------|-------------|
| "如何处理空指针"           | ✅ 强      | ❌ 差       |
| `UserService.Save`       | ⚠️ 模糊    | ✅ 精确      |
| `error code 'ECONNRESET'`| ⚠️ 有干扰  | ✅ 几乎完美  |
| "接口超时的重试机制"       | ✅ 强      | ❌ 不好     |

变量名、error code 这类 **Dense 会"理解偏"** 的场景，必须靠 BM25 补回来。

### 5.2 并行通过 chan 聚合，不是 WaitGroup

代码里用 `chan` cap=2 而非 `WaitGroup` —— 原因是 **Context Cancel 传播**：一旦有一路返回错误，其他还在跑的 gRPC 会通过 ctx 被取消，chan 仍然能收完 —— 不会漏消息。

---

## 6. Qdrant Store `qdrant_store.go`

### 6.1 Collection 结构

```go
ensureCollection(ctx) {
    if !exists(collection) {
        CreateCollection{
            Name: cfg.Collection,
            VectorsConfig: {
                Size: cfg.VectorSize,     // 例：1536 (text-embedding-3-small)
                Distance: Cosine,
            },
        }
    }
    // 自动为常用 payload field 建索引（加速 filter）：
    //   project, version, language, symbol_name
}
```

### 6.2 Upsert

每个 `CodeChunk` → Qdrant Point：

```go
Point{
    Id:      deterministicUUID(chunk.ID),
    Vectors: chunk.Embedding,             // dense
    Payload: {
        file_path, language,
        symbol_name, symbol_type,
        content, start_line, end_line,
        dependencies, metadata.*
    }
}
```

**注意**：Embedding 字段被从 payload 中剥离（避免双份存储）；回读时由 `payloadToCodeChunk` 重建除 Embedding 外的完整 CodeChunk。

### 6.3 SearchDense

```go
SearchPoints{
    CollectionName: cfg.Collection,
    Vector:         query,
    Limit:          topK,
    Filter:         buildQdrantFilter(filters),  // 硬过滤
    WithPayload:    true,
}
```

### 6.4 SearchSparse — 真实 BM25 实现

> ⚠️ **2026-05 更新（P0 #18 修复）**：此节曾描述为"基于 MatchText 的过渡
> 方案 + score 硬编码 0.5"——那是 stub，根本不能叫 ranking。现在换成了
> 标准 BM25，下面是实际实现。

#### 6.4.1 设计

`rag/bm25.go` 提供一个**进程内** `BM25Index`：
```go
type BM25Index struct {
    docs    []bm25Doc  // {chunk, length, tf map[term]count}
    df      map[string]int  // term → 包含该 term 的文档数
    numDocs int
    avgLen  float64
}
```

**流程**：
1. **索引构建** — `Build([]CodeChunk)`：对每个 chunk 的 content+symbol 做 tokenize，
   累积每 term 的文档频率 df（DF 表）；记录每个文档的长度和 TF 向量。
2. **查询** — `Search(query, topK)`：
   - 对 query 做同样的 tokenize；
   - 对每个 doc 计算 BM25 分数（见公式）；
   - 最小堆维护 topK。

**BM25 公式**（Robertson-Sparck Jones 变体，Lucene 标准）：

$$
\text{score}(q, d) = \sum_{t \in q} \text{IDF}(t) \cdot \frac{tf_{t,d} \cdot (k_1 + 1)}{tf_{t,d} + k_1 \cdot (1 - b + b \cdot \frac{|d|}{avgdl})}
$$

- $k_1 = 1.2$（TF 饱和参数），$b = 0.75$（长度归一化强度）——Lucene 默认
- IDF = $\log((N - df + 0.5) / (df + 0.5))$，负值 clamp 到 0（过于常见的
  term 没有区分度）

#### 6.4.2 Tokenizer（核心召回质量）

```go
// "func NewHTTPClient()" → ["func", "newhttpclient", "new", "http", "client"]
func tokenizeForBM25(text string) []string {
    raw := strings.FieldsFunc(text, notLetterDigit)
    for _, w := range raw {
        for _, sub := range splitCamel(w) {   // 保留原 + 拆 camelCase
            s := strings.ToLower(sub)
            if len(s) < 2 || isStopword(s) { continue }
            out = append(out, s)
        }
    }
}
```

关键点：
- **camelCase 分拆**：查询 `http client` 能命中标识符 `HTTPClient`；
- **保留原词**：精确匹配完整标识符 `NewHTTPClient` 仍然有高 TF；
- **stopword**：英文 the/a/is 不进入索引（减小索引体积、提升检索精度）；
  代码 keyword（`func`/`var`/`import`）**不**当 stopword（它们在代码搜索
  里有意义）。

#### 6.4.3 索引生命周期

```go
// QdrantStore 懒构建 + TTL 刷新
sparseMu      sync.Mutex
sparseIndex   *BM25Index
sparseBuiltAt time.Time
sparseTTL     time.Duration  // 默认 5 分钟

func ensureSparseIndex() {
    if !built || time.Since(builtAt) > ttl {
        chunks := scrollAllChunks()  // 分页拉全部 chunk
        sparseIndex.Build(chunks)
        builtAt = time.Now()
    }
}
```

- 第一次 `SearchSparse` 触发全量扫 + 构建；之后 5min 内复用；
- `Upsert` 之后 UI 想立刻搜到 → 调 `InvalidateSparseIndex()` 强制下次重建；
- 并发 `Search` 共用同一 index；只有 rebuild 路径拿 mu.Lock。

#### 6.4.4 规模与演进

- **上限 ~100k chunks** / 几十 MB RAM / 查询 sub-10ms；
- 超过后：切到 Qdrant 原生 sparse vector（Qdrant 1.8+ 支持），或外接
  Meilisearch / Elasticsearch。**不再像 stub 时代那样"TODO"——而是
  有明确 trigger 的架构升级路径**。

**验证用例**（`rag/bm25_test.go`）：
- `TestBM25_BasicRanking` — 相关文档排首位
- `TestBM25_CamelCaseTokenization` — `http client` 查到 `NewHTTPClient`
- `TestBM25_IDFDownweightsCommonTerms` — 高频词不干扰排名
- `TestBM25_UnknownTermsReturnEmpty` — 无匹配时返回空切片（而非 nil）

### 6.5 `buildQdrantFilter(filters)` — 租户隔离

```go
filters = {
    "project": "auth-service",
    "version": "v1.2",
    "language": "go",
}
   │
   ▼
Filter {
    Must: [
      FieldCondition{ key: "project",  match_value: "auth-service" },
      FieldCondition{ key: "version",  match_value: "v1.2" },
      FieldCondition{ key: "language", match_value: "go" },
    ]
}
```

**硬过滤** = 检索前先过这一层，TopK 只在匹配集合内计算 —— 精度和性能都优于"召回后再过滤"。

---

## 7. Embedder 实现

### 7.1 `OpenAIEmbedder` — 生产默认

```go
NewOpenAIEmbedder(ragCfg, llmFallback, logger)
```

要点：

- 独立的 `BaseURL/APIKey/Model`（通常是 `text-embedding-3-small` 或 `bge-m3`）；
- `llmFallback` 用意：如果 RAG 专用的 embedding endpoint 没配，就退到主 LLM 的 BaseURL（方便本地开发）；
- 自动批处理：一次 `/embeddings` 接受多个 text，上限由 cfg.EmbeddingBatchSize 控。

### 7.2 `LocalHashEmbedder` — 零外部依赖降级

```go
NewLocalHashEmbedder(dim int, logger) *LocalHashEmbedder
```

算法（不是"高级"但**很鲁棒**）：

1. `tokenize(text)` — 按字母数字+下划线切；
2. 每个 token 用 FNV64 hash 落入一个维度，权重递增；
3. `l2Normalize` 归一化。

**用途**：

- 单元测试不依赖网络；
- 生产环境 embedding endpoint 挂了的短暂 fallback（质量比远程差，但**检索不全白**）；
- 私密代码场景，用户不想把代码送给外部 embedding 服务。

### 7.3 为什么不集成本地 sentence-transformers？

考虑过。拒了的原因：

- 要带 Python 运行时，容器镜像从 50MB 膨胀到 ~1GB；
- 要 GPU 才跑得快，CI 环境不现实；
- 用户想要本地 embedding 时，MCP 的"embedding server"是更优解（外置、可独立扩缩容）。

---

## 8. Reranker `reranker.go`

```go
APIReranker.Rerank(ctx, query, results, topN) → reranked[]
```

接口假设是一个 **cross-encoder HTTP 服务**（例如本地起的 `bge-reranker-v2-m3`）：

```json
POST /rerank
{
    "query": "...",
    "documents": [ "chunk content 1", "chunk content 2", ... ],
    "top_n": 10
}
```

- 返回 `{index, relevance_score}` 数组；
- 按返回顺序重排 results 数组；
- 超时或失败时，`engine.Retrieve` 会 fallthrough 到原始 Score 排序（静默降级，不抛错）。

---

## 9. `ReconnectManager` —— 自愈

```go
type ReconnectManager struct {
    createEngine func(ctx) (*Engine, error)
    engine       atomic.Pointer[Engine]   // 或 sync.RWMutex
    stopCh       chan struct{}
}

Start(ctx, interval) {
    go loop {
        if engine == nil || engine.HealthCheck() fails {
            tryReconnect(ctx)
        }
        sleep(interval)
    }
}
```

- 启动时如果 Qdrant 还没起（docker-compose 启动顺序）→ `engine == nil`；
- 每 `interval` 重试一次建连；建成功后 `SetEngine(e)`；
- 调用方（orchestrator）通过 `GetEngine()` 拿到当前有效的 engine，可能是 nil（应 fallback 到无 RAG 回答）。

这让整体启动可以**非阻塞**：即使 Qdrant 晚 30s 起来，HTTP server 已经在服务。

---

## 10. 关键配置

```yaml
rag:
  chunk_max_tokens: 512
  top_k: 20                  # 双路各自召回的数量
  rerank_enabled: true
  rerank_top_n: 10           # 重排后最终返回数
  rerank_url: "http://localhost:8000/rerank"

  embedding_base_url: "https://api.openai.com/v1"
  embedding_api_key: "${OPENAI_API_KEY}"
  embedding_model: "text-embedding-3-small"
  embedding_batch_size: 10
```

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| **三接口 + 依赖倒置** (Embedder/VectorStore/Reranker) | 每一层都可换 mock/本地/远程；测试零网络依赖 |
| AST 解析用 Go 标准库 而非 tree-sitter | 零 C 依赖 → 静态二进制简单；Python/其他用正则 fallback |
| Markdown 解析独立成 417 行 | 文档检索频率远高于代码；值得精细处理（heading path、合并短段） |
| 索引时**把 symbol 拼进 embedding 文本** | dense 天然对 symbol 敏感，不用单独开 field |
| 双路召回用 **chan** 聚合 | ctx cancel 传播顺畅；天然支持"一路失败仍可返回" |
| **Reranker 失败降级而非报错** | 检索是 Agent 辅助能力，不应因为重排挂了而拒绝服务 |
| Qdrant payload 建索引字段固定 | 新 project/version 无须 DDL；检索端只改 filters map |
| `deterministicUUID` 入库 | reindex 幂等；不需要先 Delete 再 Upsert |
| `LocalHashEmbedder` 作为备胎 | 测试/私密场景可离线；一行切换 |
| `ReconnectManager` 独立实现 | 让启动流水线（main.go）不必等 Qdrant 就能起 HTTP |

---

## 12. 后续演进

- [ ] 真正迁移到 **Qdrant 原生 sparse vector** —— 现在的 MatchText 是过渡方案；
- [x] AST 引入 **tree-sitter** 作为可选后端 — 已实现，配置项 `tree_sitter.enabled`；支持 8 种语言（go/python/typescript/javascript/rust/java/c/cpp）。Docker 内 `CGO_ENABLED=0` 时自动 fallback 到 regex 解析，日志：`tree-sitter CGO not available, using regex fallback`；
- [ ] 对 `CodeChunk.Dependencies` 建 **call graph**，支持 "给我 X 的所有调用者" 这类检索；
- [ ] 引入 **embedding 缓存** (Redis)：同一 chunk SHA-1 命中缓存就跳过 embedding；
- [ ] 在 Retrieve 之后挂一层 **上下文打包器**：把相关度高的 K 个 chunk 拼成一段不超过 N tokens 的 prompt（避免 orchestrator 每次都重复这段逻辑）；
- [ ] `ReconnectManager` 扩展到 embedding/reranker endpoint：不只 Qdrant 会挂。

---

## 12. 实现剖析与改进方向

### 一次 `Retrieve(query)` 的完整时序

```text
query → Retrieve()
          │
          ├─ 1. Embed(query) ─────────▶ OpenAI embedding API
          │   (耗时 ~100-300ms)        ◀─── vector[1536]
          │
          ├─ 2. 并行发起双路召回（goroutine × 2）
          │   │
          │   ├─ [dense]  QdrantStore.SearchDense(vector)  ─▶ Qdrant gRPC
          │   │   (耗时 ~10-30ms)                          ◀─── top-K hits
          │   │
          │   └─ [sparse] QdrantStore.SearchSparse(query)
          │       ├─ ensureSparseIndex()  — 5min TTL 缓存命中，直接用
          │       │   （首次或过期：scrollAllChunks，耗时 1-5s）
          │       └─ BM25Index.Search(query, topK)  (耗时 <10ms in-mem)
          │
          ├─ 3. mergeResults(dense, sparse)
          │   │  按 chunk.ID 去重；取 dense score 和 sparse score 加权合并
          │   │  （当前实现：两路分别归一化后 0.5*d + 0.5*s）
          │
          ├─ 4. 可选 rerank
          │   │  if reranker != nil:
          │   │    httpClient.Post(rerank_url, {query, docs})  (~100-300ms)
          │   │    replace scores with rerank scores
          │   │  else: 按合并 score 排序
          │
          └─ 5. 截断 topN 返回
```

**总延迟分解（P50 / P99）**：

| 阶段 | P50 | P99 | 占比 |
|---|---|---|---|
| embedding | 150 ms | 600 ms | 40% |
| Qdrant dense | 15 ms | 80 ms | 5% |
| BM25 sparse | 5 ms | 30 ms | 2% |
| merge | <1 ms | <1 ms | — |
| rerank | 200 ms | 800 ms | 55% |
| **端到端** | **~370 ms** | **~1.5 s** | |

### BM25 索引的内存布局

```
BM25Index {
  docs:    []bm25Doc      // N 个文档，按 chunk 原顺序
    [0]: {chunk, length=120, tf:{parse:3, json:2, ...}}
    [1]: {chunk, length=80,  tf:{auth:5, jwt:2, ...}}
    ...
  df:      map[string]int  // term → 含该 term 的 doc 数
    "parse": 45
    "json":  120
    "auth":  12
    ...
  numDocs: 3500
  avgLen:  95.2
}
```

**规模估算**：
- 每个 chunk 平均 100 tokens × 5 bytes/token（含 map overhead）= 500 bytes tf map
- 10k chunks × 500 B = **5 MB 内存**
- df map: 唯一 term 数 ~50k × 16 B = **800 KB**
- 总内存 ≤ 10 MB for 10k chunks — 完全可承受

**扩展上限**：~100k chunks 之后内存 100+ MB，查询时间线性增长到 100ms。
超出后切 Qdrant sparse vector。

### 利弊评估

**优势（Pros）**
- ✅ 双路召回解决 code 场景的"标识符 vs 语义"二义性（见 §1.5）
- ✅ BM25 纯 Go 实现，**零外部依赖**（不用 Elasticsearch）
- ✅ camelCase 分词让"http client"查到 NewHTTPClient
- ✅ rerank 失败自动降级到按 score 排序（不会因为 rerank 服务挂掉而崩）
- ✅ Payload filter 硬过滤实现多租户隔离

**代价（Cons）**
- ⚠️ BM25 索引是**进程内**的——N 副本各自构建，占用 N × 10MB，首次查询
  各自等一次 5s 扫全量
- ⚠️ Merge 算法是固定 0.5/0.5 权重，不自适应
- ⚠️ Embedding 没有**分布式**缓存（进程内有 LRU，但跨副本不共享）
- ⚠️ rerank HTTP 调用没有熔断（依赖 rerank 服务挂了只看重试）
- ⚠️ 索引更新延迟：sparse 有 5min TTL 滞后

### 可改进点

**P0**
1. rerank client 加 gobreaker（防 rerank 慢吞吞拖累整个 Retrieve）
2. Embedding 加 Redis 二级缓存（key = sha256(text+model)）—— 省钱且减延迟

**P1**
3. BM25 跨副本共享：写 Qdrant 的 sparse vector（1.8+ 支持）
4. Merge 权重学习：用用户点击反馈调整 dense/sparse 比重
5. rerank batch：把 top-30 一次请求而非 30 次（当前实现已经 batch，但 batch 大小硬编码）

**P2**
6. AST parser 扩展：Rust / Java / TS（目前仅 Go/Python/Markdown）
7. 混合 graph 检索：扩招回（通过 import 关系拉同包 chunk）
8. 增量索引触发：文件变动立即 invalidate BM25，不等 5min TTL

---

下一篇：`05_sandbox.md` —— Docker 动态沙箱（容器创建/资源限额/SSE 流式）。
