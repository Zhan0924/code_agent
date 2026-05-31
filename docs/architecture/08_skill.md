# 08 — Skill 注册中心（`internal/skill`）

> 动态、可热插拔的"软工具"目录：把 webhook 端点或 Go 函数包装成 LLM 看得到的 `function_call` 工具，与 `tools.Registry` 同居 orchestrator dispatch 链第三级。

代码：`internal/skill/registry.go`（330 行）+ `schema_snapshot.go`（105 行）+ `_principles.go`（549 行，设计 RFC，不参与编译）。装配点：`cmd/agent/main.go:548`。API 注册入口：`internal/api/mcp_skill_handlers.go`。下游消费方：`orchestrator.dispatchTool` 第三级 + `internal/api/p0_debug_handlers.go` 的 schema snapshot 调试接口。

---

## 1. 模块定位

Skill 包是**动态工具注册表**，回答两个真实问题：

1. **运维/接入方**：在不重启进程的前提下，能否给运行中的 Agent 加一个新工具？
2. **LLM 编排器**：当 ReAct loop 拿到 `tool_call{name="parse_aws_billing"}` 时，能否在三类工具来源（内置 / MCP / 用户技能）里都找到它？

回答方式：

- 提供 `Definition` 模型 + REST API（`POST /api/v1/skills`）让运维方在 **运行时** 把"name + 描述 + JSON Schema + 执行器（webhook URL 或 Go handler）"塞入注册表；
- 提供 `Snapshot()` 给 orchestrator 把当前所有 skill 拍扁成 `[]models.ToolDefinition` 喂给 LLM；
- 提供 `Execute(name, args)` 给 orchestrator 在 LLM 反向调用某个 skill 时路由到对应执行器（webhook POST 或 Go function）。

### 1.1 与"统一 Registry 神话"的差距

`_principles.go:18-33` 描述了一个理想："Registry 的职责是把三类来源统一成单一视图，对上游 LLM 透明。"

**真实代码不是这样**：

| 工具来源    | 实际容器                              | 进入 LLM 视野的路径                              |
| ----------- | ------------------------------------- | ------------------------------------------------ |
| 内置工具    | `tools.Registry`（`internal/tools/`） | `orchestrator.GetAvailableTools` 直接 append     |
| MCP 工具    | `mcp.Gateway.toolIndex`               | `orchestrator.GetAvailableTools` 直接 append     |
| 用户 Skill  | `skill.Registry.skills`               | `skill.Registry.GetToolDefinitions()` 后 append  |

三个容器**独立存活**，由 `orchestrator.GetAvailableTools`（`orchestrator.go:1532-1545`）在每次 LLM 调用时**临时**合并，并且最终的 `[]ToolDefinition` 没有再做全局排序（参见 07_tools.md §1.1）。

`skill.Registry` 只覆盖三分之一：用户通过 API 注册的工具。所以本文范围严格限制在"用户运行时注册的 webhook/function 工具"。

### 1.2 设计 RFC 中提及但未落地的字段

`_principles.go` 列出的 `Skill` 字段里，**只有 Name / Description / Parameters 三个真实存在于 `Definition`**。未实现的字段对照表（`registry.go:74-90`）：

| RFC 字段     | 实现状态 | 影响                                                     |
| ------------ | -------- | -------------------------------------------------------- |
| `Source`     | ❌ 缺失  | 无法按来源批量注销；MCP 工具走独立路径，本字段无需求     |
| `RiskLevel`  | ❌ 缺失  | HITL 决策完全在 orchestrator 端按工具名硬编码 + 内容正则 |
| `RPS`        | ❌ 缺失  | 限流靠 API 网关层 / `internal/security`，本包不参与      |
| `Validator`  | ❌ 缺失  | 参数 schema 不做服务端校验，完全信赖 LLM 生成的 args     |

写文档时**绝不能照搬 RFC**——`_principles.go` 是设计构想，`registry.go` 是真实合同。两者偏差是本文最大的"诚实义务"。

### 1.3 与 P0-1 Schema 稳定快照的关系

`schema_snapshot.go` 是 P0 优化产物，独立于 RFC，**真实存在且全面接线**：

- `Registry.schemaStore` 字段（`registry.go:111`）持有 `atomic.Pointer[ToolSchemaSnapshot]`；
- 每次 `Register/Unregister` 调用 `schemaStore.Bump()` 让快照失效（`registry.go:160, 177`）；
- `Snapshot()`（`registry.go:222-228`）走双路：缓存命中直接 Load，未命中走 RLock + 排序重建；
- ETag 是 `sha256(name\0parameters\0...)` 的前 12 字符（`schema_snapshot.go:84-98`）。

这是本包**唯一与 LLM 端 prompt cache 直接对话**的关键资产。

---

## 1.5 设计哲学

