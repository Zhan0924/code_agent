# 02 · 领域模型 `internal/models`

> 代码：`internal/models/models.go` (251 行，零依赖)

---

## 1. 模块定位

本包只做一件事：**定义跨子系统共享的纯数据类型**。

- **零行为** —— 所有结构体都只是 `struct{}`，没有方法（除 JSON tag）；
- **零依赖** —— 只引入 `encoding/json` 和 `time`；
- **一处定义，全局复用** —— 避免 `rag` 和 `orchestrator` 各自定义一份 `CodeChunk` 导致类型漂移。

这种做法的好处：

1. 任何子系统都能依赖 `models`，而不会造成循环依赖；
2. 修改一次领域字段（比如加 `Chunk.ScopeDepth`），全局类型安全地联动；
3. 测试 mock 可以直接构造 `models.X{…}`，不用 import 庞大的子系统包。

---

## 1.5 核心设计问题

### 为什么要单独一个 `models` 包？

避免循环依赖。`Session` 被 session / api / orchestrator 三处都用；如果
定义在 session 包，其他两个都得 import session 包——渐渐所有人都 import
session，一旦 session 想用 orchestrator 里的类型就循环了。**model 是
被所有人依赖、但不依赖任何业务包的枢纽**。

### JSON schema 稳定性是前端契约

`Message` / `ToolCall` / `ToolResult` 三件套直接作为 API response 返回给
前端，字段顺序 / 名字 / 零值策略都是**约定**：
- 加字段：允许（带 omitempty）
- 改字段名 / 类型：破坏性
- 删字段：破坏性
- 改 JSON tag：破坏性

做过一次破坏性变更：`TaskState` 从 int enum 改成 string，前端全挂——
最终用 JSON tag 保持字符串但底层保 int。

### `TaskIntent` enum 是"权限边界"

新增 intent 时必须同步 3 处：
1. `orchestrator.parseIntent` 的 LLM system prompt（告诉 LLM 有这个类别）
2. switch case（业务处理）
3. HITL 判定（如果是危险类）

**遗漏任一处**的后果：新 intent 被归类到 `IntentConversation`（fallback），
危险命令不走审批。

---

## 2. 类型分组总览

```
┌─ Task / Workflow ─────────────┐  ┌─ Session / Message ─────┐
│ TaskState                     │  │ Role                    │
│ TaskIntent                    │  │ Message                 │
│ Task                          │  │ Session                 │
│ ExecutionPlan                 │  └─────────────────────────┘
│ PlanStep / StepType           │
│ TaskResult                    │  ┌─ Tool / MCP ────────────┐
└───────────────────────────────┘  │ ToolCall                │
                                   │ ToolDefinition          │
┌─ Sandbox ─────────────────────┐  │ ToolResult              │
│ SandboxRequest                │  └─────────────────────────┘
│ SandboxResult                 │
└───────────────────────────────┘  ┌─ RAG ───────────────────┐
                                   │ CodeChunk               │
┌─ HITL ────────────────────────┐  │ RetrievalResult         │
│ ApprovalRequest               │  └─────────────────────────┘
│ ApprovalResponse              │
└───────────────────────────────┘  ┌─ API / Streaming ───────┐
                                   │ ChatRequest/Response    │
                                   │ StreamEvent             │
                                   │ ReactStreamEvent        │
                                   └─────────────────────────┘
```

---

## 2.5 数据流总览

下图展示核心领域类型在各模块间的流转路径：

