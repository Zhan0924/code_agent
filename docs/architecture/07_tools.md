# 07 · 工具注册表 `internal/tools`

> 代码：
> - `registry.go` (160) — `Tool` / `Provider` 接口 + `Registry` 索引 + `Execute` 自动打点
> - `dynamic_tool.go` (153) — 运行时动态工具：`DynamicToolConfig` + webhook / inline 执行器
>
> 调用方（实际接线）：
> - `internal/orchestrator/builtin_tools.go` (117) — `builtinTool` 适配器 + `RegisterBuiltinTools` / `RegisterFileTools`
> - `internal/orchestrator/lsp_tools.go` (`RegisterLSPTools`) / `pty_tools.go` (`RegisterPTYTools`) — P1 工具按需注册
> - `internal/orchestrator/orchestrator.go:232,237,1438-1442,1532-1545` — Registry 创建 + 三级 dispatch + 聚合
>
> 测试：`registry_test.go` (224) — 覆盖注册、去重、Provider 批量、Execute metrics 打点

---

## 1. 模块定位

**"为 orchestrator 提供一个'已注册工具'索引——名字 → 实现，O(1) 命中，带 Prometheus 自动打点。**MCP 工具走 `mcp.Gateway` 自己的路由，Skill 走 `skill.Registry`，三者在 orchestrator 那一层各自被查询、聚合**——`tools.Registry` 只承载内置工具（execute_code / search_code / read_file 等）+ 运行时动态工具，不是统一容器。"**

这与 `registry.go` 包注释里描绘的"统一容器"愿景**存在差距**：`tools.Provider` 接口存在但全工程无人实现/调用，MCP 与 Skill 都没有 `RegisterProvider` 进来。下文会讲清"接口设计"与"实际接线"两层。

本包真正在做的事：

1. **`Tool` / `Provider` 抽象**：任何 `Definition() + Execute()` 都能注册；`Provider` 批量（未使用）；
2. **`Registry` 容器**：`map[string]Tool` + `sync.RWMutex`；
3. **`Execute` 带打点**：每次调用上报 `tool_execution_duration_seconds{name,source,status}` 和 `tool_execution_total`；
4. **`DynamicTool`**：REST API 注入的运行时工具（webhook 调用外部 URL；inline 占位未实现）。

---

## 1.5 设计哲学：4 个核心抉择

### Q1 — 为什么 Registry 没有"统一所有 source"？

包注释（`registry.go:1-15`）写的是"内置 + MCP + Skill + LSP 统一抽象"，但实际接线没走通：

- `mcp.Gateway` 通过 `FindServerForTool` + `CallTool` 走自己的路由（`internal/mcp/client.go:605`）；
- `skill.Registry` 通过 `FindSkill` + `Execute` 走自己的路由；
- `tools.Registry` 只承载内置 + 动态工具。

为什么没统一？三类 source 的**生命周期差异**太大：MCP 子进程崩了要 reconnect 重建工具集；Skill 是 YAML 模板加载 + 动态 reload；内置工具是 Go 闭包，启动即定。把它们塞进同一个 `map[string]Tool` 后，每次 reconnect / reload 都要算 diff（哪些要删、哪些要加、谁还在用）——复杂度爆炸。

**实际选择**：在 orchestrator 层做"**三级 dispatch + 三源聚合**"——`Registry` 保持纯粹（内置 + 动态），MCP / Skill 各管各的生命周期。代价是 `tools.Provider` 接口形同虚设（保留是为了将来真有更多 source 时还能用）。

### Q2 — 为什么 `Definitions()` 每次都 sort，不缓存？

LLM prompt 里 tool definitions 的**字节序**影响 KV cache 命中：map 遍历每次顺序不同 → 每次 prompt 不一样 → 每次 cache miss。`sort.Slice(out, by Name)` 把工具排成确定顺序保 cache 命中（[13_context.md](13_context.md) 详述）。

**为什么不缓存 sortedDef**？三个原因：

1. 注册操作仅在启动 + 极少数动态注册时发生（动态 Skill 一天可能就几次），而 `Definitions()` 也仅每个 ReAct step 调一次（不是热路径里的热路径）；
2. 缓存 + 失效要加 `dirty bool` + 双重锁 upgrade（RLock → RUnlock → Lock），代码复杂度上升不止一倍；
3. 实测 sort 50 个工具 < 50μs，对延迟（ReAct step 总耗时秒级）完全不可见。

→ 旧文档曾经描述过 `sortedDef` 缓存方案——那是**未实现的设计草稿**，已删除。