> 为什么 Skill 包要长成这个样子？回答四个最容易让人困惑的设计选择。

### Q1：为什么不直接复用 `tools.Registry`？

答：**生命周期完全不同**。

| 维度          | `tools.Registry`         | `skill.Registry`                |
| ------------- | ------------------------ | ------------------------------- |
| 注册时机      | 进程启动时静态注册       | 运行时 REST API 注入            |
| 执行体        | Go 闭包，本进程内执行    | Webhook（跨网络）或 Go 闭包     |
| 安全模型      | 由代码作者控制           | 由运维 API 调用方控制（更脆弱） |
| 失败语义      | panic / error            | HTTP timeout / 4xx / 5xx        |
| Schema 来源   | 编译期写死               | JSON Schema 由注册方 PUT 进来   |
| 是否走 prompt cache | 不在乎（小且稳定）       | 必须稳定（用户可乱序注册）       |

强行合并会让 `tools.Tool` 接口被迫处理 webhook 错误码、HTTP 超时、运行时 schema 校验等并不属于"内置工具"的关注点。两个 Registry 分开后，每个都对自己的客户群（代码作者 vs 运维 API）做最简实现。

### Q2：为什么 `Definition` 不存 `Source` 字段，却在 `ToolDefinition` 输出 `Source: "skill:" + name`？

`buildDeterministicSnapshot`（`schema_snapshot.go:87-91`）输出 `Source: "skill:" + def.Name`——这是 **per-tool 唯一标识**，不是分类标签。

意图：orchestrator 在 `dispatchTool` 里要回答"这个 tool_call 该路由到哪个执行器"。`Source` 字段在三个来源都不同（tools.Registry 写 `"builtin"`/`"file"`，MCP 写 `"mcp"`，skill 写 `"skill:<name>"`），下游可以用前缀做分流。

**但 orchestrator 实际不依赖 Source 做分流**（见 07_tools.md §2.5）——它按 `dispatchTool` 三级 fallback 顺序匹配。因此 `Source` 字段目前只在 `GET /api/v1/tools` 给前端 UI 区分图标用。

### Q3：为什么 Snapshot 排序 + ETag 重要？

LLM provider（Anthropic Claude、OpenAI）按 **prompt 字节流前缀** 做 KV cache：完全一致的前缀才能命中。Tool schema 通常列在 system message 之后、user message 之前，是**第二大可缓存块**（仅次于 system prompt）。

如果 `Snapshot` 每次返回顺序随机的 schema：

- Anthropic prompt cache 命中率从 ~92% 跌到 ~30%（`_principles.go:24` 给的生产实测数）；
- 多轮对话每条都重新计费"system + tools"段的 input tokens；
- TTFT（time-to-first-token）抬高 1~2 倍。

`schema_snapshot.go` 做两件事保证字节稳定：

1. **按 name 字母序排序**（`schema_snapshot.go:81`）——同样的工具集，无论注册顺序，输出永远相同；
2. **指针等价 ≡ 字节等价**——同 generation 下 `Snapshot()` 返回同一指针，调用方无需对比内容即可断言"和上次完全一样"。

ETag 服务于第二层场景：`GET /api/v1/tools` 把 ETag 写入响应头（`mcp_skill_handlers.go:225`），前端可做 `If-None-Match` 304；同时 ETag 也可作为 prompt cache key 的一部分。

### Q4：为什么 webhook executor 使用 `r.httpClient`（全局共享），却又在 `Execute` 里另套一层 context timeout？

`Registry.httpClient`（`registry.go:122-124`）的 `Timeout: 60s` 是**整个 request 的硬上限**——包括 dial + write + read。

`executeWebhook`（`registry.go:288-290`）在外面再套 `context.WithTimeout(ctx, def.Executor.Timeout * time.Second)`——这是**业务可调上限**，由注册时的 `Executor.Timeout` 决定（默认 30s）。

两层取最严：

- 注册方写 `Timeout: 120` → 实际仍受 `httpClient.Timeout=60s` 截断；
- 注册方写 `Timeout: 0`（缺省 30s）→ 实际 30s 触发 `context.DeadlineExceeded`。

设计权衡：**httpClient 是全局单例**（不能为每个 skill 创建独立 client，否则连接池碎片化），所以 60s 是 hard ceiling，单 skill 不能突破。如果运维真要 120s skill，得改全局配置——这是有意的"提防恶意/失误注册阻塞 goroutine"。

---

## 2. 依赖架构

### 2.1 包依赖关系

