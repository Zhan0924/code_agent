# 21 · 架构回顾、设计哲学与 Onboarding

> 收官篇。本文不再讲新模块，而是做三件事：  
> (1) 把前 20 篇的**模块依赖** 拉成一张全局图；  
> (2) 提炼贯穿全项目的 **10 条核心设计哲学**；  
> (3) 给新人一个 **Onboarding Checklist**（照着做即可上手）。

---

## 1. 全景依赖图（21 个模块一张图）

```
                            ┌───────────────────────────────────┐
                            │      Client (Browser / SDK / CLI)  │
                            └──────────┬────────────────────────┘
                                       │  HTTPS / WS / SSE
                                       ▼
    ┌──────────────────────── (17) API Layer (Gin) ────────────────────────┐
    │  Middleware Chain: recovery → requestID → tracing → metrics          │
    │                   → rate limit → auth → RBAC → handler               │
    │  Routes: /chat (3 modes) · /tasks/:id/approve · /workspaces · /mcp   │
    └──────┬───────────────────────────────────────────────────────────────┘
           │                                   │
           ▼                                   ▼
    ┌─────────────────────────────┐   ┌────────────────────────┐
    │ (18) Auth / Security / Audit │   │ (19) Observability     │
    │   - JWT / APIKey / RBAC      │   │   - Metrics (Prom)     │
    │   - HMAC / Egress Policy     │   │   - Tracing (OTel)     │
    │   - Audit Logger (SIEM)      │   │   - Errors / Pool      │
    └──────────────────────────────┘   └────────────────────────┘
           │
           ▼
    ┌─────────────────────── (09) Orchestrator ────────────────────────┐
    │  核心循环: Plan → Retrieve → ToolCall → Evaluate                  │
    │   - FailureTracker / MessagePruner / AutoTestRunner               │
    │   - PlannerBridge / EditEngine / ProjectRules                     │
    └────┬────────┬────────┬────────┬────────┬────────┬────────┬───────┘
         │        │        │        │        │        │        │
         ▼        ▼        ▼        ▼        ▼        ▼        ▼
    ┌────────┐┌───────┐┌───────┐┌────────┐┌───────┐┌──────┐┌──────────┐
    │(10)    ││(03)   ││(04)   ││(05)    ││(06)   ││(07)  ││(11)      │
    │Planner ││ LLM   ││ RAG   ││Sandbox ││ MCP   ││Tools ││Temporal  │
    │ DAG    ││Router ││Qdrant ││Docker  ││JSON-  ││Registry│Workflow │
    │        ││CB     ││AST    ││ cgroups││ RPC  ││      ││ HITL     │
    └────┬───┘└───┬───┘└───┬───┘└────┬───┘└───┬───┘└──┬───┘└────┬─────┘
         │        │        │        │        │        │        │
         │        │        ▼        │        ▼        ▼        │
         │        │   ┌────────┐    │   ┌────────┐           │
         │        │   │(15)    │    │   │(08)    │           │
         │        │   │Indexer │    │   │ Skill  │           │
         │        │   │RepoMap │    │   │Registry│           │
         │        │   └────────┘    │   └────────┘           │
         │        │                 │                         │
         └────────┴────────┬────────┴─────────────────────────┘
                           ▼
            ┌─────────── (13) Context ───────────┐
            │  Prompt Builder · KV Cache ·        │
            │  Progressive Pruner                 │
            └────────┬────────────────────────────┘
                     │
                     ▼
            ┌─────────── (12) Session ───────────┐
            │  SlidingWindow · ColdArchive ·     │
            │  Summarizer                        │
            └────────┬────────────────────────────┘
                     │
                     ▼
    ┌──────────── (14) Workspace ────────────┐
    │  Workdir + Permission Boundary         │
    └────────┬────────────────────────────────┘
             │
             ▼
    ┌──────────────── (16) Store ────────────────┐
    │  PostgreSQL (config/audit/tasks)            │
    │  Redis (session/cache/revocation)           │
    │  Qdrant (vectors)                           │
    └─────────────────────────────────────────────┘

      ┌────────────────────────────────┐
      │      (01) Config / (02) Models │  ← 被所有模块使用
      └────────────────────────────────┘

      ┌────────────────────────────────┐
      │        (20) Deploy / CI        │  ← 把上面所有打包、部署
      └────────────────────────────────┘
```

