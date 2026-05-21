# llmdoc 文档索引

全局文档地图。启动阅读顺序见 `llmdoc/startup.md`。

## must/ — 启动必读

每次新任务必读的小型稳定文档。

| 文档 | 内容 |
|------|------|
| `must/project-basics.md` | 项目身份、双项目布局、必选/可选子系统、构建测试命令 |
| `must/working-agreement.md` | DI 模式、KV-cache prompt 结构、工具分发拆分、死代码清单、测试惯例 |
| `must/doc-routing.md` | 如何按任务类型找到正确文档 |

## overview/ — 项目全景

| 文档 | 内容 |
|------|------|
| `overview/project-overview.md` | 完整项目概览：26 包地图、前端架构、部署模式 |

## architecture/ — 架构深度文档

| 文档 | 内容 |
|------|------|
| `architecture/request-flow.md` | HTTP 请求生命周期：中间件 → Orchestrator ReAct 循环 → 工具分发 → LLM → Session，含 PromptBuilder 5 区域、TokenPruner、HITL、失败跟踪 |
| `architecture/infrastructure-subsystems.md` | RAG 管线、Sandbox 安全模型、MCP JSON-RPC、Temporal HITL、Store、Workspace、Indexer、Repomap |
| `architecture/security-and-observability.md` | JWT/API Key 认证、HMAC webhook、Egress ACL 双层防御、限流、敏感检测、Prometheus 指标全表、OTel 追踪、审计日志 |

## guides/ — 工作流指南

当前为空。按需创建。

## reference/ — 稳定参考

当前为空。按需创建。

## memory/ — 历史记忆

| 文档 | 内容 |
|------|------|
| `memory/doc-gaps.md` | 已知文档缺口、死代码清单、需要未来调查的领域 |

---

## 外部参考

- `docs/architecture/` — 24 个编号的中文包级深度文档（`00_overview.md` ~ `22_recent_improvements.md`），是设计原理的权威来源
- `CLAUDE.md`（项目根和 code_agent 子目录）— Claude Code 的项目指令
- `configs/config.yaml` — 所有配置字段的参考示例
