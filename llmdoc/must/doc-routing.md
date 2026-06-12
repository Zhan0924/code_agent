# 文档路由

如何找到正确的文档。

## llmdoc 分类

| 分类 | 何时阅读 |
|------|----------|
| `llmdoc/must/` | 每次新任务开始时（已在 startup.md 中列出顺序） |
| `llmdoc/overview/` | 需要全局视角、理解项目边界和角色 |
| `llmdoc/architecture/` | 跨包流程视角（请求流 / 基础设施 / 安全+可观测三篇汇总） |
| `llmdoc/guides/` | 执行特定工作流（当前为空，按需创建） |
| `llmdoc/reference/` | 查询稳定的约定、schema、合约（当前为空，按需创建） |
| `llmdoc/memory/` | 查阅历史决策、反思记录和文档缺口 |

## llmdoc/architecture 三篇汇总

| 文档 | 涵盖内容 |
|------|----------|
| `request-flow.md` | HTTP 请求 → 中间件 → Orchestrator ReAct 循环 → 工具分发 → LLM → Session |
| `infrastructure-subsystems.md` | RAG、Sandbox、MCP、Temporal、Store、Workspace、Indexer、Repomap |
| `security-and-observability.md` | Auth、HMAC、Egress ACL、限流、敏感检测、Metrics、Tracing、Audit |

## docs/architecture/ 包级深度文档（31 篇）

**00–28（29 篇）** 采用 13 节模板：模块定位 / 设计哲学 / 依赖架构 / 数据流总览 / 实现细节 / 设计权衡 / 后续演进 / 已知缺陷一览 / 测试矩阵 / 配置示例 / 跨文档引用 / 下一篇导引。**修改特定包前，先读对应编号的文档以理解"为什么"这样设计，并查阅其已知缺陷一览**。同时**扫一眼 `30_recent_improvements.md` 最近几条时间线**，确认目标包是否已有近期改动记录，避免重复 audit。

**29 / 30** 为收尾性文档，**结构异于模板**：29 是全景回顾 + 设计哲学 + Onboarding，30 是改进时间线。两篇内部 H1 标题（`# 21 · ...` / `# 22 · ...`）尚未跟随文件名重命名，属遗留 TODO。

| 编号 | 主题 | 修改时阅读 |
|------|------|------------|
| 00 | overview | 需要项目全景视角 |
| 01 | config | 修改 Viper 配置 / 环境变量 / `${VAR}` 展开 |
| 02 | models | 修改 ToolDefinition / ToolResult / Message 等共享类型 |
| 03 | llm | 修改 LLM 客户端 / Router / gobreaker / fallback |
| 04 | rag | 修改 RAG 管线 / Qdrant / Embedder / Reranker / BM25 |
| 05 | sandbox | 修改 Docker 隔离 / WarmPool / 网络/能力剥离 |
| 06 | mcp | 修改 MCP JSON-RPC / stdio/SSE 传输 / ConnPool |
| 07 | tools | 修改工具注册表 / 排序确定性（KV cache） |
| 08 | skill | 修改 Skill 统一注册 / schema 快照 |
| 09 | orchestrator | 修改 ReAct 循环 / 工具分发 / 失败跟踪 / 自动测试 / 元认知 |
| 10 | planner | 修改 DAG 多步规划 / 评估器 / 进度追踪 |
| 11 | temporal | 修改 HITL Workflow / Signal+Timer selector |
| 12 | session | 修改 Redis hot/cold session / token 预算 |
| 13 | context | 修改 PromptBuilder 5 区域 / TokenPruner AST 元数据 |
| 14 | workspace | 修改本地 FS / 路径遍历防护 / manifest |
| 15 | indexer_repomap | 修改仓库索引 / 正则符号提取 / fsnotify |
| 16 | store | 修改 PostgreSQL 持久化 / migrations |
| 17 | api | 修改 Gin 路由 / 中间件链 / SSE/WS handler |
| 18 | auth_security | 修改 JWT / API Key / RBAC / HMAC / Egress ACL / 限流 |
| 19 | observability | 修改 Prometheus / OTel / Audit / Logging |
| 20 | deploy | 修改 Dockerfile / docker-compose / k8s / Makefile |
| 21 | agentloop | 修改 AgentLoop / 元认知（Confidence/StuckScore/Pivot）|
| 22 | multiagent | 修改 Supervisor / SubAgent / AgentPool / MessageBus / ConflictResolver / RoleSelector |
| 23 | toollearn | 修改持续学习：Collector → Distiller → Advisor / `tool_feedback` 表 |
| 24 | treesitter | 修改 Tree-sitter AST 分块 / CGO 后端 / regex 回退 |
| 25 | memory | 修改长期记忆 / Memory store |
| 26 | pty | 修改 PTY 会话 / shell_exec 持久化 / 输出限流 |
| 27 | lsp | 修改 LSP 客户端 / goto_definition / find_references / rename_symbol |
| 28 | generator | 修改项目生成器 / 组合 LLM + Sandbox + Workspace |
| 29 | conclusion | 跨包概念回顾 / 全局总结（**非 13 节模板**，内部 H1 仍为 `# 21`）|
| 30 | recent_improvements | 近期改进 / 反思 / quality gates（**非 13 节模板**，内部 H1 仍为 `# 22`）|

## 决策路径

- **"这个功能是否已接线？"** → `docs/architecture/NN_*.md` 中对应包的"已知缺陷一览"或 `llmdoc/must/working-agreement.md` 死代码清单
- **"请求是怎么处理的？"** → `docs/architecture/17_api.md` + `09_orchestrator.md`，或 `llmdoc/architecture/request-flow.md`
- **"为什么这样设计？"** → `docs/architecture/NN_*.md` 中对应包的"设计哲学"+"设计权衡"
- **"已知风险点？"** → `docs/architecture/NN_*.md` 中对应包的"已知缺陷一览"（每篇 5–10 个 P0/P1/P2 条目，带 file:line）
- **"如何部署到生产？"** → `docs/architecture/20_deploy.md`（含 10 个 DEP-N 已知缺陷与 k8s 多副本约束）
- **"安全模型？"** → `docs/architecture/18_auth_security.md`（含 AS-1~AS-8 缺陷）+ `19_observability.md`
- **"还有什么没文档化？"** → `llmdoc/memory/doc-gaps.md`

## 优先级

1. 修改单个包前 → 先读 `docs/architecture/NN_*.md` 对应编号
2. 需要跨包视角 → 读 `llmdoc/architecture/` 三篇汇总
3. 历史决策与已知缺口 → 读 `llmdoc/memory/`

**注意**：`docs/architecture/` 00–28 已在 2026-06-01 重写到 13 节模板，"已知缺陷一览"按包独立标注。包级文档优先；当包级缺陷一览覆盖某项后，应回头清理 `llmdoc/must/working-agreement.md` 中重复的死代码条目，避免双源不同步。当前两份清单仍并存，发现差异时以**代码 + 包级文档**为准。
