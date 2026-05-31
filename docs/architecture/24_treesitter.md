# 24 · Tree-sitter AST 解析 `internal/treesitter`

> 代码：
> - `parser.go` (40) — `Parser` 接口 + `Symbol` / `Chunk` 数据结构
> - `parser_cgo.go` (544) — `//go:build tree_sitter` CGO 实现，6 个语言专用 extractor
> - `parser_fallback.go` (163) — `//go:build !tree_sitter` 正则降级实现
>
> 测试：
> - `parser_test.go` (310) — CGO 模式集成测试（带 `//go:build tree_sitter` tag）
> - `parser_fallback_test.go` (209) — 正则模式独立测试

---

## 1. 模块定位

**"用同一份接口同时支持 CGO tree-sitter 和正则降级——上层代码不用关心 Docker 镜像有没有 libgcc。"**

`internal/treesitter` 给 RAG、repomap、orchestrator 提供**多语言 AST 解析**能力，主要两件事：

1. **`ExtractSymbols(lang, src)`** — 从源码抽出 `Symbol{Name, Kind, StartLine, EndLine, Visibility, Parent}` 列表，供 repomap / RAG 索引；
2. **`ChunkByAST(lang, src)`** — 把源码按符号边界切成 `Chunk`，供 RAG embedding（每个 chunk 一个 embedding，比"按行切窗口"语义保真度高得多）。

支持 8 种语言：`go / python / typescript / javascript / rust / java / c / cpp`。

模块的**最大特征**是**双实现 + build tag**：

```
src → ┌─ -tags tree_sitter ─→ parser_cgo.go ─→ libtree-sitter (CGO)
      └─ 无 tag        ─→ parser_fallback.go ─→ regexp.Regexp
```

两份代码导出**同一个** `NewCGOParser(logger) Parser`，调用方完全感知不到差异——这是 Go build tag 用来做"编译期能力探测"的典型模式。

---

## 1.5 核心设计问题

### 为什么不直接 CGO？

CGO tree-sitter 需要 libgcc + libpthread 等运行时库。本项目的 Dockerfile 用 alpine + `CGO_ENABLED=0` 静态编译——好处是镜像只 30MB，可移植性极强；代价就是 CGO 链接的 `smacker/go-tree-sitter` 包**无法编入二进制**。

build tag 在这里解决三件事：

1. **生产 Linux 静态镜像**：`CGO_ENABLED=0` + 无 tag → 编出来的二进制能在任何 glibc/musl Linux 跑，但 tree-sitter 走正则；
2. **开发环境**：本地 macOS + `go build -tags tree_sitter` → 拿到真正的 AST 精度；
3. **CI 双跑**：lint/test 阶段同时跑 fallback test + CGO test（CI 镜像装了 libgcc），保证两份实现行为一致。

### 为什么不直接砍掉 CGO 只留正则？

正则方案在**简单语言**（Go / Python / TypeScript / Rust）上能抓住 80% 的顶层符号，但：

- **嵌套类 / 嵌套函数** 抓不出 `Parent` 关系（正则只看单行）；
- **多行签名** 漏抓（`func F(\n  a int,\n  b int,\n) error` 在第二行就匹配不到）；
- **C++ 的复杂类型** 完全没法用正则——`template<typename T> shared_ptr<Foo<T>> bar()` 这种声明正则崩盘；
- **方法 vs 函数的区分**（receiver / class membership）需要看上下文，正则单行匹配区分不了；
- **可见性识别**（Java 的 `public/private/protected`、Rust 的 `pub` modifier）正则的 `^(?:pub\s+)?` 写法漏掉了 `pub(crate)` 之类的语法。

对一个把 AST 当成 RAG 检索基础设施的系统来说，"够用" 的正则不够——所以保留 CGO 实现作为**精度上限**。

### 为什么 C 和 C++ 共用同一个 parser？

