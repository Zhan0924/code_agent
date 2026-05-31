# 27 · LSP 客户端 `internal/lsp`

> 代码：
> - `protocol.go` (53) — LSP 协议数据结构（`Location` / `Range` / `Position` / `TextEdit` / `SymbolInfo` / `HoverResult` / `WorkspaceEdit`）
> - `client.go` (138) — `Client` 接口 + `client` 占位实现（所有 RPC 方法均 `return "not implemented"`）
>
> 测试：`client_test.go` (95) — 验证接口契约（`TestClient_MethodsReturnNotImplemented`）
>
> 工具注册：`internal/orchestrator/lsp_tools.go` (202) — 把 LSP 调用包装成 4 个 ReAct 工具

---

## 1. 模块定位

**"把 IDE 级别的符号语义（'这个 `User` 到底是哪个 struct？'）暴露给 agent，让 LLM 不靠 grep 也能精准跳转。"**

`internal/lsp` 为 orchestrator 的 ReAct 循环提供**四个语义工具**：

| 工具 | LSP 方法 | LLM 看到的用途 |
|------|----------|----------------|
| `goto_definition` | `textDocument/definition` | "这个符号在哪定义的？" |
| `find_references` | `textDocument/references` | "这个函数被哪些地方调用？" |
| `hover_info` | `textDocument/hover` | "这个变量的类型 / 文档是什么？" |
| `rename_symbol` | `textDocument/rename` | "把 `userId` 全局改成 `userID`" |

这四个能力和 IDE 的"右键 → Go to Definition"完全等价——区别在于**调用方是 LLM**而不是人。

### 当前实现状态：**接口齐全，wire protocol 占位**

`client.go` 把整个 `Client` 接口实现出来了，但所有 RPC 方法（`GotoDefinition` / `FindReferences` / `Rename` / `Hover` / `DocumentSymbols`）的函数体只是：

```go
return nil, fmt.Errorf("not implemented")
```

`Initialize` / `Shutdown` / `ShutdownAll` / `DidChange` 是空壳——只在内存 `map[language]*serverConn` 上挂个记号，**没有真起进程、没有发 JSON-RPC**。

这意味着：

- ✅ 编译能过、注入能跑、`cmd/agent/main.go` 启动时打印 "LSP client initialized"；
- ✅ `goto_definition` 等 4 个工具在 `tools.Registry` 里注册成功，LLM schema 里能看到；
- ❌ LLM 真的调 `goto_definition` 时，最终拿到 `LSP error: not implemented`；
- ⚠️ **必须配合 `cfg.LSP.Enabled = false`**（默认）来避免污染 LLM 工具菜单——一旦启用就是"工具说有，调用却挂"。

所以当前线上**未启用** LSP。本文档同时记录"现状"与"为什么这样设计 + 真要补全要做什么"——属于**接口已定稿、实现待填**的典型骨架包。

---

## 1.5 核心设计问题

### 为什么先把空壳骨架放出来？

`internal/lsp` 是 P1 阶段的**前置接口承诺**：

1. **orchestrator 的 4 个工具**（`lsp_tools.go`）需要一个稳定的 `LSPClient` interface 来注入——如果等到 LSP 真实现完成再加这些工具，整个 P1 计划无法平行推进；
2. **测试可写**：`client_test.go` 早早建立"接口契约必须满足这些方法签名"——以后任何真实现都得通过这套测试；
3. **配置可生效**：`cfg.LSP.Enabled` / `cfg.LSP.Servers` 的 yaml schema 已经定下，运维侧不用为了后续启用再改部署。

把"接口冻结 + 实现 stub"和"实现真 wire protocol"两件事**解耦**，让任意一方都能独立推进。这是 Go 接口先行（"接口在使用者处定义"）思想的延伸：占位实现就是给接口的"暂时填空"。

### 为什么不直接复用 `gopls` 或 IDE 的现成 LSP 客户端？

