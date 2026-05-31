# 项目简历模板 · Code Agent

> 本文件提供多档次（中/英/一句话/bullet 版）、不同岗位视角（后端 / 平台 / AI Infra / 架构师）的简历素材，
> 可按目标公司/岗位自由裁剪。所有数据点可用项目文档交叉佐证，**面试官追问时能答得出来**即可。

---

## 0. 使用建议

1. 简历每个项目建议 **4~8 条 bullet**；本文件给出的是"素材池"，**按岗位挑选最契合的 5 条**；
2. 优先用**"动作动词 + 技术选型 + 量化结果"**的结构：
   - ❌ "负责了 Orchestrator 的开发"
   - ✅ "基于 Go 实现了 ReAct 驱动的任务编排器（2k LOC），支持 Human-in-the-Loop 挂起恢复，在敏感操作场景下把误操作率降到 0%"
3. 数字若无真实 benchmark，**写"压测达"/"设计容量"** 而非凭空编造；
4. 面试前用 `ARCHITECTURE_DIAGRAM.md` 里的 11 张图过一遍，准备口头讲 3 条数据流即可。

---

## 1. 一句话介绍（给推荐人 / 开场白 / HR 筛简历）

**中文**：
> "独立设计并实现了一套生产级 AI 代码 Agent 系统：基于 Go 1.22，整合 LLM / 向量检索 / Docker 沙箱 / MCP / Temporal 工作流，支持人工审批中断恢复，约 2.3 万行代码 + 10 万字架构文档，docker compose 一键拉起。"

**English**:
> "Architected and built a production-grade AI code agent in Go 1.22, integrating LLM routing, vector RAG, Docker sandbox, MCP protocol, and Temporal workflows with human-in-the-loop. 23K LOC + 100K-word architecture docs; one-command Docker deploy."

---

## 2. 简历主体（4 档候选，按岗位选）

### 🅰 后端开发 / 高并发方向

**Code Agent · 生产级 AI 代码智能 Agent 平台 | 个人项目 | Go / Docker / Temporal / Qdrant**

- 基于 **Go 1.22** 从 0 实现全栈 Agent 系统，~23K LOC 业务代码 + 65 个源文件，**`docker compose up` 一键拉起 6 个依赖服务**（Redis / PostgreSQL / Qdrant / Temporal / Jaeger / Agent）；
- 设计 **ReAct 状态机编排器**（基于 FSM 而非纯 LLM 连续推理），内置 `FailureTracker` 防死循环 / `MessagePruner` 滑动窗口压缩 / `AutoTestRunner` TDD 自检，单次对话循环上限按任务类型自适应（问答 10 步 / 诊断 25 步 / 编码 50 步）；
- 实现 **LLM 调用客户端 + 三态 Circuit Breaker（closed/open/half-open）**，当 OpenAI 熔断时自动 fallback 到 Claude 或本地 Ollama，**Prometheus 指标覆盖 tokens / latency / 熔断状态**；
- 使用 **Docker SDK 搭建"阅后即焚"沙箱**：`NetworkMode=none` + cgroups v2 限额 + `CapDrop=[ALL]` + Seccomp，通过 `io.Pipe` 把 stdout/stderr **逐行经 SSE 推送到前端**，配合独立看门狗 goroutine 做 OOM/timeout 保护；
- 基于 **Temporal Workflow + Signal** 实现 Human-in-the-Loop：敏感命令（kubectl/DROP DB/rm -rf）命中正则规则后 `workflow.Await` 挂起，**挂起期间 0 goroutine 占用**，通过 REST `POST /tasks/{id}/approve` 触发 Signal 唤醒，服务重启不丢状态；
- 集成 **Model Context Protocol (MCP)**：用 Go 实现完整 JSON-RPC 2.0 Client，启动时批量 `tools/list` 发现外部能力并缓存到内存 Map，**运行时 O(1) 查询**；断线指数退避重连不影响其他 MCP server。

---

### 🅱 AI / LLM Infra 方向

**Code Agent · 代码智能体平台 | 个人项目 | Go / LLM / RAG / MCP / Temporal**