```
                   ┌──────────────────────┐
                   │  internal/api        │
                   │  - handleAddSkill    │
                   │  - handleListTools   │
                   │  - currentSnapshot   │
                   └──────────┬───────────┘
                              │ Register / Snapshot / List
                              ▼
                  ┌────────────────────────┐
                  │  skill.Registry        │
                  │  ─────────────         │
                  │  skills    map         │ ←─── _principles.go (设计 RFC)
                  │  functions map         │      （不参与编译）
                  │  mu        RWMutex     │
                  │  schemaStore           │
                  │  httpClient (60s)      │
                  └─────────┬──────────────┘
                            │
              Register/Unregister 时 Bump
                            ▼
                  ┌────────────────────────┐
                  │  schemaSnapshotStore   │
                  │  ─────────────         │
                  │  gen     atomic.Uint64 │
                  │  current atomic.Ptr    │
                  └─────────┬──────────────┘
                            │ Load(builder)
                            ▼
                  ┌────────────────────────┐
                  │  ToolSchemaSnapshot    │
                  │  - Tools (sorted)      │
                  │  - Generation          │
                  │  - ETag (sha256[:12])  │
                  └────────────────────────┘
                            │
            ┌───────────────┼─────────────────┐
            ▼               ▼                 ▼
   orchestrator.    api/p0_debug    /api/v1/tools
   GetAvailable    SchemaSnapshot   X-Tools-Etag
   Tools           debug endpoint   header
```

### 2.2 出向依赖（导入）

| 包                            | 用途                                            |
| ----------------------------- | ----------------------------------------------- |
| `encoding/json`               | Definition.Parameters / args 透传不解码         |
| `net/http`                    | webhook executor（`http.Client.Do`）           |
| `crypto/sha256` + `encoding/hex` | ETag 计算                                    |
| `sync` + `sync/atomic`        | RWMutex + atomic.Pointer lock-free snapshot read |
| `internal/models`             | `models.ToolDefinition` / `models.ToolResult`   |
| `go.uber.org/zap`             | 结构化日志                                      |

**没有依赖**：`internal/llm`（schema 直接是 `models.ToolDefinition`）、`internal/temporal`（HITL 完全在 orchestrator）、`internal/audit`（不直接埋点）、`internal/security`（不做 URL 白名单/header 注入校验）。

### 2.3 入向依赖（被谁导入）

- `cmd/agent/main.go:548` —— 启动时 `skill.NewRegistry(logger)`
- `internal/orchestrator/orchestrator.go` —— 通过匿名 interface 引用（解耦避免循环）：
  ```go
  skillRegistry interface {
      FindSkill(name string) (string, bool)
      Execute(ctx, name, args) (*models.ToolResult, error)
      GetToolDefinitions() []models.ToolDefinition
  }
  ```
- `internal/api/router.go` —— `Server.skillRegistry *skill.Registry` 字段 + `SetSkillRegistry`
- `internal/api/mcp_skill_handlers.go` —— REST CRUD
- `internal/api/p0_debug_handlers.go` —— Schema snapshot 调试接口

---

## 2.5 数据流总览

### 流 1：启动时初始化

```
main.go:548
  skillReg := skill.NewRegistry(logger)
    │
    ├─ skills    = make(map[string]*Definition)    // 空
    ├─ functions = make(map[string]FunctionHandler) // 空
    ├─ httpClient = &http.Client{Timeout: 60s}     // 全局共享
    ├─ logger.With("component", "skill")           // 加 tag
    └─ schemaStore.{gen=0, current=nil}            // atomic.Pointer zero

main.go (后续)
  apiServer.SetSkillRegistry(skillReg)
  orch.SetSkillRegistry(skillReg)                   // 通过 interface 注入
```

**初始状态**：注册表为空，第一次 `Snapshot()` 会构造一个 0 工具的 snapshot（ETag = sha256("") 前 12 字符固定为 `e3b0c44298fc`），缓存住。

### 流 2：运行时注册新 skill

```
POST /api/v1/skills
{
  "name": "parse_aws_billing",
  "description": "Parse AWS CUR billing CSV and return summary",
  "parameters": {"type":"object","properties":{"s3_uri":{"type":"string"}}},
  "executor": {
    "type": "webhook",
    "url": "http://billing-tools:8080/parse",
    "method": "POST",
    "timeout": 45
  }
}
        │
        ▼
api/mcp_skill_handlers.go:handleAddSkill
        │
        ├─ json.Unmarshal → skill.Definition
        ├─ skillRegistry == nil → 503
        ├─ skillRegistry.Register(&def)
        │     │
        │     ├─ 字段校验（name/description/url/method/timeout 缺省值填充）
        │     ├─ r.mu.Lock()
        │     ├─ duplicate check
        │     ├─ r.skills[name] = def
        │     ├─ r.schemaStore.Bump()        # gen++, current.Store(nil)
        │     └─ logger.Info("skill registered")
        │
        └─ 200 {name, status:"active"}
```

