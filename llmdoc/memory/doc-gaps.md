# 文档缺口

已知的文档不足和需要未来调查的领域。

> **2026-06-01 边界澄清**：本文同时记录**跨包缺口**与**死代码 / 接线缺失**两类条目。`docs/architecture/NN_*.md` 各篇的"已知缺陷一览"自 2026-06-01 起按包独立维护并带 file:line。**两份清单暂时并存**，当包级缺陷一览覆盖某项后，应回头清理这里对应条目，避免双源不同步。下方表格的"包级文档"列指明最新权威来源。
>
> **2026-06-03 二次复核**：对 `30_recent_improvements.md::F 节 P1 待办` 与本文跨包缺口做了**逐项代码核查**，确认 4 项已修复未同步、2 项半接线、6 项仍属实。详见文末"## 2026-06-03 二次复核摘要"。

## 死代码 — 已实现未接线（与包级文档双源）

> **2026-06-01 复核**：以 `cmd/agent/main.go` / `internal/api/router.go` / `internal/orchestrator/*` 为准。6 项历史死代码条目已接线，仅余 3 项未接 + 2 项孤儿。

**仍未接线 / 孤儿**：

| 组件 | 位置 | 状态 | 包级文档 |
|------|------|------|----------|
| `chatApi.stream()` | 前端 `code_agent_ui/src/api/client.ts`（兄弟仓库，行号略） | 前端定义 `/chat/stream` 调用但 `ChatPage.tsx` 直接 fetch `/chat/react-stream` | （前端，无包级文档） |
| `internal/audit` / `internal/errors` | 整包孤儿 | 零生产 importer（构建可过但无调用点） | `docs/architecture/19_observability.md` |
| `internal/pool` | 单一 importer | 仅 `session/manager.go` 使用 | `docs/architecture/19_observability.md` |

**已接线（保留供历史回溯，下次审计删除）**：

| 组件 | 接线点 |
|---|---|
| `llm.Router` | `cmd/agent/main.go:433` `orch.SetRouter(llmRouter)` |
| `SpeculativeToolCache` | `internal/orchestrator/orchestrator.go:89,231` 已是 `Orchestrator` 字段 |
| Git 工具 | `internal/orchestrator/builtin_tools.go:100` `gitDefs := gitToolDefinitions()` |
| `LLMSummarizer` | `cmd/agent/main.go:194` `sessionMgr.Summarizer = session.NewLLMSummarizer(...)` |
| `ConnPool` ↔ `Gateway` | `internal/mcp/...`（按 working-agreement.md:80 描述已切换为 `map[string]*ConnPool`） |
| `RedisRateLimiter` | `internal/api/router.go:190` `auth.NewRedisRateLimiter(...)` |
| MCP `healthChecker` 自启动 | `cmd/agent/main.go` MCP 初始化块尾 `mcpGateway.StartHealthCheck(30*time.Second)`；池级 slot 死亡 → reconnect 兜底 |
| MCP SSE transport | `internal/mcp/transport_sse.go` + `dialTransport(cfg, ...)` 分派；`NewGateway` 不再白名单 stdio，由 `dialTransport` 直接报错未识别 transport |

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

## Token 估算（2026-06-03 已统一）

历史上存在双估算器（`session` 用 `len/4+1`，`llm` 用 rune 分类）。**当前代码两者已收敛**：

- `internal/llm/client.go:337` `EstimateTokens` → `llm.FastEstimate`
- `internal/session/manager.go:540` `estimateTokens` → `llm.FastEstimate`
- 精确路径：`internal/llm/tokenizer.go:71` `ExactTokenCount` 用 tiktoken-go（`pkoukk/tiktoken-go`），`orchestrator/react_core.go:155` 已使用
- 原 `30_recent_improvements.md::F 节` "P1-tiktoken 用 tiktoken-go 替换启发式 EstimateTokens" 待办**已完成**，需从该清单移除

## 流程缺口

| 缺口 | 说明 |
|------|------|
| macOS CI matrix 缺失 | CI 仅在 Linux (Docker alpine) 执行。macOS 特有的 symlink 行为（`/tmp` → `/private/tmp`）未被 CI 覆盖，导致 workspace 路径遍历误报 bug 长期存在（已修复，但 CI 仍未覆盖 macOS） |
| 工具注册清单指南 | 新增内置工具需在 9 处注册（见 `must/working-agreement.md`），但缺少完整的分步操作指南（`guides/` 待创建） |

## 未调查领域

> 2026-06-01 更新：以下领域大多已在 `docs/architecture/` 重写过程中被覆盖；保留仅供回溯。

- ~~`configs/config.allinone.yaml` 具体内容~~ → 见 `docs/architecture/20_deploy.md` §5.2
- ~~`deploy/entrypoint.sh`（allinone 进程管理脚本）~~ → 见 `docs/architecture/20_deploy.md` §5.3
- ~~`Dockerfile.p0test` / `Dockerfile.test` 细节~~ → 见 `docs/architecture/20_deploy.md` §5.6
- ~~Distiller 策略持久化~~ → 见 `docs/architecture/23_toollearn.md`（main.go pgStore 已接线 `Migrate` + `orch.SetToolLearnStore`）
- ~~MultiAgent Supervisor 集成路径~~ → 见 `docs/architecture/22_multiagent.md`
- ~~Metacognition 对 ReAct 影响范围~~ → 见 `docs/architecture/21_agentloop.md`

**仍待调查**：
- Warm Pool 与 Manager.Execute 的实际命中率与冷启动延迟（缺生产 metrics 样本）
- Repomap 正则提取的精度限制（多行 receiver、Python decorator、TS 装饰器）
- `_principles.go` 理想架构 vs 当前实现的剩余差异（已部分对账，但完整差异表未生成）

