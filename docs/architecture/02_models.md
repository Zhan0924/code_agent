# 02 · 领域模型 `internal/models`

> 代码：`internal/models/models.go` (333 行，零依赖；只引 `encoding/json` + `time`)
>
> 被引用次数（`grep -r "models\." internal/ | wc -l` ≈ 800+）—— 几乎每个子系统都依赖它

---

## 1. 模块定位

**"所有跨子系统流转的数据形状都在这里钉死，业务包不许各自再定义一份。"**

`internal/models` 是整个 agent 系统的**类型枢纽**——一个**零行为、零依赖、纯 struct** 的包：

- **零行为** —— 所有 struct 都只是数据，唯一的"方法"来自 JSON tag（序列化契约）；
- **零依赖** —— 只引入 `encoding/json` 和 `time`，连 `uuid` 都不引；
- **一处定义** —— `Session` / `Message` / `ToolCall` / `CodeChunk` 在 session/api/orchestrator/rag 多处使用，避免类型漂移。

这种克制带来三个直接收益：

1. **任何子系统都能 import `models`** 而不会造成循环依赖——它是依赖图里的"叶子节点 / 共同源头"；
2. **修改一次领域字段**（如加 `Message.Pinned`）全局类型安全地联动；
3. **测试 mock 直接 `models.X{...}`**——不用 import 庞大的子系统包。

---

## 1.5 核心设计问题

### 为什么单独一个 `models` 包，不让定义留在第一个使用者里？

`Session` 被 session/api/orchestrator 三处都用；如果定义在 session 包，其他两个都得 import session。渐渐**所有人都 import session**——一旦 session 想用 orchestrator 的类型就循环了。

`models` 的定位是**"被所有人依赖、但不依赖任何业务包的枢纽"**。Go 模块系统里这种"共享类型包"是规范做法——`net/http.Request` / `database/sql.Rows` 都是同样的模式。

### JSON Schema 稳定性 = 前端契约

`Message` / `ToolCall` / `ToolResult` 直接作为 API response 返回前端，字段顺序 / 名字 / 零值策略都是**约定**：

| 操作 | 是否破坏性 |
|------|-----------|
| 加字段（带 `omitempty`） | ✅ 兼容 |
| 改字段名 | ❌ 破坏 |
| 改字段类型 | ❌ 破坏 |
| 删字段 | ❌ 破坏 |
| 改 JSON tag | ❌ 破坏 |
| 加新 enum 值 | ⚠️ 半破坏（前端 switch 默认分支没处理就崩） |

历史教训：`TaskState` 曾从 `int` enum 改成 `string`，前端全挂——最终保字符串字面但内部 const 仍是稳定的小写英文，跨语言客户端能安全反序列化。

### `TaskIntent` enum 是"权限边界"

新增 intent 时必须**同步三处**：

1. `orchestrator.parseIntent` 的 LLM system prompt（告诉 LLM 有这个类别）
2. orchestrator/router 的 switch case（业务处理）
3. HITL 判定（如果是危险类，挂上 require_approval）

**遗漏任一处**的后果：新 intent 被归类到 `IntentConversation`（fallback），危险命令绕开审批——这是**安全漏洞而非 bug**。所以每次加 intent 都要做"三件套审查"，并由测试覆盖。

### 为什么 `Embedding []float32` 放在 `CodeChunk` 里而不是分离？

简化 mock 和单测——测试时 `CodeChunk{Embedding: []float32{0.1, 0.2}}` 直接构造即可。

**入库时由 `qdrant_store.go` 主动剥离**：写 Qdrant 前 `chunk.Embedding = nil` 再 marshal payload；查询返回时也不带 Embedding 字段（只保留 ID + payload + score）。这样**内存态完整、持久态精简**——文档存储不会爆。

代价：`json.Marshal(chunk)` 在不剥离的情况下会包含 1536 个 float32，6KB 多。所以序列化前一定要剥（这是个隐性约定，每个 RAG 入库路径都要手动处理）。

---

## 2. 类型分组总览

`models.go` 333 行里定义了 **24 个公开类型**，按用途分 7 组：

