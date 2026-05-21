# 08 · Skill 注册中心 `internal/skill` + REST 入口

> 代码：
> - `internal/skill/registry.go` (268) — Skill 定义、注册表、Webhook 执行器、内置函数执行器
> - `internal/api/mcp_skill_handlers.go` (203) — MCP/Skill/Tools 的 REST CRUD
>
> 测试：`registry_test.go` (153) — 覆盖注册冲突、Webhook 模拟、function 调用、并发

---

## 1. 模块定位

**"让用户能在不写 MCP Server 的前提下，给 Agent 加一把新的手。"**

MCP（见 `06_mcp.md`）是强大但重量级的：要写一个独立的进程、遵循 JSON-RPC 协议。很多实际需求其实简单到**只是一次 HTTP POST**：

- 把消息发到 Slack 频道；
- 触发 Jenkins 的某个 Job；
- 调一个内部 CRM 查单；
- 跑一段 Go 里写好的工具函数。

对这些场景，本包提供**轻量级 Skill**：

- **Webhook 类型**：定义一个 HTTP 端点 + JSON Schema → Registry 会帮你把调用转换成一次 HTTP POST；
- **Function 类型**：在代码里注册一个 Go 函数 → 直接内联调用，无网络。

两种 skill 对 LLM 来说**完全透明**，都以 `tools/function_call` 形式出现。

---

## 1.5 核心设计问题

### Skill vs MCP vs 内置 Tool 有啥区别？

三者都是"给 LLM 提供能力"，但定位不同：

| 层 | 调用 | 扩展方式 | 典型 |
|---|---|---|---|
| 内置 Tool | Go 函数（orchestrator 里 switch） | 改代码+重部署 | `read_file`, `grep` |
| MCP | JSON-RPC 出站 | 运行时注册 MCP server | github/jira/jenkins |
| Skill | 模板化 prompt 的 meta-tool | 运行时 CRUD skill 定义 | `security_review`, `code_migration` |

Skill 的**独特能力**：把一个"提示词+参数约束+工作流"封装成 LLM 可见的
工具。用户不写代码就能加新能力。

### 为什么 Skill 要有 risk level？

Skill 可能串联敏感操作（`security_review` 会自动扫仓库读敏感文件）。
每个 Skill 声明 `risk_level`（0/1/2/3），调度时：
- 0: 任意 role 可调
- 1: dev 以上
- 2: admin 或走 HITL
- 3: 必须 HITL + 审计

### 为什么 Skill 用 JSONSchema 描述参数？

LLM 的 function-calling 协议要求 `parameters` 字段是 JSONSchema。用户新加
Skill 时直接写 JSONSchema → 原生兼容 → LLM 自动校验。

缺点：用户要学 JSONSchema。缓解：提供模板 + 预定义参数类型。

---

## 2. 依赖架构

```
              ┌──── REST (mcp_skill_handlers.go) ────┐
              │ POST   /api/v1/skills                 │
              │ DELETE /api/v1/skills/:name           │
              │ GET    /api/v1/skills                 │
              │ GET    /api/v1/tools   (聚合视图)     │
              └────────┬──────────────────────────────┘
                       │
                       ▼
              ┌───────────────────────┐
              │ skill.Registry        │
              │ ├─ defs  map[name]*Def│
              │ ├─ fn    map[name]Fn  │  (函数式)
              │ └─ http  *http.Client │
              └─────┬──────────┬──────┘
                    │          │
                    │          ▼ (Webhook 调用)
                    │   ┌─────────────────────┐
                    │   │ 外部 HTTP 服务       │
                    │   │ (Slack/Jenkins/...) │
                    │   └─────────────────────┘
                    │
                    ▼ (作为 tools.Provider)
              ┌───────────────────────┐
              │ tools.Registry        │  ← 统一给 orchestrator
              └───────────────────────┘
```

---

## 2.5 数据流总览

本模块有两条主数据流：**注册流**和**执行流**。