LSP 是个简单的协议（JSON-RPC 2.0 + 固定 method 名），但开源 Go 客户端要么 vendor 了整个 gopls 内部数据结构（太重），要么早已不维护（如 `sourcegraph/go-lsp`）。

本项目 `internal/mcp/client.go` 已经实现了一套**通用 JSON-RPC 2.0 客户端**（含 stdio/socket 传输 + reconnect + request/response 匹配）——LSP 和 MCP 都是 JSON-RPC 2.0，**底层传输完全可以复用**。`client.go` 里 `serverConn` 旁边那行注释：

```go
// TODO: actual process management and JSON-RPC communication
// This would reuse patterns from internal/mcp/client.go
```

明确写出了演进路径：把 mcp.Client 抽出一个 `internal/jsonrpc` 公共包，LSP 在它之上加 LSP-specific 的 method 包装。这是**不创建第三个 JSON-RPC 实现**的明智选择。

### 为什么 LSP 和 tree-sitter 同时存在？两者不重叠吗？

不重叠——LSP 提供 tree-sitter 给不了的**类型语义**：

| 能力 | tree-sitter | LSP |
|------|-------------|-----|
| 符号列表（哪些函数 / 类） | ✅ ExtractSymbols | ✅ DocumentSymbols |
| **跨文件跳转**（这个调用的目标在哪个文件） | ❌ 只能在单文件 AST | ✅ 解析 import + type checker |
| **类型推断**（`x.Foo()` 的 Foo 是谁） | ❌ 字面意义"看不懂" | ✅ 类型系统 |
| **跨文件重命名**（同时改所有引用） | ❌ 名字匹配不可靠 | ✅ 语义级 |
| 速度 | <10ms | 100ms-2s（取决于 workspace 大小） |

→ tree-sitter 是"廉价的近似"，LSP 是"昂贵的精确"。orchestrator 的 `goto_definition` 工具**首选 LSP**，LSP 不可用时**降级到 tree-sitter symbol table** 做近似（见 [24_treesitter](24_treesitter.md) §6.3）。

**注意**：当前 `lsp_tools.go` 里**没有**这个降级——LSP 报错就直接返回 `LSP error: not implemented` 给 LLM。真正的双源融合（"先问 LSP，挂了用 tree-sitter 撑"）是补完阶段要做的——见 §11 演进列表。

### 为什么按 language 一对一管 LSP server？

每个 LSP server 进程只懂一种语言（`gopls` 只会 Go，`pylsp` 只会 Python，`rust-analyzer` 只会 Rust）。`servers map[string]*serverConn` 的 key 就是 language ID。

代价：用户跑 monorepo（Go + TS + Python 混合）时启动 3 个长驻进程——`gopls` 单 workspace 内存常驻 ~500MB，三种语言并存就是 1.5GB。这个成本必须接受，是 LSP 协议本身的限制。

优化方向：**懒启动**——agent 第一次调 `goto_definition file:src/foo.py:42:10` 时再启 `pylsp`，而不是配置里列出来就全启。当前 `Initialize` 是显式调用，正好支持这个语义。

---

## 2. 依赖架构

```
                ┌──────────────────────────────────────────┐
                │  LLM 通过 ReAct 调 goto_definition       │
                │  args = {file, line, column}             │
                └────────────────┬─────────────────────────┘
                                 │
                                 ▼
              ┌──────────────────────────────────────────┐
              │ orchestrator.toolGotoDefinition          │
              │  ├─ fileToURI(path) → "file:///..."     │
              │  ├─ line-1, col-1  (1→0-based 转换)      │
              │  └─ o.lspClient.GotoDefinition(...)      │
              └────────────────┬─────────────────────────┘
                               │
                               ▼
              ┌──────────────────────────────────────────┐
              │ lsp.Client (interface)                   │
              │ ┌──────────────────────────────────────┐ │
              │ │ 当前: client{} 占位                  │ │
              │ │   return errors.New("not implemented")│ │
              │ │                                       │ │
              │ │ 未来: realClient{}                   │ │
              │ │   ├─ 选 server by language           │ │
              │ │   ├─ 发 textDocument/definition      │ │
              │ │   └─ 解 LSP Location → 内部 Location │ │
              │ └──────────────────────────────────────┘ │
              └────────────────┬─────────────────────────┘
                               │ (未来)
                               ▼
              ┌──────────────────────────────────────────┐
              │ serverConn (per language)                │
              │ ┌──────────────────────────────────────┐ │
              │ │ cmd: exec.Cmd ("gopls serve")       │ │
              │ │ stdin/stdout: JSON-RPC 通道         │ │
              │ │ pending: map[id]chan Response       │ │
              │ │ readLoop goroutine (类 mcp.Client)  │ │
              │ └──────────────────────────────────────┘ │
              └────────────────┬─────────────────────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │ gopls / pylsp / ...  │
                    │ (外部进程)            │
                    └──────────────────────┘
```