```
┌─ Task / Workflow ────────────┐  ┌─ Session / Message ─────┐
│ TaskState     (6 个 enum)    │  │ Role        (4 个 enum) │
│ TaskIntent    (6 个 enum)    │  │ Message                 │
│ Task                          │  │ CacheControl            │
│ ExecutionPlan                 │  │ Session                 │
│ PlanStep / StepType           │  └─────────────────────────┘
│ TaskResult                    │
└──────────────────────────────┘  ┌─ Tool / MCP ────────────┐
                                   │ ToolCall                │
┌─ Sandbox ─────────────────────┐  │ ToolDefinition          │
│ SandboxRequest                │  │ ToolResult              │
│ SandboxResult                 │  └─────────────────────────┘
└───────────────────────────────┘
                                   ┌─ RAG ───────────────────┐
┌─ HITL ────────────────────────┐  │ CodeChunk               │
│ ApprovalRequest               │  │ RetrievalResult         │
│ ApprovalResponse              │  └─────────────────────────┘
└───────────────────────────────┘
                                   ┌─ Structured Output ─────┐
┌─ API / Streaming ─────────────┐  │ ResponseFormatType      │
│ ChatRequest / ChatResponse    │  │ ResponseFormat          │
│ StreamEvent                   │  │ JSONSchemaFormat        │
│ ReactStreamEvent              │  └─────────────────────────┘
└───────────────────────────────┘
```

---

## 2.5 数据流总览

```text
═══════════════════ 类型在系统里的旅行轨迹 ═══════════════════

[Frontend]
    │
    │  POST /api/v1/chat   { session_id, message, stream, output_format? }
    ▼
[ChatRequest]
    │
    ▼
[api.Handler] ── parseIntent ──▶ Task{Intent, State=pending}
    │
    ▼
[Orchestrator.processMessage]
    │
    │  Build prompt
    │  ├─ system + RAG + history → []Message
    │  └─ register tools → []ToolDefinition
    ▼
[LLM.ChatCompletion(ChatRequest{Messages, Tools, OutputFormat?})]
    │
    ▼
[ChatResponse{Content, ToolCalls: []ToolCall}]
    │
    │  for each ToolCall:
    │    ┌──────────────┐
    │    │ executeTool  │ → ToolResult{Content, IsError}
    │    └──────────────┘
    │       ├─ file_tools → ToolResult.Content = 文件 patch
    │       ├─ sandbox   → SandboxResult → ToolResult
    │       ├─ rag       → []RetrievalResult → ToolResult
    │       └─ mcp       → 外部 JSON → ToolResult
    │
    │  append Message{Role=tool, Content, ToolCallID}
    │
    ▼  循环直到 ToolCalls=[] 或 step ≥ maxSteps
    │
    ▼
[Session] (Redis hot + cold) ── messages append
    │
    │  SSE 推送：
    │  StreamEvent{Type, Data: JSON}                  ← 普通 chat
    │  ReactStreamEvent{Type, Step, ToolName, ...}    ← ReAct 高频
    │
    ▼
[Frontend EventSource] 渲染
```

**敏感命令命中黑名单时**：

```text
Orchestrator.suspendForApproval()
    │
    ├─ Task.State = suspended
    │
    ▼
ApprovalRequest{TaskID, Action, RiskLevel, Details}
    │  通过 SSE 推送到前端
    │
    ▼ 用户决策
ApprovalResponse{TaskID, Approved, Comment, Params?}
    │  通过 POST /api/v1/approvals/:taskID
    │
    ▼ Temporal Signal 唤醒 workflow
Workflow.Continue → 恢复 ReAct 循环
```

---

## 3. Task 生命周期 FSM

`TaskState` 定义任务状态机，**两份一致的解释**分别在 `orchestrator/orchestrator.go` 和 `temporal/workflows.go`：

```
pending ──► planning ──► executing ──┬──► completed
                │                    │
                ▼                    ├──► failed
           suspended  ◄──────────────┤       (HITL 审批点)
                │                    │
                └────────────────────┘
                │
                ▼
            cancelled
```

