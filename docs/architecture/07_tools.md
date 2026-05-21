# 07 · 工具注册表 `internal/tools`

> 代码：
> - `registry.go` (160) — `Tool` / `Provider` 接口 + `Registry` 索引 + `Execute` 分发
>
> 测试：`registry_test.go` (224) — 覆盖注册、去重、Provider 批量、Execute metrics 打点

---

## 1. 模块定位

**"让 LLM 的每一次 `function_call` 都能命中正确的手，不关心这只手是谁长出来的。"**

在本包诞生**之前**，`orchestrator.getAvailableTools()` 和 `orchestrator.executeTool()` 是一长串 if/else：

```go
// 旧世界（反面教材）：
if name == "execute_code" { return sandbox.Execute(...) }
if name == "search_code"  { return rag.Search(...) }
if name == "edit_file"    { return fileTools.Edit(...) }
if strings.HasPrefix(name, "mcp_") { return mcpGw.Call(...) }
if strings.HasPrefix(name, "skill_") { return skillReg.Call(...) }
// ... 每新增一类就要再加一条 + 把依赖一路塞进 orchestrator 构造函数
```

这种耦合一旦工具种类 > 5 就会失控。本包提供：

1. **统一接口 `Tool`**：任何实现了 `Definition() + Execute()` 的 struct 都能被注册；
2. **批量导入 `Provider`**：MCP 网关、Skill 注册中心、内置 file tools 都可以**一次性**把自己的全部工具塞进来；
3. **并发安全的索引**：`map[name]Tool` + `sync.RWMutex`；
4. **自动打点**：每次 `Execute` 顺手把 duration/status 上报到 Prometheus。

---

## 1.5 核心设计问题

### 为什么需要"工具注册表"而不是代码里写死？

**静态注册（硬编码）**：`orchestrator.executeTool` 里大 switch。
- ✅ 编译期类型安全
- ❌ 加新工具要改 orchestrator 代码；不利于 MCP / Skill 这类"运行时动态注册"

**动态注册表**：工具实现方 Register 自己到 Registry，orchestrator 通过
Registry 查和调。
- ✅ 新工具（MCP server / user-defined Skill）零侵入加入
- ✅ LLM 看到的 tool_schema 由 Registry 按**稳定顺序**生成
- ❌ 运行时查找，类型安全全靠约定

**决策**：动态。代价是一次静态 sort 来保证 KV cache 稳定性。

### 为什么 Definition 必须按名字排序？

LLM prompt 里 tool definition 数组的**字节顺序**影响 KV cache 命中。
如果用 map 遍历，同一组 tools 每次序列化顺序都可能不同 → 每次 prompt
都 miss cache → 延迟和费用各 × 2（见 [13_context](13_context.md)）。

`sort.Slice(defs, func(i,j) bool { return defs[i].Name < defs[j].Name })`
看起来是"无关紧要的排序"，实际上是性能关键路径。

### 为什么 Registry 是并发安全的？

添加 MCP server 的请求来自 API（管理面），调用 tool 的请求来自
orchestrator（数据面）——两者并发。用 `sync.RWMutex`：读多（数据面每
请求都读）/写少（管理面偶尔加删）。

---

## 2. 依赖架构

```
          ┌─── orchestrator.go ──────────┐
          │  getAvailableTools()          │── reg.Definitions() ──► LLM prompt
          │  executeTool(name, args)      │── reg.Execute(...)   ──► ToolResult
          └───────────────────────────────┘
                          │
                          ▼
                ┌────────────────────┐
                │   tools.Registry   │   map[string]Tool + RWMutex
                └────────┬───────────┘
             Register /  │  RegisterProvider
                 ────────┼────────
     ┌────────┬─────┬────┴────┬───────────┐
     ▼        ▼     ▼         ▼           ▼
  file-edit  git  run_cmd   mcp.Gateway  skill.Registry
  file-read  ...           (Provider)   (Provider)
  (builtin Tool impls)     批量把所有    批量把所有
                           MCP tools    Skill tools
                           注册进来      注册进来

                          │ 每次 Execute
                          ▼
                ┌────────────────────┐
                │ metrics.ToolExec*  │  Prometheus 指标
                └────────────────────┘
```