---

## 2.5 数据流总览

```text
═══════════════ 当前（占位）状态 ═══════════════

LLM:  "go to definition of UserService at file: src/auth.go line 42 column 8"
  │
  ▼
[ReAct loop dispatch]
  ↓ args = {"file":"src/auth.go", "line":42, "column":8}
[orchestrator.toolGotoDefinition]
  ├─ uri := "file://src/auth.go"
  ├─ lspClient.GotoDefinition(ctx, uri, 41, 7)
  │     │
  │     ▼ (client.go:111)
  │   return nil, errors.New("not implemented")
  │
  └─ wrap as ToolResult{Content: "LSP error: not implemented", IsError: true}
       ↓
[LLM 看到] "LSP error: not implemented" → 自然回退到 search_code / grep

═══════════════ 未来（实现完成后）状态 ═══════════════

[orchestrator.toolGotoDefinition]
  ├─ uri := "file:///workspace/src/auth.go"
  ├─ lspClient.GotoDefinition(ctx, uri, 41, 7)
  │     │
  │     ▼ (realClient.GotoDefinition)
  │   conn := pickServer(detectLang(uri))  # "go" → gopls
  │   req := {"jsonrpc":"2.0","id":N,"method":"textDocument/definition",
  │           "params":{"textDocument":{"uri":uri},
  │                     "position":{"line":41,"character":7}}}
  │   resp := conn.Call(req, timeout)
  │   locations := parseLSPLocations(resp)  # [{uri, range:{start,end}}]
  │   return convertToInternalLocation(locations), nil
  │     │
  │     ▼ 失败时
  │   if lspErr := err; lspErr != nil:
  │       # 降级路径（未实现）
  │       return tsParser.findDefinitionByName(symbol)
  │
  └─ "src/user.go:15:6" + "src/user.go:42:1"  (返回给 LLM)
```

---

## 3. 协议数据类型

```go
type Location struct {
    URI       string  // file:///abs/path
    StartLine int     // 0-based (LSP 规范)
    StartCol  int     // 0-based
    EndLine   int
    EndCol    int
}

type Range struct { Start, End Position }
type Position struct { Line, Character int }  // 0-based

type TextEdit struct {
    Range   Range
    NewText string
}

type WorkspaceEdit struct {
    Changes map[string][]TextEdit  // URI → 该文件的所有编辑
}

type HoverResult struct {
    Contents string   // markdown 文档（gopls）或纯文本（pylsp）
    Range    *Range   // 可选：hover 高亮的源码范围
}

type SymbolInfo struct {
    Name       string
    Kind       int        // LSP SymbolKind 枚举（function=12, class=5, ...）
    Range      Range
    Children   []SymbolInfo
    Detail     string     // 类型签名等附加信息
    Deprecated bool
}
```

### 3.1 为什么内部用 0-based 而 LLM 看到的是 1-based？

LSP 协议规定 `line/character` 都是 **0-based**。但 LLM 看代码 / 编辑器 / grep 输出都是 **1-based**（"`auth.go:42`"）。

`lsp_tools.go` 在边界做转换：