```go
"cpp": cpp.GetLanguage(),
"c":   cpp.GetLanguage(),    // ← 共用
```

`tree-sitter-cpp` 的语法是 C 的超集，**可以解析所有合法 C 程序**。引入独立的 `tree-sitter-c` 包会让二进制体积增加而收益微乎其微（C 的语法节点类型几乎是 C++ 的子集）。代价是少数 C 特有的边界情形（C99 复合字面量等）会按 C++ 语法解析——目前未发现影响 symbol 提取。

---

## 2. 依赖架构

```
                      ┌────────────────────────────────────┐
                      │  cfg.TreeSitter.Enabled = true     │
                      │  cmd/agent/main.go:587-597         │
                      └──────────────┬─────────────────────┘
                                     │
                                     ▼
                      ┌────────────────────────────────────┐
                      │  treesitter.NewCGOParser(logger)   │
                      │  ┌──────────────────────────────┐  │
                      │  │ build tag tree_sitter:       │  │
                      │  │   cgoParser{} (CGO 真实现)   │  │
                      │  │ else:                        │  │
                      │  │   fallbackParser{} (regex)   │  │
                      │  │   logger.Warn("CGO not avail")│ │
                      │  └──────────────────────────────┘  │
                      └────┬──────────────┬────────────────┘
                           │              │
              ┌────────────┘              └─────────────┐
              ▼                                         ▼
   ┌─────────────────────────┐         ┌──────────────────────────┐
   │ orchestrator.           │         │  rag.Engine              │
   │ SetTreeSitterParser     │         │  SetTreeSitterParser     │
   │ (p1_bridge.go)          │         │  (ts_adapter.go 适配)    │
   │                         │         │                          │
   │  → goto_definition      │         │  → AST-aware chunking    │
   │  → find_references      │         │    替代固定窗口切分      │
   │  → rename_symbol        │         │                          │
   └─────────────────────────┘         └──────────────────────────┘
                                                ▼
                                       ┌──────────────────────────┐
                                       │  repomap.Generator       │
                                       │  SetTreeSitterParser     │
                                       │  (ts_repomap_adapter.go) │
                                       │                          │
                                       │  → 符号表索引            │
                                       └──────────────────────────┘
```

---

## 2.5 数据流总览

```text
═════════════ 一次 RAG 索引文件的全流程 ═════════════

1. indexer 扫描到 /workspace/main.go (≤1MB)
       ↓
2. ragEngine.IndexFile("main.go")
       ↓ 检测语言
3. lang := "go"
   parser.ChunkByAST("go", content) → []Chunk
       ↓ build tag 分支
   ┌────────────── CGO 路径 ──────────────┐    ┌── Fallback 路径 ──┐
   │ ① sitter.NewParser().SetLanguage(go)│    │ ① regexp 逐行匹配│
   │ ② tree := parser.Parse(content)     │    │ ② Symbol[i].End  │
   │ ③ extractSymbolsFromNode(root, ...)│    │    = Symbol[i+1]. │
   │    → 递归 walk AST                  │    │      Start - 1   │
   │    → switch nodeType (function/class/...)│  │ ③ 整段切 Chunk   │
   │    → 提取 StartLine/EndLine/Vis    │    │                  │
   │ ④ 按 Symbol 切 Chunk{}              │    │ 缺：嵌套 / 多行  │
   │    Content = lines[Start-1:End]    │    │     签名 / Parent │
   └─────────────────────────────────────┘    └──────────────────┘
       ↓
4. for chunk := range chunks:
     emb := embedder.Embed(chunk.Content)
     qdrant.Upsert("code", id, emb, payload={
       symbol_name: chunk.SymbolName,
       symbol_type: chunk.SymbolType,
       start_line: chunk.StartLine,
       end_line:   chunk.EndLine,
     })
       ↓
5. 搜索时返回的 hit 自带"哪个函数/类"——前端可以精确跳转
```