**图的三个层级**：

| 层 | 模块 | 共性 |
|---|---|---|
| **底层基础设施** | 01 config · 02 models · 19 observability · 18 auth/security · 20 deploy | 无业务逻辑；被全系统引用 |
| **中层业务引擎** | 03 llm · 04 rag · 05 sandbox · 06 mcp · 07 tools · 08 skill · 10 planner · 11 temporal · 15 indexer · 12 session · 13 context · 14 workspace · 16 store | 单一职责的"能力块" |
| **顶层组合编排** | 09 orchestrator · 17 api · 21 conclusion | 把能力块组合成业务闭环 |

---

## 2. 10 条核心设计哲学

这 20 个模块数十万字文档，浓缩成 10 条可复用的设计原则：

### ① **分层依赖倒置：底层 0 依赖，顶层组合一切**

```
config / models / errors / pool → 无出度
业务引擎 → 仅依赖基础设施 + 对等业务块
orchestrator → 依赖一切；API → 依赖 orchestrator
```

保证任何单模块都能独立测试，且没有循环依赖。

### ② **"配置即代码" + "零配置也能跑"**

- 每个模块都有 `DefaultConfig()`；
- 所有 default 值**生产安全**（默认最严 egress、默认开验签、默认 readOnly）；
- 配置分层 `yaml → env → flag`；
- `mustGenerateSecret()` 式默认：忘配也不会用弱 key。

→ **新人 5 分钟能 `make run`；老手精调一年不嫌粒度细**。

### ③ **确定性状态机 > 自由发挥 LLM**

- 核心循环（orchestrator ReAct）是**代码控制流**，不是 LLM prompt；
- Planner 是**DAG** 而非 LLM 连续推理；
- HITL 通过 Temporal Signal 唤醒 —— 整个系统状态机可回溯。

→ LLM 只负责"决策 + 生成"，不负责"调度"。

### ④ **安全即默认（Security-by-Default）**

| 层面 | 默认 |
|---|---|
| 网络 | Egress deny-all + 屏蔽 metadata IP |
| 执行 | Sandbox `NetworkMode=none` + cgroups |
| 认证 | JWT/HMAC/APIKey 三轨 |
| 审计 | 所有 approval / sandbox / mcp 事件落 SIEM |
| 密钥 | SHA256 hash 存储；不在日志出现 |
| RBAC | 默认拒绝，显式 allow |

→ 漏配一项比漏配不等于"有洞"。

### ⑤ **软降级 > 硬依赖**

- LLM down → circuit breaker open → fallback 模型；
- Qdrant down → RAG 返回空，chat 仍工作；
- Jaeger down → tracing 软关，不 panic；
- Docker down → sandbox 禁用，warn；
- Redis down → 内存 degrade store；
- 外部 MCP down → skill 标记 offline，不拉挂；

→ "任一组件挂不意味着 Agent 挂"。

### ⑥ **流式优先（Streaming First）**

- Sandbox stdout 逐行推送（非 wait full）；
- LLM chat SSE 流式；
- 前端 UI 渐进渲染；
- 日志逐条发 Loki（非 batch）；
- Metrics push 非 pull（Push Gateway 场景）；

→ 长任务 UX 好；小任务也不浪费时延。

### ⑦ **可观测性 = Metrics × Tracing × Audit**

三者不是替代，是互补：

| 维度 | 做什么 |
|---|---|
| Metrics | **告警** + SLO + HPA |
| Tracing | **排障**（单请求 waterfall） |
| Audit | **合规**（谁、何时、干了啥） |

贯穿三者的**关联 ID** = `request_id + trace_id`。

### ⑧ **接口小而稳定，实现可替换**

- `llm.Provider` 一个 interface，OpenAI / Claude / 本地都实现；
- `skill.Skill` 一个 interface，MCP / Builtin / Script 都实现；
- `store.Store` 一个 interface，PG / Redis / 内存都实现；
- `embedder.Embedder` interface，OpenAI / 本地 TEI 都实现；

→ **Mock 测试是天然的**；**未来替换供应商无惧**。