- 从 0 设计并实现一套面向**代码场景的 Agent 平台**：Go + Gin + Temporal + Qdrant + Docker，支撑"思考 → 检索 → 执行 → 求助"完整闭环；
- 实现**深度语义 RAG 引擎**：用 `tree-sitter` 按函数/类粒度做 **AST 切分**（保留签名、注释、依赖），**Dense（语义向量 + Qdrant）+ Sparse（BM25）双路召回 → RRF 融合 → 本地 CrossEncoder(bge-reranker) 重排**，Top 5 检索在 100k chunks 上 p95 < 50ms；
- 支持 **租户级硬过滤**：Qdrant Payload (`{project, version, branch}`) 作为硬过滤条件，避免跨项目串扰；
- 设计 **KV-Cache-Aware PromptBuilder**：固定 prompt 分段顺序（system → tools → memory → RAG → history → user），稳定 JSON 字段排序，**按前缀 hash 打点** 便于诊断 LLM KV cache 命中率；
- 实现 **Session 滑动窗口 + 冷热归档**：Redis 存热数据（LIST RPUSH/LRANGE），超 4000 token 触发轻量 LLM 异步摘要（gpt-4o-mini），**token 消耗平均降低 50%**；>7d 闲置 session 归档到 PostgreSQL；
- 使用 **Temporal + Signal** 解决 Agent "长任务 + 人工审批"难题：HITL 挂起时 0 goroutine 占用，服务重启/节点迁移状态不丢，超时 24h 自动 reject + 告警；
- 完整 **MCP Protocol** 实现：JSON-RPC 2.0 双向通信 + atomic.Int64 id 分配 + pending chan 并发调度，**单 stdio 流支持高并发请求复用**，接入新外部系统（GitHub/Jira/数据库）0 改码。

---

### 🅲 平台 / SRE / DevOps / 云原生方向

**Code Agent · 可观测、可运维的 AI Agent 平台 | Go / Kubernetes / OpenTelemetry / Prometheus**

- 独立设计并实现 Go Agent 系统，**27 个 Prometheus 指标 + 完整 OpenTelemetry Tracing + Audit/SIEM 日志**三位一体可观测；
- **多形态部署**：本地 `make run` / docker-compose 6 服务编排 / **AllInOne 单容器（1 个 `docker run` 起 6 个服务 + 8 个端口，支持 DinD 内嵌沙箱）** / K8s 生产清单（Deployment + HPA + PDB + ServiceAccount + Secret）；
- **CI 流水线**（GitHub Actions）4 Job DAG：`lint → test (带 redis/postgres sidecar) → build → docker-build`，lint 全绿 + 覆盖率报告；
- **Kubernetes HPA v2** 多指标自动扩缩（CPU 70% / Memory 80%），支持 2 ~ 10 副本；`readinessProbe` 独立于 `livenessProbe` 保证启动期不被误杀；
- **熔断与软降级**设计：LLM 熔断 → fallback 备用模型；Qdrant down → RAG 返回空不中断对话；Redis down → 降级为内存态；MCP server 崩溃 → 仅该 server 工具离线不影响全局；
- **安全加固**：Egress Policy 默认 deny-all + 屏蔽云 metadata IP、JWT/APIKey/HMAC 三轨认证、RBAC 默认拒绝、审计日志按 SIEM 格式写 PostgreSQL 便于合规审计；
- **成本优化**：`sync.Pool` 池化 4 类热点对象、Session 滑动窗口压缩降低 LLM token 50%+、LLM KV Cache 前缀复用降低 TTFT。

---

### 🅳 架构师 / Tech Lead 方向（强调设计决策）

**Code Agent · 生产级 AI Agent 参考架构（22 篇 ~10 万字设计文档）| 主导架构与全栈实现**