**关键差异**：CGO 路径下，`chunk.EndLine` 是 AST 节点的真实闭合 `}` 位置；fallback 路径下，`EndLine` 是"下一个符号 - 1"，多个函数之间的空白行 / 注释会被算进**当前**函数。前者用于 RAG 检索更精确，后者**够用**但召回上下文略偏多。

---

## 3. `Parser` 接口

```go
type Parser interface {
    ExtractSymbols(language, content string) ([]Symbol, error)
    ChunkByAST(language, content string) ([]Chunk, error)
    SupportedLanguages() []string
}

type Symbol struct {
    Name       string
    Kind       string  // function / method / class / interface / struct / variable / type
    Signature  string  // 仅 CGO 模式填充
    StartLine  int     // 1-based
    EndLine    int
    Parent     string  // enclosing class/struct
    Visibility string  // public / private / protected / package
}

type Chunk struct {
    SymbolName   string
    SymbolType   string
    Content      string   // 原始源码片段
    StartLine    int
    EndLine      int
    Dependencies []string // 预留：调用图 / import 关系（当前未填充）
    Signature    string
}
```

### 3.1 为什么 `Symbol.EndLine` 用 int 而不是文件偏移？

行号比字节偏移**对人类更友好**——错误消息、IDE 跳转、grep 输出全是基于行号的。tree-sitter 内部用 `StartByte/EndByte`，CGO 实现里把字节位置转成 `node.StartPoint().Row + 1` 暴露给上层。

### 3.2 为什么 `Signature` 只在 CGO 模式填？

CGO 模式可以精确拿到 "函数声明的完整文本"（节点的源码区间），fallback 正则只能匹配函数名所在的单行。塞个不完整的 signature 比留空更误导，所以 fallback 直接不填。

### 3.3 为什么 `Dependencies` 留空？

调用图（"这个函数调用了哪些其他函数"）需要解析 `call_expression` 节点 + 解 import 表，工程量大。当前 RAG 检索靠"语义相似度 + BM25"已经能找到相关符号，依赖图是**优化空间**而非**当前需求**。预留字段，等 Phase 2 再做。

---

## 4. `parser_cgo.go` —— CGO 实现

### 4.1 语言映射

```go
var langMap = map[string]*sitter.Language{
    "go":         golang.GetLanguage(),
    "python":     python.GetLanguage(),
    "typescript": typescript.GetLanguage(),
    "javascript": javascript.GetLanguage(),
    "rust":       rust.GetLanguage(),
    "java":       java.GetLanguage(),
    "cpp":        cpp.GetLanguage(),
    "c":          cpp.GetLanguage(),
}
```

`*sitter.Language` 是 tree-sitter native object 的不透明指针，包级单例（`GetLanguage()` 返回同一对象）。每次 `ExtractSymbols` 新建 `Parser` 实例并 `SetLanguage`，**parser 实例本身不线程安全**——并发调用必须每个 goroutine 自己 new。

### 4.2 通用递归骨架

```go
func extractSymbolsFromNode(node, source, lang, parent, &symbols):
    switch language {
    case "go":     extractGoSymbol(node, ...)
    case "python": extractPythonSymbol(node, ...)
    ...
    }
    
    // 计算下一层的 parent（仅当当前节点是容器：class/struct/impl/interface）
    newParent := parent
    if isContainerNode(nodeType, language) {
        if name := findChildName(node, source); name != "" {
            newParent = name
        }
    }
    
    // 递归遍历所有子节点
    for i in 0..node.ChildCount():
        extractSymbolsFromNode(node.Child(i), source, lang, newParent, &symbols)
```

**关键设计**：

1. **先 emit 自己再递归子节点**——所以输出顺序是源码顺序（pre-order），方便后续按 `StartLine` 切 chunk；
2. **`parent` 沿子树继承**——`isContainerNode` 决定哪些 node 类型会"重置" parent；
3. **多语言共用同一个 walker**——extractor 只关心 "看到我的节点类型时干什么"，遍历逻辑统一。