---

## 2.5 数据流总览

本模块有两条主数据流：**注册流**（启动时 + 运行时动态）和**执行流**（每次 LLM tool_call）。

```text
═══════════════════════ 注册流 (启动 + 运行时) ═══════════════════════

┌────────────────┐  ┌────────────────┐  ┌────────────────────────┐
│BuiltinProvider │  │ mcp.Gateway    │  │ skill.Registry         │
│(file/git/cmd)  │  │ (Provider)     │  │ (Provider)             │
└───────┬────────┘  └───────┬────────┘  └───────────┬────────────┘
        │                   │                       │
        │  Tools()          │  Tools()              │  Tools()
        ▼                   ▼                       ▼
┌────────────────────────────────────────────────────────────────┐
│            Registry.RegisterProvider(p)                         │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │ for t := range p.Tools():                                │  │
│  │   mu.Lock()                                              │  │
│  │   if tools[t.Name] exists → skip (先注册者优先)           │  │
│  │   else → tools[t.Name] = t                              │  │
│  │   mu.Unlock()                                            │  │
│  └──────────────────────────────────────────────────────────┘  │
└────────────────────────────────┬───────────────────────────────┘
                                 │
                                 ▼
                  map[string]Tool (RWMutex 保护)


═══════════════════════ 执行流 (每次 tool_call) ═══════════════════════

┌─────────────────────┐
│ orchestrator.       │
│ reactLoop 每步循环  │
└──────────┬──────────┘
           │
     ┌─────┴──────────────────────────────┐
     │                                    │
     ▼                                    ▼
reg.Definitions()              reg.Execute(name, args)
     │                                    │
     ▼                                    ▼
┌─────────────────────┐    ┌─────────────────────────────────┐
│ sort.Slice by Name  │    │ mu.RLock()                      │
│ → []ToolDefinition  │    │ t := tools[name]                │
│ (确定性顺序保证      │    │ mu.RUnlock()                    │
│  LLM KV-cache命中)  │    │ ★ 锁外执行 ★                    │
└─────────┬───────────┘    │ result := t.Execute(ctx, args)  │
          │                └────────────────┬────────────────┘
          ▼                                 │
┌─────────────────────┐                     ▼
│ 注入 LLM prompt     │    ┌─────────────────────────────────┐
│ tools: [...]        │    │ Prometheus 指标记录              │
│ (OpenAI function    │    │ tool_execution_duration_seconds  │
│  calling format)    │    │ tool_execution_total{name,ok/err}│
└─────────────────────┘    └────────────────┬────────────────┘
                                            │
                                            ▼
                           ┌─────────────────────────────────┐
                           │ *ToolResult{Content, IsError}   │
                           │ → 返回 orchestrator             │
                           └─────────────────────────────────┘
```

---

## 3. 两个核心接口

### 3.1 `Tool` —— 单个能力

```go
type Tool interface {
    Definition() models.ToolDefinition
    Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error)
}
```

两个方法各司其职：

| 方法 | 何时调用 | 作用 |
|---|---|---|
| `Definition()` | 组装 LLM prompt 时 | 返回 JSON Schema，告诉 LLM 这个工具叫什么、怎么传参 |
| `Execute()` | LLM 决定调用时 | 按传入的 raw JSON 执行，返回结果 |

实现者必须 **concurrent-safe** —— 多个 goroutine 可能同时调用同一个 `Tool` 实例（比如 LLM 一次回来两个工具调用，orchestrator 并发执行）。

### 3.2 `Provider` —— 工具批发商

```go
type Provider interface {
    Name() string
    Tools() []Tool
}
```

动机：MCP Gateway 一次提供几十个工具（github-mcp 一个 server 就有 `create_issue`、`list_prs`、`search_repos`……），不可能让调用者循环一遍 `reg.Register()`。