### Q3 — 注册冲突为什么"先到先得"而非"后来覆盖"？

`Register` 在 name 已存在时**返回错误**（`registry.go:74-76`）：

```go
if _, exists := r.tools[def.Name]; exists {
    return fmt.Errorf("tool %q already registered", def.Name)
}
```

后来覆盖看似友好（"hot reload tools"），但**会让 bug 更难发现**：两个 Provider 各自注册了 `read_file`，覆盖语义意味着结果取决于注册顺序，且不报警。先到先得 + 显式 `Unregister → Register` 让命名冲突立刻暴露在启动日志。

代价：MCP server reconnect 后若工具集变了（如 server 升级新增工具），调用方要先 Unregister 老工具——但当前 MCP 路径根本不走 Registry，这个代价没真正发生。

### Q4 — `Execute` 内嵌打点 vs 装饰器包一层？

每次 `Execute` 都打 `tool_execution_duration_seconds` + `tool_execution_total`（`registry.go:144-159`）。理论上更优雅的做法是 `WithMiddleware(metricsMiddleware)`，让调用方决定是否打点。

**实际选择**内嵌打点的原因：

- 全工程**只有 orchestrator 这一个调用方**；多层抽象的"灵活性"没人需要；
- 内嵌的代价是固定 4 行代码 + 强制三标签（name/source/status），收益是"任何路径打 Execute 都自动有指标"；忘了的可能性为 0。

将来若真有第二个调用方且需求不同（如 sub-agent 不打 metric），再抽 middleware 不迟（YAGNI）。

---

## 2. 依赖架构

```
              ┌────────── cmd/agent/main.go ──────────┐
              │  apiServer.SetGenerator(...)          │
              │  orch := NewOrchestrator(...)         │ ← 构造时内置 toolRegistry
              └─────────────────┬────────────────────-┘
                                │
                                ▼
              ┌──────────────────────────────────────┐
              │  Orchestrator                         │
              │   .toolRegistry  *tools.Registry      │ ← 内置 + LSP + PTY + Dynamic
              │   .mcpGateway   *mcp.Gateway          │ ← 独立路由
              │   .skillRegistry  interface           │ ← 独立路由
              └─────────┬───────────────┬─────────────┘
                        │               │
            ┌───────────┘               └────────────┐
            ▼                                        ▼
     ┌──────────────────────┐         ┌──────────────────────────────┐
     │ tools.Registry        │         │ MCP / Skill 路由              │
     │   map[name]Tool       │         │ Gateway.CallTool             │
     │   sync.RWMutex        │         │ SkillRegistry.Execute        │
     │   sort + Execute      │         │ 各自打 metrics                │
     └─────────┬─────────────┘         └──────────────────────────────┘
   Register /  │  Unregister
       ───────┼───────
   ┌────┬────┴────┬─────────┬──────────┐
   ▼    ▼         ▼         ▼          ▼
RegisterBuiltinTools  RegisterFileTools  RegisterLSPTools  RegisterPTYTools  RegisterDynamicTool
(execute_code,        (read_file/        (goto_definition  (shell_exec)      (REST API 注入的
 search_code)         write_file/...     /find_references                     webhook 工具)
                      run_tests/run_     /hover_info)
                      workspace_cmd +
                      git_*)

每次 LLM 调用前：
  orch.GetAvailableTools():
     = toolRegistry.Definitions()      ← 内置 + Dynamic + LSP + PTY
     + mcpGateway.GetAvailableTools()  ← MCP
     + skillRegistry.GetToolDefinitions() ← Skill
```

---

## 2.5 数据流总览

### 流 A：启动注册（`NewOrchestrator` 内部）