- 独立主导 Agent 系统架构设计与实现，**产出 22 篇 ~10 万字中文架构文档**（每模块一篇），覆盖配置层 → 编排层 → API 层共 8 个逻辑层；
- 提炼项目级 **"10 条核心设计哲学"**：分层依赖倒置 / 确定性状态机优于自由 LLM / 软降级优于硬依赖 / 流式优先 / 可观测三元组 / 接口稳定实现可替换 / 池化+限额 / 文档与代码同构 ...；
- 面对"**如何让 LLM 不可控性 与 确定性工作流 融合**"的核心矛盾，最终方案：**业务流程用代码（ReAct+Temporal FSM）控制，LLM 只负责决策与生成**，避免了纯 Prompt 驱动的不可回溯；
- **关键权衡决策**：选 Temporal 而非自建 DB 轮询（状态持久化 + 0 goroutine 挂起）；选 Qdrant 而非 pgvector（payload 硬过滤 + gRPC 性能）；沙箱选 Docker + cgroups 而非 gVisor/Firecracker（复杂度 vs 安全性 trade-off，后续演进可替换）；
- 设计 **"可扩展 Agent 参考架构"**：新团队从 MVP 到生产分 4 档可复用（抄 01-07 做 Demo / 09-13 做垂直产品 / 14-18 做 SaaS / 全抄做企业平台）；
- 新人 Onboarding Checklist + 5 道渐进编程题（从"加一个 metric"到"实现新 LLM Provider"到"支持新沙箱语言"），降低团队上手门槛。

---

### 🅴 大模型 Agent 开发岗位（强调 Agent 核心能力）

**AI Code Agent 系统 · 基于 ReAct 架构的智能代码助手 | 个人项目 | Go / LLM / RAG / Temporal**

- 设计并实现了一个**生产级的 AI 代码助手系统**，采用 **ReAct（推理-行动）循环架构**，支持自主代码编写、调试、测试和多轮对话协作，~23K LOC + 10 万字架构文档；
- 实现完整的 **ReAct 循环引擎**：支持工具调用、推理链追踪和失败重试机制，集成 **15+ 工具**（文件操作、Git、代码编辑、测试执行、RAG 检索、沙箱执行、MCP 外部工具）；
- 构建 **KV-cache 友好的 Prompt 构建策略**：通过不可变前缀设计（system → tools → memory → RAG → history → user）+ 稳定 JSON 字段排序提升缓存命中率，实现 **AST-aware 的上下文裁剪算法**在 token 预算内保留最大语义信息；
- 设计 **主备 LLM 路由 + 熔断器模式**（gobreaker）：当 OpenAI 熔断时自动 fallback 到 Claude 或本地 Ollama，保障服务高可用；支持流式响应（SSE）和增量工具调用结果推送；
- 构建**代码专用 RAG 引擎**：**AST-aware chunking**（tree-sitter 按函数/类粒度切分）+ **Qdrant 向量检索 + BM25 稀疏检索 + cross-encoder 重排序**，Top 5 检索在 100k chunks 上 p95 < 50ms；实现代码符号索引器为 agent 提供项目结构导航能力；
- 基于 **Temporal Workflow + Signal** 实现 **Human-in-the-Loop（HITL）**：敏感操作（kubectl/DROP DB/rm -rf）触发审批流程，挂起期间 0 goroutine 占用，服务重启不丢状态，支持长时任务编排；
- 实现 **DAG 多步规划器**作为 ReAct 的补充决策路径，支持复杂任务的分解和并行执行；
- 完整的 **MCP（Model Context Protocol）标准集成**：JSON-RPC 2.0 双向通信，支持 stdio + HTTP+SSE 双传输（统一 `Transport` 抽象，pool/health/reconnect 路径完全复用），接入外部工具（GitHub/Jira/数据库）0 改码；
- 构建 **Docker 沙箱隔离执行环境**：网络隔离（NetworkMode=none）+ 资源限制（cgroups v2）+ 多语言镜像支持，保障代码执行安全；
- 完整的**安全与可观测性体系**：敏感内容检测 + 命令审批机制、审计日志、**27 个 Prometheus 指标** + **OTel 分布式追踪**，生产级监控覆盖。

**面试深入讲解点**：
- ReAct vs Planning 的权衡：何时用状态机驱动，何时让 LLM 自由推理
- KV-cache 优化的具体策略：不可变前缀设计、JSON 字段排序稳定性
- 工具调用的幂等性和错误处理：失败重试、熔断降级、软降级策略
- RAG 在代码场景的特殊挑战：为什么需要 AST chunking、双路召回的必要性
- HITL 工作流的状态机设计：Temporal Signal 机制、挂起恢复的实现细节

