# Code Agent 系统架构与数据流总图

> 本文档使用 Mermaid 图形化描述 Code Agent 的完整架构层次和**7 条典型数据流动**路径。
> 每张图都可在 VS Code Mermaid 预览、GitHub、语雀中直接渲染。

## 目录

1. [分层系统架构（鸟瞰图）](#1-分层系统架构鸟瞰图)
2. [模块依赖关系图（Component Diagram）](#2-模块依赖关系图)
3. [数据流 ①：普通对话 `/chat` 完整生命周期](#3-数据流-普通对话-chat-完整生命周期)
4. [数据流 ②：RAG 代码检索（双路召回）](#4-数据流-rag-代码检索双路召回)
5. [数据流 ③：沙箱代码执行（SSE 流式）](#5-数据流-沙箱代码执行sse-流式)
6. [数据流 ④：HITL 人工介入（Temporal 挂起-唤醒）](#6-数据流-hitl-人工介入temporal-挂起-唤醒)
7. [数据流 ⑤：MCP 动态工具调用](#7-数据流-mcp-动态工具调用)
8. [数据流 ⑥：Session 上下文管理与压缩](#8-数据流-session-上下文管理与压缩)
9. [数据流 ⑦：LLM 路由 + 熔断 + 降级](#9-数据流-llm-路由--熔断--降级)
10. [状态机总览](#10-状态机总览)
11. [部署拓扑图（K8s 生产）](#11-部署拓扑图k8s-生产)

---

## 1. 分层系统架构（鸟瞰图）

```mermaid
graph TB
    subgraph Client["🌐 Client Layer"]
        Web["React UI<br/>(Vite + TypeScript)"]
        CLI["CLI / SDK"]
        IDE["IDE Plugin"]
    end

    subgraph Edge["🚪 Edge / Gateway Layer"]
        LB["Load Balancer<br/>(NGINX / ALB)"]
        WAF["WAF<br/>(Optional)"]
    end

    subgraph API["🔌 API Layer (Gin)"]
        Router["Router"]
        MW["Middleware Chain<br/>Recovery→RequestID→Trace→Metrics→<br/>RateLimit→Auth→RBAC"]
        H_Chat["/chat handlers<br/>(JSON/SSE/WS)"]
        H_Task["/tasks handlers"]
        H_WS["/workspaces handlers"]
        H_MCP["/mcp handlers"]
        H_Skill["/skills handlers"]
    end

    subgraph Orch["🧠 Orchestration Layer"]
        Orchestrator["Orchestrator<br/>(ReAct Loop)"]
        Planner["Planner<br/>(DAG Generator)"]
        Temporal["Temporal Worker<br/>(Workflow + Signal)"]
        FT["Failure Tracker"]
        MP["Message Pruner"]
        ATR["Auto Test Runner"]
        EE["Edit Engine"]
    end

    subgraph Capability["⚡ Capability Layer"]
        LLM["LLM Router<br/>(OpenAI/Claude/Local)"]
        RAG["RAG Engine<br/>(BM25 + Dense + Rerank)"]
        Sandbox["Sandbox<br/>(Docker + cgroups)"]
        MCP["MCP Client<br/>(JSON-RPC 2.0)"]
        Tools["Tool Registry"]
        Skill["Skill Registry"]
        Indexer["Indexer<br/>(AST Parser)"]
        RepoMap["RepoMap<br/>(File Watcher)"]
    end

    subgraph Context["📝 Context & Session Layer"]
        Session["Session Manager<br/>(Sliding Window)"]
        Summarizer["Summarizer"]
        Pruner["Context Pruner"]
        PromptBuilder["Prompt Builder<br/>(KV Cache Aware)"]
        Workspace["Workspace<br/>(Permission Boundary)"]
    end

    subgraph Storage["💾 Storage Layer"]
        PG[("PostgreSQL<br/>config/audit/tasks")]
        Redis[("Redis<br/>session/cache/revocation")]
        Qdrant[("Qdrant<br/>vectors")]
        FS["FileSystem<br/>(workspaces)"]
    end

    subgraph Ext["🌍 External Services"]
        OpenAI["OpenAI API"]
        Anthropic["Anthropic API"]
        MCPServers["MCP Servers<br/>(GitHub/Jira/DB)"]
    end

    subgraph Obs["🔭 Observability"]
        Prom["Prometheus"]
        Jaeger["Jaeger (OTel)"]
        SIEM["SIEM / Audit Log"]
        Grafana["Grafana"]
    end

    Web --> LB
    CLI --> LB
    IDE --> LB
    LB --> WAF --> Router
    Router --> MW
    MW --> H_Chat & H_Task & H_WS & H_MCP & H_Skill

    H_Chat --> Orchestrator
    H_Task --> Temporal
    H_WS --> Workspace
    H_MCP --> MCP
    H_Skill --> Skill

    Orchestrator --> Planner
    Orchestrator --> FT & MP & ATR & EE
    Orchestrator --> LLM & RAG & Sandbox & MCP & Tools & Skill
    Orchestrator --> Temporal
    Orchestrator --> PromptBuilder

    PromptBuilder --> Session
    Session --> Summarizer
    Session --> Pruner
    Session --> Workspace

    Indexer --> RepoMap
    Indexer --> RAG

    LLM -.HTTPS.-> OpenAI & Anthropic
    MCP -.stdio/HTTP.-> MCPServers

    Session --> Redis
    Workspace --> FS
    RAG --> Qdrant
    Orchestrator --> PG
    Temporal --> PG

    API --> Prom
    API --> Jaeger
    Orch --> SIEM

    classDef client fill:#e1f5ff,stroke:#0288d1,color:#000
    classDef edge fill:#fff3e0,stroke:#f57c00,color:#000
    classDef api fill:#f3e5f5,stroke:#7b1fa2,color:#000
    classDef orch fill:#fce4ec,stroke:#c2185b,color:#000
    classDef cap fill:#e8f5e9,stroke:#388e3c,color:#000
    classDef ctx fill:#fff9c4,stroke:#f9a825,color:#000
    classDef store fill:#efebe9,stroke:#5d4037,color:#000
    classDef ext fill:#eceff1,stroke:#455a64,color:#000
    classDef obs fill:#e0f2f1,stroke:#00796b,color:#000

    class Web,CLI,IDE client
    class LB,WAF edge
    class Router,MW,H_Chat,H_Task,H_WS,H_MCP,H_Skill api
    class Orchestrator,Planner,Temporal,FT,MP,ATR,EE orch
    class LLM,RAG,Sandbox,MCP,Tools,Skill,Indexer,RepoMap cap
    class Session,Summarizer,Pruner,PromptBuilder,Workspace ctx
    class PG,Redis,Qdrant,FS store
    class OpenAI,Anthropic,MCPServers ext
    class Prom,Jaeger,SIEM,Grafana obs
```

**8 个逻辑层** 自上而下：

| 层 | 职责 | 关键组件 |
|---|---|---|
| Client | 入口点 | Web UI / CLI / IDE |
| Edge | 流量控制 | LB / WAF |
| API | 协议层 | Gin Router + Middleware |
| Orchestration | 大脑 | Orchestrator + Planner + Temporal |
| Capability | 能力块 | LLM / RAG / Sandbox / MCP / Tools / Skill |
| Context & Session | 记忆 | Session / Pruner / PromptBuilder / Workspace |
| Storage | 状态 | PG / Redis / Qdrant / FS |
| Observability | 监控 | Prometheus / Jaeger / SIEM |

---

## 2. 模块依赖关系图

```mermaid
graph LR
    subgraph Infra["底层基础设施（0 依赖）"]
        config["config"]
        models["models"]
        errors["errors"]
        pool["pool"]
        metrics["metrics"]
        tracing["tracing"]
    end

    subgraph Base["基础服务"]
        audit["audit"]
        security["security"]
        auth["auth"]
        store["store"]
    end

    subgraph Engine["业务引擎"]
        llm["llm"]
        rag["rag"]
        sandbox["sandbox"]
        mcp["mcp"]
        tools["tools"]
        skill["skill"]
        indexer["indexer"]
        repomap["repomap"]
    end

    subgraph Context2["上下文层"]
        session["session"]
        context["context"]
        workspace["workspace"]
    end

    subgraph Ctrl["编排层"]
        planner["planner"]
        temporal["temporal"]
        orchestrator["orchestrator"]
    end

    subgraph Entry["入口"]
        api["api"]
        cmd["cmd/agent"]
    end

    config --> Base & Engine & Context2 & Ctrl & Entry
    models --> Base & Engine & Context2 & Ctrl & Entry
    errors --> Base & Engine & Context2 & Ctrl & Entry
    metrics --> Engine & Context2 & Ctrl & Entry
    tracing --> Engine & Ctrl & Entry
    pool --> mcp & sandbox & api

    security --> auth & api & sandbox
    audit --> api & orchestrator & sandbox
    store --> session & orchestrator & audit

    indexer --> rag
    repomap --> indexer
    tools --> skill
    mcp --> skill

    session --> context
    workspace --> session
    context --> orchestrator

    llm --> orchestrator & planner
    rag --> orchestrator
    sandbox --> orchestrator
    skill --> orchestrator
    planner --> orchestrator
    temporal --> orchestrator

    orchestrator --> api
    api --> cmd

    style config fill:#ffcdd2
    style models fill:#ffcdd2
    style errors fill:#ffcdd2
    style orchestrator fill:#c8e6c9
    style api fill:#c8e6c9
```

**原则**：箭头方向 = 依赖方向；**无循环**。

---

## 3. 数据流 ①：普通对话 `/chat` 完整生命周期

```mermaid
sequenceDiagram
    autonumber
    actor U as User
    participant FE as Web UI
    participant G as Gin Router
    participant MW as Middleware Chain
    participant H as Chat Handler
    participant O as Orchestrator
    participant S as Session Manager
    participant P as Pruner
    participant PB as PromptBuilder
    participant R as RAG Engine
    participant Q as Qdrant
    participant L as LLM Router
    participant OAI as OpenAI API
    participant RD as Redis
    participant PG as PostgreSQL
    participant AU as Audit
    participant PM as Prometheus
    participant JG as Jaeger

    U->>FE: 输入: "帮我分析订单表慢查询"
    FE->>G: POST /api/v1/chat<br/>{session_id, message}
    G->>MW: HTTP Request

    rect rgb(240,240,255)
        Note over MW: 中间件链（按序执行）
        MW->>MW: 1. Recovery (panic 捕获)
        MW->>MW: 2. RequestID 注入
        MW->>JG: 3. Start Server Span
        MW->>PM: 4. Metrics 开始计时
        MW->>RD: 5. RateLimit 令牌桶 -1
        MW->>MW: 6. JWT 解析 → User
        MW->>MW: 7. RBAC 检查
    end

    MW->>H: c.Request with ctx

    H->>S: GetHistory(session_id)
    S->>RD: LRANGE session:xxx
    RD-->>S: [msg1, msg2, ..., msgN]

    S->>P: CheckTokenLimit(history)
    alt tokens > threshold
        P->>S: Summarize(old_msgs)
        S->>L: summary prompt
        L-->>S: summary
        S->>RD: UPDATE session (pruned)
    end
    S-->>H: messages

    H->>O: ProcessMessage(ctx, messages, msg)

    rect rgb(230,255,230)
        Note over O,Q: 语义检索阶段
        O->>R: Retrieve(query="订单表慢查询")
        par Dense 路
            R->>L: Embed(query)
            L-->>R: vector[768]
            R->>Q: SearchVector(vector, top_k=20)
            Q-->>R: dense_results
        and Sparse 路
            R->>R: BM25 Score
            R->>Q: SearchPayload(keywords)
            Q-->>R: sparse_results
        end
        R->>R: Fusion + Rerank (本地 CrossEncoder)
        R-->>O: top 5 chunks
    end

    O->>PB: BuildPrompt(sys_prompt, rag, history, tools)
    PB->>PB: 计算 KV Cache 前缀 hash
    PB->>PM: record cache_prefix_hash
    PB-->>O: final_prompt (messages[])

    rect rgb(255,250,230)
        Note over O,OAI: LLM 调用 + 熔断
        O->>L: Chat(ctx, prompt, tools)
        L->>L: 选 primary provider<br/>(检查 CB state)
        alt CB=closed
            L->>OAI: POST /v1/chat/completions
            OAI-->>L: 200 + tokens
            L->>PM: record tokens_used/duration
        else CB=open
            L->>L: Fallback to backup model
            L->>PM: fallback_total +1
        end
        L-->>O: assistant_msg + tool_calls[]
    end

    alt 无 tool_calls
        O->>S: AppendMessage(assistant_msg)
        S->>RD: RPUSH session
        O-->>H: final_response
    else 有 tool_calls
        loop 每个 tool_call
            O->>O: (见流 ③ 沙箱 / 流 ⑤ MCP)
        end
        O->>O: 再次调 LLM (携带 tool results)
    end

    H-->>G: JSON response
    G->>JG: End span (status=200)
    G->>PM: Observe duration
    G->>AU: Log {user, session, tokens, duration}
    AU->>PG: INSERT audit_log
    G-->>FE: 200 OK + content
    FE-->>U: 渲染回复
```

**关键点**：

- **3 个并行**：Dense/Sparse 双路检索可并行；Audit 写入异步；Metrics/Tracing 不阻塞主流程；
- **4 处 IO 优化**：Redis pipelining、Qdrant 批量查询、LLM streaming、Jaeger batch；
- **3 道闸门**：RateLimit（429）、Auth（401）、RBAC（403）；任一失败快速返回。

---

## 4. 数据流 ②：RAG 代码检索（双路召回）

```mermaid
graph LR
    Q[用户查询<br/>'订单慢查询'] --> QN[Query Normalizer<br/>去停用词/提取实体]

    QN --> Dense[Dense 路]
    QN --> Sparse[Sparse 路]

    subgraph DensePath["向量语义检索"]
        Dense --> Embed[Embedder<br/>OpenAI ada / Local TEI]
        Embed --> DVec[Query Vector<br/>768 dim]
        DVec --> DSearch[Qdrant<br/>cosine similarity<br/>top_k=20]
        DSearch --> DFilter[Payload Filter<br/>project='auth-service'<br/>version='v1.2']
        DFilter --> DRes[Dense Results<br/>20 chunks]
    end

    subgraph SparsePath["关键词精确匹配"]
        Sparse --> BM25[BM25 Scorer<br/>tf-idf]
        BM25 --> SSearch[Qdrant Text Index<br/>inverted index]
        SSearch --> SRes[Sparse Results<br/>20 chunks]
    end

    DRes --> Fusion[Reciprocal Rank Fusion<br/>RRF score = Σ 1/(k+rank)]
    SRes --> Fusion

    Fusion --> Merged[Merged Top 40]

    Merged --> Rerank[CrossEncoder Reranker<br/>bge-reranker-v2-m3<br/>本地部署]
    Rerank --> Final[Final Top 5]

    Final --> ContextAssembly[Context Assembly<br/>按依赖关系排序<br/>go imports → struct → func]

    ContextAssembly --> Out[RAG Context<br/>injected into prompt]

    subgraph Ingestion["⚙️ 入库流水线（异步）"]
        Files[代码文件<br/>*.go / *.py / *.md]
        Files --> Parser[tree-sitter<br/>AST Parser]
        Parser --> Chunks[Semantic Chunks<br/>func-level/class-level]
        Chunks --> Enrich[Enrich<br/>· comment<br/>· signature<br/>· dependencies]
        Enrich --> Embedder2[Embedder]
        Embedder2 --> Indexer[Indexer]
        Indexer --> Qdrant_W[(Qdrant<br/>upsert)]
    end

    Qdrant_W -.-> DSearch
    Qdrant_W -.-> SSearch

    style Dense fill:#e3f2fd
    style Sparse fill:#fff3e0
    style Fusion fill:#f3e5f5
    style Rerank fill:#e8f5e9
    style Ingestion fill:#fafafa,stroke-dasharray:5 5
```

**三层召回金字塔**：

- **召回**（Recall）：双路 → 40 个候选；高召回率；
- **排序**（Ranking）：RRF 融合 → Top 40 粗排；
- **重排**（Rerank）：CrossEncoder → Top 5 精排；精度 > 召回；

---

## 5. 数据流 ③：沙箱代码执行（SSE 流式）

```mermaid
sequenceDiagram
    autonumber
    participant O as Orchestrator
    participant SB as Sandbox Manager
    participant DC as Docker Client
    participant CT as Ephemeral Container
    participant FE as Web UI (SSE)
    participant PM as Prometheus
    participant AU as Audit

    O->>SB: Execute(lang=python, code="df.describe()")

    rect rgb(255,240,240)
        Note over SB,DC: 安全沙箱创建
        SB->>SB: 1. 校验 language (白名单)
        SB->>SB: 2. 打 tar 归档 (code + deps)
        SB->>DC: 3. ContainerCreate
        Note right of DC: Config:<br/>Image: python-runner:3.11<br/>NetworkMode: none<br/>ReadonlyRootfs: true<br/>User: 1000:1000<br/>CapDrop: [ALL]<br/>SeccompProfile: default
        DC-->>SB: container_id
        SB->>DC: 4. ContainerUpdate<br/>Memory=512MB<br/>CPUQuota=100000 (1 core)<br/>PidsLimit=64
        SB->>DC: 5. CopyToContainer(/app, tar)
        SB->>DC: 6. ContainerStart
    end

    SB->>AU: Log sandbox_start

    SB->>DC: ContainerAttach (stdout+stderr)
    DC-->>SB: io.Reader (multiplexed stream)

    par 主 goroutine: 读取输出
        loop 逐行读
            DC->>SB: stdout line
            SB->>SB: pool.SmallBytePool.Get()
            SB->>FE: SSE: data: {type:"stdout", line}
            FE->>FE: append to console
            SB->>SB: pool.Put()
        end
    and 看门狗 goroutine
        loop 每 500ms
            SB->>DC: ContainerStats
            DC-->>SB: {mem_rss, cpu%}
            alt mem > limit
                SB->>DC: ContainerKill (OOM 预防)
                SB->>PM: sandbox_oom +1
            else timeout reached
                SB->>DC: ContainerStop
                SB->>PM: sandbox_timeout +1
            end
        end
    end

    DC->>SB: EOF + exit_code
    SB->>DC: ContainerWait
    DC-->>SB: {StatusCode: 0}

    SB->>DC: ContainerRemove (-f)
    Note right of DC: "阅后即焚"<br/>容器立即销毁

    SB->>PM: record execution_duration<br/>+ execution_total{status}
    SB->>AU: Log sandbox_end
    SB->>FE: SSE: data: {type:"done", code:0}
    FE->>FE: close EventSource
    SB-->>O: ExecutionResult{stdout, stderr, exit}
```

**5 层防御**：

1. **镜像隔离**：白名单基础镜像
2. **网络隔离**：`NetworkMode: none`
3. **文件系统隔离**：`ReadonlyRootfs`
4. **资源隔离**：cgroups limits
5. **能力隔离**：drop all caps + seccomp

---

## 6. 数据流 ④：HITL 人工介入（Temporal 挂起-唤醒）

```mermaid
sequenceDiagram
    autonumber
    actor U as User
    participant O as Orchestrator
    participant TW as Temporal Worker
    participant TS as Temporal Server
    participant PG as PostgreSQL
    participant API as /tasks API
    participant FE as Web UI

    Note over O: 任务: 执行 "kubectl apply prod.yaml"
    O->>O: 命中敏感规则 (kubectl/drop db/...)
    O->>TW: StartWorkflow(DeployFlow, params)
    TW->>TS: WorkflowExecutionStarted
    TS->>PG: persist workflow_state

    activate TW
    TW->>TW: Activity 1: validate manifest ✅
    TW->>TW: Activity 2: diff prod cluster ✅

    rect rgb(255,230,230)
        Note over TW,TS: 🛑 挂起等待人工确认
        TW->>TW: workflow.Await(<br/>  "approval_signal",<br/>  timeout=24h)
        TW->>TS: 状态: ApprovalPending
        TS->>PG: state=suspended<br/>diff + risk_summary 持久化
        deactivate TW
    end

    Note over TS: Worker 不占 goroutine<br/>只持久化在 DB

    TS->>API: Webhook: task_pending(task_id)
    API->>FE: WebSocket push<br/>{event:"approval_required", task}
    FE->>U: 🔔 弹窗: "部署到 prod<br/>变更: 3 个 deploy ....<br/>风险: HIGH"
    U->>FE: 点击 [授权继续]
    FE->>API: POST /tasks/{id}/approve<br/>{decision:"approve",comment:"ok"}
    API->>API: RBAC check (admin role)
    API->>TS: SignalWorkflow("approval_signal", {ok:true,by:user})

    rect rgb(230,255,230)
        Note over TS,TW: 工作流唤醒恢复
        TS->>PG: load workflow_state
        activate TW
        TS->>TW: dispatch signal
        TW->>TW: Await 返回 {ok:true}
        TW->>TW: Activity 3: execute kubectl ✅
        TW->>TW: Activity 4: verify rollout ✅
        TW->>TW: WorkflowCompleted
        TS->>PG: state=completed
        deactivate TW
    end

    TS->>API: Webhook: task_completed
    API->>FE: WebSocket push<br/>{event:"task_done", result}
    FE->>U: ✅ 显示执行日志

    alt 用户拒绝
        U->>FE: [拒绝]
        FE->>API: POST /approve {decision:"reject"}
        API->>TS: Signal({ok:false})
        TW->>TW: Await 返回 {ok:false}
        TW->>TW: 记录拒绝 + 终止 workflow
        TS->>PG: state=rejected
    else 24h 超时
        TW->>TW: Await 超时
        TW->>TW: 自动拒绝 + 告警
    end
```

**关键设计点**：

- **Temporal Signal 是唤醒**，不是轮询 — 挂起时 0 资源占用；
- **workflow state 全持久化** — 服务重启、节点迁移都不丢；
- **超时兜底** — 防止遗忘审批导致 goroutine 泄漏；
- **审计全留痕** — 每次 approval/reject/timeout 都进 SIEM。

---

## 7. 数据流 ⑤：MCP 动态工具调用

```mermaid
sequenceDiagram
    autonumber
    participant Cfg as config.yaml
    participant MC as MCP Client Pool
    participant MS as MCP Server<br/>(e.g., GitHub MCP)
    participant SR as Skill Registry
    participant O as Orchestrator
    participant L as LLM

    rect rgb(240,240,255)
        Note over Cfg,SR: 启动时: 连接 + 发现
        Cfg->>MC: 读取 mcp_servers[]
        loop 每个 server
            MC->>MS: spawn / HTTP connect
            MC->>MS: JSON-RPC: initialize {protocol_version}
            MS-->>MC: {capabilities, server_info}
            MC->>MS: JSON-RPC: tools/list
            MS-->>MC: [{name, description, input_schema}, ...]
            MC->>SR: RegisterTools(server_name, tools)
        end
        SR->>SR: 建内存 Map<br/>tool_fqn → (server, schema)
    end

    Note over MC: 常驻: 心跳保活 + 断线重连<br/>(reconnect.go)

    rect rgb(230,255,230)
        Note over O,L: 运行时: 调用
        O->>SR: ListAvailableTools()
        SR-->>O: [local_tools..., mcp_tools...]
        O->>L: Chat + tools=[...]
        L-->>O: tool_call{name:"github.create_issue",<br/>args:{title, body}}

        O->>SR: LookupTool("github.create_issue")
        SR-->>O: {server:"github-mcp", schema}

        O->>O: Validate args against schema
        O->>MC: Call(server, "tools/call", params)

        MC->>MC: pool.Get RPCRequest
        MC->>MC: id = atomic++
        MC->>MS: JSON-RPC: tools/call<br/>{id, method, params}
        MC->>MC: 注册 pending[id] = chan
        MS->>MS: 执行 (连 GitHub API)
        MS-->>MC: JSON-RPC response {id, result}
        MC->>MC: pending[id] <- result
        MC->>MC: pool.Put
        MC-->>O: {issue_url, number}

        O->>L: Append tool_result
        L-->>O: final answer with issue link
    end

    alt MCP Server 崩溃
        MS->>MC: EOF / disconnect
        MC->>MC: 标记 offline
        MC->>SR: Disable(server_name)
        par
            MC->>MC: exp backoff 重连
        and
            O->>SR: 查询工具时<br/>该工具标记 ⚠️offline
        end
        MC->>MS: reconnect success
        MC->>SR: Enable(server_name)
    end
```

**4 个关键机制**：

- **启动时批量发现** → 构建工具内存表；
- **运行时零开销查询** → O(1) map lookup；
- **JSON-RPC 2.0 pending 表** → 并发请求复用同一 stdio 连接；
- **断线自愈** → 不影响其他 MCP server。

---

## 8. 数据流 ⑥：Session 上下文管理与压缩

```mermaid
stateDiagram-v2
    [*] --> NewSession: 用户首次对话
    NewSession --> Active: CreateSession(user_id)

    Active --> MessageAdded: AppendMessage

    MessageAdded --> TokenCheck: compute tokens

    TokenCheck --> Active: tokens < threshold (4000)
    TokenCheck --> Pruning: tokens >= threshold

    state Pruning {
        [*] --> SelectOldMsgs: 选最老 N 条<br/>保留最近 M 条
        SelectOldMsgs --> CallSummarizer
        CallSummarizer --> ReplaceWithSummary: summary (~300 tokens)
        ReplaceWithSummary --> RecordMetrics: context_compression_total +1
        RecordMetrics --> [*]
    }

    Pruning --> Active: 压缩后 tokens < threshold

    Active --> ColdArchive: 长时间无活动 (>7d)
    ColdArchive --> ColdStore: RPOPLPUSH hot → cold
    ColdStore --> [*]: session_cold_archive_total +1

    Active --> HotHit: 用户再访问
    HotHit --> Active

    ColdStore --> Revived: 用户再访问
    Revived --> LoadFromPG: SELECT messages FROM cold
    LoadFromPG --> Active

    note right of Pruning
      滑动窗口算法:
      - 保留 System prompt
      - 保留最近 10 条
      - 中间部分 LLM 摘要
      - tokens: 4000+ → 1500
    end note
```

### Session 数据结构

```mermaid
graph LR
    subgraph Hot["🔥 热存储 (Redis)"]
        H1["session:{id}:meta<br/>HSET user_id/created/ttl"]
        H2["session:{id}:messages<br/>LIST 近 50 条"]
        H3["session:{id}:summary<br/>STRING 历史摘要"]
    end

    subgraph Cold["❄️ 冷存储 (PostgreSQL)"]
        C1["sessions 表<br/>id, user, created, archived"]
        C2["messages 表<br/>session_id, role, content, ts"]
    end

    Hot -.archive (7d idle).-> Cold
    Cold -.restore (on access).-> Hot
```

---

## 9. 数据流 ⑦：LLM 路由 + 熔断 + 降级

```mermaid
graph TB
    Start[orchestrator.Chat<br/>请求] --> Router[LLM Router]

    Router --> Choose{选择 Provider}

    Choose -->|default| Primary[Primary: OpenAI GPT-4o]
    Choose -->|user override| Alt[User-selected model]
    Choose -->|cost-aware| Cheap[GPT-4o-mini]

    Primary --> CB1{Circuit Breaker<br/>state?}

    CB1 -->|closed| Call1[HTTPS POST<br/>api.openai.com]
    CB1 -->|half-open| Limited[允许 1 个试探请求]
    CB1 -->|open| Skip[跳过 Primary]

    Call1 --> Result1{Response?}

    Result1 -->|2xx| Success[返回结果<br/>记录 tokens/latency]
    Result1 -->|5xx/timeout| Failure1[计数 +1]
    Result1 -->|429 rate limit| Retry{重试?}

    Failure1 --> CheckCB{失败率 > 50%<br/>in 30s?}
    CheckCB -->|yes| TripCB[CB → open<br/>冷却 60s<br/>metrics: circuit_breaker_state=2]
    CheckCB -->|no| Retry

    Retry -->|exp backoff<br/>max 3 次| Call1
    Retry -->|达上限| Skip

    TripCB --> Skip
    Skip --> Fallback[降级路径]

    Limited --> Call1
    Limited -.成功.-> CloseCB[CB → closed]
    Limited -.失败.-> TripCB2[重新 open]

    Fallback --> SecCheck{Secondary<br/>available?}
    SecCheck -->|Claude| Call2[HTTPS<br/>api.anthropic.com]
    SecCheck -->|local| CallLocal[Local LLM<br/>Ollama/llama.cpp]
    SecCheck -->|all down| Error[返回 503<br/>LLM_FAILURE]

    Call2 --> Result2{Response?}
    CallLocal --> Result2
    Result2 -->|2xx| Success
    Result2 -->|fail| Error

    Success --> Metrics1[Observe:<br/>llm_request_total<br/>llm_tokens_used_total<br/>llm_request_duration]
    TripCB --> Metrics2[llm_circuit_breaker_state]
    Fallback --> Metrics3[llm_fallback_total +1]

    Metrics1 --> Return[返回 orchestrator]
    Error --> Return
    Metrics2 -.-> Metrics1
    Metrics3 -.-> Metrics1

    style Primary fill:#c8e6c9
    style Fallback fill:#fff9c4
    style Error fill:#ffcdd2
    style TripCB fill:#ff8a65
```

**三状态 Circuit Breaker**：

| State | 行为 | 条件进入 |
|---|---|---|
| **Closed** | 正常透传 | 成功率 ≥ 50% |
| **Open** | 全部拒绝（走 fallback） | 失败率 > 50% in 30s |
| **Half-Open** | 放 1 个试探 | open 冷却 60s 后 |

---

## 10. 状态机总览

### Task（Orchestrator 视角）

```mermaid
stateDiagram-v2
    [*] --> Created
    Created --> Planning: orchestrator.Run()
    Planning --> Executing: plan generated
    Planning --> Failed: plan error

    Executing --> ToolCalling: need tool
    ToolCalling --> Executing: tool result
    ToolCalling --> ApprovalPending: sensitive command

    ApprovalPending --> Executing: user approve (Signal)
    ApprovalPending --> Rejected: user reject
    ApprovalPending --> Rejected: timeout 24h

    Executing --> Completed: success
    Executing --> Failed: error 3 次后
    Executing --> PartiallyCompleted: 部分 step 失败

    Completed --> [*]
    Failed --> [*]
    Rejected --> [*]
    PartiallyCompleted --> [*]
```

### Workflow（Temporal 视角）

```mermaid
stateDiagram-v2
    [*] --> WorkflowStarted
    WorkflowStarted --> ActivityRunning
    ActivityRunning --> ActivityCompleted: success
    ActivityRunning --> ActivityRetrying: transient error
    ActivityRetrying --> ActivityRunning: exp backoff
    ActivityRetrying --> ActivityFailed: max attempts
    ActivityCompleted --> WorkflowCompleted
    ActivityCompleted --> WaitingForSignal: Await
    WaitingForSignal --> ActivityRunning: Signal received
    WaitingForSignal --> WorkflowTimeout: timeout
    ActivityFailed --> WorkflowFailed
    WorkflowCompleted --> [*]
    WorkflowFailed --> [*]
    WorkflowTimeout --> [*]
```

### Circuit Breaker（LLM 视角）

```mermaid
stateDiagram-v2
    [*] --> Closed: 启动
    Closed --> Open: 失败率 > 50% in 30s
    Open --> HalfOpen: 冷却 60s 后
    HalfOpen --> Closed: 1 次试探成功
    HalfOpen --> Open: 试探失败
    Closed --> Closed: 正常调用
    Open --> Open: 拒绝 (走 fallback)
```

---

## 11. 部署拓扑图（K8s 生产）

```mermaid
graph TB
    subgraph External["🌐 Internet"]
        User[Users]
        CDN[CDN]
    end

    subgraph Ingress["🚪 Ingress Layer"]
        ING[Ingress Controller<br/>NGINX / Istio Gateway]
        TLS[cert-manager<br/>Let's Encrypt]
    end

    subgraph K8s["☸️ Kubernetes Cluster"]
        subgraph Apps["Application Tier"]
            direction LR
            subgraph NSA["ns: code-agent"]
                D1[Deployment<br/>code-agent × 3 replica]
                SVC1[Service<br/>ClusterIP :8080]
                HPA[HPA<br/>CPU 70% / Mem 80%<br/>min 2, max 10]
                PDB[PodDisruptionBudget<br/>minAvailable: 2]
            end
        end

        subgraph DataStores["Data Tier"]
            direction LR
            subgraph NSR["ns: infra"]
                Redis_Cluster[Redis Cluster<br/>Operator]
                PG_HA[PostgreSQL HA<br/>CloudNativePG<br/>1 primary + 2 replica]
                Qdrant_STS[Qdrant<br/>StatefulSet x 3]
            end
        end

        subgraph Workflow["Workflow Tier"]
            direction LR
            Temporal_STS[Temporal Cluster<br/>frontend + history + matching]
            Temporal_UI[Temporal UI]
        end

        subgraph Observ["Observability Tier"]
            direction LR
            PromOp[Prometheus Operator]
            Grafana_D[Grafana]
            Jaeger_D[Jaeger]
            Loki[Loki + Promtail]
            AlertM[AlertManager]
        end

        subgraph Security["Security & Policy"]
            direction LR
            NetworkPolicy[NetworkPolicies]
            OPA[OPA Gatekeeper]
            Sealed[sealed-secrets]
        end
    end

    subgraph ExtDeps["🌍 External Dependencies"]
        OpenAI_Ext[OpenAI API]
        Claude_Ext[Anthropic API]
        GitHub_Ext[GitHub API]
        Sentry_Ext[Sentry]
    end

    User -->|HTTPS| CDN
    CDN --> ING
    TLS -.cert.-> ING
    ING --> SVC1

    SVC1 --> D1

    D1 -->|tcp 6379| Redis_Cluster
    D1 -->|tcp 5432| PG_HA
    D1 -->|grpc 6334| Qdrant_STS
    D1 -->|grpc 7233| Temporal_STS
    D1 -->|docker sock<br/>via DinD / sysbox| Sandbox

    subgraph Sandbox["Sandbox Runtime"]
        DinD[DinD Pod<br/>privileged]
    end

    HPA -.scale.-> D1
    PDB -.protect.-> D1

    D1 -->|HTTPS| OpenAI_Ext
    D1 -->|HTTPS| Claude_Ext
    D1 -->|JSON-RPC| GitHub_Ext
    D1 -->|errors| Sentry_Ext

    D1 -.metrics.-> PromOp
    D1 -.traces OTLP.-> Jaeger_D
    D1 -.logs.-> Loki
    PromOp --> Grafana_D
    PromOp --> AlertM

    NetworkPolicy -.限制.-> NSA
    OPA -.校验.-> D1
    Sealed -.inject secrets.-> D1

    style K8s fill:#e8f5e9
    style Apps fill:#fff9c4
    style DataStores fill:#efebe9
    style Workflow fill:#fce4ec
    style Observ fill:#e0f2f1
    style Security fill:#ffccbc
```

### 网络流量分类

```mermaid
graph LR
    subgraph Ingress["入站 Ingress"]
        T1[用户 HTTPS]
    end

    subgraph Internal["集群内 E/W"]
        T2[Agent ↔ Redis]
        T3[Agent ↔ Qdrant]
        T4[Agent ↔ PG]
        T5[Agent ↔ Temporal]
    end

    subgraph Egress["出站 Egress"]
        T6[Agent → OpenAI]
        T7[Agent → MCP Servers]
        T8[Agent → Sentry]
    end

    subgraph Denied["⛔ NetworkPolicy 拒绝"]
        T9[Agent → Cloud Metadata<br/>169.254.169.254]
        T10[Agent → Private IPs<br/>10.0.0.0/8]
        T11[Sandbox → 任何网络]
    end

    style Denied fill:#ffcdd2
```

---

## 附：架构图索引

| 图号 | 标题 | 用途 |
|---|---|---|
| #1 | 分层系统架构 | 给 CTO/新人看全貌 |
| #2 | 模块依赖 | Go 包设计审查 |
| #3 | /chat 生命周期 | 排障 & 性能分析 |
| #4 | RAG 双路召回 | 检索质量优化 |
| #5 | 沙箱执行 SSE | 安全审计 |
| #6 | HITL 挂起-唤醒 | 合规场景 |
| #7 | MCP 工具调用 | 接入新外部系统 |
| #8 | Session 压缩 | 成本优化 |
| #9 | LLM 熔断降级 | 稳定性分析 |
| #10 | 状态机总览 | 协议文档 |
| #11 | K8s 部署拓扑 | 运维交付 |

---

**说明**：所有 Mermaid 图兼容：

- ✅ GitHub / GitLab README 直接渲染
- ✅ VS Code Mermaid Preview
- ✅ 语雀 / Notion / Confluence
- ✅ `mermaid-cli` 导出 PNG/SVG：
  ```bash
  npx -p @mermaid-js/mermaid-cli mmdc -i ARCHITECTURE_DIAGRAM.md -o out/
  ```