```text
orchestrator.NewOrchestrator(cfg, deps...):
  ① o.toolRegistry = tools.NewRegistry()        (orchestrator.go:232)
  ② o.RegisterBuiltinTools(o.toolRegistry)      (orchestrator.go:237)
       → builtinTool{execute_code, source:"builtin"}
       → builtinTool{search_code, source:"builtin"}
  ③ if workspaceMgr != nil:
       o.RegisterFileTools(o.toolRegistry)
       → builtinTool{read_file, write_file, patch_file, edit_file, apply_diff,
                     list_files, create_directory, run_tests, run_workspace_cmd}
       → builtinTool{git_status, git_diff, git_log, ...}
  ④ if cfg.LSP.Enabled:
       o.RegisterLSPTools(o.toolRegistry)
       → goto_definition, find_references, hover_info, rename_symbol
  ⑤ if cfg.PTY.Enabled:
       o.RegisterPTYTools(o.toolRegistry)
       → shell_exec（PTY 持久 shell session）

每个 builtinTool 是一个轻 adapter：
   struct { def models.ToolDefinition; handler func(ctx, args) }
   Definition() → def
   Execute() → handler(ctx, args)


运行时（REST API：POST /api/v1/tools）:
  → tools.NewDynamicTool(cfg)         (dynamic_tool.go:71)
     ├─ ExecutorType == "webhook"     → createWebhookExecutor (HTTP POST 到 cfg.URL)
     └─ ExecutorType == "inline"      → createInlineExecutor  (占位未实现)
  → o.RegisterDynamicTool(tool)       (orchestrator.go:1628)
     → toolRegistry.Register(tool)


MCP / Skill 路径：
  config.MCP.Servers → mcp.NewGateway(...) → 独立维护 servers + toolIndex
  config.Skills      → skill.NewRegistry(...) → 独立维护 skills map
  ★ 两者都不走 tools.Registry ★
```

### 流 B：每次 LLM 请求前的工具菜单组装

```text
orchestrator.GetAvailableTools()  (orchestrator.go:1532)
  ┌────────────────────────────────────────────────────────┐
  │ tools = []                                              │
  │ if toolRegistry:                                        │
  │   tools += toolRegistry.Definitions()  (sorted by Name) │
  │ if mcpGateway:                                          │
  │   tools += mcpGateway.GetAvailableTools()               │
  │ if skillRegistry:                                       │
  │   tools += skillRegistry.GetToolDefinitions()           │
  │ return tools                                             │
  └────────────────────────────────────────────────────────┘
                            │
                            ▼
              llm.ChatCompletion(messages, tools=tools)
                            │
                            ▼
              resp.ToolCalls = [...]
```

**注意 3 个 source 内部各自有序，但拼起来后整体 NOT sorted**——`tools.Registry.Definitions()` 内部已 sort，MCP/Skill 的顺序由 map 遍历决定。这是一个**已知的 KV cache 优化空缺**：若三者都按 Name 全局排序，前缀 cache 命中率还能再高几个百分点（详见 §12 P0）。

### 流 C：单次 tool_call dispatch（核心）

```text
orchestrator.executeTool(ctx, tc):                                (orchestrator.go:1360)
  ① HITL 风险拦截：
       def = toolRegistry.Get(tc.Name).Definition()
       if def.RiskLevel >= 2 && !skipHITL(ctx):
           return "⚠️ Tool requires approval"  + IsError=true     ← 拦截到此为止
  ② 投机缓存查询：
       if toolCache.Get(scope, tc.Name, tc.Args) hit:             ← idempotent reads
           return cached
  ③ Transaction snapshot（仅写工具）：
       captureForTransaction(tc)                                  ← 写前快照 .bak
  ④ dispatchTool(ctx, tc):                                        (orchestrator.go:1426)
       三级 dispatch：
       ┌─────────────────────────────────────────────────────┐
       │ a. if mcpGateway.FindServerForTool(tc.Name):         │  ← 第一级：MCP
       │      → mcpGateway.CallTool(...)                      │
       │      → metrics.MCPCallTotal{server,tool,status}.Inc  │
       │      return                                          │
       │ b. if toolRegistry.Get(tc.Name) exists:              │  ← 第二级：内置 / Dynamic
       │      → toolRegistry.Execute(...)                     │
       │      (Registry 自带 ToolExecutionDuration 打点)       │
       │      return                                          │
       │ c. if skillRegistry.FindSkill(tc.Name):              │  ← 第三级：Skill
       │      → skillRegistry.Execute(...)                    │
       │      return                                          │
       │ d. return ErrToolNotFound                            │
       └─────────────────────────────────────────────────────┘
  ⑤ Tool feedback collector（学习）：
       o.toolCollector.Record(name, args, success, duration, errMsg, sessionID)
  ⑥ 缓存写回 + invalidate：
       if success: toolCache.Put(...)
       if write tool: toolCache.Invalidate(scope)
  ⑦ return (result, err)
```

→ **dispatch 顺序是有意的**：MCP 优先于内置，确保运行时新挂的 MCP server 能 shadow 内置同名工具（虽然推荐 §1.5 Q1 用前缀避免冲突，但 shadow 语义保留了灵活性）。

---

## 3. 两个核心接口

### 3.1 `Tool` —— 单个能力

```go
type Tool interface {
    Definition() models.ToolDefinition
    Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error)
}
```