### 4.3 每种语言的特殊处理

| 语言 | 关注节点 | 可见性判定 |
|------|----------|------------|
| Go | `function_declaration / method_declaration / type_declaration` | 首字母大写 = public |
| Python | `function_definition / class_definition` | 下划线开头 = private（PEP-8） |
| TypeScript / JavaScript | `function_declaration / class_declaration / interface_declaration / method_definition / arrow_function` | 一律 public（TS modifier 暂未提取） |
| Rust | `function_item / struct_item / impl_item / trait_item / enum_item` | 看子节点 `visibility_modifier` |
| Java | `method_declaration / class_declaration / interface_declaration` | 看子节点 `modifiers` 文本 |
| C / C++ | `function_definition / class_specifier / struct_specifier` | 一律 public |

**重要细节**：

- **Go 的 `method_declaration`** 用 `findChildByType(node, "field_identifier")` 拿方法名——receiver 类型在另一个子树，本实现**没提取 receiver type**作为 Parent（潜在改进点：把 `(*UserService) Login` 的 `UserService` 填到 `Parent`）；
- **Go 的 `type_declaration`** 内部还有一层 `type_spec`，要再下探一层找 struct/interface 的真实关键字；
- **TypeScript 的箭头函数赋值**（`const handler = () => {...}`）需要走 `node.Parent()` 反查 `variable_declarator`——这是少数从子节点反向访问父节点的场景；
- **Java 的 `javaVisibility`** 拿 `modifiers` 子节点的文本做关键词匹配——`public/private/protected` 任一出现即返回；都没有的话 fallback 到 `"package"`（Java 默认包级私有）。

### 4.4 `ChunkByAST` 切片策略

```go
for _, sym := range symbols:
    startIdx = sym.StartLine - 1
    endIdx   = sym.EndLine        # CGO 模式：tree-sitter 的真实结束行
    chunk = Chunk{
        Content: strings.Join(lines[startIdx:endIdx], "\n"),
        ...
    }

# 空文件兜底：整文件作为 <file> chunk
if len(chunks) == 0 && len(content) > 0:
    chunks = [Chunk{SymbolName: "<file>", SymbolType: "file", Content: content}]
```

**为什么没合并相邻短函数为一个 chunk？** 简单语言（如 Python 的 `__init__` 是 3 行函数）会产生大量小 chunk。但 RAG 的策略是**用召回数量补单 chunk 信息密度**——top-k=10 时即使每个 chunk 很小，加起来也是完整上下文。把相邻短函数合并反而会让"召回了 A 函数也带出无关 B 函数"这种语义污染。

---

## 5. `parser_fallback.go` —— 正则降级

### 5.1 入口"撒谎"

```go
// 注意：函数名仍叫 NewCGOParser，但实际返回 fallbackParser
func NewCGOParser(logger *zap.Logger) Parser {
    logger.Warn("tree-sitter CGO not available, using regex fallback")
    return &fallbackParser{logger: logger.With(zap.String("component", "treesitter-fallback"))}
}
```

**这是有意为之**：build tag 选择文件后，整个 `treesitter` 包对外暴露**完全一样的 API**，调用方代码（`main.go:588`）一字不改即可。唯一可观察的差异是启动日志多了一行 WARN。

### 5.2 正则定义（节选）