**注意：URL 不做白名单**——任何 URL 都被接受。如果运维把 URL 指向 `http://169.254.169.254/...`（AWS EC2 metadata 服务），webhook 调用会直接命中——SSRF 风险完全暴露。详见 §11 P0 项。

### 流 3：LLM 拿到 skill 列表（Snapshot 热路径）

```
orchestrator.GetAvailableTools (orchestrator.go:1541-1542)
  if o.skillRegistry != nil {
      tools = append(tools, o.skillRegistry.GetToolDefinitions()...)
  }
        │
        ▼
GetToolDefinitions → Snapshot().Tools
        │
        ▼
schemaStore.Load(builder)
  ┌─ cur := s.current.Load()                # atomic read，无锁
  ├─ if cur != nil → return cur             # 99% 热路径走这里（纳秒级）
  └─ else:
       gen := s.gen.Load()
       snap := builder(gen)                 # 持有 r.mu.RLock 重建
       s.current.CompareAndSwap(nil, snap)  # 失败也无所谓（同 gen 等价）
       return snap
```

`builder` 内部（`buildDeterministicSnapshot`）：

```
1. names := []                              # 从 r.skills map 取 key
2. sort.Strings(names)                      # ← 决定性：字母序排序
3. for _, n := range names:
     tools = append(tools, ToolDefinition{
         Name, Description, Parameters,
         Source: "skill:" + n,
     })
     h.Write([]byte(n))                     # ETag 输入
     h.Write([]byte{0})                     # 分隔符
     h.Write(def.Parameters)                # ETag 输入
     h.Write([]byte{0})
4. etag := hex(sha256.Sum)[:12]
```

**关键性能数据**：测试中 1000 并发 `Snapshot()` 调用全部返回**同一指针**（`schema_snapshot_test.go:75-104`），证明 lock-free 热路径生效。

### 流 4：LLM 反向调用 skill

```
LLM 返回 tool_call{name:"parse_aws_billing", arguments:{"s3_uri":"s3://..."}}
        │
        ▼
orchestrator.executeTool → dispatchTool (orchestrator.go:1426-1449)
  1. 尝试 mcpGateway.CallTool   → 不匹配（不是 MCP 工具）
  2. 尝试 toolRegistry.Execute  → 不匹配（不是内置工具）
  3. 尝试 skillRegistry.FindSkill + Execute
        │
        ▼
skill.Registry.Execute(ctx, "parse_aws_billing", args)
  ├─ r.mu.RLock()
  ├─ def, ok := r.skills[name]
  ├─ fn, hasFn := r.functions[name]
  ├─ r.mu.RUnlock()                          # ← 立刻释放，避免长 HTTP 阻塞 Registry
  │
  ├─ hasFn? → fn(ctx, args)                  # Function executor 优先级最高
  ├─ def.Executor.Type == "webhook" → executeWebhook
  │     ├─ json.Marshal({skill, args})       # body 永远是 {"skill":"name","args":<raw>}
  │     ├─ ctx, cancel := WithTimeout(ctx, def.Executor.Timeout*time.Second)
  │     ├─ http.NewRequestWithContext(method, url, body)
  │     ├─ Content-Type: application/json
  │     ├─ 注入 def.Executor.Headers         # ⚠ 不做 header 名/值过滤
  │     ├─ r.httpClient.Do(req)              # 全局 60s timeout 兜底
  │     ├─ io.LimitReader(resp.Body, 1<<20)  # 1MB 上限
  │     ├─ isError := resp.StatusCode >= 400
  │     └─ return ToolResult{Content, IsError}
  │
  └─ def.Executor.Type == "function" 但 !hasFn → ToolResult{IsError, "not registered"}
```

**对比 MCP 调用**：MCP gateway 走 JSON-RPC stdin/stdout，单进程内通信；skill webhook 是跨网络 HTTP，受 DNS/网络/防火墙影响。两者在 orchestrator 层透明合并，但 SRE 排障时差异巨大。

---

## 3. 数据模型 / 接口实现细节

### 3.1 `Definition` 结构（`registry.go:74-81`）

```go
type Definition struct {
    Name        string          `json:"name"`         // 必填，且必须唯一
    Description string          `json:"description"`  // 必填，给 LLM 看的自然语言
    Parameters  json.RawMessage `json:"parameters"`   // JSON Schema，原样透传
    Executor    ExecutorConfig  `json:"executor"`     // 执行器配置
    CreatedAt   time.Time       `json:"created_at"`   // 注册时填充
}
```

字段语义解读：

- `Parameters` 用 `json.RawMessage` 而非 `interface{}`：避免反序列化-再序列化的字节漂移，保证 ETag 稳定（Anthropic prompt cache 命中关键）；
- `CreatedAt` 由 `Register` 自动填充（`registry.go:150`），客户端传入会被覆盖；
- 没有 `UpdatedAt` / `Version` —— 当前 Registry 不支持"修改"操作，只支持 Unregister + Register。