| 方法            | 何时调用              | 作用                                                                                      |
|-----------------|----------------------|-------------------------------------------------------------------------------------------|
| `Definition()`  | 组装 LLM prompt 时    | 返回 JSON Schema + Source + RiskLevel —— LLM 函数调用菜单                                  |
| `Execute()`     | LLM 决定调用时        | 按 raw JSON args 执行，返回 `*ToolResult{Content, IsError}`                                |

**实现者必须 concurrent-safe**：orchestrator 可能并发执行多个 tool_call（`parallel_tools.go:32` `parallelExecuteTools`）。

### 3.2 `Provider` —— 工具批发商（**未使用**）

```go
type Provider interface {
    Name() string
    Tools() []Tool
}
```

设计意图：MCP Gateway / Skill Registry 把自己的工具一次性塞进 Registry。

**实际状态**：

- `internal/mcp/client.go` 没实现 `tools.Provider`；
- `internal/skill/` 也没实现；
- `tools.Registry.RegisterProvider` 全工程无调用方（`rg -n "RegisterProvider" /Users/qiankun/code/agent` 0 hits in production code）。

保留这个接口是为了将来——如果有第 N 类 source（如远程 RPC 工具池），它能直接挂进来。

---

## 4. `Registry` —— 容器核心

```go
type Registry struct {
    mu    sync.RWMutex
    tools map[string]Tool
}
```

**唯一容器是 `map[string]Tool`**，没有缓存、没有反向索引、没有 source 分组。锁策略：

| 操作                                  | 锁类型           |
|---------------------------------------|------------------|
| `Register` / `Unregister`             | `mu.Lock()` 写锁 |
| `Get` / `Definitions` / `Execute` 查表 | `mu.RLock()` 读锁 |
| `Execute` 的真正调用                   | **锁外执行**     |

**锁外执行的原因**：工具调用可能花几秒到几十秒（`run_tests` 跑测试套件、`execute_code` 等沙箱启动）；持锁会冻结整个 registry 让其他 ReAct step 全卡。

`Execute` 模式（`registry.go:137-160`）：

```go
r.mu.RLock()
t, ok := r.tools[name]
r.mu.RUnlock()           // ← 拿到指针就立刻释放
if !ok { return nil, ErrToolNotFound }

start := time.Now()
result, err := t.Execute(ctx, args)   // 锁外
elapsed := time.Since(start).Seconds()

status := "success"
if err != nil { status = "error" }
else if result != nil && result.IsError { status = "tool_error" }
metrics.ToolExecutionDuration.WithLabelValues(name, source, status).Observe(elapsed)
metrics.ToolExecutionTotal.WithLabelValues(name, source, status).Inc()
```

三种 status 的语义差异：

| status        | 触发条件                              | 监控含义                                              |
|---------------|---------------------------------------|------------------------------------------------------|
| `success`     | `err==nil && !result.IsError`         | 正常完成                                              |
| `error`       | `err != nil`                          | **系统级失败**（超时、panic、网络）—— 报警阈值       |
| `tool_error`  | `err==nil && result.IsError==true`    | **工具语义失败**（脚本退出非 0、参数无效）—— 调 LLM 的活 |

分开记录是 Grafana 面板的关键：`error` 涨说明基础设施挂了；`tool_error` 涨说明 LLM 在重复犯错（如反复 `read_file` 不存在的路径），后者要通过提示词改进解决。

---

## 5. `builtinTool` 适配器 ——orchestrator 是怎么用 Registry 的

`tools.Registry` 设计成"任何 `Tool` 实现都能注册"，但 orchestrator 内置工具是写在 `orchestrator.go` / `file_tools.go` / `git_tools.go` 里的方法，签名 `func(o *Orchestrator, ctx, args) (*ToolResult, error)`。

`builtin_tools.go` 用一个**薄 adapter struct** 把方法升级成 `Tool` 接口：

```go
type builtinTool struct {
    def     models.ToolDefinition
    handler func(context.Context, json.RawMessage) (*models.ToolResult, error)
}

func (t *builtinTool) Definition() models.ToolDefinition { return t.def }
func (t *builtinTool) Execute(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
    return t.handler(ctx, args)
}
```

`RegisterBuiltinTools` / `RegisterFileTools` 用 **method-value closure** 拿到带 `o` 接收者的函数：

```go
&builtinTool{
    def: models.ToolDefinition{
        Name:       "read_file",
        Source:     "builtin",
        Parameters: ...,
        RiskLevel:  0,
    },
    handler: o.toolReadFile,    // ← method value，已绑定 o
}
```