```text
┌─────────────┐                                    ┌─────────────┐
│  Frontend   │──(ChatRequest)──▶│  API Layer  │──▶│ Orchestrator│
└─────────────┘                  └─────────────┘   └──────┬──────┘
                                                          │
      ┌───────────────────────────────────────────────────┘
      │
      ▼ [parseIntent]
┌─────────────────────────────────────────────────────────────────┐
│                    TaskIntent / TaskState                         │
│  pending → planning → executing → completed/failed/suspended    │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼ [buildMessages]
┌─────────────────────────────────────────────────────────────────┐
│  Session Messages: []Message{Role, Content, ToolCalls}           │
│  + SystemPrompt + RAG CodeChunks                                 │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼ [LLM ChatCompletion]
┌─────────────────────────────────────────────────────────────────┐
│  ChatResponse{Content, ToolCalls[]ToolCall}                      │
│  ToolCall: {ID, Name, Arguments(JSON)}                          │
└──────────────────────────┬──────────────────────────────────────┘
                           │
            ┌──────────────┴──────────────┐
            ▼                             ▼
┌──────────────────────┐      ┌───────────────────────────────┐
│ 无 ToolCalls         │      │ 有 ToolCalls → 逐一执行        │
│ → 返回 Content       │      │ executeTool(ToolCall)          │
└──────────────────────┘      └──────────────┬────────────────┘
                                             │
                                             ▼
┌─────────────────────────────────────────────────────────────────┐
│  ToolResult{Content, IsError}                                    │
│    ├── file_tools: 文件内容 / patch 结果                         │
│    ├── sandbox:  SandboxResult{Stdout, Stderr, ExitCode}        │
│    ├── RAG:      []RetrievalResult{Chunk, Score}                │
│    └── MCP:      外部服务返回的 JSON                             │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                           ▼ [SSE 推送]
┌─────────────────────────────────────────────────────────────────┐
│  StreamEvent / ReactStreamEvent                                  │
│  {Type: thinking|tool_call|tool_result|content|done,             │
│   Data: 对应 JSON payload}                                       │
│  ──▶ Frontend EventSource 解析渲染                               │
└─────────────────────────────────────────────────────────────────┘
```

---

## 3. Task 生命周期 FSM

`TaskState` 定义了任务状态机：

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
| `planning`   | 正在调用 LLM 生成 `ExecutionPlan`                  |
| `executing`  | 按 plan 顺序/并发执行 `PlanStep`                   |
| `suspended`  | 命中敏感规则，正在等待 `ApprovalResponse` (HITL)   |
| `completed`  | 所有 step 成功，`TaskResult.Success = true`        |
| `failed`     | 任一 step 失败且无可重试路径                       |
| `cancelled`  | 用户显式终止或超时                                 |

这个 FSM 在 `orchestrator/orchestrator.go` 和 `temporal/workflows.go` 两处各持一份一致解释。

### 3.1 `Task` 结构

```go
type Task struct {
    ID          string          // UUID
    SessionID   string          // 绑定到 Session
    UserInput   string          // 原始用户输入
    Intent      TaskIntent      // 解析后的意图
    State       TaskState
    Plan        *ExecutionPlan  // 可能为 nil（planning 之前）
    Result      *TaskResult     // 可能为 nil（completed 之前）
    Metadata    json.RawMessage // 自由扩展字段
    CreatedAt   time.Time
    UpdatedAt   time.Time
    CompletedAt *time.Time
}
```

> `Metadata` 用 `json.RawMessage` 而不是 `map[string]any` —— 这样在序列化/反序列化往返中不会丢失数字精度（`json.Number`）或字段顺序。

### 3.2 `TaskIntent` —— 意图路由表

```go
IntentCodeQuery     // 读/问：文件、docs、符号
IntentCodeExecute   // 执行：脚本、测试、build
IntentDiagnose      // 诊断：读日志、跑命令、看进程
IntentDeploy        // 部署：kubectl、docker push
IntentMCPCall       // 纯外部工具调用（GitHub/Jira/DB）
IntentConversation  // 普通闲聊（跳过 planner）
```

`orchestrator/orchestrator.go` 在 ReAct 循环开始前先由 LLM 做一次分类，决定是否走 planner、是否要拉 repomap、是否默认挂 HITL。

---

## 4. ExecutionPlan 与 PlanStep

### 4.1 `PlanStep`

```go
type PlanStep struct {
    ID          string          // 步骤 UUID
    Type        StepType        // 下表 5 种
    Description string          // LLM 的自然语言说明
    Tool        string          // 工具名（如 "search_code"）
    Parameters  json.RawMessage // 工具参数
    DependsOn   []string        // 依赖的其他 step ID → DAG
    Status      TaskState
    Output      string
}
```

`DependsOn` 是精髓：`planner/executor.go` 基于它构 DAG 并做拓扑排序 → 并发执行无依赖 step。详见 `10_planner.md`。

### 4.2 `StepType`

```go
StepTypeLLMCall   // 纯 LLM 推理 / 子决策
StepTypeRAGQuery  // 检索语料
StepTypeSandbox   // 容器内执行脚本
StepTypeMCPTool   // 外部 MCP 工具
StepTypeHITL      // 挂起等人工审批
```

