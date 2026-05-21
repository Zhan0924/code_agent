# 文档缺口

已知的文档不足和需要未来调查的领域。

## 死代码 — 已实现未接线

这些组件已完整编译但运行时从不调用，需要决定是启用还是删除：

| 组件 | 位置 | 状态 |
|------|------|------|
| `llm.Router` | `internal/llm/router.go` | Heavy/Medium/Light 模型路由，无 main.go 或 Orchestrator 引用 |
| `SpeculativeToolCache` | `internal/orchestrator/speculative_cache.go` | Orchestrator 无此字段，无调用点 |
| Git 工具 | `internal/orchestrator/git_tools.go` | `gitToolDefinitions()` 未纳入 `getAvailableTools()`，LLM 无法调用 |
| `LLMSummarizer` | `internal/session/summarizer.go` | Summarizer 接口定义，`Manager.buildSummary()` 使用内联截断 |
| `ConnPool` ↔ `Gateway` | `internal/mcp/pool.go` | ConnPool 实现完整但 Gateway 仍使用单连接/服务器 |
| MCP SSE 传输 | `internal/mcp/client.go` | `doc.go` 提及 SSE 支持，`NewGateway` 跳过非 stdio 服务器 |
| Redis `RedisRateLimiter` | `internal/auth/redis_ratelimit.go` | 完整实现但中间件使用进程内 token bucket |
| `chatApi.stream()` | `code_agent_ui/src/api/client.ts` | 前端定义了 `/chat/stream` 调用但 ChatPage 直接 fetch `/chat/react-stream` |

## 功能缺口 — 前端

| 缺口 | 说明 |
|------|------|
| HITL 审批 UI | `approvalApi` 和类型已定义，但无审批页面或通知组件 |
| 无测试 | 零测试文件，无测试框架依赖 |
| 无状态管理 | 无 Context / Redux / zustand，页面间仅 localStorage 共享 |
| 无错误边界 | 未捕获的 React 错误会崩溃整个应用 |
| 无 SSE 重连 | ChatPage 仅捕获初始 fetch 错误，无 EventSource 重试 |
| WebSocket 未使用 | Vite 配置了 `/ws` 代理但无组件使用 WebSocket |

## 配置与安全缺口

| 缺口 | 说明 |
|------|------|
| CORS 硬编码 | `allowedOrigins` 是代码中的硬编码 map（仅 localhost），应来自配置 |
| WebSocket 无认证 | `/api/v1/ws` 在 auth 组内但创建会话时写死 "ws-user"，不提取身份 |
| 审计日志未入库 | `audit.Logger` 是 zap 包装器，`store.audit_logs` 表存在但两者未关联 |
| `testing.Short()` 未实现 | `make test-short` 存在但零个测试文件检查该标志 |
| 配置验证缺口 | 不验证：RAG 嵌入 URL（当 provider=openai）、Temporal.Host 格式、敏感模式正则可编译性、Tracing.Endpoint 格式 |

## Token 估算不一致

两个不同精度的估算器：

- `internal/llm/client.go` (`EstimateTokens`) — rune 分类，CJK 准确
- `internal/session/manager.go` (`estimateTokens`) — `len/4+1`，CJK 低估 ~3x

Session 层使用低精度版本，可能导致上下文窗口管理偏差。

## 未调查领域

- `configs/config.allinone.yaml` 具体内容
- `deploy/entrypoint.sh`（allinone 进程管理脚本）
- `Dockerfile.p0test` / `Dockerfile.test` 细节
- Warm Pool 与 Manager.Execute 的集成点
- Store 从 Orchestrator 的实际调用路径
- `_principles.go` 设计文档中描述的理想架构 vs 当前实现的差异全貌
- Indexer 校验和存储持久化（当前仅内存，重启丢失）
- Repomap 正则提取的精度限制（多行 receiver、Python decorator 等）
- Distiller 策略持久化（`pg_store.go` 已存在但 main.go 未接线确认）
- MultiAgent Supervisor 在 Orchestrator 中的集成路径（`main.go` 接线状态未确认）
- Metacognition 对 ReAct 循环决策的实际影响范围

## 文档结构待办

- `llmdoc/guides/` — 当前为空，未来按需创建工作流指南（如：如何添加新工具、如何接线死代码、如何添加新 MCP 服务器）
- `llmdoc/reference/` — 当前为空，未来按需创建稳定参考（如：配置字段全表、API 端点全表、工具定义 schema）