实现 `Provider` 的典型例子：

- `mcp.Gateway.Tools()` → 返回当前所有已连接 server 发布的工具的 `Tool` 包装；
- `skill.Registry.Tools()` → 返回所有注册的自定义 skill；
- `orchestrator/file_tools.go` 的 `BuiltinFileToolProvider()` → 一次性注册 `read_file` / `write_to_file` / `replace_in_file` 等。

---

## 4. `Registry` —— 核心容器

```go
type Registry struct {
    mu    sync.RWMutex
    tools map[string]Tool
}
```

**为什么只有一个 map 就够？**

- key 用工具全名（如 `mcp_github_create_issue` / `skill_http_echo` / `read_file`）确保全局唯一；
- 注册时报错（duplicate）即可避免命名冲突；
- 查找是 O(1)，和 tool 数量无关。

锁策略：

| 操作 | 锁类型 |
|---|---|
| `Register / Unregister` | `mu.Lock()` 写锁 |
| `Get / Definitions / Execute 的查表` | `mu.RLock()` 读锁 |
| `Execute` 的真正调用 | **不持锁** (释放锁后再调 Tool) |

→ 不持锁执行的原因：工具调用可能花几秒到几十秒（如 `run_command`、`execute_code`），持锁会把整个 registry 冻结。拿到 Tool 指针后释放锁，让别人可以并发注册/查询。

---

## 5. `Register` —— 单个注册

```go
Register(t Tool) error {
    if t == nil          → error
    def := t.Definition()
    if def.Name == ""    → error
    mu.Lock()
    if _, exists := tools[def.Name]:
        return fmt.Errorf("tool %q already registered", def.Name)
    tools[def.Name] = t
    mu.Unlock()
}
```

**刻意不允许覆盖注册**。原因：

- 工具名冲突通常是 bug（两个 Provider 用了同样的命名），应早期失败；
- 如果要更新，显式调 `Unregister(name)` 再 `Register(t)`，语义清晰。

→ 代价：MCP Server reconnect 时若 tool 名变了，需要先 Unregister 旧的（orchestrator 会在 `onMCPReconnected` 回调里处理）。

---

## 6. `RegisterProvider` —— 批量导入

```go
RegisterProvider(p Provider) int {
    n := 0
    for _, t := range p.Tools():
        if err := Register(t); err == nil:
            n++
    return n
}
```

**对错误的宽容**：一个 tool 名冲突时跳过继续，不中断 provider 的其他 tool。

典型调用路径（在 `cmd/agent/main.go` 启动时）：

```go
reg := tools.NewRegistry()

// 1. 内置工具（file / git / run_cmd / search_code / execute_code）
reg.RegisterProvider(orchestrator.NewBuiltinProvider(sandboxMgr, ragEngine, workspaceMgr))

// 2. MCP 工具（初始化时已连的 server）
reg.RegisterProvider(mcpGateway)

// 3. Skill 工具（用户自定义）
reg.RegisterProvider(skillRegistry)

logger.Info("tools registered", zap.Int("count", reg.Len()))
```

运行时动态增加：

```go
// 用户新挂一个 MCP Server
mcpGateway.AddServer(cfg)          // 见 06_mcp.md §8
reg.RegisterProvider(mcpGateway)   // 再叠加一次；已存在的跳过，新的进来
```

---

## 7. `Definitions` —— 给 LLM 的菜单

```go
Definitions() []models.ToolDefinition {
    out := make([]ToolDefinition, 0, len(tools))
    for _, t := range tools:
        out = append(out, t.Definition())
    sort.Slice(out, by Name)       // 确定性排序
    return out
}
```

**为什么排序？**

- LLM prompt 的确定性 = 更好的 **prompt cache 命中**（OpenAI / Anthropic 都对长 prompt 做 prefix cache）；
- 日志/测试快照更稳定；
- Definitions 返回的 slice 每次 chat 都要塞进 LLM API 的 `tools` 参数，按名字排序避免同一组工具呈现为不同序列导致 cache miss。