### ⑨ **池化热点 + 限额兜底**

热点分配用 `sync.Pool`；但 Pool 有上限（超大对象丢弃）；  
同理：cache 有 TTL；session 有滑动窗口；msg history 有 pruner；token 有硬 cap；  
Temporal workflow 有 retry policy + exp backoff；

→ **系统永远不会"慢慢撑爆"**。

### ⑩ **文档与代码同构**

- 每个 `internal/<module>/` 配一篇 `docs/architecture/<nn>_<module>.md`；
- 文档里引用代码**文件 + 行号**，避免"对不上"；
- 设计权衡表是必写项（**"为什么不这么做"** 比"做了什么"更有价值）；
- 后续演进是 checklist（下个 sprint 抄来当 TODO）。

→ **3 个月后回来改代码，文档还准**。

---

## 3. 关键演进路线图（top-5）

汇总前 20 篇每篇的"后续演进"，选**最高优先级 5 项**：

| # | 演进项 | 收益 | 难度 |
|---|---|---|---|
| 1 | **K8s `PodSecurityContext` + `NetworkPolicy` + `PodDisruptionBudget` 完整化** | 生产级安全 + 滚动升级稳定 | 中 |
| 2 | **Helm Chart 打包 + ArgoCD GitOps** | 多环境一致部署；回滚秒级 | 中 |
| 3 | **Prompt KV 缓存全链路打通 + LLM KV cache 显式利用** | LLM 成本 -30% ~ -50% | 高 |
| 4 | **Observability exemplars** (metrics → traceID 一键跳转) | 排障速度 10× | 低 |
| 5 | **Sysbox / gVisor 替代 DinD** | 沙箱安全性阶跃提升 | 高 |

按这个顺序推进，6 个月可把系统从"Demo 可用"推到"真正生产级"。

---

## 4. 新人 Onboarding Checklist

### 第 1 天 · 跑起来

- [ ] `git clone` + `go version`（≥ 1.22）
- [ ] 阅读 `README.md` 5 分钟（项目愿景）
- [ ] 阅读 `docs/architecture/00_overview.md` 15 分钟（系统全貌）
- [ ] `make docker-up` 启动全栈 Compose
- [ ] 访问 `http://localhost:8080/healthz` 看到 `{"status":"ok"}`
- [ ] 访问 `http://localhost:16686`（Jaeger）、`http://localhost:8088`（Temporal UI）
- [ ] `curl -X POST http://localhost:8080/api/v1/chat -d '{"message":"hello"}'` 跑通最小对话

### 第 2-3 天 · 读架构

- [ ] 按 `00_overview` → `09_orchestrator` → `17_api` 三篇主线建立全局观；
- [ ] 选 1 个自己感兴趣的模块（推荐 `04_rag` 或 `11_temporal`）精读；
- [ ] 打开对应源码对照文档；划一次断点跑单测（`dlv test -run TestXxx`）。

### 第 4-5 天 · 修小 Bug

- [ ] 在 GitHub Issues 找 `good-first-issue` 标签的；
- [ ] 改代码 → `make lint` → `make test-short` 全绿 → PR；
- [ ] 学习 CI 日志怎么看（`.github/workflows/ci.yml` 4 个 job）。

### 第 2 周 · 加一个小 feature

推荐题目（从易到难）：

1. **给 `/healthz` 加一个字段**：返回 Redis/PG/Qdrant 各自是否连通；
2. **新加一个 builtin tool**：`get_current_time` —— 体会 Tool Registry 流程；
3. **新加一条 metric**：统计 workspace 切换次数；
4. **支持一个新的 LLM provider**：按 `llm.Provider` 接口实现；
5. **写一个新的 MCP server**（Python/TS），启动后跑 `/mcp/servers` 发现它；
6. **给 Sandbox 加一种新语言**（Rust）：改 tar 打包 + 镜像预置。

### 第 3-4 周 · 参与核心

- [ ] 拿到一个 orchestrator/planner 相关 issue；
- [ ] 写设计文档（遵循 `XX_yourmodule.md` 格式）；
- [ ] 在架构评审会上过一遍；
- [ ] 实现 + 文档 + 测试；
- [ ] Merge + 复盘。

### 毕业标准