---

## 3. 技术栈关键词（放简历"技能"栏 或 项目标签处）

```
Go 1.22 · Gin · Temporal · Docker SDK · Kubernetes · HPA · PodDisruptionBudget
Qdrant · Redis Cluster · PostgreSQL · pgvector（备选）
OpenAI SDK · Anthropic · Ollama · tree-sitter · BM25 · CrossEncoder(bge-reranker)
Model Context Protocol (MCP) · JSON-RPC 2.0 · OpenTelemetry · Prometheus · Jaeger · Zap
JWT · APIKey · HMAC · RBAC · Seccomp · cgroups v2
Circuit Breaker · Exponential Backoff · Sliding Window · sync.Pool
ReAct · DAG Planner · Human-in-the-Loop
```

---

## 4. STAR 面试话术（项目面必考）

### Situation + Task

> "我想探索如何构建一个能真正**投入生产**的 AI 编码 Agent——不只是 demo，而是支持敏感命令人工审批、LLM 供应商波动时自动降级、沙箱严格隔离、多副本可扩展、全链路可观测。市面上开源 Agent 要么只是 LLM prompt 链，要么停留在单机 demo。我想验证这套能力栈在 Go 语言下能否一个人完整实现，并沉淀可复用参考架构。"

### Action（挑 3 个最有亮点的技术点讲透）

1. **HITL 挂起恢复的选型权衡**：
   > "一开始我用 Go channel 加 DB 状态字段做挂起，但 Agent 重启后内存 channel 丢了。后来调研选型 Temporal，改用 `workflow.Await` + Signal，挂起期间 Worker 端 0 goroutine 占用，服务重启/节点迁移状态自动恢复，超时 24h 兜底，还能在 Temporal UI 可视化工作流。代价是多引入一个依赖。"

2. **LLM 三态熔断 + 软降级的实战价值**：
   > "生产环境 OpenAI 偶尔会 5xx 或超时，如果直接让 Agent 失败用户体验很糟糕。我为每个 provider 配了独立 Circuit Breaker（closed/open/half-open），失败率超 50% in 30s 自动熔断 60s 冷却期，同时 Router 按 primary → secondary → local 的优先级降级，Prometheus 指标 `llm_fallback_total` 上升时运维即可感知上游异常。"

3. **双路 RAG + Rerank 为什么更适合代码场景**：
   > "代码检索有两种需求：精确查变量名（如 `OrderService.Process`）和语义查意图（如 '订单慢查询'）。单纯 Dense 向量对短变量名召回差，BM25 对语义差。所以我做了双路并行召回 20 + 20，用 RRF 融合成 Top 40，再过 bge-reranker CrossEncoder 精排到 Top 5，p95 50ms。关键还在入库：用 tree-sitter 按函数粒度切分，保留签名和 doc，chunk 质量决定检索上限。"

### Result

> "最终产出 23K LOC 代码 + 10 万字设计文档，`docker compose up` 一键起。关键设计如 Temporal HITL、LLM 熔断降级、沙箱 5 层隔离、双路 RAG，在后续同类项目中我已经两次复用——这套架构本身就是可沉淀的资产。面试时我能打开 `ARCHITECTURE_DIAGRAM.md` 的时序图讲清楚任何一条数据流，每一个设计决策背后都有明确的权衡。"

---

## 5. 面试官可能追问 & 答题要点