```go
// 入参 1-based → 内部 0-based
locations, err := o.lspClient.GotoDefinition(ctx, uri, params.Line-1, params.Column-1)

// 出参 0-based → 1-based
sb.WriteString(fmt.Sprintf("%s:%d:%d\n", uriToFile(loc.URI), loc.StartLine+1, loc.StartCol+1))
```

这是个**便利的边界**——LLM 看到的字符串完全符合人类直觉，而 lsp 包内部严格遵循协议。后续接其他 LSP server 时这个约定不用动。

### 3.2 为什么 URI 用 `file://` 而不是裸路径？

LSP 规范要求文件路径必须是 URI 格式。`fileToURI` / `uriToFile` 在边界翻译：

```go
func fileToURI(path string) string {
    if strings.HasPrefix(path, "file://") { return path }
    return "file://" + path
}
```

**已知不完整**：Windows 上路径含 `\` 时需要转 `/`，且 drive letter 要 `file:///C:/...` 三斜杠——当前实现只覆盖 Linux/macOS。补全时建议用 `url.URL{Scheme:"file",Path:path}.String()`。

### 3.3 为什么 `WorkspaceEdit.Changes` 是 map 而不是 list？

LSP rename 返回的就是"每个文件 → 该文件的所有编辑"——map 形式天然，不用扫两次。代价是遍历顺序不固定（map 迭代）；对 `rename_symbol` 工具来说**确定输出顺序无关紧要**（LLM 看的是文件名+编辑数，不关心顺序）。

---

## 4. `Client` 接口

```go
type Client interface {
    Initialize(ctx, language, rootPath string) error
    Shutdown(language string) error
    ShutdownAll() error

    GotoDefinition(ctx, uri string, line, col int) ([]Location, error)
    FindReferences(ctx, uri string, line, col int) ([]Location, error)
    Rename(ctx, uri string, line, col int, newName string) (*WorkspaceEdit, error)
    Hover(ctx, uri string, line, col int) (*HoverResult, error)
    DocumentSymbols(ctx, uri string) ([]SymbolInfo, error)

    DidChange(ctx, uri, content string) error
}
```

### 4.1 接口设计原则

- **per-language 生命周期**：`Initialize(language, ...)` / `Shutdown(language)`——同一个 client 实例管理多种语言；`ShutdownAll` 用于进程退出时清理；
- **method 与 LSP 1:1**：5 个查询方法直接对应 5 个 LSP method，没引入抽象层；
- **`DidChange` 是 notification（无 response）**：客户端代码改了文件后必须告诉 LSP server，否则 server 还用旧 AST 算定义——这是常见坑；
- **`error` 与 nil 结果分离**：找不到定义不是 error（返回 `nil, nil` 或 空 slice），LSP server 挂了才是 error。这让工具层的 `len(locations) == 0` 判断有意义。

### 4.2 为什么没有 `Completion` / `SignatureHelp`？

补全 / 签名帮助是**人交互**专用——LLM 不需要"打到一半给我提示"。LLM 是一次性生成完整代码，所以这两个 LSP 方法对 agent 场景**无用**。

类似地没暴露 `documentHighlight` / `codeLens` / `formatting`——后两个走 `run_workspace_cmd "gofmt -w"` 更简单。**接口故意收窄到"对 agent 有用的 5 个 method"**，避免维护负担。

---

## 5. 当前占位实现 (`client.go`)

### 5.1 `Initialize` 的语义

```go
func (c *client) Initialize(ctx, language, rootPath string) error {
    if _, exists := c.servers[language]; exists {
        return nil   // 幂等
    }
    c.logger.Info("LSP server initialized (placeholder)", zap.String("language", language))
    c.servers[language] = &serverConn{language: language}
    return nil
}
```

**幂等性是约定**：测试 `TestClient_InitializeShutdown` 显式验证"二次 Initialize 不报错"。这是为了让 orchestrator 在每次工具调用前都能 `Initialize` 一遍而不用记状态：

```go
// 未来 toolGotoDefinition 的伪代码：
lang := detectLanguage(uri)
o.lspClient.Initialize(ctx, lang, workspaceRoot)  // 必要时启 server，已启就 noop
return o.lspClient.GotoDefinition(...)
```