- [ ] 能独立在 production K8s 上排查一个 P0；
- [ ] 能对新人讲清楚 "HITL 为什么用 Temporal 而不用 DB 轮询"；
- [ ] 至少贡献一篇 `docs/architecture/` 文档；
- [ ] 能说出 10 条设计哲学中的 5 条并举项目内例子。

---

## 5. 结语

这是一个**典型的 "可扩展 Agent 平台" 参考架构**：

- 做过 "演示 Demo" 的团队 → 抄 01-07，能跑起个 MVP；
- 做过 "垂直 Agent 产品" 的团队 → 抄 09-13，能搭起 ReAct 循环 + HITL；
- 做过 "多租户 SaaS" 的团队 → 抄 14-18，能上线给真实用户；
- 做过 "生产级 AI 平台" 的团队 → 全抄，能扛住大规模流量与合规审计。

**架构的终极目标从来不是"最牛"，而是**：

> **让对的人，在对的时间，用对的工具，做对的事。**

每一条 middleware、每一个 interface、每一张 metric 图，最终都是为了回到这句话。

—— 全文完

---

## 附：20 篇文档速查表

| # | 文件 | 核心主题 |
|---|---|---|
| 00 | overview | 系统全景 + 愿景 |
| 01 | config | Viper 配置 + 验证 + 环境分层 |
| 02 | models | 共享领域模型（Message/Task/Event） |
| 03 | llm | Provider 抽象 + 熔断 + 路由 + 回退 |
| 04 | rag | AST 切分 + Qdrant + BM25+Dense 双路 + Rerank |
| 05 | sandbox | Docker 隔离 + cgroups + 流式 I/O |
| 06 | mcp | JSON-RPC 2.0 Client + 重连 + Tool 缓存 |
| 07 | tools | Tool Registry + 并发安全 + Schema 组装 |
| 08 | skill | Skill 聚合层：Builtin + MCP + Script |
| 09 | orchestrator | ReAct 循环 + Failure + Pruner + AutoTest |
| 10 | planner | 任务 DAG + Step 优化 + Bridge |
| 11 | temporal | Workflow + Signal + HITL + 可恢复 |
| 12 | session | 滑动窗口 + 冷归档 + Summarizer |
| 13 | context | Prompt Builder + KV Cache + Pruner |
| 14 | workspace | 工作区隔离 + 权限边界 |
| 15 | indexer+repomap | 仓库索引 + 文件监听 + RepoMap 生成 |
| 16 | store | PG（config/audit）+ Redis（session/revocation） |
| 17 | api | Gin + 中间件栈 + SSE/WS + DI 启动 |
| 18 | auth_security | JWT/APIKey/HMAC/Egress/Audit |
| 19 | observability | Metrics + Tracing + Errors + Pool |
| 20 | deploy | Dockerfile / Compose / K8s / CI / Makefile |
| 21 | conclusion | 全景图 + 设计哲学 + Onboarding |
| **22** | **recent_improvements** | **近期修复汇总**（2026-05 周期）|

---

## 附 B：演进路线图（按优先级）

本路线图是对各篇"后续演进"章节的统一汇总。**P 标优先级、S 标估算工作量**。

### 一、安全纵深补强

- **P0 S3** Egress ACL 真正接线到 LLM / MCP / rerank 客户端（类库已有，见 [18 §6.4](18_auth_security.md)）
- **P0 S2** Redis 限流接入 `router.setupMiddleware`（类库已有，见 `auth/redis_ratelimit.go`）
- **P0 S1** 默认所有出站 HTTP 经 egress validator（white-list 配置化）
- **P1 S5** 沙箱加 user namespace 隔离（gVisor / Kata）
- **P1 S3** 审计日志写 Kafka / 异地归档（现在仅本地）
- **P1 S3** API Key 支持 rotation / expiry

### 二、可靠性升级

- **P0 S2** LLM streaming 路径接入 SharedBreaker（当前只 non-stream 接入）
- **P0 S2** HTTP shutdown 时 drain detached-context goroutines（见 [17 §Q3](17_api.md)）
- **P1 S5** LLM retryWithBackoff 加 jitter + 加到主路径
- **P1 S5** Temporal worker 弹性（Dial 失败后定时重试）
- **P2 S8** 跨区域多活 / 读写分离