```text
═══════════════ 注册流 (REST API → Registry) ═══════════════

┌─────────────────────────────────────────────────────────────┐
│ POST /api/v1/skills  (JSON body)                            │
│ {name, description, input_schema, webhook_url, ...}         │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ handler: ValidateSchema + 构造 skill.Definition             │
│   Type: webhook / function                                  │
│   InputSchema: JSON Schema (给 LLM function calling)        │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ skill.Registry.Register(def)                                 │
│   skills[name] = def                                        │
│   status = Active                                           │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼ (作为 tools.Provider)
┌─────────────────────────────────────────────────────────────┐
│ tools.Registry.RegisterProvider(skillRegistry)               │
│ → skill 的 Definition 变为 LLM 可调用的 tool                 │
└─────────────────────────────────────────────────────────────┘


═══════════════ 执行流 (LLM tool_call → 外部服务) ═══════════════

┌─────────────────────────────────────────────────────────────┐
│ LLM 返回 tool_call: {name: "skill_xxx", arguments: {...}}   │
└──────────────────────────┬──────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ orchestrator.executeTool → skill.Registry.Execute(name,args)│
└──────────────────────────┬──────────────────────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
┌──────────────────────┐    ┌─────────────────────────────┐
│ Type == "webhook"    │    │ Type == "function"          │
└──────────┬───────────┘    └─────────────┬───────────────┘
           │                              │
           ▼                              ▼
┌──────────────────────┐    ┌─────────────────────────────┐
│ HTTP POST            │    │ handler(ctx, args)          │
│ → webhook_url        │    │ (进程内 Go 函数)            │
│ Body: args JSON      │    │ 直接返回结果                 │
│ Timeout: skill配置   │    └─────────────┬───────────────┘
└──────────┬───────────┘                  │
           │                              │
           ▼                              │
┌──────────────────────┐                  │
│ resp.StatusCode      │                  │
│ 2xx → IsError=false  │                  │
│ 4xx/5xx → IsError=   │                  │
│   true               │                  │
│ Body → LimitReader   │                  │
│   (防内存爆炸)        │                  │
└──────────┬───────────┘                  │
           │                              │
           └──────────────┬───────────────┘
                          ▼
┌─────────────────────────────────────────────────────────────┐
│ *ToolResult{Content: response_body, IsError}                 │
│ → 返回 orchestrator → 注入下一轮 LLM 消息                    │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. 核心数据结构

### 3.1 `Definition` —— Skill 描述

```go
type Definition struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    Parameters  json.RawMessage `json:"parameters"`   // JSON Schema
    Executor    ExecutorConfig  `json:"executor"`
    CreatedAt   time.Time       `json:"created_at"`
}

type ExecutorConfig struct {
    Type    string            // "webhook" | "function"
    URL     string            // webhook 的端点
    Method  string            // "POST" (默认) / "PUT" / ...
    Headers map[string]string // 认证头等
    Timeout time.Duration     // 调用超时
}
```

对比 MCP：

| MCP Tool                   | Skill                          |
|----------------------------|--------------------------------|
| server 进程提供            | 自包含 JSON 定义                |
| schema 是 server 声明的    | schema 是用户声明的             |
| 调用走 JSON-RPC stdin      | 调用走 HTTP / Go 函数           |

### 3.2 `Registry`

```go
type Registry struct {
    mu     sync.RWMutex
    defs   map[string]*Definition               // 完整定义
    fns    map[string]FunctionHandler           // 函数式 skill
    http   *http.Client                         // 共享的 HTTP 客户端
    logger *zap.Logger
}

type FunctionHandler func(ctx context.Context,
                         args json.RawMessage) (*models.ToolResult, error)
```

一个 skill 可能**只在 `defs` 里**（webhook）、**也可能在 `fns` 里**（function）；`Execute` 时先看 type 再分发。

### 3.3 `SkillStatus` —— List 返回

```go
type SkillStatus struct {
    Name        string
    Description string
    Type        string         // "webhook" | "function"
    CreatedAt   time.Time
    Parameters  json.RawMessage
    Endpoint    string         // webhook 才有
}
```

前端 `SkillsPage.tsx` 渲染列表 + "删除"按钮。

---

## 4. 注册流程

### 4.1 Webhook 注册（通过 REST）

```
POST /api/v1/skills
{
  "name": "send_slack_message",
  "description": "Post a message to Slack channel #ops",
  "parameters": { "type":"object", "properties": { "text":{"type":"string"} } },
  "executor": {
    "type": "webhook",
    "url":  "https://hooks.slack.com/services/XXX/YYY",
    "method": "POST",
    "headers": { "Content-Type": "application/json" },
    "timeout": "10s"
  }
}
```

处理（`handleAddSkill`）：

```
1. BindJSON → Definition
2. 校验 Name / URL / Parameters 非空
3. 校验 Name 不与已有 skill / mcp tool / builtin 冲突
4. skillRegistry.Register(def)
5. tools.RegisterProvider(skillRegistry)  重新批量导入（幂等）
6. 返回 201 Created + SkillStatus
```

### 4.2 `Register` 内部

```go
Register(def *Definition) error {
    # 校验
    - def.Name ≠ ""
    - def.Parameters is valid JSON
    - def.Executor.Type in {"webhook","function"}
    - if webhook:  def.Executor.URL ≠ ""
    - if function: fns[name] 必须已经存在（否则没有 handler）

    # 加锁写入
    mu.Lock()
    if _, exists := defs[def.Name]: return ErrSkillExists
    def.CreatedAt = time.Now()
    defs[def.Name] = def
    mu.Unlock()
}
```

### 4.3 Function 注册（代码内调用）

```go
skillReg.RegisterFunction("current_time", func(ctx, args) (*ToolResult, error) {
    return &models.ToolResult{Content: time.Now().Format(time.RFC3339)}, nil
})