## 文档结构待办

- `llmdoc/guides/` — 当前为空，未来按需创建工作流指南（如：如何添加新工具、如何接线死代码、如何添加新 MCP 服务器）
- `llmdoc/reference/` — 当前为空，未来按需创建稳定参考（如：配置字段全表、API 端点全表、工具定义 schema）

## 与 `docs/architecture/` 的协同

`docs/architecture/NN_*.md` 中 00–28（29 篇）含"已知缺陷一览"，按文档分别维护 P0/P1/P2 条目，带 file:line。修改单个包前应**优先看包级缺陷一览**；本文件用于：
1. **跨包缺口**（如 macOS CI matrix、工具名清单分散在 18 文件）—— 不归任何单包
2. **前端缺口** —— `docs/architecture/` 仅覆盖后端
3. **过渡期双源**：上方"死代码"表中条目同时存在于本文件与包级文档；两者一致前以**代码 + 包级文档**为准

---

## 2026-06-03 二次复核摘要

对 `30_recent_improvements.md::F 节 P1 待办` 与跨包缺口逐项 grep+Read 核查。**`30_recent_improvements.md` 自身 F 节需同步删除已完成项**。

### A. 已完成、文档需移除

| 原待办 | 实际接线点 |
|---|---|
| P1-Redis-RL-wire | `internal/api/router.go:188-195` Redis 可用时走 `auth.NewRedisRateLimiter`，否则 fallback in-memory；生产 Redis 强依赖 |
| P1-tiktoken | `internal/llm/tokenizer.go:39` import `pkoukk/tiktoken-go`，`ExactTokenCount` 已被 `react_core.go:155` 使用 |
| 双 Token 估算器 | `session/manager.go:540` 与 `llm/client.go:337` 均委托 `llm.FastEstimate`，`len/4+1` 在 session 中已不存在 |

### B. 部分完成、需更新表述

| 原待办 | 实际状态 | 剩余工作 |
|---|---|---|
| P1-Egress-wire | LLM ✅ `main.go:165`；MCP ✅ `main.go:337-341`；Reranker ❌ `rag/reranker.go:64` 仍裸 `http.Client`；Embedder ❌ `rag/embedder.go:57` 用默认 openai client | 把 egress transport 注入 reranker / embedder |
| P1-Streaming-breaker | `client.go:302` 流式入口已 `sharedBreaker.Allow()`，但**未在流式失败路径调 `RecordFailure`**（非流式在 `client.go:238-239` 有调）| 流式错误路径补 `sharedBreaker.RecordFailure` |

### C. 仍属实、待修复（带 file:line）

> **2026-06-04 更新**：ORC-1 / ORC-2 / ORC-3 / P1-shutdown-drain / CFG-EXPAND / TOOL-NAMES 已在本日落地的 9 PR 中修复（见 `docs/architecture/30_recent_improvements.md::F 节 完成状态对账`），本表删除已完成行。仅保留**当前仍未修复**条目。

| 编号 | 现象 | 位置 |
|---|---|---|
| TOOL-NAMES-XPKG | orchestrator 包内硬编码已收敛到 `tool_names.go`；但跨包硬编码（`multiagent/` / `agentloop/` / `planner/`）仍未迁移 —— 需要先决定共享常量包位置（避免 import cycle）再展开 | `rg --count '\"read_file\"\|\"write_file\"' internal/{multiagent,agentloop,planner}` |
| HITL-INPROC-LOSS | In-process HITL 回退通道在以下任一场景下导致 approval 返回 `404 no pending approval`：(a) 进程在 suspend 与 approve 之间重启；(b) `approvalCh[taskID]` 30 分钟超时清理后；(c) 前端用过期 taskID。Temporal 路径不受影响。修复方向待定（Redis 持久化 / 状态机恢复 / 强制 Temporal） | `orchestrator/orchestrator.go:659-718`（suspend + 30min cleanup）+ `orchestrator/orchestrator.go:751-757`（404 lookup） |

### D. 论述偏差（举例不准确，底层问题仍在）

| 论述 | 修正 |
|---|---|
| "long-running shell_exec / run_tests 无视 parallel cancel" | `shell_exec`/`run_tests` 非幂等，**不走 parallel 路径**；cancel 失灵的真正根因是这些工具内部不 select ctx |
| "go.mod 1.25 vs golangci 1.22 vs CI 1.22 三处分歧" | `.golangci.yml:3 go: "1.22"` 确实落后；但**没有标准 CI workflow**（`.github/workflows/` 只有 `pr-review.yml`），属 DEP-2 范畴。"CI Go 1.22" 是 CLAUDE.md 旧描述 |

### E. 同步动作

- [ ] `30_recent_improvements.md::F 节 P1 待办` 删除：P1-Redis-RL-wire / P1-tiktoken
- [ ] `30_recent_improvements.md::F 节 P1 待办` 改写：P1-Egress-wire（仅剩 rerank/embedder）/ P1-Streaming-breaker（仅缺 RecordFailure）
- [ ] `must/working-agreement.md::已接线` 表 RedisRateLimiter / tiktoken / 双估算器 三项可下沉为"历史回溯"
- [ ] `must/working-agreement.md::工具分发拆分` 把 "9 处" 修订为 "18+ 文件含硬编码工具名"
- [ ] 在 `docs/architecture/09_orchestrator.md::§11 P0` 把 ORC-1 / ORC-3 / shutdown-drain 标 file:line 并保留