| 状态         | 语义                                               |
|--------------|----------------------------------------------------|
| `pending`    | 已收到用户输入，等待 intent 解析                   |
| `planning`   | 正在调用 LLM 生成 `ExecutionPlan`（planner 路径）  |
| `executing`  | ReAct 循环 / planner 按 DAG 执行                   |
| `suspended`  | 命中敏感规则，等待 `ApprovalResponse` (HITL)       |
| `completed`  | 所有 step 成功，`TaskResult.Success=true`          |
| `failed`     | 任一关键 step 失败且无可重试路径                   |
| `cancelled`  | 用户显式终止或超时                                 |

### 3.1 `Task` 结构

```go
type Task struct {
    ID          string          // UUID v4
    SessionID   string          // 绑定到 Session
    UserInput   string          // 原始用户输入
    Intent      TaskIntent
    State       TaskState
    Plan        *ExecutionPlan  // nil（ReAct 路径）或非 nil（planner 路径）
    Result      *TaskResult     // nil 直到 completed/failed
    Metadata    json.RawMessage // 自由扩展，调用方约定 schema
    CreatedAt   time.Time
    UpdatedAt   time.Time
    CompletedAt *time.Time      // 指针：用 nil 表示尚未完成
}
```

**为什么 `Metadata` 用 `json.RawMessage` 而不是 `map[string]any`？**

往返时不会丢失数字精度（`json.Number` 会变成 `float64`），也不会改变字段顺序。代价是必须先 `json.Unmarshal(metadata, &target)` 才能用——但这只在需要读时才付。

### 3.2 `TaskIntent` — 六类意图

```go
const (
    IntentCodeQuery     // 读/问：搜代码、读 docs、查符号
    IntentCodeExecute   // 执行：跑脚本、测试、build
    IntentDiagnose      // 诊断：读日志、跑命令、查进程
    IntentDeploy        // 部署：kubectl、docker push、CI
    IntentMCPCall       // 纯外部工具调用（GitHub/Jira/DB）
    IntentConversation  // 闲聊（fallback，跳过 planner）
)
```

`orchestrator.parseIntent` 在 ReAct 循环开始前由 LLM 做一次分类，决定：

- 是否走 planner（IntentDeploy/IntentDiagnose 倾向于 plan）；
- 是否拉 repomap（IntentCodeQuery/IntentCodeExecute 必拉）；
- HITL 默认挂载（IntentDeploy 默认强挂；其他靠 sensitive_patterns）；
- maxSteps（见 [21_agentloop](21_agentloop.md)：`getMaxSteps` 按 intent 硬编码 10/15/20/25/50）。

---

## 4. `ExecutionPlan` 与 `PlanStep`

### 4.1 `PlanStep`

```go
type PlanStep struct {
    ID          string          // 步骤 UUID
    Type        StepType        // 5 种
    Description string          // LLM 的自然语言说明
    Tool        string          // 工具名（如 "search_code"）
    Parameters  json.RawMessage // 工具参数
    DependsOn   []string        // 依赖的其他 step ID → DAG
    Status      TaskState
    Output      string
}
```

**`DependsOn` 是 planner 的核心**：`planner/executor.go` 基于它构 DAG 并做拓扑排序——无依赖的 step 并发执行。详见 [10_planner](10_planner.md)。

### 4.2 `StepType`

```go
StepTypeLLMCall   // 纯 LLM 推理 / 子决策
StepTypeRAGQuery  // 检索语料
StepTypeSandbox   // 容器内执行脚本
StepTypeMCPTool   // 外部 MCP 工具
StepTypeHITL      // 挂起等人工审批
```

**HITL 作为一种 step 而非独立事件**：允许 plan 里显式插入 `[..., risky_op, HITL, follow_up, ...]`——workflow 停在此处等 Signal、之后继续，**不需要重新 plan**。这是 plan-then-execute 的关键设计：审批点是计划的一部分，不是计划之外的中断。

---

## 5. `Session` & `Message`

### 5.1 `Message` — 比上一版多 3 个字段

```go
type Message struct {
    ID           string          // UUID
    Role         Role            // user / assistant / system / tool
    Content      string
    ToolCalls    []ToolCall      // 只在 role=assistant 时携带
    ToolCallID   string          // 只在 role=tool 时回填
    Metadata     json.RawMessage
    Timestamp    time.Time
    TokenCount   int             // 离线预估，供 pruner 使用
    CacheControl *CacheControl   // ← 新增：Anthropic prompt cache 标记
    Pinned       bool            // ← 新增：pruner 永不裁剪此条
}
```

