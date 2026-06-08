# llmdoc 文档索引

全局文档地图。启动阅读顺序见 `llmdoc/startup.md`。

## must/ — 启动必读

每次新任务必读的小型稳定文档。

| 文档 | 内容 |
|------|------|
| `must/project-basics.md` | 项目身份、双项目布局、必选/可选子系统、构建测试命令 |
| `must/working-agreement.md` | DI 模式、KV-cache prompt 结构、工具分发拆分（含 9 处分布式白名单）、死代码清单、测试惯例 |
| `must/doc-routing.md` | 如何按任务类型找到正确文档（含 31 篇 `docs/architecture/` 全索引） |

## overview/ — 项目全景

| 文档 | 内容 |
|------|------|
| `overview/project-overview.md` | 完整项目概览：26 包地图、前端架构、部署模式 |

## architecture/ — 架构深度文档（llmdoc 汇总版）

| 文档 | 内容 |
|------|------|
| `architecture/request-flow.md` | HTTP 请求生命周期：中间件 → Orchestrator ReAct 循环 → 工具分发 → LLM → Session，含 PromptBuilder 5 区域、TokenPruner、HITL、失败跟踪 |
| `architecture/infrastructure-subsystems.md` | RAG 管线、Sandbox 安全模型、MCP JSON-RPC、Temporal HITL、Store、Workspace、Indexer、Repomap |
| `architecture/security-and-observability.md` | JWT/API Key 认证、HMAC webhook、Egress ACL 双层防御、限流、敏感检测、Prometheus 指标全表、OTel 追踪、审计日志 |

> 包级深度文档（每包独立一篇）见 `docs/architecture/00_*.md` ~ `30_*.md`，索引见 `must/doc-routing.md`。

## guides/ — 工作流指南

当前为空。按需创建。

## reference/ — 稳定参考

当前为空。按需创建。

## memory/ — 历史记忆

| 文档 | 内容 |
|------|------|
| `memory/doc-gaps.md` | 已知文档缺口、死代码清单、需要未来调查的领域 |
| `memory/reflections/2026-05-21-new-subsystems.md` | 反思：multiagent/toollearn/metacognition/planner-evaluator 新增未及时记录 |
| `memory/reflections/2026-05-28-quality-improvements.md` | 反思：P0-P2 全量质量修复（8 commits）——工具注册分散问题、macOS 路径误报、Provider 测试债务、apply_diff 工具、tool_progress 流式 |
| `memory/reflections/2026-05-29-docker-compose-image-tag-trap.md` | 反思：docker build vs docker compose build 镜像标签不一致陷阱 + P1 功能 Docker 验证（tree-sitter / PTY）|
| `memory/reflections/2026-06-01-architecture-docs-rewrite.md` | 反思：21 篇 `docs/architecture/` 13 节模板深度重写（00–20）+ 8 篇新增包文档（21–28）；29/30 暂未纳入 |
| `memory/reflections/2026-06-07-verifier-retry-and-process-as-artifact.md` | 反思：verifier retry-once 门控 + ToolResult.Metadata 透传契约；编排器减重模式（决策抽纯函数） |
| `memory/reflections/2026-06-07-ui-freeze-chain-defense-in-depth.md` | 反思：消除 UI 假死链 5 层防御（LLM 进度三件套 + Replay 合成 done + 前端 watchdog/reconcile + 内联渲染）；编排器减重模式第二证据点 |

---

## 外部参考

- `docs/architecture/` — **31 篇编号的中文包级深度文档**（`00_overview.md` ~ `30_recent_improvements.md`）。
  - **00–28**（29 篇）：13 节模板（模块定位 / 设计哲学 / 依赖架构 / 数据流总览 / 实现细节 / 设计权衡 / 后续演进 / 已知缺陷一览 / 测试矩阵 / 配置示例 / 跨文档引用 / 下一篇导引），是设计原理 + 已知缺陷的权威来源。
  - **29_conclusion.md** / **30_recent_improvements.md**：**结构异于模板** —— 29 是全景回顾 + 设计哲学 + Onboarding，30 是改进时间线（按现象/根因/修复/验证/相关章节）。两篇文件名编号已是新顺序，但内部 `# 21` / `# 22` 标题尚未跟随重命名，属遗留 TODO。
- `CLAUDE.md`（项目根和 code_agent 子目录）— Claude Code 的项目指令
- `configs/config.example.yaml` — 所有配置字段的参考示例（`config.yaml` 与 `config.allinone.yaml` 被 `.dockerignore` 排除，不在仓库中）