→ 这种 adapter 模式让"工具实现保持 orchestrator 内部方法"和"Registry 要求 `Tool` 接口"两边都不用妥协。代价是每个工具多 5 行注册代码——对一组 10+ 工具，重复但单一职责清晰。

git 工具有点特殊：所有 git_* 工具共享同一个 dispatch 函数 `executeGitTool`，所以 closure 形态略不同（`builtin_tools.go:101-113`）：

```go
for _, def := range gitDefs {
    gitDef := def                     // ← 必须 capture（Go for-loop 闭包陷阱）
    tool := &builtinTool{
        def: gitDef,
        handler: func(ctx, args) (*ToolResult, error) {
            tc := models.ToolCall{Name: gitDef.Name, Args: args}
            return o.executeGitTool(ctx, tc)
        },
    }
    reg.Register(tool)
}
```

`gitDef := def` 是 Go 经典坑——直接用 `def` 在循环结束后所有闭包都会 capture 到同一个变量（指向最后一个元素）。

---

## 6. `dynamic_tool.go` —— 运行时 webhook 工具

REST API `POST /api/v1/tools` 让用户在不重启 agent 的前提下挂入新工具。

### 6.1 配置（`dynamic_tool.go:24-48`）

```go
type DynamicToolConfig struct {
    Name           string
    Description    string
    Parameters     json.RawMessage  // JSON Schema
    ExecutorType   ExecutorType     // "webhook" | "inline"
    ExecutorConfig json.RawMessage  // 对应类型的 config
    RiskLevel      int              // 0=safe, 1=moderate, 2=high
    TTLSeconds     *int64           // 可选过期（未实现回收）
    CreatedAt      time.Time
}

type WebhookExecutorConfig struct {
    URL            string
    Method         string             // 默认 POST
    Headers        map[string]string
    TimeoutSeconds int                // 默认 30
}

type InlineExecutorConfig struct {
    Language       string             // "bash" / "python" / "javascript"
    Code           string
    TimeoutSeconds int
}
```

### 6.2 Webhook 执行器（`dynamic_tool.go:105-144`）

`NewDynamicTool(cfg)` 根据 `ExecutorType` 派生执行闭包：

```go
case ExecutorTypeWebhook:
    return func(ctx context.Context, args json.RawMessage) (*ToolResult, error) {
        ctx, cancel := context.WithTimeout(ctx, timeout)
        defer cancel()
        req, _ := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, bytes.NewReader(args))
        req.Header.Set("Content-Type", "application/json")
        for k, v := range cfg.Headers { req.Header.Set(k, v) }
        resp, err := http.DefaultClient.Do(req)
        if err != nil { return &ToolResult{Content: err.Error(), IsError: true}, nil }
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        if resp.StatusCode >= 400 {
            return &ToolResult{Content: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body), IsError: true}, nil
        }
        return &ToolResult{Content: string(body), IsError: false}, nil
    }
```

**安全 caveats**：

- 用 `http.DefaultClient`（**不是** orchestrator 已 wired 的 egress validator）——动态 webhook 可指向任意 URL，包括内网；建议生产改成项目里的 egress-controlled `*http.Client`；
- 没有重试 / 熔断；HTTP 4xx/5xx 会被 LLM 当成 `IsError=true` 看到，由 LLM 决定是否重试；
- args（来自 LLM）直接当 body —— webhook 接收方必须能容错任意 JSON。

### 6.3 Inline 执行器（**未实现**）

`createInlineExecutor` (`dynamic_tool.go:146-153`) 当前只返回占位错误：

```go
return &models.ToolResult{
    Content: fmt.Sprintf("Inline executor not yet implemented (language=%s)", cfg.Language),
    IsError: true,
}, nil
```

预期实现是走 `sandbox.Manager.Execute`（[05_sandbox.md](05_sandbox.md)）跑用户提供的 code——目前没接上。这是 §12 演进项。

### 6.4 与 Registry 的对接

`orchestrator.go:1627-1648` 暴露三个方法给 REST handler：

```go
func (o *Orchestrator) RegisterDynamicTool(tool tools.Tool) error
func (o *Orchestrator) UnregisterDynamicTool(name string) bool
func (o *Orchestrator) GetTool(name string) (tools.Tool, bool)
```

REST handler（`internal/api/handlers/dynamic_tool_handlers.go`）的典型流程：