三个值得注意的字段：

**`TokenCount` 冗余存储是有意为之**：裁剪算法每条消息上用到的次数远多于它被更新的次数（写一次 → 读数十次）。`session/redis_manager.go` 在 append 时调 `llm.FastEstimate` 算一次存进去。

**`CacheControl *CacheControl`**：

```go
type CacheControl struct {
    Type string `json:"type"` // "ephemeral" — cached for the duration of the request
}
```

当 `CacheControl != nil` 时 `llm.Client` 会在序列化消息时注入 Anthropic 的 `cache_control` 字段，让该 message 成为 prompt cache 的断点——后续 turn 复用时 system prompt 部分计费降到 10%。**典型用法**：在系统 prompt + 工具定义之后的第一条历史消息上打 marker，让"工具定义"这段固定 token 进入缓存（[03_llm](03_llm.md) §prompt caching）。

**`Pinned bool`**：`agentloop.PruneMessages` 看到 `Pinned=true` 的消息会永远保留——用户/上游显式标记的关键上下文（比如用户上传的需求描述、关键报错）即使在裁剪时也不会丢。详见 [21_agentloop](21_agentloop.md) §7 PruneMessages。

### 5.2 `Session`

```go
type Session struct {
    ID        string
    UserID    string
    ProjectID string    // ← 多项目场景：同一用户的不同项目隔离
    Messages  []Message
    Summary   string    // session/summarizer.go 异步生成的历史摘要
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

`Summary` 体现滑动窗口设计：超 `summary_threshold_tokens` 阈值时把老消息替换成一段摘要——既减 token 又保留语义延续性（详见 [12_session](12_session.md)）。`ProjectID` 让同一用户的"前端项目"和"后端项目"的对话上下文互不污染。

---

## 6. Tool 三兄弟

```go
type ToolCall struct {
    ID   string          // 稳定 ID；tool_result 反向关联
    Name string
    Args json.RawMessage // 工具参数（JSON）
}

type ToolDefinition struct {
    Name        string
    Description string
    Parameters  json.RawMessage // JSON Schema
    Source      string          // "builtin" | "mcp:<server_name>"
    RiskLevel   int             // ← 0=safe, 1=moderate, 2=high(需 approval)
}

type ToolResult struct {
    ToolCallID string
    Content    string
    IsError    bool
}
```

| 谁生产           | 谁消费                                          |
|------------------|------------------------------------------------|
| `ToolDefinition` | `tools/registry.go` 汇总 → LLM 的 `tools` 参数 |
| `ToolCall`       | LLM 输出 → `orchestrator.executeTool` 分发     |
| `ToolResult`     | 执行完 → 作为 `Role=tool` 的 Message 喂回 LLM  |

**`Source` 字段的运维价值**：

- `"builtin"` — 本地 Go 实现（read/write/edit/run_tests 等）
- `"mcp:github"` — 来自名为 `github` 的 MCP server
- `"mcp:filesystem"` — 同上

当 MCP server 断开时，`tools.Registry` 可以**精确下线该 server 的所有工具**而不影响 builtin——`Definitions()` 返回的列表自动过滤掉 source 匹配的项（[06_mcp](06_mcp.md) §dynamic tool registry）。

**`RiskLevel` 用法**：orchestrator 在分发前看 `RiskLevel`：

- 0（safe）：直接执行（read_file / search_code / list_dir）
- 1（moderate）：执行 + 审计日志（edit_file / write_file / run_tests）
- 2（high）：暂停走 HITL approval（`execute_code` 跑任意脚本、MCP github push 等）

注意 `RiskLevel` 是**预声明**而非动态判断——工具注册时就钉死，避免运行时算法漂移。**敏感命令检测**（regex 匹配 `DROP DATABASE`）是另一道独立防线，与 RiskLevel 双层叠加。

---

## 7. Sandbox 请求/结果

```go
type SandboxRequest struct {
    Language     string            // python | go | bash | node
    Code         string
    Files        map[string]string // 额外挂载文件（如 requirements.txt）
    Env          map[string]string
    Timeout      time.Duration
    StreamOutput bool              // true → 边跑边推 SSE
}