### 5.2 `Shutdown` 容错

```go
func (c *client) Shutdown(language string) error {
    conn, ok := c.servers[language]
    if !ok { return nil }   // 关一个不存在的 server 不是错误
    delete(c.servers, language)
    _ = conn
    return nil
}
```

"关一个本就不存在的 server 不是 error"——配合 `defer lspClient.ShutdownAll()` 让进程退出时永远不会因为状态不一致而 panic。

### 5.3 占位 RPC 方法

```go
func (c *client) GotoDefinition(...) ([]Location, error) {
    return nil, fmt.Errorf("not implemented")
}
// 其余 4 个 RPC 方法相同
```

`DidChange` 是唯一例外——它返回 `nil` 表示"成功"（实际上没干任何事）：

```go
func (c *client) DidChange(...) error {
    return nil
}
```

为什么 DidChange 不报 "not implemented"？因为它是 LSP **notification**（无 response 类型），调用方不会等响应。如果 DidChange 报错，业务代码反而要决定"忽略错误还是 fail"——返回 nil 让上层可以无脑调用，等真实现 ready 再补 wire 逻辑，业务代码无需改动。

---

## 6. 实现路线（从占位到生产）

把 `client.go` 从占位推到生产需要 4 步：

### 6.1 抽离 JSON-RPC 公共包

`internal/jsonrpc/` 从 `internal/mcp/client.go` 抽出来，包含：

- `Conn` 接口：`Call(method, params) (response, error)` + `Notify(method, params) error`
- `StdioTransport`：fork `*exec.Cmd`，挂 stdin/stdout 做 pipe；
- 请求/响应匹配：每条 request 分配自增 ID，`pending map[id]chan Response`，读 loop 按 ID 分发；
- 重连 / context 取消 / 超时——已经在 mcp 实现过。

预计 250 行（mcp 现有 380 行减掉 LSP 不需要的 SSE transport）。

### 6.2 LSP method 包装

每个 LSP method 一个 thin wrapper：

```go
func (rc *realClient) GotoDefinition(ctx, uri string, line, col int) ([]Location, error) {
    lang := rc.detectLang(uri)
    conn := rc.servers[lang].rpc
    if conn == nil { return nil, ErrNoServer }
    
    params := map[string]any{
        "textDocument": map[string]string{"uri": uri},
        "position": map[string]int{"line": line, "character": col},
    }
    var raw []rawLSPLocation
    if err := conn.Call(ctx, "textDocument/definition", params, &raw); err != nil {
        return nil, err
    }
    return convertLocations(raw), nil
}
```

5 个 method 全做完约 200 行（含错误处理 + LSP→internal 数据结构转换）。

### 6.3 Server lifecycle

```go
func (rc *realClient) Initialize(ctx, language, rootPath string) error {
    srvCfg := rc.cfg.Servers[language]
    cmd := exec.CommandContext(ctx, srvCfg.Command, srvCfg.Args...)
    transport, err := jsonrpc.NewStdioTransport(cmd)
    if err != nil { return err }
    
    // LSP initialize handshake
    initParams := map[string]any{
        "processId": os.Getpid(),
        "rootUri":   "file://" + rootPath,
        "capabilities": clientCapabilities(),
    }
    if err := transport.Call(ctx, "initialize", initParams, &initResult); err != nil {
        return err
    }
    transport.Notify(ctx, "initialized", nil)
    
    rc.servers[language] = &serverConn{language, transport}
    return nil
}
```

handshake 是 LSP 协议必须的——不发 `initialize` server 不工作，且 `initialized` notification 也是规范要求。

### 6.4 `DidChange` 真接入

当 `write_file` / `edit_file` / `patch_file` 工具修改文件后，orchestrator 应该调 `lspClient.DidChange(uri, newContent)` 通知 server。**当前没接**——LSP 启用后这是必做项，否则 server 用的还是磁盘文件初始版本，类型推断会过期。

---

