# 项目全景

## 系统定位

code_agent 是一个生产级 ReAct 代码智能体平台，核心能力是通过 LLM 驱动的多轮工具调用循环（ReAct loop）完成代码理解、搜索、编辑、测试执行和部署审批等任务。

系统由 Go 后端 + React 前端两个兄弟项目组成，通过 HTTP/SSE/WebSocket 通信。Go 后端是单二进制服务，使用手动 DI 在 `cmd/agent/main.go` 中组装 26 个内部包。

## 核心设计原则

1. **可选子系统降级** — 除 Redis 和 LLM 外，所有子系统故障均降级为 nil 并继续运行
2. **KV-cache 友好** — Prompt 5 区域结构和工具定义排序确保最大化 LLM provider 的 KV cache 命中
3. **HITL（Human-in-the-Loop）** — 敏感操作通过 Temporal 信号或进程内通道暂停等待人工审批
4. **防御纵深** — 沙箱全能力剥离 + 只读 rootfs + 网络隔离 + egress DNS rebinding 防御 + 敏感模式检测

## 后端包地图

### 核心请求路径

| 包 | 文件数 | 职责 |
|---|---|---|
| `internal/api` | 11 | Gin 路由器、中间件链、SSE/WS handler、DI 胶水层 |
| `internal/orchestrator` | 20 | ReAct 主循环 + 文件/git/编辑工具 + 失败跟踪 + 自动测试 + 项目规则注入 |
| `internal/llm` | 9 | OpenAI 兼容客户端，primary/fallback 路由，gobreaker 熔断 |
| `internal/session` | 5 | Redis hot/cold 会话管理，滑动窗口 token 预算 |
| `internal/context` | 5 | PromptBuilder（5 区域）+ TokenPruner（AST 元数据多信号裁剪） |
| `internal/tools` | 2 | 工具注册表（并发安全 map，确定性排序输出） |

### 基础设施

| 包 | 文件数 | 职责 |
|---|---|---|
| `internal/rag` | 18 | AST 感知分块 → 嵌入 → Qdrant 稠密 + BM25 稀疏双召回 → 可选交叉编码器重排 |
| `internal/sandbox` | 8 | Docker 隔离执行（网络=none，能力全剥离，只读 rootfs，暖池） |
| `internal/mcp` | 7 | JSON-RPC 2.0 MCP 客户端，连接池，stdio 传输，健康检查 |
| `internal/temporal` | 4 | HITL 审批工作流（Signal+Timer selector） |
| `internal/store` | 2 | PostgreSQL 持久化（tasks/audit_logs/api_keys/approvals），自动迁移 |
| `internal/workspace` | 1 | 本地 FS 管理器，路径遍历防护，manifest 持久化 |
| `internal/indexer` | 2 | 仓库增量索引（sha256 校验和 → ragEngine.IndexCode） |
| `internal/repomap` | 4 | 正则符号提取 + fsnotify 监听 + 缓存 |

### 安全与可观测

| 包 | 文件数 | 职责 |
|---|---|---|
| `internal/auth` | 7 | JWT HS256 + API Key（SHA-256 + constant-time 比较）+ Redis 撤销 + 限流 |
| `internal/security` | 4 | HMAC webhook 验证 + Egress ACL（双层 DNS rebinding 防御） |
| `internal/metrics` | 2 | Prometheus 指标（API/LLM/RAG/Sandbox/MCP/Session/HITL/Planner/Cost） |
| `internal/tracing` | 1 | OTel OTLP gRPC 追踪，W3C TraceContext 传播 |
| `internal/audit` | 2 | 结构化审计日志（zap），覆盖 HITL/Sandbox/MCP/敏感阻断 |

### 辅助

| 包 | 文件数 | 职责 |
|---|---|---|
| `internal/config` | 4 | Viper 配置加载 + 多错误验证 + `${VAR}` 展开 |
| `internal/skill` | 6 | 统一工具注册（builtin/MCP/user-webhook），schema 快照（原子指针 + 代际计数） |
| `internal/planner` | 3 | 可选 DAG 多步规划器（ReAct 的替代模式） |
| `internal/pool` | 2 | `sync.Pool` 泛型封装（byte slice / buffer / JSON encoder） |
| `internal/models` | 1 | 共享数据类型（ToolDefinition, ToolResult, Message 等） |
| `internal/errors` | 2 | 错误类型定义 |
| `internal/generator` | 1 | 项目生成器（组合 LLM + Sandbox + Workspace） |

## 前端架构

12 个 TS/TSX 源文件，无外部状态管理库，无测试。

- **路由**：7 个页面（Chat / Workspace / MCP / Skills / Tools / Dashboard / Health），均在 `Layout` 下
- **核心页面**：ChatPage 通过 POST `/api/v1/chat/react-stream` 获取 SSE 流，手动解析 `data:` 行渲染 ReAct trace
- **编辑器**：WorkspacePage 使用 Monaco Editor，支持多标签、文件树、Cmd+S 保存
- **状态共享**：仅通过 localStorage（session_id + workspace_id），无 Context / Redux / zustand
- **HITL**：`approvalApi` 和 `ApprovalRequest` 类型已定义，但无审批 UI 页面
- **样式**：单文件 `index.css`（900 行），暗色主题，CSS 自定义属性

## 部署模式

| 模式 | 入口 |
|------|------|
| 本地二进制 | `make build` → `bin/code-agent` |
| 多阶段 Docker | `Dockerfile`（golang:1.25-alpine → alpine:3.20） |
| 全合一 Docker | `Dockerfile.allinone`（含 Redis/PG/Qdrant/DinD） |
| Kubernetes | `deployments/k8s/deployment.yaml`（2-10 副本 HPA） |
| Docker Compose | `docker-compose.yml`（agent + redis + postgres + qdrant + temporal） |