### 三、RAG 质量提升

- **P1 S5** embedding 加 Redis 二级缓存（进程内已有，分布式共享能省大量 API $）
- **P1 S8** 迁移 BM25 到 Qdrant 原生 sparse vector（规模 > 100k chunks 时）
- **P1 S5** AST 解析扩到更多语言（Rust / Java / TS）
- **P2 S8** 混合 graph 检索：通过 import 关系 + 调用关系扩召回

### 四、成本 / 性能

- **P1 S3** 替换启发式 `EstimateTokens` 为 `tiktoken-go`
- **P1 S5** LLM 调用自动降级到便宜模型（intent=简单查询走 haiku）
- **P2 S5** prompt 结构化缓存（自动维护"稳定前缀"）

### 五、运维 / 可观测

- **P1 S3** Tail-based tracing sampling（OTel Collector）
- **P1 S3** Grafana dashboard 随项目发布（infra as code）
- **P2 S5** Cost 指标专用面板（按 provider / user / project 汇总）

### 六、开发者体验

- **P1 S3** Skill / MCP CRUD 前端界面完善
- **P1 S3** 本地开发用的 `code-agent dev` 命令（一键起所有依赖）
- **P2 S5** 插件式 Tool 加载（无需改代码 + 重部署）

**完整 P0/P1 已完成清单见 [22_recent_improvements.md](22_recent_improvements.md)。**

---

## 附 C：每模块"实现-缺陷-改进"速览表

本表把 23 篇文档的"实现剖析"章节高度浓缩成一页，方便查阅。

| 模块 | 实现关键 | 主要缺陷 | 优先改进 |
|---|---|---|---|
| **config** | Viper 四层 + expandEnv 白名单 | expandEnv 手动维护 | reflect 自动遍历 |
| **models** | 单包避免循环依赖 | 不支持多模态 Content | ContentBlock schema |
| **llm** | Provider 接口 + gobreaker + Shared | Interval=Timeout bug | retryWithBackoff 启用 |
| **rag** | AST + Qdrant + BM25 + Rerank | 进程内 BM25 不共享 | Embedding Redis 缓存 |
| **sandbox** | Docker 瞬态容器 + 硬化 HostConfig | cold 启动 400ms | Warm pool |
| **mcp** | JSON-RPC 2.0 + stdio/SSE + 自动重连 | 每 call 是 round-trip | tool 流式 |
| **tools** | sync.Map + 稳定排序 | 无 per-tool 限流 | ACL + quota |
| **skill** | 三种 Kind（builtin/webhook/mcp） | webhook 无熔断 | 加 gobreaker |
| **orchestrator** | ReAct + 10 生产要素 | 串行 tool_calls | 并行执行 |
| **planner** | DAG + 并发 + checkpoint | 并发度硬编码 | 按 tool 类型调 |
| **temporal** | Workflow + HITL Await | 部署复杂 | Dial 失败重试 |
| **session** | hot/cold 分层 + 压缩 | 压缩失败无兜底 | 增量摘要 |
| **context** | KV cache 分层 + fingerprint 去重 | Pruner O(N²) | running sum |
| **workspace** | safePath + 租户隔离 | symlink 未防 | EvalSymlinks |
| **indexer/repomap** | hash 去重 + debounce | 单 goroutine | 并行索引 |
| **store** | database/sql + 自动 Migrate | 无 migration version | golang-migrate |
| **api** | Gin + 中间件栈 + SSE/WS | detach ctx 无 drain | drain on shutdown |
| **auth_security** | JWT + APIKey + HMAC + Egress + Audit | 多个已写未接 | 接线 + HMAC nonce |
| **observability** | metrics + tracing + logs + audit | head sampling 漏 | tail-based |
| **deploy** | 多阶段 Docker + K8s + CI | 缺 Helm / PDB | Helm Chart |

**阅读指南**：
- 查具体模块 → 对应 `NN_*.md` 的"实现剖析"章节
- 横向对比 → 上表
- 按类型汇总（如"所有安全类改进"） → [00_overview §9](00_overview.md#9-系统级实现剖析与演进全景)
- 已经修复的 → [22_recent_improvements](22_recent_improvements.md)
