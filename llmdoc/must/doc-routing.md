# 文档路由

如何找到正确的文档。

## llmdoc 分类

| 分类 | 何时阅读 |
|------|----------|
| `llmdoc/must/` | 每次新任务开始时（已在 startup.md 中列出顺序） |
| `llmdoc/overview/` | 需要全局视角、理解项目边界和角色 |
| `llmdoc/architecture/` | 修改具体子系统前，理解流程、不变量和所有权边界 |
| `llmdoc/guides/` | 执行特定工作流（当前为空，按需创建） |
| `llmdoc/reference/` | 查询稳定的约定、schema、合约（当前为空，按需创建） |
| `llmdoc/memory/` | 查阅历史决策、反思记录和文档缺口 |

## architecture 文档索引

| 文档 | 涵盖内容 |
|------|----------|
| `request-flow.md` | HTTP 请求 → 中间件 → Orchestrator ReAct 循环 → 工具分发 → LLM → Session |
| `infrastructure-subsystems.md` | RAG、Sandbox、MCP、Temporal、Store、Workspace、Indexer、Repomap |
| `security-and-observability.md` | Auth、HMAC、Egress ACL、限流、敏感检测、Metrics、Tracing、Audit |

## 已有的中文架构文档

`docs/architecture/` 包含 24 个编号的中文深度文档（`00_overview.md` 到 `22_recent_improvements.md`）。这些文档是包级设计原理的权威来源。修改特定包前，先查阅对应编号的文档理解 **为什么** 这样设计。

**注意**：`docs/architecture/` 中的部分设计描述是理想状态（如 Router 集成），不一定反映当前接线状态。以 llmdoc 中的死代码清单为准。

## 决策路径

- "这个功能是否已接线？" → `llmdoc/must/working-agreement.md`（死代码清单）
- "请求是怎么处理的？" → `llmdoc/architecture/request-flow.md`
- "RAG/沙箱/MCP 怎么工作？" → `llmdoc/architecture/infrastructure-subsystems.md`
- "认证和安全机制？" → `llmdoc/architecture/security-and-observability.md`
- "还有什么没文档化？" → `llmdoc/memory/doc-gaps.md`