HITL 作为一种"步骤"而非"事件"非常关键 —— 它允许 plan 里显式 `[…, step_3 = HITL, step_4, …]`，让 workflow 可以 **停在此处** 等 Signal，之后继续，**不需要重新 plan**。

---

## 5. Session & Message

### 5.1 `Message`

```go
type Message struct {
    ID         string
    Role       Role          // user / assistant / system / tool
    Content    string
    ToolCalls  []ToolCall    // assistant 消息可携带 tool_calls
    ToolCallID string        // tool 消息回填 assistant 的 tool_call_id
    Metadata   json.RawMessage
    Timestamp  time.Time
    TokenCount int           // 离线预估的 token 数，供 pruner 使用
}
```

> `TokenCount` 冗余存储是**有意为之**：裁剪算法在每条消息上用到的次数远多于它被更新的次数（写一次 → 可能被读数十次）。

### 5.2 `Session`

```go
type Session struct {
    ID        string
    UserID    string
    Messages  []Message
    Summary   string // 由 session/summarizer.go 异步生成的历史摘要
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

`Summary` 字段体现滑动窗口设计：超阈值时把老消息替换成一段摘要 → 既减少 token，又保留语义延续性（详见 `12_session.md`）。

---

## 6. Tool 相关三兄弟

```go
ToolDefinition { Name, Description, Parameters (JSON Schema), Source }
ToolCall       { ID, Name, Args (json.RawMessage) }
ToolResult     { ToolCallID, Content, IsError }
```

| 谁生产         | 谁消费                                         |
|----------------|------------------------------------------------|
| `ToolDefinition` | `tools/registry.go` 汇总 → LLM 的 `tools` 参数 |
| `ToolCall`     | LLM 输出 → `orchestrator.RunReactStream` 分发  |
| `ToolResult`   | 执行完 → 作为 `Role=tool` 的 Message 喂回 LLM  |

`ToolDefinition.Source` 字段是关键：

- `"builtin"` — 本地 Go 实现（search/read/edit 等）
- `"mcp:github"` — 来自名为 `github` 的 MCP server
- `"mcp:jira"` — 同上

这保证了当 MCP server 断开时，可以精确下线其工具（见 `07_tools.md`）。

---

## 7. Sandbox 请求/结果

```go
SandboxRequest { Language, Code, Files, Env, Timeout, StreamOutput }
SandboxResult  { ExitCode, Stdout, Stderr, Duration, Killed }
```

`Files` 字段允许"多文件场景"（如 Python 脚本 + requirements.txt），`StreamOutput=true` 时 `sandbox/manager.go` 会以 SSE 逐行推送而不是等待完成。`Killed` 区分 "进程正常退出但 code!=0" 与 "被超时/OOM 杀掉"，是排障关键。

---

## 8. RAG 返回

```go
CodeChunk {
    ID, FilePath, Language,
    SymbolName, SymbolType,        // "UserService.Save", "method"
    Content, StartLine, EndLine,
    ScopeDepth,                    // AST 嵌套深度 → pruner 决定粒度
    Dependencies []string,         // 该函数调用的其他符号
    Metadata map[string]string,    // 租户隔离用：project/version/…
    Embedding []float32,           // 内存态；入库时剔除
}

RetrievalResult { Chunk, Score, Source }
```

`Source` 字段保留"命中来自哪一路"：

- `"dense"` — 向量语义检索
- `"sparse"` — BM25 精确匹配
- `"reranked"` — 交叉编码器重排后的综合

用途：在 UI 里可以给每个结果标注来源；调参时也能分别评估两路召回的质量。

---

## 9. HITL 模型

```go
ApprovalRequest {
    TaskID, SessionID,
    Action,              // "delete table users"
    RiskLevel,           // low / medium / high / critical
    Details,             // 完整命令
    RequestedAt,
}