```
POST /api/v1/tools  {name, parameters, executor_type, executor_config, risk_level}
  → tools.NewDynamicTool(cfg)
  → orch.RegisterDynamicTool(tool)
  → 200 OK，工具立刻进入下一轮 ReAct 的 GetAvailableTools()
```

---

## 7. 命名规范

接口不强制，但 orchestrator 注册的约定是：

| 来源       | 前缀 / 形态              | 示例                                          |
|------------|--------------------------|----------------------------------------------|
| 内置基础   | snake_case 无前缀         | `execute_code` / `search_code`               |
| 内置文件   | 直接动词                  | `read_file` / `write_file` / `edit_file` / `apply_diff` |
| 内置 git   | `git_` 前缀               | `git_status` / `git_diff` / `git_log`        |
| 内置 LSP   | 自然短语                  | `goto_definition` / `find_references` / `hover_info` / `rename_symbol` |
| 内置 PTY   | `shell_exec`              | （PTY 持久 shell session）                    |
| MCP        | server 内部定义           | `read_file` / `create_issue`（**无前缀，可能冲突，见 §1.5 Q5**） |
| Skill      | 自定义 YAML 字段          | 用户随意命名                                  |
| Dynamic    | 用户命名                  | 用户随意命名                                  |

→ MCP 工具与内置工具同名（如 filesystem-mcp 的 `read_file` 撞上内置 `read_file`）时，**dispatchTool 中 MCP 路由优先**（`orchestrator.go:1428-1435`）。这是有意的设计（让 MCP 能 shadow 内置），但若不小心会导致内置工具被静默覆盖。生产配置应手动让 MCP server 名 + tool 名不冲突。

---

## 8. RiskLevel 与工具级 HITL

`ToolDefinition.RiskLevel` 字段是 P5 设计——`executeTool` 入口检查（`orchestrator.go:1369` 起）：

| RiskLevel | 含义                          | 示例工具                                     |
|-----------|------------------------------|---------------------------------------------|
| 0         | safe（只读 / 沙箱）           | `read_file` / `search_code` / `execute_code`（沙箱里） |
| 1         | moderate（写工作区文件 / 修改既有文件 / 白名单宿主 exec） | `write_file` / `patch_file` / `edit_file` / `apply_diff` / `run_workspace_cmd` |
| 2         | high（外部副作用 / 远端写）   | `git_commit` / `git_push` / `create_directory`（OS 级）|

`RiskLevel >= 2` 时 `executeTool` 调用 `waitToolApproval`（`tool_approval.go`）：

1. 在 SSE 流里发 `approval_request` 事件（携带 `tool_name` / `tool_call_id` / args 摘要 / `risk_level` 标签）
2. 创建 `toolApprovalCh[task.ID]` 通道，阻塞 `executeTool` 最多 5 分钟
3. 前端 ChatPage 渲染 Approval 模态框，用户点同意/拒绝 → `POST /api/v1/tasks/:id/approve`
4. `HandleApproval` 先看 `toolApprovalCh` 再回退到任务级 `approvalCh`，命中后把 `ApprovalResponse` 投递到通道
5. `executeTool` 解除阻塞：批准 → fallthrough 正常执行；拒绝 → 返回 `❌ Tool '…' rejected by user`；超时 → 返回 `approval timed out`

> ⚠ **fail-safe 兜底**：multiagent / planner 等其他 `executeTool` caller 没有把 `task` + `sink` 注入 ctx，此时仍走旧的"直接返回错误"路径，避免在没有审批通道时悄悄放过高风险工具。

`ctxKeySkipHITL` 上下文键可以绕过该 gate（自动化测试 / Temporal activity 回调）；生产路径不要主动塞这个 key。

`write_file` 历史上是 RiskLevel=2，2026-06-03 起降为 1：写动作发生在 `/tmp/agent-workspaces/<id>` 隔离目录中，与 `patch_file` 同档。

`run_workspace_cmd` 历史上是 RiskLevel=2，2026-06-05 起降为 1：`validateWorkspaceCommand` 命令白名单 + `minimalCommandEnv` 环境变量擦除 已对宿主 exec 提供静态护栏，常规工作区命令（`go test` / `pytest` / `ls` 等）不再走 HITL，留出审批容量给真正越界的远端写。如需收回到 2，调整 `file_tools.go::ToolRunWorkspaceCmd` 的 `RiskLevel` 字段即可（HITL 阈值 `>=2` 见 `orchestrator.go:1460`）。

### 8.1 `validateWorkspaceCommand` 双返契约（2026-06-08 B2）