type SandboxResult struct {
    ExitCode int
    Stdout   string
    Stderr   string
    Duration time.Duration
    Killed   bool              // 区分"正常退出 code!=0" vs "被 timeout/OOM 杀掉"
}
```

**`Killed` 字段是排障关键**：

- `ExitCode != 0` + `Killed=false` → 用户代码逻辑错误（脚本里 `sys.exit(1)`）
- `ExitCode != 0` + `Killed=true` → 容器超时或 OOM 被强制 kill
- `ExitCode == 137` + `Killed=true` → SIGKILL（通常是 OOM）

如果不区分这两类，用户报告"我的代码退出码 137"时很难排查——`Killed` 让 orchestrator 在错误消息里加 hint："Process killed (likely OOM or timeout)"。

`StreamOutput=true` 时 `sandbox.Manager` 以 SSE 逐行推送而不是等待完成——适合长跑的脚本（`pytest -v` 输出测试进度）。详见 [05_sandbox](05_sandbox.md)。

---

## 8. RAG 返回

```go
type CodeChunk struct {
    ID           string
    FilePath     string
    Language     string
    SymbolName   string                  // 例 "UserService.Save"
    SymbolType   string                  // "function" | "class" | "method"
    Content      string
    StartLine    int
    EndLine      int
    ScopeDepth   int                     // AST 嵌套深度 → pruner 决定粒度
    Dependencies []string                // 该函数调用的其他符号
    Metadata     map[string]string       // 租户隔离：project/version/...
    Embedding    []float32               // 内存态；入 Qdrant 前剥离
}

type RetrievalResult struct {
    Chunk  CodeChunk
    Score  float64
    Source string  // "dense" | "sparse" | "reranked"
}
```

**`Source` 保留命中来路**——三路混合检索的可视化基础（[04_rag](04_rag.md)）：

- `"dense"` — 向量语义检索（Qdrant ANN）
- `"sparse"` — BM25 精确匹配（关键词/标识符召回）
- `"reranked"` — 交叉编码器重排后的综合得分

UI 里可以给每个结果标注来源；调参时也能分别评估两路召回的质量。

**`ScopeDepth` 给 pruner 用**：当上下文紧张需要丢 chunk 时，优先丢 ScopeDepth 高的（局部细节），保留 ScopeDepth=0 的（顶层声明）——见 [13_context](13_context.md) §AST-aware pruner。

**`Metadata["project"]` 是租户隔离的关键**：Qdrant filter `metadata.project = $project_id` 让多项目共用同一 collection 互不污染。

---

## 9. HITL 模型

```go
type ApprovalRequest struct {
    TaskID      string
    SessionID   string
    Action      string    // "delete table users"
    RiskLevel   string    // "low" | "medium" | "high" | "critical"
    Details     string    // 完整命令文本
    RequestedAt time.Time
}

type ApprovalResponse struct {
    TaskID   string
    Approved bool
    Comment  string
    Params   json.RawMessage  // 用户可追加"允许但限制 row<=100"
}
```

**`Params` 的扩展性**：用户审批时可以**带参数细化**——比如审批"删表"但限制只删指定数据库；audit log 同时记录原始命令 + 审批参数。

`ApprovalRequest.RiskLevel` 是**字符串**而 `ToolDefinition.RiskLevel` 是**整数**——历史原因（前者面向前端展示，后者面向后端逻辑），尚未统一。`§12 演进列表`里列了"统一成枚举"。

两者通过 `/api/v1/approvals/:taskID` 互通，配合 `temporal/workflows.go` 的 `workflow.Await(Signal)` 实现真正的**可恢复中断**——workflow 进程崩了重启仍能从挂起点继续。

---

## 10. 结构化输出

```go
type ResponseFormatType string
const (
    ResponseFormatText       = "text"
    ResponseFormatJSONObject = "json_object"
    ResponseFormatJSONSchema = "json_schema"
)