// 然后再声明对应的 Definition（让 LLM 能看到）：
skillReg.Register(&Definition{
    Name:        "current_time",
    Description: "Return the current server time in RFC3339.",
    Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
    Executor:    ExecutorConfig{Type: "function"},
})
```

**两步拆开**的原因：`RegisterFunction` 是代码级别的（在 main.go 里调）；`Register` 是元数据级别的（可能来自 DB / REST）。分开便于 function 的实现和声明分别维护。

---

## 5. `Execute` —— 分发执行

```go
Execute(ctx, name, args):
  mu.RLock
  def, ok := defs[name]
  mu.RUnlock
  if !ok: return nil, ErrSkillNotFound

  switch def.Executor.Type:
    case "webhook":
        return executeWebhook(ctx, def, args)
    case "function":
        fn := fns[name]
        if fn == nil: return nil, ErrFunctionNotBound
        return fn(ctx, args)
    default:
        return nil, ErrUnknownExecutor
```

### 5.1 `executeWebhook` 细节

```go
executeWebhook(ctx, def, args):
  # 1. 构造请求
  method := def.Executor.Method or "POST"
  body   := args                  # LLM 已经给了 valid JSON
  req    := http.NewRequestWithContext(ctx, method, def.Executor.URL, body)
  for k, v in def.Executor.Headers:
      req.Header.Set(k, v)
  req.Header.Set("Content-Type", "application/json")

  # 2. 超时控制
  timeout := def.Executor.Timeout or 30s
  ctx2, cancel := context.WithTimeout(ctx, timeout)

  # 3. 发送
  resp, err := r.http.Do(req)
  if err != nil:
      return &ToolResult{Content: err.Error(), IsError: true}, nil
  defer resp.Body.Close()

  # 4. 读 body (截到 1 MiB)
  body := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
  status := resp.StatusCode

  # 5. 决定 IsError
  return &ToolResult{
      Content: string(body),
      IsError: status >= 400,
      Metadata: { "http_status": status },
  }, nil
```

几个关键设计：

- **ctx 双层**：orchestrator 给的 ctx（整体超时） + skill 自己的 ctx（调用级超时），取最小；
- **错误不抛**：HTTP 4xx/5xx 作为 `IsError=true` 返回，**不返回 Go 层 error**。原因：LLM 能看到 content，自己决定是否重试/道歉；如果返回 error 会让 orchestrator 把这一步当成系统错误提前中止 tool-loop；
- **body 限 1 MiB**：防止外部服务大量文本把 context 塞爆；
- **shared `http.Client`**：连接池复用，减少 TLS 握手开销。

---

## 6. 与 `tools.Registry` 的桥接

`skill.Registry` 实现了 `tools.Provider` 接口：

```go
// Name — 让 Provider 标识自己
func (r *Registry) Name() string { return "skill" }

// Tools — 把每个 Definition 包装成 tools.Tool
func (r *Registry) Tools() []tools.Tool {
    out := []tools.Tool{}
    for _, def := range r.defs:
        out = append(out, &skillTool{reg: r, def: def})
    return out
}

// skillTool 是一个适配器
type skillTool struct { reg *Registry; def *Definition }

(t *skillTool) Definition() models.ToolDefinition {
    return models.ToolDefinition{
        Name:        t.def.Name,
        Description: t.def.Description,
        Parameters:  t.def.Parameters,
        Source:      "skill",             // 关键 label
    }
}