### 3.2 `ExecutorConfig` 结构（`registry.go:84-90`）

```go
type ExecutorConfig struct {
    Type    string            `json:"type"`              // "webhook" | "function"
    URL     string            `json:"url,omitempty"`     // webhook only，必填
    Method  string            `json:"method,omitempty"`  // webhook only，默认 POST
    Headers map[string]string `json:"headers,omitempty"` // webhook only，原样转发
    Timeout int               `json:"timeout,omitempty"` // 秒，默认 30
}
```

校验逻辑（`Register` 内部，`registry.go:131-149`）：

| 字段          | 校验规则                          | 失败行为                            |
| ------------- | --------------------------------- | ----------------------------------- |
| `Name`        | 非空                              | `return fmt.Errorf(...)`            |
| `Description` | 非空                              | 同上                                |
| `Type`        | 空 → 填 "webhook"                 | 不做 enum 校验，未知值会在 Execute 时落入 default 分支返回错误 |
| `URL`         | Type=webhook 时必填               | 失败                                |
| `Method`      | 空 → 填 "POST"                    | 不做 HTTP verb 合法性校验           |
| `Timeout`     | 0 → 填 30                         | 负数不做校验，理论上传 -1 会立刻超时 |

**未做的校验**：

- URL 协议白名单（http/https/file? 都接受）；
- URL host 白名单（无 SSRF 防护）；
- Header 名/值过滤（注册方可注入 `Authorization: <victim_token>`）；
- Parameters 是否是合法 JSON Schema（Register 不解析）；
- Name 字符集（如果传入 `"' OR 1=1 --"` 也照收，后续 LLM 喂入可能产生注入风险）。

### 3.3 `Registry` 结构（`registry.go:103-112`）

```go
type Registry struct {
    skills     map[string]*Definition           // 主存储
    functions  map[string]FunctionHandler       // Go 函数旁路
    mu         sync.RWMutex                     // 保护 skills + functions
    httpClient *http.Client                     // 全局共享，60s timeout
    logger     *zap.Logger

    schemaStore schemaSnapshotStore             // P0-1 lock-free snapshot 缓存
}
```

**两个 map 的关系**：

- `skills[name]` 持有完整 Definition；
- `functions[name]` 持有 Go 闭包（`FunctionHandler` 类型）；
- 同一个 name 可以**同时**出现在两个 map：先 `Register({Type:"function"})` 创建 schema 入口，再 `RegisterFunction(name, fn)` 绑定实现；
- `Execute` 检查"function 优先"（`registry.go:253-254`）：只要 `functions[name]` 存在就走 Go 闭包，**无视** `def.Executor.Type`。

这个"function 优先"语义有个微妙后果：如果运维 PUT 了一个 `Type:"webhook"` 的 skill，然后启动代码恰好 `RegisterFunction(name, ...)`，webhook URL 就被静默旁路。当前代码不会发生（`RegisterFunction` 没有任何调用方），但未来引入"内置 skill"时需注意。

### 3.4 `schemaSnapshotStore`（`schema_snapshot.go:46-72`）

```go
type schemaSnapshotStore struct {
    gen     atomic.Uint64
    current atomic.Pointer[ToolSchemaSnapshot]
}
```

并发协议：

1. **写路径**（`Bump`）—— 由 `Register/Unregister` 在持有 `mu.Lock` 时调用：
   - `gen.Add(1)` 让 generation 单调递增；
   - `current.Store(nil)` 失效缓存；
   - **注意**：bump 不重建快照，下一次读时 lazy 重建。

2. **读路径**（`Load`）—— 持有 `mu.RLock` 或无锁均可（`current.Load()` 本身是原子操作）：
   - 快路径：`current.Load() != nil` 直接返回；
   - 慢路径：调用 `builder(gen)` 重建，再 CAS 装入 `current`；
   - CAS 失败（别人先装了一个）也照样返回自己构造的——因为同 gen 下 builder 输出**字节等价**，指针不同但内容相同。

**为什么 CAS 失败不重试**：避免活锁。两个 goroutine 同时进入重建分支，各自计算完毕都尝试 CAS，第二个失败方丢弃自己的 snapshot（GC 回收）并返回自己手里的指针——调用方拿到的内容是对的，只是少了一个"全局指针等价"性质。下一次 `Snapshot()` 调用就能收敛。

### 3.5 ETag 构造（`schema_snapshot.go:84-98`）