type ResponseFormat struct {
    Type       ResponseFormatType
    JSONSchema *JSONSchemaFormat  // 仅 Type=json_schema 时非 nil
}

type JSONSchemaFormat struct {
    Name        string
    Description string
    Schema      json.RawMessage  // JSON Schema 定义
    Strict      bool             // true → OpenAI strict mode
}
```

**三档输出格式**：

- **text**：默认；自由文本
- **json_object**：保证返回合法 JSON（但无 schema 约束）
- **json_schema**：保证返回符合 `Schema` 的 JSON（OpenAI 4o / Anthropic prefill 都支持）

`ChatRequest.OutputFormat *ResponseFormat` 是**业务侧调用 LLM 时强制输出 schema** 的入口——典型用法：parseIntent 时强制返回 `{intent: "code_query", confidence: 0.95}`。

**`Strict=true`** 让 OpenAI 走 schema-constrained decoding（生成时每个 token 都受 schema 限制），代价是延迟略增但**绝不会产生 schema 违反**的输出——比"返回后用 jsonschema.Validate 校验 + 失败重试"省一倍 LLM 调用。

详见 [03_llm](03_llm.md) §structured output。

---

## 11. API 与流式事件

### 11.1 `ChatRequest` / `ChatResponse`

```go
type ChatRequest struct {
    SessionID    string          // 空 → 新建 session
    Message      string          // required
    Stream       bool            // true → SSE；false → 阻塞返回
    OutputFormat *ResponseFormat // 可选强制 schema
}