(t *skillTool) Execute(ctx, args) (*ToolResult, error) {
    return t.reg.Execute(ctx, t.def.Name, args)
}
```

> 代码里是通过 `GetToolDefinitions()` 获取裸定义而非 `Tools()`；但概念上是一样的 —— 把 Definition 转成 `tools.Tool` 接口。

---

## 7. REST 端点全景 (`mcp_skill_handlers.go`)

```
┌─────── MCP Server Management ─────────────────────────────┐
│ POST    /api/v1/mcp/servers         → handleAddMCPServer    │
│ DELETE  /api/v1/mcp/servers/:name   → handleRemoveMCPServer │
│ GET     /api/v1/mcp/servers         → handleListMCPServers  │
├─────── Skill Management ──────────────────────────────────┤
│ POST    /api/v1/skills              → handleAddSkill        │
│ DELETE  /api/v1/skills/:name        → handleRemoveSkill     │
│ GET     /api/v1/skills              → handleListSkills      │
├─────── Unified View ──────────────────────────────────────┤
│ GET     /api/v1/tools               → handleListTools       │
│   = builtin defs + mcp defs + skill defs (合并输出)          │
└───────────────────────────────────────────────────────────┘
```

### 7.1 `handleAddSkill` 骨架

```go
(s *Server) handleAddSkill(c *gin.Context) {
    var def skill.Definition
    if err := c.ShouldBindJSON(&def); err != nil {
        c.JSON(400, err); return
    }
    if err := s.skills.Register(&def); err != nil {
        c.JSON(409, err); return
    }
    // 让 tools.Registry 重新拉一遍 (幂等)
    s.tools.RegisterProvider(s.skills)
    c.JSON(201, s.skills.List()[…])
}
```

### 7.2 `handleListTools` —— 聚合视图

```go
// GET /api/v1/tools
// 前端 ToolsPage.tsx 用它显示"全部 LLM 可用能力"
return c.JSON(200, {
    "total": s.tools.Len(),
    "items": s.tools.Definitions(),   // 已排序
})
```

---

## 8. 使用示例（典型 Skill）

### 8.1 Webhook Skill —— 调 Jenkins

```bash
curl -X POST http://localhost:8080/api/v1/skills \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "trigger_jenkins_build",
    "description": "Trigger build of a Jenkins job",
    "parameters": {
      "type":"object",
      "required":["job"],
      "properties": {
        "job": {"type":"string","description":"Jenkins job name"}
      }
    },
    "executor": {
      "type":"webhook",
      "url":"https://jenkins.internal/job/{{job}}/build",
      "method":"POST",
      "headers": {"Authorization":"Basic dXNlcjp0b2tlbg=="},
      "timeout":"15s"
    }
  }'
```

> URL 模板里的 `{{job}}` 目前需要用户在 webhook server 一侧自己消化。"URL template"是 **TODO**（见 §11）。

### 8.2 Function Skill —— 读配置项

```go
// 在 main.go 启动时
skills.RegisterFunction("get_feature_flag", func(ctx, args) (*ToolResult, error) {
    var in struct{ Key string `json:"key"` }
    json.Unmarshal(args, &in)
    val := featureFlag.Get(in.Key)
    return &models.ToolResult{Content: val}, nil
})