```go
var symbolPatterns = map[string][]symbolPattern{
    "go": {
        {re: `^func\s+(\w+)\s*\(`,                    kind: "function", visCheck: isGoExported},
        {re: `^func\s+\([^)]+\)\s+(\w+)\s*\(`,        kind: "method",   visCheck: isGoExported},
        {re: `^type\s+(\w+)\s+struct\b`,              kind: "struct",   visCheck: isGoExported},
        {re: `^type\s+(\w+)\s+interface\b`,           kind: "interface",visCheck: isGoExported},
    },
    "rust": {
        {re: `^(?:pub\s+)?fn\s+(\w+)`,                kind: "function"},
        {re: `^impl(?:<[^>]+>)?\s+(\w+)`,             kind: "type"},
        ...
    },
    ...
}
```

**已知缺陷**：

- `^func` 起始锚——多行声明（首行只有 `func F(`）漏匹配；
- `c` / `cpp` 的 `^(?:\w+\s+)+(\w+)\s*\([^;]*$` 误报：变量声明 `static int x = foo();` 满足模式；
- Java 的 method 模式 `(?:public|private|protected)\s+...` 会把构造函数也算进去（构造函数没有返回类型——这反而是想要的行为）；
- 没 `Parent` 提取——所有 Symbol 的 `Parent` 都是空字符串。

**正则模式承认自己粗糙**，目标只是"启动一个 RAG 索引"——精度由 CGO 模式负责。

### 5.3 切片：用"下一个符号开始"作为结束

```go
for i, sym := range symbols:
    endLine = sym.StartLine
    if i+1 < len(symbols):
        endLine = symbols[i+1].StartLine - 1     # ← 关键
    else:
        endLine = len(lines)                     # 最后一个符号吃到 EOF
```

副作用：**两个相邻函数之间的注释 / 空行 / 未识别的 var declaration**会被算入**前一个**函数的 chunk。对 embedding 来说是噪声但可接受。

---

## 6. 与上游模块的接入

### 6.1 main.go 启动逻辑

```go
// cmd/agent/main.go:587
if cfg.TreeSitter.Enabled {
    tsParser := treesitter.NewCGOParser(logger)
    orch.SetTreeSitterParser(tsParser)
    if ragEngine != nil {
        ragEngine.SetTreeSitterParser(&tsParserAdapter{parser: tsParser})
    }
    if repomapGen != nil {
        repomapGen.SetTreeSitterParser(&tsRepomapAdapter{parser: tsParser})
    }
    logger.Info("tree-sitter parser initialized", zap.Strings("languages", tsParser.SupportedLanguages()))
}
```

`cfg.TreeSitter.Enabled` 是 yaml 顶层段：

```yaml
tree_sitter:
  enabled: true
  languages: ["go", "python", "typescript", "javascript", "rust", "java", "c", "cpp"]
  max_file_size: 1048576
```

注意 `languages` 字段当前**未生效**（parser 一启动就支持全部 8 种）——预留给未来"只为部分语言注册"的能力。

### 6.2 三个 adapter 解耦

`treesitter.Chunk` 与 `rag.TreeSitterChunk` / `repomap.TreeSitterChunk` 是**结构相同但类型不同**的镜像——因为 RAG / repomap 不应该 import `treesitter` 包（避免下游包反向依赖）。adapter 做字段级 1:1 映射：

```go
// cmd/agent/ts_adapter.go
func (a *tsParserAdapter) ChunkByAST(language, content string) ([]rag.TreeSitterChunk, error) {
    chunks, err := a.parser.ChunkByAST(language, content)
    ...
    result := make([]rag.TreeSitterChunk, len(chunks))
    for i, c := range chunks {
        result[i] = rag.TreeSitterChunk{ /* 字段同名复制 */ }
    }
    return result, nil
}
```

这是 Go 经典的**"interface 在使用者处定义"**模式——`rag.TreeSitterParser` 接口在 rag 包里定义，treesitter 包不知道也不需要知道。

### 6.3 orchestrator 的 P1 bridge

`internal/orchestrator/p1_bridge.go` 定义了 orchestrator 想要的 parser 接口（仅 `ExtractSymbols` + `ChunkByAST`），并把 treesitter 实例注入到 LSP / symbol 工具的依赖里。

```go
type treeSitterParser interface {
    ExtractSymbols(language, content string) ([]treesitter.Symbol, error)
    ChunkByAST(language, content string) ([]treesitter.Chunk, error)
}
```

`goto_definition` / `find_references` / `hover_info` / `rename_symbol` 在 LSP 不可用时**降级用 treesitter**（见 [27_lsp](27_lsp.md)）。

---

## 7. 当前 Docker 部署的实际状态

**结论**：当前线上 Docker 镜像运行的是**正则 fallback** 模式。

证据链：

1. `Dockerfile` 构建阶段使用 `CGO_ENABLED=0` 静态编译；
2. 二进制没有 `tree_sitter` build tag；
3. 启动日志包含 WARN：`"tree-sitter CGO not available, using regex fallback"`；
4. P1 验证（[20_deploy](20_deploy.md) §镜像新鲜度）的 strings 检查里没有 `smacker/go-tree-sitter` 符号。

要切到 CGO 模式有两条路径：

**方案 A（推荐）**：改 Dockerfile builder 阶段
```dockerfile
RUN CGO_ENABLED=1 GOOS=linux go build -tags tree_sitter -o /build/bin/code-agent ./cmd/agent
# 同时需要安装 libgcc + musl-dev（alpine）或 build-essential（debian）
```
镜像体积从 30MB → 约 80MB（CGO 拉进 libc 等）。

**方案 B**：单独一个 `Dockerfile.cgo` 用 debian:bookworm-slim，体积 200MB+，但兼容性最好。

权衡：单元测试 / API 集成测试在两种模式下都能跑（fallback 测试见 `parser_fallback_test.go`），所以**当前用 fallback 是有意识的决定**——优先要小镜像 + 可移植性。

---

## 8. 性能 & 安全考量

### 8.1 单次解析耗时（典型经验值）

| 文件大小 | CGO 模式 | Fallback 模式 |
|----------|----------|---------------|
| 500 行 Go | ~2ms | ~0.5ms |
| 2000 行 Python | ~8ms | ~2ms |
| 5000 行 TypeScript | ~25ms | ~6ms |

正则更快但精度低；tree-sitter 真实 AST 但分配开销更大（每个节点都是 native object）。RAG 索引是后台批量任务，2x-4x 差异完全可接受。

### 8.2 大文件保护

`cfg.TreeSitter.MaxFileSize = 1048576`（1MB）是**调用方约束**——treesitter 包本身不做大小检查，由 indexer / RAG 在调用前过滤。极端情况下 tree-sitter 解析 5MB 文件可能用掉数百 MB RAM（AST 节点树膨胀），这个上限是经验值。

### 8.3 panic 隔离

CGO 调用如果 native parser 内部段错误会直接 SIGSEGV 整个 Go 进程。当前实现**没有 `recover()`** 包裹——历经数月运行未触发；如果以后接到用户提交的"奇怪源码"导致崩溃，需要在 `ExtractSymbols` 入口加 panic recovery。

### 8.4 并发安全

- `sitter.Parser` 实例**非并发安全**——每次 `ExtractSymbols` 内部 `sitter.NewParser()` 是必须的；
- `*sitter.Language`（GetLanguage 返回值）是**全局只读单例**，并发读取安全；
- `fallbackParser` 没有可变状态，**完全并发安全**。

---

## 9. 与其他模块的边界

| 上游 | 用法 |
|------|------|
| [04_rag](04_rag.md) | 用 `ChunkByAST` 做 AST-aware 切片，替代固定窗口；失败 fallback 到行切 |
| [25_memory](25_memory.md) | 暂未使用——记忆模块按消息切，不按代码切 |
| [27_lsp](27_lsp.md) | LSP 不可用时用 treesitter `ExtractSymbols` 实现 `goto_definition` 等工具的降级 |
| `internal/indexer` | 仓库初次扫描时调 `ChunkByAST` 生成 symbol table |
| `internal/repomap` | 把 symbols 渲染成"仓库导览" markdown 喂给 LLM |

`treesitter` 包**不被任何其他业务包反向依赖**——是典型的"叶子能力包"。

---

## 10. 设计权衡

| 抉择 | 动机 |
|------|------|
| build tag 双实现 | 保留 CGO 精度的同时让默认 Dockerfile 仍可静态编译 |
| 函数名都叫 `NewCGOParser` | 让调用方代码与 build tag 解耦，零成本切换 |
| C 复用 cpp parser | 节省二进制体积，C 是 C++ 子集 |
| `Symbol.Signature` 仅 CGO 填 | 不完整 signature 比 nil 更误导 |
| `Dependencies` 字段预留但留空 | 调用图工程量大，当前 RAG 召回足够 |
| 不在包内做大文件检查 | 上限是业务策略；解析能力本身不该自己设限 |
| 每次新建 `sitter.Parser` 实例 | tree-sitter parser 状态不安全；池化收益不大 |
| Go method 不提取 receiver type 作为 Parent | `field_identifier` 不挂在 receiver 子树，提取需要额外的 type_identifier 查找——后续可补 |
| Fallback 用"下个符号 -1"作为 EndLine | 简单可靠；精度让位给 CGO 模式 |

---

## 11. 后续演进

- [ ] **Dockerfile.cgo**：单独提供 CGO 版本镜像，由 CI 同时构建两份
- [ ] **Go receiver type → Parent**：从 `method_declaration` 的 receiver 子节点提取类型名，让 `(u *UserService) Login` 的 `Parent="UserService"`
- [ ] **languages 配置生效**：把 yaml `tree_sitter.languages` 真的当白名单用，启动时按需注册 langMap
- [ ] **Signature 抽取（fallback）**：用多行预读拿到"匹配行 + 直到 `{` 或 `:` 的完整声明"
- [ ] **Dependencies 调用图**：解析 `call_expression` / `method_call` 节点填 Dependencies
- [ ] **panic recovery**：CGO 入口加 `defer recover()` 防御奇怪源码触发 SIGSEGV
- [ ] **AST diff**：multiagent 冲突检测用 AST diff 替代"任意两写就冲突"（见 [22_multiagent](22_multiagent.md) §冲突）
- [ ] **增量解析**：tree-sitter 原生支持基于 edit 的增量 re-parse，当前每次全文重解；接入后文件编辑后的 RAG 重索引能加速 5-10 倍
- [ ] **更多语言**：bash / yaml / json / sql / lua 都有官方 grammar，按需添加
- [ ] **tree-sitter query 文件**：`internal/treesitter/queries/` 已建目录但空。将 hard-coded extractor 切换为 `.scm` query 文件，可读性更好

---

## 12. 设计教训

`treesitter` 的双实现是从一次"线上崩溃 + 紧急回滚"演化来的：

1. **第一版**：只有 CGO。Docker 构建时 `CGO_ENABLED=0` 直接编不过——build error 在 CI 一眼看到。
2. **过渡版**：把 `parser_cgo.go` 移到 `parser.go` 外的子包 `cgo/`，主包只导出接口。结果 import path 变得诡异（业务代码必须 `import treesitter/cgo`），可读性下降。
3. **当前版**：用 build tag 让两份实现**直接共存在同一个包内**。调用方 `treesitter.NewCGOParser(logger)` 在两种构建下都能编译——这是 Go build tag 设计的"教科书式"用法。

教训：**Go build tag 适合做"同一接口的不同 backend"**，比子包拆分更轻量。但要警惕：tag 控制的代码不能跨文件共享私有符号（每份实现自带辅助函数）——这是为什么 `nodeText` / `findChildByType` 等只在 `parser_cgo.go` 里。

---

下一篇：[`25_memory.md`](25_memory.md) —— 长期记忆系统：会话向量化、压缩与召回。