type ChatResponse struct {
    SessionID string
    TaskID    string
    Message   string
    State     TaskState
    Approval  *ApprovalRequest  // 命中 HITL 时非 nil
}
```

**Stream=true 时 `ChatResponse` 不返回**——前端从 SSE 流里拿 `ReactStreamEvent` 序列。Stream=false 时**等 ReAct 全部跑完**才返回（最长可能几分钟，所以 server.write_timeout=600s）。

### 11.2 `StreamEvent` — 通用 SSE 信封

```go
type StreamEvent struct {
    Type   string          // session | step_start | thinking | tool_call | ... | done | error
    Data   json.RawMessage // 类型内部解释
    TaskID string
}
```

**信封模型**：外部只看 `Type`，`Data` 是它的 payload。前端按 `Type` 分发到不同 React 组件。优点是**新增事件类型不破坏现有客户端**——不认识的 `Type` 跳过即可。

### 11.3 `ReactStreamEvent` — ReAct 专用扁平结构

```go
type ReactStreamEvent struct {
    Type       string      // step_start | thinking | tool_call | tool_result | rag_context | message | error | done
    Step       int         // 当前 ReAct 步号 (1-based)
    MaxSteps   int
    Content    string      // 文本（thinking/message/error）
    ToolName   string      // 工具名
    ToolArgs   string      // JSON 字符串化（前端可直接展示）
    ToolCallID string      // 关联用
    IsError    bool
    Intent     string      // 仅 step_start 携带
    TaskID     string
    Metadata   interface{} // 灵活扩展（如 step_start 携带 budget 信息）
}
```

**为什么和 `StreamEvent` 双轨而不是合一**：

- `StreamEvent.Data` 是 `json.RawMessage` —— 前端必须 **`JSON.parse(event.data).then(d => JSON.parse(d.data))`** 两次解析；
- `ReactStreamEvent` 字段**全扁平**——前端 `JSON.parse(event.data).toolName` 一次解析；
- ReAct 步频很高（每步几条事件，单 chat 可能上百事件），节省一次解析对前端是 20-30% 的解码省。

**`ToolArgs` 是 string 而不是 `json.RawMessage`**：故意冗余编码——前端拿来直接 `<pre>{toolArgs}</pre>` 渲染即可，不用 parse → stringify 往返。

---

## 12. 设计权衡

| 决策 | 动机 |
|------|------|
| 所有 ID 用 `string` 而非 `uuid.UUID` | JSON 往返零成本；不同生成策略（UUID v7 / ULID）可混用 |
| 敏感字段放 `json.RawMessage` 而非 `any` | 序列化往返稳定；避免数字类型被改成 `float64` |
| `Embedding` 放在 `CodeChunk` 而非独立表 | 简化 mock；写入 Qdrant 前在 `qdrant_store.go` 剥离 |
| HITL 视作 `StepType` 而非独立机制 | 让 planner 主动规划 `[..., risky_op, HITL, follow_up]` |
| 双套 Stream 事件 | `StreamEvent` 供普通 chat；`ReactStreamEvent` 供 ReAct 高频推送 |
| `models` **零业务方法** | 避免下游包循环依赖；演进时不牵连行为代码 |
| `RiskLevel` 双类型（int + string） | 历史遗留；前后端分别习惯不同表达；待统一 |
| `CacheControl` 留在 Message 而非 LLM 子包 | 让 session 持久化时能携带；切换 provider 时透传一致 |
| `Pinned` 是 Message 字段而非外部 set | 持久化一致；重启后仍知道哪条不能裁 |
| `Embedding []float32` 而非 `[]float64` | Qdrant 默认 float32；节省一半内存 |

---

## 13. 后续演进

P0（影响前端契约或安全）：

- [ ] 把 `ToolDefinition.RiskLevel`（int）和 `ApprovalRequest.RiskLevel`（string）**统一**为枚举类型——消除"前端拿到 1 还是 'high' 的混乱"
- [ ] 加 `SchemaVersion int` 字段到 `Message` / `Session`——支持后续 schema migration
- [ ] `Message.Content` 改 `[]ContentBlock`（文本 / 图片 / 工具结果）对齐 Anthropic Vision schema（当前是 string，多模态死路）

P1（演进性）：

- [ ] `OpenAPI Schema` 自动从 `models` struct 生成（当前 `api/openapi.yaml` 手维护，容易漂移）
- [ ] `CodeChunk.Embedding` 升级 `[]float16`（量化减半存储）—— 需 Qdrant 6.0+
- [ ] `Metadata json.RawMessage` 换成 `protobuf.Any`——为 IDE 扩展提供强类型编码

P2（可选）：

- [ ] `TaskIntent` 从字符串升级为 `struct{Type string; Subtype string}`——支持嵌套分类
- [ ] `Embedding` 字段拆出 `EmbeddedChunk struct{CodeChunk; Embedding []float32}`——让 RAG 入库时少一层"剥离"约定（但破坏性）

---

## 14. 与其他模块的边界

`models` 是叶子节点，被所有人依赖、不依赖任何业务包。下面是反向依赖图（精简）：

```
                       models
       ┌─────┬─────┬─────┼─────┬─────┬─────┐
       ▼     ▼     ▼     ▼     ▼     ▼     ▼
     llm   rag  session orch tools planner mcp
       │     │     │     │     │     │     │
       │     │     │     │     │     │     │
       └─────┴─────┴──┐  │  ┌──┴─────┴─────┘
                     ▼  ▼  ▼
                       api
                        │
                        ▼
                      main
```

**禁忌**：

- `models` 不许 import 任何 `internal/*` 包；
- 在子系统包里定义"和 models 重复但稍有不同"的类型——发现就重构合并；
- 直接在 `models` 里加方法（如 `Message.AddToolCall(...)`）——所有行为留给消费方。

---

## 15. 设计教训

这个包从 **150 行扩展到 333 行**，每次扩展都有过两次讨论：

1. **`CacheControl` 该放 Message 还是 LLM 包？** 一开始放 LLM 包，导致 session 持久化时无法保留 cache 标记，重启后 cache miss。回到 Message 才一致。
2. **`Pinned` 该是字段还是外部 `map[messageID]bool`？** 外部 map 在多副本部署时同步困难，且重启会丢；字段方案永远跟着 message 走，没有同步问题。
3. **`ToolDefinition.RiskLevel` 该统一 string 还是 int？** 当时改成 string 影响前端两个组件，"以后再统一"——结果一直没统一。这是 **"以后再改" 永远不会改** 的经典案例。

教训：**Data model 的扩展要看持久化路径**。"放哪个包"看似只是包结构问题，实际决定了"重启后还能不能恢复"。

---

下一篇：[`03_llm.md`](03_llm.md) —— OpenAI 兼容客户端、主备路由、熔断器与 prompt caching。