```go
h := sha256.New()
for _, n := range names {
    def := skills[n]
    tools = append(tools, models.ToolDefinition{
        Name, Description, Parameters,
        Source: "skill:" + def.Name,
    })
    h.Write([]byte(n))           // name
    h.Write([]byte{0})            // \0 分隔，避免 ab|c 与 a|bc 撞 hash
    h.Write(def.Parameters)       // JSON Schema 原始字节
    h.Write([]byte{0})
}
etag := hex.EncodeToString(h.Sum(nil))[:12]
```

ETag **没有覆盖 Description**——这是有意的：Description 变化（typo 修复、措辞润色）不影响 LLM 调用语义，不应让 prompt cache miss。

ETag **没有覆盖 Executor**——webhook URL 变了不算 schema 变化，对 LLM 透明。

**陷阱**：如果两个 skill 的 Parameters 字符级别不一致（缩进、字段顺序），即便语义相同，ETag 也会变。建议运维方在注册前对 Parameters 做 canonical JSON 编码。

---

## 4. Register / Unregister 时序

### 4.1 Register 流程

```
Register(def *Definition) error
    │
    ├─ 字段校验
    │     ├─ Name 必填
    │     ├─ Description 必填
    │     ├─ Executor.Type 缺省 "webhook"
    │     ├─ Executor.Type=="webhook" 时 URL 必填
    │     ├─ Method 缺省 "POST"
    │     └─ Timeout 缺省 30
    │
    ├─ def.CreatedAt = time.Now()           # 由服务端填充
    │
    ├─ r.mu.Lock()
    ├─ if _, exists := r.skills[def.Name]; exists
    │       return fmt.Errorf("skill '%s' already exists", def.Name)
    ├─ r.skills[def.Name] = def
    ├─ r.schemaStore.Bump()                 # ← 关键：让快照失效
    ├─ r.mu.Unlock()                        # （defer）
    │
    └─ logger.Info("skill registered", name, executor.type)
```

**幂等性**：不幂等。重复注册同名 skill 返回 409 Conflict（API 层），客户端需先 Unregister 再 Register。

**事务性**：单写锁内完成，要么全成功要么全失败——失败时 schemaStore 未 Bump，快照不会失效（其他读不受影响）。

### 4.2 Unregister 流程

```
Unregister(name string) error
    │
    ├─ r.mu.Lock()
    ├─ if _, ok := r.skills[name]; !ok
    │       return fmt.Errorf("skill '%s' not found", name)
    ├─ delete(r.skills, name)
    ├─ r.schemaStore.Bump()
    ├─ r.mu.Unlock()
    │
    └─ logger.Info("skill unregistered", name)
```

**不会**同步删除 `functions[name]`——遗留的 Go 闭包继续占内存。当前 `RegisterFunction` 无调用方，影响为零；未来引入时需补 `delete(r.functions, name)`。

### 4.3 并发安全验证

- `registry_test.go` 覆盖：基本 Register/Unregister/List/Execute；
- `schema_snapshot_test.go:75-104` 1000 并发 Snapshot 调用断言全部拿到**同一指针**（pointer equality），实测无 race；
- 但**没有**测试"Register 并发 + Snapshot 并发"的混合场景，依赖 `sync.RWMutex` 的语义保证。

---

## 5. Execute 路径深入

### 5.1 路径选择优先级（`registry.go:242-274`）

```
1. RLock + lookup (skills, functions) + RUnlock     ← 极短临界区
2. 如果 skill 不存在 → return error
3. 如果 functions[name] 存在 → 走 Go 函数闭包
4. 否则按 def.Executor.Type 分支：
     "webhook"  → executeWebhook
     "function" → 但 !hasFn → return error  ⚠ 不可达分支（步骤 3 已拦截）
     default    → return ToolResult{IsError, "Unknown executor type"}
```

**第 4.2 分支的微妙性**：`def.Executor.Type == "function"` 但 `functions[name]` 未注册——逻辑上是"用户声明了一个 function 类型 skill 但忘记注册 Go handler"。当前没有任何调用方注册 function-type skill（API 只接受 webhook 类型，Function 实现是预留扩展点），所以这个分支永远是死代码。

### 5.2 Webhook 执行细节（`registry.go:277-330`）

请求构造：

```go
body, _ := json.Marshal(map[string]interface{}{
    "skill": def.Name,
    "args":  json.RawMessage(args),       // args 原样透传，不解码
})

reqCtx, cancel := context.WithTimeout(ctx, def.Executor.Timeout * time.Second)
req, _ := http.NewRequestWithContext(reqCtx, method, def.Executor.URL, bytes.NewReader(body))
req.Header.Set("Content-Type", "application/json")
for k, v := range def.Executor.Headers {
    req.Header.Set(k, v)                  // ⚠ 完全信赖注册方
}
```

请求体格式固定为 `{"skill":<name>,"args":<raw>}`——webhook 实现方必须按此约定解析。`args` 字段保持 `json.RawMessage` 是为了避免双重编码（LLM 给的 args 本来就是 JSON 字符串，重新 Marshal 会变成转义字符串）。