## 7. 配置

```yaml
lsp:
  enabled: false                  # 默认关闭
  initialization_timeout: 30s
  request_timeout: 5s
  max_concurrent_requests: 10
  servers:
    go:
      command: gopls
      args: [serve]
      languages: [go]
    python:
      command: pylsp
      languages: [python]
    typescript:
      command: typescript-language-server
      args: [--stdio]
      languages: [typescript, javascript, tsx, jsx]
    rust:
      command: rust-analyzer
      languages: [rust]
```

`servers` 是 map，**key 是 server 名**（不是 language——一个 server 可能服务多种语言，如 ts server）。`languages` 字段告诉客户端哪些文件路由到这个 server。

`max_concurrent_requests` 的语义是"同时 in-flight 的 LSP 请求上限"——gopls 在大 workspace 下处理 `find_references` 可能要 2s，并发太多会阻塞。当前 stub 没用到，补完时需要在 `realClient` 加 semaphore。

---

## 8. 与 orchestrator 的集成

### 8.1 注入

```go
// cmd/agent/main.go:635
if cfg.LSP.Enabled {
    lspCfg := lsp.Config{Servers: ..., Timeout: cfg.LSP.MaxConcurrentRequests}
    lspClient := lsp.NewClient(lspCfg, logger)
    orch.SetLSPClient(lspClient)
    defer lspClient.ShutdownAll()
}
```

`SetLSPClient` 同时触发 `RegisterLSPTools`——也就是说**没启用 LSP 就不会注册这 4 个工具**，LLM 的工具菜单干净。这是有意为之：让"功能不可用"在工具列表层面就消失，比让 LLM 调到一半再报错友好得多。

### 8.2 工具注册的 RiskLevel

```go
if lt.name == "rename_symbol" {
    tool.def.RiskLevel = 2  // 跨文件修改 = 高风险
}
```

`rename_symbol` 是唯一**写**类 LSP 工具——一次调用可能同时改 20 个文件。RiskLevel=2 让上层（[18_auth](18_auth_security.md) 的 sensitive 检测）可以决定"是否需要 human-in-the-loop 审批"。其他 3 个工具（goto/find/hover）都是只读，RiskLevel=0。

### 8.3 多代理白名单

`internal/multiagent/sub_agent.go` 的 `allowedTools()`（见 [22_multiagent](22_multiagent.md) §6.3）：

| AgentType | LSP 工具 |
|-----------|----------|
| `AgentCode` | goto_definition / find_references / hover_info / rename_symbol |
| `AgentTest` | （不包含） |
| `AgentReview` | goto_definition / find_references / hover_info |

review agent 拿不到 `rename_symbol`——只读审查不应做改动。这是工具层的最小权限原则。

---

## 9. 与 tree-sitter 的降级关系

[24_treesitter](24_treesitter.md) §6.3 提到："LSP 不可用时用 treesitter `ExtractSymbols` 实现 `goto_definition` 等工具的降级"。

**当前代码并未实现这个降级**——`lsp_tools.go` 失败时直接返回 error：

```go
locations, err := o.lspClient.GotoDefinition(ctx, uri, params.Line-1, params.Column-1)
if err != nil {
    return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
}
```

补完降级路径要做的：

```go
locations, err := o.lspClient.GotoDefinition(...)
if err != nil && o.tsParser != nil {
    // 降级：在 tree-sitter symbol table 里找同名符号
    // 优点：跨进程不依赖、毫秒级
    // 缺点：同名符号会全部返回（无类型推断）
    fallback := o.tsFallbackDefinition(uri, params.Line, symbolAtPosition(...))
    return wrapResult(fallback, "(via tree-sitter, no type info)"), nil
}
```

降级的代价是**精度损失**——`tree-sitter` 找到的是"名字一样"的符号，可能命中 5 个无关同名函数；LSP 通过类型系统知道"真的就是这一个"。给 LLM 加个标注 "(via tree-sitter)" 让它知道自己拿到的是近似结果，需要交叉验证。

---

## 10. 设计权衡