---

## 8. `Execute` —— 带打点的分发

```go
Execute(ctx, name, args):
  mu.RLock
  t, ok := tools[name]
  mu.RUnlock
  if !ok:
    return nil, ErrToolNotFound

  start  := time.Now()
  source := t.Definition().Source   // "builtin" / "mcp" / "skill"
  result, err := t.Execute(ctx, args)

  elapsed := time.Since(start)
  status  := inferStatus(err, result)  // "success" / "error" / "tool_error"

  metrics.ToolExecutionDuration.WithLabelValues(name, source, status).Observe(elapsed)
  metrics.ToolExecutionTotal   .WithLabelValues(name, source, status).Inc()

  return result, err
```

三种 status 的区别：

| status | 条件 | 含义 |
|---|---|---|
| `success` | err == nil && !result.IsError | 正常完成 |
| `error` | err != nil | 系统级错误（超时/panic/网络）|
| `tool_error` | err == nil && result.IsError | 工具**语义**错误（脚本退出非 0 / MCP 返回 `isError:true`） |

分开记录是为了让 Grafana 面板能区分**我的系统坏了**（error 率上涨）和**LLM 生成的参数有问题**（tool_error 率上涨）。

---

## 9. 与其他模块的边界

### 9.1 向上：orchestrator 的黑盒依赖

orchestrator 只拿一个 `*tools.Registry`：

```go
type Orchestrator struct {
    tools *tools.Registry
    // ...
}

(o) chat(messages []Message) ... {
    // 1. 装菜单
    defs := o.tools.Definitions()
    resp := o.llm.Call(messages, defs)

    // 2. 执行 LLM 点的菜
    for _, call := range resp.ToolCalls:
        result, err := o.tools.Execute(ctx, call.Name, call.Args)
        // ...
}
```

orchestrator 不知道一个工具到底是 "builtin / mcp / skill"，也不需要知道。

### 9.2 向下：Tool 实现者的义务

任何 `Tool` 实现者：

```go
type myTool struct { /* deps */ }

(t *myTool) Definition() models.ToolDefinition {
    return models.ToolDefinition{
        Name:        "my_tool",
        Description: "what it does",
        Parameters:  json.RawMessage(`{"type":"object","properties":{...}}`),
        Source:      "builtin",    // 重要：让 Execute 的 metrics 有意义
    }
}

(t *myTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
    // 1. Unmarshal args 到具体的 struct
    // 2. ctx 超时控制
    // 3. 返回 *models.ToolResult{ Content: "...", IsError: false }
}
```

关键点：

- **ctx 必须透传**；orchestrator 会给每次工具调用设总超时（默认 60s）；
- **panic 要抓**：虽然 orchestrator 外层有 recover，但 Tool 实现最好自己兜底；
- `ToolResult.Content` 是字符串 —— 太大的结果（>32KB）应自己截断并打标注。

---

## 10. 命名规范

虽然接口不强制，但约定俗成：

| 来源 | 前缀 | 示例 |
|---|---|---|
| 内置 | 无前缀，snake_case | `read_file`、`write_to_file`、`execute_code` |
| MCP | `mcp_<server>_<tool>` | `mcp_github_create_issue` |
| Skill | `skill_<name>` | `skill_send_slack`、`skill_deploy_staging` |

这套前缀既避免冲突，也让 LLM 仅从名字就能推测能力范围。

---

## 11. 设计权衡

| 抉择 | 动机 |
|---|---|
| 用 `interface` 而非 `map[string]func(args)` | Tool 可能需要内部依赖（sandbox 客户端、仓库句柄等），struct 方法比闭包更自然 |
| Registry 不感知 source 类型（builtin/mcp/skill） | 强保持单一职责；source 信息通过 `ToolDefinition.Source` 字段带过来 |
| `Definitions()` 排序固定 | LLM prompt cache 命中率；日志/快照稳定 |
| 注册冲突**不**覆盖 | 捕获命名 bug 早；动态场景强制显式 Unregister → 更清楚 |
| Execute 内打 metrics 而非装饰器外包 | 保证任何调用方都能享受打点；避免 orchestrator/其它地方忘了 |
| 不实现 Tool 级权限 | 权限属于上层策略（HITL / audit），本层保持纯净 |
| `Provider` 接口**只**提供 Tools()，不管生命周期 | 生命周期由各 Provider 自己负责（MCP Gateway 有 reconnect，Skill 有 CRUD），Registry 不插手 |
| 没实现工具别名/tag | YAGNI；将来真需要再加 `RegisterAlias(name, target)` |