响应处理：

```go
resp, err := r.httpClient.Do(req)
if err != nil {
    return ToolResult{
        Content: fmt.Sprintf("Webhook call to %s failed: %v", def.Executor.URL, err),
        IsError: true,
    }, nil                                // 注意：返回 nil error，错误信息内嵌 ToolResult
}
defer resp.Body.Close()

respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))   // 1MB cap
isError := resp.StatusCode >= 400

if isError {
    content = fmt.Sprintf("HTTP %d from %s:\n%s", resp.StatusCode, def.Executor.URL, content)
}
```

**关键设计选择**：

1. **错误内嵌而非返回**——所有 webhook 失败都返回 `(ToolResult{IsError:true}, nil)`，让 orchestrator 把错误作为 observation 喂回 LLM，由 LLM 自主决定重试或换工具。返回 Go-level error 会让 orchestrator 中断 ReAct loop。

2. **1MB 响应上限**——硬截断，超出部分丢失。超大响应（如返回完整文件内容）需 skill 实现方自己分页/链接化。

3. **status >= 400 即 IsError**——粒度粗糙：3xx 不算错（webhook 应处理重定向），2xx + 错误业务码靠 Body 文本表达（IsError=false）。

4. **URL 在错误消息里裸露**——`Content: "Webhook call to <URL> failed"` 会被作为 tool result 回喂给 LLM。如果 URL 含 secret query param（`?token=...`），会泄漏到 LLM context 甚至前端 SSE 流。运维需保证 URL 无敏感信息。

---

## 6. 设计权衡

### 6.1 为什么不在 Register 时校验 JSON Schema？

**取舍**：

- ✅ 跳过校验：Register 极快，schema 错误延迟到 LLM 调用时发现（LLM 会拒绝 malformed schema 工具）；
- ❌ 校验：增加 `xeipuuv/gojsonschema` 依赖，编译期 schema 也要预编译；常见的局部错误（缺 `type` 字段）能提前发现。

当前**未校验**。生产实践中 schema 错误极少（运维通常 copy 现有 skill 改改），收益不大。

### 6.2 为什么 `Execute` 把 RLock 持续时间压到最短？

```go
r.mu.RLock()
def, ok := r.skills[name]
fn, hasFn := r.functions[name]
r.mu.RUnlock()                        # ← 在 HTTP 调用前释放
// ... HTTP call (可能耗时几秒) ...
```

**对照反例**：

```go
r.mu.RLock()
defer r.mu.RUnlock()                  # ❌ 整个 HTTP 调用都持有读锁
// ... HTTP call ...
```

后者会让一个慢 webhook（30s timeout）阻塞所有 Register/Unregister 30 秒——Register 需要写锁，写锁等待期间任何新读也都被阻塞。

代价：`def *Definition` 指针在 RUnlock 后可能被并发 Unregister 删除，但 Go GC 仍会保留底层对象直到当前 goroutine 释放——所以 `def.Executor.URL` 读取永远安全。

### 6.3 为什么用 `atomic.Pointer` 而不是 `sync.Map`？

- `sync.Map` 优化的是"键级别"并发读写，不适合本场景的**整体快照**语义；
- `atomic.Pointer[T]` 给的是"整块 immutable 数据原子替换"，正好匹配"schema 快照"模型；
- 内存开销：旧 snapshot 在最后一个引用消失前由 GC 回收，注册频繁时可能短暂双倍占用。生产场景每秒不会有多次 Register，不构成问题。

### 6.4 为什么不在 Snapshot 时把工具列表 cache 到磁盘？

ToolDefinition 列表完全是运行时构造的——重启后 `skill.Registry` 是空的，所有 webhook skill 都丢失。

这是**有意**的：

- Skill 是运行时配置，应由控制面（CI/CD、外部 controller）在启动后通过 API 重新注入；
- 持久化会引入"重启后 schema 缓存 vs 实际工具不一致"的状态偏差风险；
- 简化模型：进程是 stateless，状态在外部。

**代价**：运维方必须在外部维护"应该注册哪些 skill"的清单。生产实践中通常配合 Kubernetes Operator 或 init container 完成。

---

## 7. 后续演进（P0/P1/P2）

### P0（已知风险，必须修复）

1. **Webhook URL SSRF 防护缺失**
   - 现状：任何 URL 接受，可访问内网 metadata 服务（`169.254.169.254`）、localhost、私有 IP；
   - 建议：注册时调用 `internal/security.EgressValidator.Validate(url)`，拒绝命中 CIDR 黑名单的 URL；
   - 优先级：P0（生产部署必须，否则租户可窃取 cloud metadata）。