`file_tools.go:127` 起的 `validateWorkspaceCommand(cmd) → (rejection, warning string)` 同时承担硬护栏与软提示两个职责，调用方按返回值组合处理：

| `rejection` | `warning` | 调用方行为 |
|---|---|---|
| 非空 | — | 命令直接驳回（`ToolResult.Content` 为 `Command rejected: …`），不执行 |
| 空 | 非空 | 命令放行，但 `toolRunWorkspaceCmd`（`file_tools.go:776-777`）把 warning 拼到 `ToolResult.Content` 末尾 `⚠️ workspace cmd warning: …` |
| 空 | 空 | 默认快路径，无注释 |

当前 warning 触发模式：

- `| head` / `| tail` / `|head` / `|tail` —— 长跑生产者会在 N 行后被 `SIGPIPE` 截断，造成"命令成功但输出残缺"的歧义；推荐 LLM 改写为 `> file && head -N file` 拿全量再分页。

非 stdio 路径的 `pty_tools.go` 调用点显式 `_` 忽略 warning（PTY 内交互命令的语义不适合软提示）。详细心跳与执行链路见 `09_orchestrator.md` §Q4.6。

---

## 9. 投机缓存（Speculative Cache）

`executeTool` 入口的第二道关（`orchestrator.go:1377-1383`）：

```go
scope := o.cacheScope()                     // 通常是 workspace ID
if cached, hit := o.toolCache.Get(scope, tc.Name, tc.Args); hit {
    return cached, nil                       // ← 直接返回，不再 dispatch
}
```

后置写回 + 失效（`orchestrator.go:1403-1410`）：

```go
if err == nil && !result.IsError { toolCache.Put(scope, tc.Name, tc.Args, result) }
if toolCache.shouldInvalidate(tc.Name) { toolCache.Invalidate(scope) }
```

- **缓存键**：`(scope=workspaceID, name, args 的哈希)`；
- **可缓存工具**（idempotent）：`read_file` / `search_code` / `list_files` 等；
- **失效工具**（writes）：`write_file` / `edit_file` / `patch_file` / `apply_diff` / `run_workspace_cmd` —— 一旦执行就清掉整个 scope。

→ 命中场景：ReAct 循环里 LLM 第二次"我再看看 main.go"，直接走缓存（5ms），不重新读文件。

详细实现在 `tool_cache.go`，不在 `tools.Registry` 内——`Registry` 自身没有缓存。

---

## 10. 设计权衡

| 抉择 | 动机 |
|------|------|
| Registry 不统一所有 source | MCP / Skill 的生命周期差异太大，三级 dispatch 在 orchestrator 层做 |
| `tools.Provider` 接口保留但无人实现 | 留给将来可能的第 N 类 source；删了也没人骂 |
| `Definitions()` 每次 sort | 50 工具 < 50μs，不值得 sortedDef 缓存的复杂度 |
| Register 冲突报错而非覆盖 | 命名冲突立刻暴露在启动日志，比静默覆盖好 |
| Execute 内嵌打点而非 middleware | 全工程只有 orch 一个调用方，YAGNI |
| `builtinTool` adapter struct | orchestrator 方法签名与 Tool 接口解耦，零侵入注册 |
| dispatch 三级顺序 MCP > Registry > Skill | MCP 可 shadow 内置（运行时灵活性）；Skill 兜底（最不常用） |
| Dynamic webhook 用 http.DefaultClient | MVP 简单，但走不到 egress validator——已识别 P0 |
| Inline 执行器留占位未实现 | 等 sandbox 接口稳定后再接；UI 已有相关入口但灰色 |
| 工具级 HITL 走进程内 channel + SSE，不走 Temporal | Temporal workflow 是任务级单位；工具级粒度过细且需阻塞当前 `executeTool`，进程内 channel 更直接。durable HITL 仍由任务级 `suspendForApproval` 提供 |
| 没有 per-tool 限流 / ACL | 留给上层（auth / rate-limit middleware） |

---

## 11. 后续演进

### P0（生产前必须）

- [ ] **全局 sorted 工具列表**：`orchestrator.GetAvailableTools` 把三个 source 合并后**统一**按 Name 排序，提升 LLM prompt cache 命中
- [ ] **Dynamic webhook 走 egress validator**：当前 `http.DefaultClient` 可访问任意 URL（含内网）；改用项目里的 `egressHTTPClient`（已在 `main.go` 注入到 MCP，但 Dynamic 没用）
- [x] ~~**工具级 HITL 接通审批闭环**~~：2026-06-03 完成 —— `executeTool` 检测 `RiskLevel >= 2` 时 `waitToolApproval` 发 SSE `approval_request` 事件、阻塞 5 分钟等 `POST /tasks/:id/approve` 注入 `toolApprovalCh`，前端 ChatPage Approval 模态框已对接
- [ ] **`Unregister` 批量化**：MCP server 下线时要清掉它注册的所有 tool（如果将来真把 MCP 接进 Registry），加 `UnregisterByPrefix` / `UnregisterBySource`