---

## 12. 后续演进

- [ ] **Tool Middleware**：接受 `Middleware(Tool) Tool` 做 cross-cutting（auth, logging, rate limiting）；当前是 hard-coded metrics；
- [ ] **分组 / 命名空间**：大项目可能有上百个 MCP tool，可以按 provider 分组，让 LLM 一次只看一部分（减 token）；
- [ ] **工具级超时/重试策略**：目前由 orchestrator 统一控制；可以让 Tool 在 `Definition()` 里声明自己的期望超时；
- [ ] **`Execute` 返回流式 `chan string`**：sandbox stream / MCP stream 目前各走各的 API，没法经过统一 Registry；将来可加 `Streaming()` 接口；
- [ ] **`Unregister` 批量化**：MCP Server 下线时要删掉它注册的所有 tool，目前靠 Provider 侧维护名字列表手工删；加一个 `UnregisterByPrefix("mcp_github_")` 更方便；
- [ ] **工具健康检查**：与 MCP `healthChecker` 类似，内置工具也可能挂（sandbox docker daemon down），Registry 可以缓存 "tool 可用性"；
- [ ] **metrics 按用户维度拆分**：目前 labels 是 `(name, source, status)`；加 `user_id` 可以看谁在疯狂调 expensive tool —— 配合 `18_auth` 里的用户画像；
- [ ] **失败工具自动下线**：连续 N 次 error → 临时从 Definitions() 里剔除，避免 LLM 反复重试坏工具。

---

## 10. 实现剖析与改进方向

### Registry 的并发模型

```go
type Registry struct {
    mu        sync.RWMutex
    tools     map[string]*Tool   // name → Tool（含 handler）
    sortedDef []models.ToolDef   // 排序后的 definition 列表（缓存）
    dirty     bool               // sortedDef 是否需要重建
}

func (r *Registry) Register(t *Tool) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    if _, exists := r.tools[t.Name]; exists { return ErrAlreadyRegistered }
    r.tools[t.Name] = t
    r.dirty = true   // 下次 Definitions() 重建 sortedDef
    return nil
}

func (r *Registry) Definitions() []models.ToolDef {
    r.mu.RLock()
    if !r.dirty { defer r.mu.RUnlock(); return r.sortedDef }
    r.mu.RUnlock()

    r.mu.Lock(); defer r.mu.Unlock()
    r.rebuildSortedDef()  // 按 Name 排序
    return r.sortedDef
}
```

**关键**：排序是**结果缓存**而不是每次重排。register 频率低（启动时 + 少数
动态加），read 频率高（每步 ReAct）。

### Pros
- ✅ 动态注册让 MCP / Skill 无侵入接入
- ✅ 稳定排序保 KV cache（见 [13_context](13_context.md)）
- ✅ 并发安全 RW mutex

### Cons
- ⚠️ 没有 tool 级别的熔断 / 限流
- ⚠️ 没有 "per-user 允许调用哪些 tool" 的 ACL
- ⚠️ Registration 顺序决定 ID（如果用序号 ID），不稳定

### 改进方向
- **P1** — per-tool 限流：防止 LLM 疯狂调 search_code 100 次
- **P1** — 工具 ACL：按 claims.Role 过滤可见工具
- **P2** — 动态卸载：热卸载某个 MCP server 时同步清掉它注册的所有 tool

---

下一篇：`08_skill.md` —— Skill 注册中心：HTTP webhook / 内置函数两种自定义工具的实现。