2. **Header 注入未过滤**
   - 现状：`def.Executor.Headers` 原样透传，注册方可注入 `Authorization: Bearer <stolen>`；
   - 建议：在 `executeWebhook` 维护一份禁止覆盖的 header 白名单（至少 `Authorization` / `Cookie` / `Host`）；
   - 优先级：P0。

3. **HITL / RiskLevel 实现缺失**
   - 现状：`_principles.go` 描述了 `RiskLevel=2 → ErrNeedApproval` 流程，但 `Definition` 没有 RiskLevel 字段、`Execute` 没有任何 HITL 检查；
   - 建议：要么把 `_principles.go` 中相关章节标记为"未实现"，要么落地 RiskLevel + 与 orchestrator 的 HITL 集成（参见 12_hitl.md）；
   - 优先级：P0（文档与实现不一致是首要诚信问题）。

### P1（功能完善）

4. **JSON Schema 校验**
   - 注册时调用 `gojsonschema.NewSchemaLoader` 预编译 Parameters，失败拒绝；
   - 收益：提前捕获 schema 错误，LLM 端不再因 malformed tool 报错。

5. **Audit 埋点**
   - 当前 webhook 调用不写入 `internal/audit`，无法追溯"是哪个 user 调用了哪个 skill"；
   - 建议：在 `Execute` 入口写 audit event（skill name, args 摘要, caller userID）。

6. **Metrics**
   - 没有 Prometheus counter 记录 skill 调用次数 / 错误率；
   - 建议：embed `prometheus.CounterVec(name, status)` 类似 `tools.Registry`（参见 07_tools.md §4）。

7. **Update 操作**
   - 当前只能 Unregister + Register，存在"短暂消失"窗口；
   - 建议：加 `Update(name, def)` 在写锁内完成原子替换。

### P2（性能 / 体验）

8. **Function 类型 skill 实落地**
   - 当前 `RegisterFunction` 无调用方，function 分支是死代码；
   - 设想：允许通过 `internal/agentloop` 把内部 Go 函数注册成 skill（替代 builtin_tools.go 的硬编码）；

9. **Schema canonical 编码**
   - 注册时把 Parameters 做 canonical JSON（按字段名排序）再存储，避免 indent 漂移导致 ETag 抖动；

10. **Rate limiting**
    - 按 skill name 限 RPS（RFC 提到的 `RPS` 字段），配合 `internal/security` 做精细化保护。

---

## 8. 设计教训

1. **RFC 文档不是合同**——`_principles.go` 549 行写得言之凿凿，但 RiskLevel / RPS / Source 字段全部未实现。下次写 RFC 时应明确标记每条款的实现状态（implemented / planned / abandoned），而不是把所有"未来想做"的功能都用现在时态描述。

2. **lock-free read 不是免费的**——`atomic.Pointer` 看起来零成本，但每次 Bump 都让 GC 多回收一个旧 snapshot；如果注册速度跟读速度同量级（如 1000 QPS Register），内存抖动会变成 bottleneck。本场景因 Register 是低频运维操作（每天几十次）才合算。

3. **错误内嵌 vs Go error 不是品味问题**——在 LLM ReAct loop 里，所有可观测的失败都应作为 observation 回喂给 LLM，让模型自主决策。Go-level error 会中断 ReAct，丢失"让 LLM 重试 / 换工具"的机会。`executeWebhook` 选 "return ToolResult{IsError, msg}, nil" 是有意的。

4. **ETag 字段选择要保守**——把所有字段都喂进 hash 看似"安全"，实则会让无害变更（描述润色）破坏 prompt cache。只 hash 真正影响 LLM 调用语义的字段（Name + Parameters）。

5. **接口解耦 vs 类型暴露**——orchestrator 通过匿名 interface 引用 skill.Registry（避免 import cycle），但 API 层直接 `*skill.Registry`（需要类型方法）。这种"内层接口 + 外层具体类型"是 Go 项目常见的折中。

---

## 9. 相关章节

- **07_tools.md** §1.1 §2.5：`tools.Registry` 与 `skill.Registry` 的边界划分；orchestrator 三级 dispatch。
- **06_mcp.md** §1.2：MCP 工具与 skill 工具在 `GetAvailableTools` 的合并路径。
- **09_orchestrator.md**（待重写）：`dispatchTool` / `executeTool` / HITL / 推测缓存。
- **12_hitl.md**（待重写）：RiskLevel 真正落地的位置（不在本包）。
- **18_auth.md**（待重写）：API 端鉴权决定"谁可以 PUT skill"。

---

下一篇：[`09_orchestrator.md`](09_orchestrator.md) —— ReAct loop 主控、tool 三级分发、HITL 拦截、speculative cache —— 把前八篇的 Registry / LLM client / RAG / Sandbox / MCP / Tools / Skill 全部捏合成一个 chat 请求的完整闭环。