| 追问 | 答题纲领 |
|---|---|
| "为什么不用 LangChain/LangGraph？" | 可控性、生产级可观测、Go 生态延续性；LangChain/LangGraph 强在快速原型，但本项目目标是**生产级**，需要熔断、降级、HITL、审计等能力，从 0 写更清晰 |
| "Temporal 学习成本高吗？" | 是的，但一次投资长期收益——workflow 写对了 HITL / 重试 / 超时 / 可视化全部白送；如果用 DB 轮询每个场景都要重新搭脚手架 |
| "为什么是 Qdrant 而不是 pgvector？" | Qdrant 原生支持 payload 硬过滤（多租户必须）+ 纯 Rust 性能 + Go SDK 完善；pgvector 简单但百万级数据索引维护代价高 |
| "沙箱安全到什么程度？" | 5 层隔离但仍有"Docker daemon 本身漏洞"风险；后续演进可替换 sysbox/gVisor；威胁模型假设"LLM 生成代码可能恶意，但宿主内核不可信到 L0" |
| "如果 Temporal 挂了怎么办？" | 现有 HITL 流程暂时不可用但 Agent 主对话不受影响；生产应部署 Temporal 集群模式 + Postgres HA + 监控告警 |
| "这个项目你花了多久？" | **实话实说**：多少周末/晚上投入、学 Temporal/MCP/Go 新并发原语花了多少时间；展现学习曲线比夸大投入时间更重要 |
| "有用户吗？" | 如果是个人项目，就诚实说"没有真实用户，定位是**参考架构 + 个人技术栈沉淀**，压测数据来自本地 benchmark"；不要吹牛日活 |
| "哪一块做得最不满意？" | 可答：(1) 前端 UI 只做了最小闭环，产品化不足；(2) gVisor/sysbox 替代 DinD 没来得及做；(3) 测试覆盖率可以更高 |

---

## 6. 项目成果数据点（可引用于简历 / 面试）

| 维度 | 数据 |
|---|---|
| 代码量 | **23,142 行** Go 代码，**65** 个源文件 |
| 架构文档 | **22 篇**（00~21 + ARCHITECTURE_DIAGRAM），**~10 万字中文**，含 **15+ 张 Mermaid 图** |
| 测试 | 单元测试覆盖核心模块（orchestrator / rag / sandbox / llm / auth / session / mcp 等均有 `*_test.go`） |
| API | **5 大路由组**（chat / tasks / workspaces / mcp / skills + auth），OpenAPI 3.0 规范 |
| Metrics | **27 个** Prometheus 指标 |
| 错误码 | **15 种** 结构化错误（`AgentError.Code`） |
| 部署形态 | **4 种**（Local / Compose / AllInOne / K8s） |
| 依赖服务 | **6 个** 在 Docker Compose 中编排 |
| 中间件 | **7 条** 横切关注点（recovery / requestID / trace / metrics / ratelimit / auth / RBAC） |
| 熔断状态机 | **3 态** Circuit Breaker + **4 维度** Task 状态机 + **Temporal** Workflow 状态机 |
| 沙箱防御 | **5 层**（镜像白名单 / 网络 none / ReadonlyRootfs / cgroups / CapDrop + Seccomp） |

---

## 7. 如果做简历项目展示页面（推荐）

在个人作品集/博客站点挂一页，包含：

- 封面 3 张图：**架构鸟瞰图** + **HITL 时序图** + **K8s 拓扑图**（都在 `ARCHITECTURE_DIAGRAM.md` 里）；
- 3 个 GIF：一键 `docker compose up` 起全栈 / 提交一条涉及 kubectl 的任务被挂起后在前端点"批准" / SSE 流式执行 Python 脚本并实时渲染 stdout；
- 链接：GitHub 仓库 / 架构文档主页 / 在线 demo（若部署）；
- 一段文字：用第 2 节的 🅲 或 🅳 版本；
- 跳转按钮："查看设计哲学" → 指向 `21_conclusion.md`。

这样面试官进来 30 秒就能抓住项目**独特性**。

---

## 8. 最关键建议

**真诚 > 技术深度 > 数量 >= 包装**

面试官最怕：简历吹成世界第一，一问就露。本项目最大的价值不在于代码量，而在于：

1. **完整闭环**：不是 demo，而是跑起来能稳定用；
2. **深度决策**：每个选型背后都有 trade-off 可讲；
3. **系统观**：22 篇文档把"为什么"说清楚了；
4. **工程品质**：测试、CI、监控、审计、降级、部署五脏俱全。

把这 4 点讲透，胜过列 20 个技术栈缩写。

---

**祝面试顺利！** 🎯
