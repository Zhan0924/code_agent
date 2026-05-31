# 启动阅读顺序

每次新任务开始时，按此顺序阅读 `/must/` 文档。

## 必读文档（按顺序）

1. `llmdoc/must/project-basics.md` — 项目身份、双项目布局、必选/可选子系统、构建与测试
2. `llmdoc/must/working-agreement.md` — DI 模式、KV-cache prompt 结构、工具分发拆分（含 9 处分布式白名单）、死代码清单、测试惯例
3. `llmdoc/must/doc-routing.md` — 如何找到正确的文档：llmdoc 分类 + 31 篇 `docs/architecture/` 全索引

## 升级阅读

按修改的子系统选择对应的包级深度文档（`docs/architecture/NN_*.md`）—— **这是设计原理与已知缺陷的权威来源**：

### 核心请求路径
- **修改 API / 路由 / 中间件 / SSE / WS** → `docs/architecture/17_api.md`
- **修改 Orchestrator / ReAct 循环 / 工具分发** → `docs/architecture/09_orchestrator.md`
- **修改 PromptBuilder / TokenPruner** → `docs/architecture/13_context.md`
- **修改 Session 管理 / token 预算** → `docs/architecture/12_session.md`
- **修改 LLM 客户端 / Router / 熔断** → `docs/architecture/03_llm.md`
- **修改工具注册 / Skill** → `docs/architecture/07_tools.md` + `docs/architecture/08_skill.md`

### 基础设施
- **修改 RAG / Qdrant / Embedder / Reranker** → `docs/architecture/04_rag.md`
- **修改 Sandbox / Docker 隔离 / WarmPool** → `docs/architecture/05_sandbox.md`
- **修改 MCP / JSON-RPC** → `docs/architecture/06_mcp.md`
- **修改 Planner / DAG / Evaluator** → `docs/architecture/10_planner.md`
- **修改 Temporal / HITL Workflow** → `docs/architecture/11_temporal.md`
- **修改 Store / PostgreSQL / migrations** → `docs/architecture/16_store.md`
- **修改 Workspace / 文件系统** → `docs/architecture/14_workspace.md`
- **修改 Indexer / Repomap** → `docs/architecture/15_indexer_repomap.md`

### 安全与可观测
- **修改 JWT / API Key / 限流 / RBAC** → `docs/architecture/18_auth_security.md`
- **修改 HMAC / Egress ACL / SSRF 防御** → `docs/architecture/18_auth_security.md`
- **修改 Metrics / Tracing / Audit / Logging** → `docs/architecture/19_observability.md`

### 部署与工程化
- **修改 Dockerfile / docker-compose / k8s / Makefile** → `docs/architecture/20_deploy.md`

### 新增子系统（21–28）
- **修改 AgentLoop / 元认知** → `docs/architecture/21_agentloop.md`
- **修改 MultiAgent / Supervisor / RoleSelector** → `docs/architecture/22_multiagent.md`
- **修改 ToolLearn / 反馈学习 / Distiller** → `docs/architecture/23_toollearn.md`
- **修改 Tree-sitter / AST 分块** → `docs/architecture/24_treesitter.md`
- **修改 Memory / 长期记忆** → `docs/architecture/25_memory.md`
- **修改 PTY / 交互式 Shell** → `docs/architecture/26_pty.md`
- **修改 LSP / 符号 / 重命名** → `docs/architecture/27_lsp.md`
- **修改 Generator / 项目脚手架** → `docs/architecture/28_generator.md`

### 总览与索引
- **需要全景视角** → `docs/architecture/00_overview.md` 或 `llmdoc/overview/project-overview.md`
- **跨包概念回顾** → `docs/architecture/29_conclusion.md`（**结构异于 13 节模板**：全景图 + 10 条设计哲学 + Onboarding Checklist；内部标题仍为 `# 21 · 架构回顾`，编号待与文件名对齐）
- **近期改进 / 反思** → `docs/architecture/30_recent_improvements.md`（**结构异于 13 节模板**：改进时间线，按"现象/根因/修复/验证/相关章节"；内部标题仍为 `# 22 · Recent Improvements`）

> 升级阅读列表中所有 `NN_*.md`（00–28，共 29 篇）已统一到 13 节模板，含 file:line 级别的"已知缺陷一览"。29/30 暂未纳入模板重写。

### llmdoc 汇总版（多包合并）
- **修改请求流程/编排器** → `llmdoc/architecture/request-flow.md`
- **修改 RAG/沙箱/MCP/Temporal/存储** → `llmdoc/architecture/infrastructure-subsystems.md`
- **修改认证/安全/可观测性** → `llmdoc/architecture/security-and-observability.md`
- **不确定某个功能是否已实现** → `llmdoc/memory/doc-gaps.md`（死代码和未接线功能清单）

> **优先级**：`docs/architecture/NN_*.md` 是单包深度（含 13 节模板的已知缺陷一览）；`llmdoc/architecture/*` 是跨包汇总（适合"我要快速理解请求是怎么流转的"这类问题）。修改单个包前优先读 `docs/architecture/`。