ApprovalResponse {
    TaskID, Approved bool, Comment,
    Params json.RawMessage,  // 用户可追加"允许但限制 row<=100"之类
}
```

这两类型在 `api/handlers.go` 的 `/api/v1/approvals/*` 接口里互通，配合 `temporal/workflows.go` 的 `workflow.Await(Signal)` 实现真正的 **可恢复中断**。

---

## 10. 流式事件

### 10.1 `StreamEvent` — 通用 SSE 信封

```go
StreamEvent {
    Type   string          // session|step_start|thinking|tool_call|…|done|error
    Data   json.RawMessage // 类型内部解释
    TaskID string
}
```

设计为 **信封模型**：外部只看 `Type`，`Data` 是它的 payload。前端按 `Type` 分发到不同 React 组件。

### 10.2 `ReactStreamEvent` — 结构化 ReAct 专用

```go
ReactStreamEvent {
    Type       string      // step_start|thinking|tool_call|tool_result|rag_context|message|error|done
    Step       int         // 当前 ReAct 步号
    MaxSteps   int
    Content    string      // 文本
    ToolName   string
    ToolArgs   string      // JSON 字符串化（前端直接展示）
    ToolCallID string
    IsError    bool
    Intent     string
    TaskID     string
    Metadata   interface{}
}
```

与 `StreamEvent` 相比，这里字段全**扁平化**，是为 `/api/v1/chat/react-stream` 接口量身设计 —— 前端不需要再 `JSON.parse(data)` 第二次。性能上对前端是 20-30% 的解码省。

---

## 11. 为什么这样设计？

| 设计决策 | 动机 |
|---|---|
| 所有 ID 用 `string` 而非 `uuid.UUID` | JSON 往返零成本；不同生成策略（UUID v7 / ULID）可混用 |
| 敏感字段放 `json.RawMessage` 而非 `any` | 序列化往返稳定；避免数字类型被改成 `float64` |
| `Embedding` 放在 `CodeChunk` 而非独立表 | 简化 mock；写入 Qdrant 前在 `qdrant_store.go` 剥离 |
| HITL 视作 `StepType` 而非独立机制 | 让 planner 能主动规划 `[…risky_op…, HITL, …follow_up…]` |
| 双套 Stream 事件 | `StreamEvent` 供普通 chat；`ReactStreamEvent` 供 ReAct 主循环高频推送 |
| `models` **零业务方法** | 避免下游包循环依赖；领域模型演进时不牵连行为代码 |

---

## 12. 后续演进

- [ ] 引入 `OpenAPI Schema` 自动从 `models` 结构体生成（目前 `api/openapi.yaml` 手维护，容易漂移）；
- [ ] `Message` 增加 `Attachments []Attachment` 支持图片/二进制输入（Claude 3.5 Vision 对接时需要）；
- [ ] `CodeChunk.Embedding` 从 `[]float32` 升级为 `[]float16`（量化减半存储）—— 需要 Qdrant 6.0+；
- [ ] 把 `Metadata json.RawMessage` 换成 `protobuf.Any` —— 为异构客户端（IDE 扩展）提供强类型编码。

---

## 11. 实现剖析与改进方向

### 每个类型的序列化契约

```
Message           ← LLM 协议的公共基石（OpenAI schema 映射）
 ├─ Role          "user" / "assistant" / "system" / "tool"
 ├─ Content       string / structured content（当前仅 string）
 ├─ ToolCalls     []ToolCall（只在 role=assistant 时出现）
 └─ ToolCallID    string（只在 role=tool 时出现，对应哪个 call）

ToolCall          ← LLM 发起的工具调用请求
 ├─ ID            稳定，tool_result 反向关联
 ├─ Type          "function"
 └─ Function      {Name, Arguments: json.RawMessage}

ToolResult        ← 工具执行的返回
 ├─ Content       工具输出（被 smartTruncateOutput 限长）
 └─ IsError       true 时 LLM 会收到"这步失败了"信号

Session           ← 对话容器
Task              ← 单次 ReAct 迭代的状态
TaskIntent        ← enum：决定路由到哪条业务链路
ApprovalRequest   ← HITL 的挂起-放行消息体
```

### Pros
- ✅ 单一包避免循环依赖
- ✅ JSON tag 直接决定 API 契约
- ✅ Intent enum 让 switch 覆盖有编译检查

### Cons
- ⚠️ Message.Content 是 string，不支持多模态（图片 / 附件）
- ⚠️ TaskIntent 新增需改 3 处（见 §1.5）
- ⚠️ 没有版本字段（message schema 演进难做向后兼容）

### 改进方向
- **P1** — Content 改 `[]ContentBlock`（文本 / 图片 / 工具结果）对齐 Anthropic schema
- **P1** — 加 `SchemaVersion` 字段以支持未来 migration
- **P2** — TaskIntent 从 string enum 升级为 struct（带 metadata）

---

下一篇：`03_llm.md` —— OpenAI 兼容客户端、主备路由、熔断器与指数退避。