### P1（运维质量）

- [ ] **工具级 HITL 持久化**：现在 `toolApprovalCh` 是进程内 map，进程重启则 pending tool approval 必然 timeout。要 durable 需把 channel 改成 Temporal signal（仿 `suspendForApprovalTemporal` 的工作流）或 Redis stream
- [ ] **Inline 执行器实现**：接 `sandbox.Manager.Execute`（[05_sandbox.md](05_sandbox.md)），让用户上传一段 bash / python 注册成工具
- [ ] **TTL 回收**：`DynamicToolConfig.TTLSeconds` 字段已定义但无回收 goroutine，过期工具至今还在 Registry
- [ ] **per-user ACL**：按 `claims.Role` 过滤可见工具——配合 [18_auth.md](18_auth.md)
- [ ] **per-tool 限流**：防止 LLM 疯狂调 `search_code` 100 次撑爆 RAG
- [ ] **失败工具自动降级**：连续 N 次 error → 临时从 `Definitions()` 剔除，避免反复重试
- [ ] **metrics 按用户拆分**：`tool_execution_total{user_id, ...}` 看谁在烧 expensive tool

### P2（演化方向）

- [ ] **流式 Tool 接口**：`Execute` 返回 `chan string`，让 sandbox stream / MCP CallToolStream 走统一抽象（目前各走各的）
- [ ] **真把 MCP / Skill 接进 Registry**：实现 `mcp.Gateway.Tools()` / `skill.Registry.Tools()` 满足 `Provider`；接进来后才能用统一 ACL / 限流；要处理 reconnect / reload 的 diff
- [ ] **工具健康检查 + 自动下线**：内置工具也可能挂（docker daemon down → execute_code 总失败）；Registry 维护"可用性"位
- [ ] **工具版本协商**：MCP `tools/list` 返回时带 version，Registry 记录 → LLM prompt 里曝光 → 调用时校验

---

## 12. 设计教训

1. **接口的"统一"愿景不等于实现的"统一"**：包注释把 `tools.Registry` 描述为"所有 source 的统一入口"，实际却是三级 dispatch + 三源聚合。这不是 bug——MCP / Skill 各管生命周期更稳——但**文档要诚实反映**，否则新来的人会以为 Provider 接口在用而上面去找。这次重写的核心就是把"愿景"与"现状"分开讲。

2. **`sortedDef` 缓存的诱惑**：每次 `Definitions()` 都 sort 看起来浪费，但实测 < 50μs，且加缓存的代价是双重锁 + dirty 位 + 升级路径。**不为臆想的性能问题而引入复杂度**是反复要提醒自己的。旧文档里描述的 sortedDef + dirty 实现是 LLM 自己脑补的——和实际代码不符。

3. **method-value closure 是把方法适配成接口的最简范式**：`o.toolReadFile` 直接当 `func(ctx, args)` 用，绑定接收者隐式完成；无需写 `func(ctx, args) { return o.toolReadFile(ctx, args) }` 这种包装。但 for-loop 里 capture 循环变量是 Go 经典坑（`builtin_tools.go:103` 的 `gitDef := def`），加这一行才安全。

4. **打点放在哪一层决定它会被忘掉的概率**：把 metrics 放在 `Registry.Execute` 内嵌，任何路径调 Execute 都自动有指标。如果放在调用方（orchestrator），未来加了第二个调用方的人会忘记打点，监控盲点就此诞生。**重要的横切关注点要放在被横切的中心**。

5. **dispatch 顺序 = 默认 shadow 语义**：MCP > Registry > Skill 的三级路由意味着 MCP 默认 shadow 同名内置工具。这给了运维"用 MCP 替换内置实现"的能力，但也是冲突的源头。命名冲突应该在 §1.5 Q5 / §12 P0 提到的命名空间前缀方案上根治——但目前先靠"配置层人工保证唯一"。

---

下一篇：[`08_skill.md`](08_skill.md) —— Skill 注册中心：YAML 模板 + 内置工具组合的"软工具"，与 `tools.Registry` 同居 orchestrator dispatch 第三级。