| 抉择 | 动机 |
|------|------|
| 接口先于实现 | 让 orchestrator 工具、配置、测试三路并行推进 |
| 占位返回 "not implemented" | 显式比沉默更安全；LLM 拿到 error 会自然换工具 |
| `Enabled: false` 默认 | 工具菜单不被永远报错的工具污染 |
| per-language 一个 server | LSP 协议本身的约束；无优化空间 |
| `Initialize` 幂等 | 调用方无需记忆状态，简化使用 |
| 0-based 内部 / 1-based 外部 | 协议规范 vs 人类习惯，边界做转换最清晰 |
| 没暴露 Completion / SignatureHelp | agent 不需要交互式补全 |
| 复用 mcp 的 JSON-RPC 而非引入第三个 | 减重，未来抽 internal/jsonrpc 共用 |
| `rename_symbol` RiskLevel=2 | 跨文件改动需要更高审批门槛 |
| `DidChange` 占位返回 nil（不是 error） | notification 无 response；让调用方可以无脑调 |
| 没在工具层做 tree-sitter 降级 | 占位阶段先把接口稳了，降级是补完阶段的事 |

---

## 11. 后续演进

- [ ] **真接 JSON-RPC**：抽 `internal/jsonrpc` 公共包 + 在 lsp 包实现 5 个 method 的真 wire 调用
- [ ] **LSP initialize handshake**：发 `initialize` + `initialized` notification + 解析 `serverCapabilities`
- [ ] **`DidChange` 接入文件工具**：`write_file` / `edit_file` / `patch_file` 后通知 LSP server
- [ ] **tree-sitter 降级路径**：`lsp_tools.go` 失败时回退到 `o.tsParser.ExtractSymbols` + 名字匹配
- [ ] **懒启动**：移除 `Initialize` 显式调用，第一次某语言的工具调用时再 fork server
- [ ] **请求超时 / 并发上限**：`cfg.LSP.RequestTimeout` 与 `MaxConcurrentRequests` 在 `realClient` 生效
- [ ] **Windows 路径支持**：`fileToURI` 用 `url.URL` 兼容 `file:///C:/...`
- [ ] **DocumentSymbols 工具暴露**：把 `DocumentSymbols` 也注册成 ReAct 工具（"列出本文件所有符号"）
- [ ] **server crash 自动重启**：模仿 `mcp.Client` 的 reconnect，server 异常退出后自动 respawn
- [ ] **可观察性**：`lsp_requests_total{method,language,status}` / `lsp_request_duration_seconds`
- [ ] **跨 server hover 合并**：同一文件多语言（vue/svelte 单文件含 ts+css）时合并多 server 结果
- [ ] **LSP client capabilities 配置化**：当前 hardcoded，应该跟着配置走（哪些 server 启用哪些 feature）

---

## 12. 与人类 IDE 使用的类比

| 人类 IDE | 本模块（完整实现后） |
|----------|----------------------|
| 右键 → Go to Definition | `goto_definition` 工具 |
| F2 重命名（含跨文件） | `rename_symbol` 工具 |
| Ctrl/Cmd + click 跳转 | `goto_definition` |
| Hover 弹窗 | `hover_info` |
| Find Usages | `find_references` |
| 大纲视图 | `DocumentSymbols`（接口已留） |
| Quick Fix / Lightbulb | ❌ 不暴露（agent 不需要交互式辅助） |
| 实时诊断（红波浪线） | ❌ 暂未规划（可以从 LSP `publishDiagnostics` 拿） |

agent 和人类用 IDE 的最大区别是**没有"看一眼就懂"**——所有反馈必须文本化喂给 LLM。所以补全 / signature help / lightbulb 这些"光标停在那就给提示"的人交互特性对 agent 完全无用，而"跳过去看清楚再说"这类**显式查询**才是核心需求。LSP 暴露的 4 个工具正好是这类核心需求的子集。

---

下一篇：[`28_generator.md`](28_generator.md) —— 项目生成器：从描述到可运行项目骨架。