skills.Register(&skill.Definition{
    Name: "get_feature_flag",
    Description: "Read internal feature flag value",
    Parameters: json.RawMessage(`{"type":"object","required":["key"],"properties":{"key":{"type":"string"}}}`),
    Executor: skill.ExecutorConfig{Type: "function"},
})
```

---

## 9. 对比：Builtin / MCP / Skill 全矩阵

| 维度 | Builtin | MCP | Skill (webhook) | Skill (function) |
|---|---|---|---|---|
| 实现位置 | Go 代码内 | 独立进程 | 外部 HTTP | Go 代码内 |
| 加入时机 | 编译时 | 启动 + 运行时 | 运行时 | 启动时 |
| 协议 | 函数调用 | JSON-RPC stdio | HTTP JSON | 函数调用 |
| 延迟 | <1ms | 2-5ms | 5-50ms (内网) | <1ms |
| 故障隔离 | ❌ panic 影响主进程 | ✅ 子进程隔离 | ✅ HTTP 天然隔离 | ❌ panic 影响主进程 |
| schema 来源 | 代码硬编码 | server 自报 | 用户输入 | 用户输入 |
| 开发门槛 | 高（改代码重编译） | 中（写 MCP server） | 低（一个 HTTP 端点） | 中（改代码重启） |
| 权限范围 | 全进程权限 | 子进程可独立限权 | 仅网络 | 全进程权限 |

选型建议：

- **Builtin**：极高频、需要直接读写进程内状态的（file tools, sandbox, RAG）；
- **MCP**：复杂、多工具、有 schema 演进的第三方集成（GitHub/GitLab/DB）；
- **Skill (webhook)**：简单一两个接口的集成 / 内部工具 / quick hack；
- **Skill (function)**：希望代码集成但不想经过重编译-发布流程的 —— 比较少见，多数选 Builtin 更好。

---

## 10. 设计权衡

| 抉择 | 动机 |
|---|---|
| **两种 executor type 而非单一** | function 只是 webhook 的特例吗？不是 —— function 可以直接操作进程内状态（如 session），webhook 做不到 |
| Webhook 错误转成 IsError 而非 Go error | 让 LLM 看到 body 自主决定重试；系统错误才抛 Go error 中断 tool loop |
| Body 限 1 MiB | 防御性：外部服务可能回 10 MB HTML 错误页 |
| `RegisterFunction` 与 `Register` 分离 | function 的 body 是代码产物，Definition 是元数据 —— 两者生命周期不同（函数重编译才变，定义运行时变） |
| 不默认持久化 | 运行时状态 —— 重启后用户重新注册（或由 `16_store` 里的 persistence 层加载） |
| 不支持 GraphQL / gRPC webhook | YAGNI；真需要时加 `ExecutorConfig.Type = "grpc"` 分支 |
| 用共享 `http.Client` | 连接池复用；TLS 握手是大头 |
| skill 名字前缀不强制 | 用户命名自由；但推荐 `skill_` 前缀以便肉眼区分（见 `07_tools` §10 规范） |

---

## 11. 后续演进

- [ ] **URL / Header 模板化**：让用户在 `url` 里写 `{{path}}`、在 `headers` 里写 `{{args.token}}`，由 Skill Registry 在 execute 时插值（Go text/template）；
- [ ] **持久化**：把 `defs` 落到 PostgreSQL 的 `skills` 表，重启后自动恢复；
- [ ] **Auth 策略细化**：当前任何用户注册的 skill 对所有用户可见；加 owner / visibility 字段 + 中间件过滤；
- [ ] **Webhook 重试**：对 5xx 或 timeout 自动指数退避（当前单次失败即返回）；
- [ ] **Circuit Breaker per Skill**：某个 webhook 连续失败就短路几分钟，避免拖累 orchestrator；
- [ ] **Webhook 签名**：请求带 HMAC 头（配合 `internal/security/hmac.go`），让外部 webhook 能验证来自本 Agent；
- [ ] **Skill Version**：支持同名不同版 skill，LLM 按策略选版；
- [ ] **Streaming Response**：webhook 返回 SSE 时逐块喂给 LLM（需要 `tools.Tool` 接口升级，见 `07_tools` §12）；
- [ ] **干测试 `dry_run`**：定义 skill 时先 Actual Run 一次（注入 mock args）验证可达，防止用户误配 URL。

---

## 12. 实现剖析与改进方向

### Skill 执行的分发

```text
LLM tool_call (skill_name, args)
  │
  ▼
SkillRegistry.Execute(skill_name, args)
  │
  ├─ Skill.Kind == "builtin":
  │    直接调 Go handler(args)
  │
  ├─ Skill.Kind == "http_webhook":
  │    HMAC-sign + POST Skill.URL
  │    等 response JSON → 翻成 ToolResult
  │
  └─ Skill.Kind == "mcp":   (另一路由到 mcp gateway)
       forward to mcp.Gateway.CallTool
```

### Pros
- ✅ 三种 Kind 覆盖 90% 扩展需求
- ✅ JSONSchema 校验参数（LLM 协议原生支持）
- ✅ risk_level 和 HITL 集成

### Cons
- ⚠️ webhook skill 无熔断；慢 endpoint 拖累 orchestrator
- ⚠️ 参数校验仅在入口，handler 内部不可信输入仍要自己防御
- ⚠️ Skill 版本管理缺失（改定义后在途 tool_call 怎么办？）

### 改进方向
- **P0** — webhook skill 加 gobreaker + 超时
- **P1** — Skill 有 `version` 字段，新版本兼容同 skill_name 的旧 call
- **P1** — Skill handler 级别的 per-user quota
- **P2** — Skill SDK（Python / TypeScript）方便用户写 webhook

---

下一篇：`09_orchestrator.md` —— 核心大脑：ReAct 循环、工具调度、项目规则、失败跟踪、自动测试。
