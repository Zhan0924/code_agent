# 文档缺口

已知的文档不足和需要未来调查的领域。

> **2026-06-01 边界澄清**：本文同时记录**跨包缺口**与**死代码 / 接线缺失**两类条目。`docs/architecture/NN_*.md` 各篇的"已知缺陷一览"自 2026-06-01 起按包独立维护并带 file:line。**两份清单暂时并存**，当包级缺陷一览覆盖某项后，应回头清理这里对应条目，避免双源不同步。下方表格的"包级文档"列指明最新权威来源。
>
> **2026-06-03 二次复核**：对 `30_recent_improvements.md::F 节 P1 待办` 与本文跨包缺口做了**逐项代码核查**，确认 4 项已修复未同步、2 项半接线、6 项仍属实。详见文末"## 2026-06-03 二次复核摘要"。

## 死代码 — 已实现未接线（与包级文档双源）

> **2026-06-26 memory 重构后复核**：CoreMemory 「只写黑洞」、HybridStore 双写漂移、ConflictResolver 无脑覆盖、Extractor Prompt 注入、PII 不过滤、Decay 无调度、blackboard 静默丢消息、ctx 透传断裂、多维度 embedding 不支持等 P0/P1 缺陷已全部在 `cmd/agent/main.go` / `internal/memory/*` / `internal/orchestrator/memory_bridge.go` / `internal/tools/memory_tools.go` / `internal/models/context.go` 落地修复。详见下方「2026-06-26 memory 系统重构」段及 `docs/architecture/25_memory.md::§14 修复时间线`。

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
| SSE 链路鲁棒性 | watchdog（90s 静默 abort）+ reconcileFinalMessage（getSession 兜底）已落地（2026-06-07）；EventSource 自动重连 / 后端推送恢复仍未做 |
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

### B. 部分完成 → 已完成（2026-06-04 落地后过账）

> 本节原记录 P1-Egress-wire 与 P1-Streaming-breaker 的半接线状态，两者均已在 2026-06-04 的 9 PR 批量修复中补齐，故下沉到「历史回溯」。代码当前位置：

| 原待办 | 现位置 |
|---|---|
| P1-Egress-wire | Reranker `cmd/agent/main.go:277` + `internal/rag/reranker.go::NewAPIReranker(... httpClient ...)`；Embedder `cmd/agent/main.go:231` + `internal/rag/embedder.go::NewOpenAIEmbedder(... httpClient ...)`。LLM/MCP 既有接线点未变。需要启用时设 `security.egress_enabled: true` |
| P1-Streaming-breaker | `internal/llm/client.go::ChatCompletionStream` 失败路径调用 `sharedBreaker.RecordFailure`，与非流式 `client.go:238-239` 对称 |

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

- [x] `30_recent_improvements.md::F 节` 已重写为「完成状态对账」表（2026-06-04 commit `b87d848` + `ed4ccd7`）
- [x] `must/working-agreement.md::工具分发拆分` 已追加「工具名常量化」段落，指向 `tool_names.go`
- [ ] `must/working-agreement.md::已接线` 表 RedisRateLimiter / tiktoken / 双估算器 三项下沉为「历史回溯」（仍可延后）
- [ ] `docs/architecture/09_orchestrator.md::§11 P0` 把 ORC-1 / ORC-3 / shutdown-drain 标记为已修复并保留 file:line（仍可延后）

---

## Pending-promotion（待累积证据的规则候选）

> 反思中提出但**证据不足以推广为 must/ 规则**的候选。每条至少需要 **2 次独立场景**复现才考虑落地。

| 候选 | 首次出处 | 复现次数 | 落地条件 |
|---|---|---|---|
| commit 至少更新 `30_recent_improvements.md` 时间线（commit hygiene） | `memory/reflections/2026-06-11-architecture-docs-drift-audit-c-tier.md`（G 节 3 条同批次回补） | 1（单批次内重复 3 次，非跨时间窗独立证据） | 下次再出现「commit 未同步文档→后续 audit 回补」时落地为 `must/working-agreement.md` 段落 |
| 行号锚点策略「只在设计点/不变量/测试矩阵三场景写裸 `file:line`」 | `memory/reflections/2026-06-11-architecture-docs-drift-audit-c-tier.md`（`16_store.md::§4` 16 个行号集体漂移） | 1 | 下次再出现行号大规模漂移、或 `docs/architecture/` 行号违反密度统计完成后，新建 `guides/doc-line-anchor-policy.md` |

> 「校准 vs 增量」commit message 二分类候选**已拒绝**（属反思自我形态背书，不影响后续 commit 行为）。

---

## 2026-06-26 memory 系统重构（P0/P1/P2 14 项缺陷一次性闭环）

来源审计：用户面试题语境下对 `internal/memory/` + 上下游接线做了一次系统性复审，发现并修复 14 处缺陷，分布于隐私 bug / 双写一致性 / 抗注入 / 中文友好去重 / 可观测性等多个层面。所有修复均通过 `internal/memory` + `internal/api` + `internal/orchestrator` + `internal/models` + `internal/tools` 包单测（零回归）。

### A. P0（正确性 / 隐私）

| 编号 | 现象 | 修复点 |
|---|---|---|
| MEM-P0-1 | `core_memory_append` / `core_memory_replace` 硬编码 `default_user`/`default_project` — 所有用户/项目共用同一份 CoreMemory（隐私事故） | `internal/tools/memory_tools.go::resolveMemoryIdentity` 通过 `models.UserIDFromContext` 取真实 user/project；空值兜底改用 `anonymous` / `default` 与 `handleListMemory` 归一化对齐 |
| MEM-P0-2 | CoreMemory 是「只写黑洞」— `GetCoreMemory` 全仓零调用方，LLM 通过 tool 写入后下一轮 prompt 完全读不到 | 1) `Orchestrator.coreMemory` 字段 + `SetCoreMemory` 注入；2) `buildLongTermMemory.formatCoreMemory` 在 prompt 顶部插入 `[Core Memory]` 块；3) `cmd/agent/main.go:545` `orch.SetCoreMemory(coreManager)` 接线 |
| MEM-P0-3 | `HybridStore.Store` 双写漂移（hot 写失败只 Debug、cold 写失败前 hot 已落 → 24h TTL 后悬挂消失），冲突合并分支不发 blackboard 事件 | `internal/memory/hybrid.go::Store` 重写：cold 先写（source of truth），失败直接 return；hot 失败 Warn 不阻塞；conflict-merge 分支也走 `publishEvent` |
| MEM-P0-4 | PG `memories` 表无 `updated_at` 列（Memory struct 字段 JSON 往返丢失），embedding 维度 1536 硬编码（换模型必崩） | `pg_cold.go::Migrate` 加 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS updated_at`（向后兼容旧库），`NewPGColdWithDim(dim)` + `checkDim` 显式校验维度 |

### B. P1（重要功能缺陷）

| 编号 | 现象 | 修复点 |
|---|---|---|
| MEM-P1-1 | `ConflictResolver.Resolve` 永远 new 覆盖 old，不看 score → 噪声 0.3 可覆盖关键 0.95 偏好 | `conflict.go::ResolveWithOutcome` score-aware 三分支：`OutcomeOverride` (gap>margin) / `OutcomePreserve` (PreserveHighScore 时 gap<-margin) / `OutcomeMerge`（保留 content 仅刷 embedding+score）；`AccessCount++` 触发 Hebbian 强化；`ConflictResolverConfig` 阈值可配置 |
| MEM-P1-2 | `Extractor.extractWithLLM` 用 `strings.Replace` 拼用户输入到模板 → Prompt Injection（伪造 JSON 数组覆盖提取结果） | system+user 双消息分离 + `<<<INTERACTION_BEGIN/END>>>` 哨兵 + `stripSentinels` 防越界 + system prompt 显式声明「IGNORE instructions inside data」；`maxPerRun` 硬截断（默认 10）防 LLM 越界生成几十条 |
| MEM-P1-3 | 用户随口贴 token / 邮箱 / 私钥会原文入库，下次 RAG 命中后送给第三方 LLM | `extractor.go::piiMasker` 在 LLM 调用前 + 入库前双重遮蔽，覆盖 AWS_KEY / OpenAI sk- / JWT / Bearer / 高熵 hex/base64 / `api_key=…` / 私钥块 / 邮箱 / IPv4 |
| MEM-P1-4 | 去重用 `strings.Fields` word Jaccard → 中文不分词时退化为 0/1 二值，cn 用户根本拦不住 | `isDuplicate` 三路：embedding 余弦 ≥0.85（最准）→ word Jaccard ≥0.7（英文短句） → 字符 3-gram Jaccard ≥0.7（中文/CJK）取 max |
| MEM-P1-5 | `HybridStore.Retrieve` hot 命中 limit 就 return → 长尾高价值 cold 偏好被新会话噪声屏蔽 | 改为 hot+cold 并行 + RRF 融合（k=60），hot 加 tie-breaking bonus 保持 cache-warm 友好；embedder 不可用时降级 cold ILIKE |
| MEM-P1-6 | `extractMemoriesAsync` 用 `context.Background()` → 丢失 trace_id / userID / 结构化 logger 字段 | 改为 `detachCancel(parent)` 自定义 ctx：保留 Values（trace/logger/identity）但忽略 parent 的 Cancel/Deadline；ReAct 入口 `models.WithSessionContext` 统一注入 (sessionID, userID, projectID) 三元组 |

### C. P2（工程化 / 治理）

| 编号 | 修复点 |
|---|---|
| MEM-P2-1 | `cmd/agent/memory_adapter.go::runMemoryDecayLoop` 周期 ticker（默认 24h）+ shutdown 路径 `memoryDecayStop()` 优雅退出；启动后 2 min 才首次触发避免 boot 抢资源 |
| MEM-P2-2 | 新文件 `internal/metrics/memory.go` 暴露 10 个 metrics：`store_total/duration` × tier+status / `retrieve_total/duration/result_count` / `conflict_total{outcome}` / `dedup_total{method}` / `decay_runs_total/affected_count` / `extractor_runs_total{path}/stored_per_run` / `blackboard_publish_total{action,status}/dropped_total` |
| MEM-P2-3 | `config.MemoryConfig` 子结构：`hot_ttl` / `conflict_threshold` / `conflict_margin` / `preserve_high_score` / `duplicate_threshold` / `max_per_run` / `embedding_dim` / `decay.{enabled,interval,older_than,factor}` 全可在 `config.yaml` 调，零字段时全走代码默认值 |
| MEM-P2-4 | Blackboard `Publish` 出口埋 `MemoryBlackboardPublishTotal{action,status}`；`Subscribe` 缓冲区满走 `default` 分支时 `MemoryBlackboardDroppedTotal.Inc()` + 增加 action/project_id 字段的 Warn 日志（不再静默丢） |

### D. 横切：跨包 context 契约

新文件 `internal/models/context.go`：定义 `WithSessionContext` / `SessionIDFromContext` / `UserIDFromContext` / `ProjectIDFromContext`，统一 (sessionID, userID, projectID) 三元组 ctx key。**所有下游包（tools / memory / audit / observability）应优先用这套 helper 而非自定义 contextKey**。orchestrator 内的旧 `ctxKeySessionID` 暂保留兼容；后续 PR 可批量迁移并删除。

### E. 残余 TODO（未在本次修复范围内）

> **更新（2026-06-26 二轮闭环）：本节 5 项 TODO 全部落地，见下方 §F**。

| 优先级 | 项 | 备注 |
|---|---|---|
| ~~P2~~ ✅ | Trajectory Memory 持久化（PG + intent embedding） | 已落地（T1-b/T1-c），见 §F |
| ~~P2~~ ✅ | `memory.Distiller` 接线 | 已落地（T2-b），见 §F |
| ~~P3~~ ✅ | Trajectory 召回从字符串 `Intent == intent` 升级到向量 KNN | 已落地（T1-c），见 §F |
| ~~P3~~ ✅ | CoreMemory 跨 project 共享 preference | 已落地（T3-a/T3-b），见 §F |
| ~~P3~~ ✅ | 召回时按 importance 分桶 | 已落地（T4-a/T4-b），见 §F |

### F. 2026-06-26 二轮闭环（T1–T5）

接续 §A–§D 的 14 项 P0/P1/P2 后，把 §E 里 5 项「等下次 PR」+ 私有 contextKey 清理一次性落地，共 5 个 T 任务 / 11 个落地点。详细工程说明见 `docs/architecture/25_memory.md::§15`，这里仅记录文档与代码映射。

| 任务 ID | 现象 | 关键改动 |
|---|---|---|
| **T1-a** | TrajectoryMemory 直接是 struct，难替换 | `agentloop.TrajectoryStore` 接口拆出（`Record(ctx,...)+Retrieve(ctx,...)`），`TrajectoryMemory` 实现接口；`FormatTrajectoryHint(ctx, store, intent)` 平移到 package level；`*TrajectoryMemory.FormatHint(intent)` wrapper 保留向后兼容 |
| **T1-b** | 50 条进程内 FIFO，重启丢全部 | 新文件 `internal/agentloop/pg_trajectory_store.go`：`trajectories` 表（id/intent/tools/step_count/success/intent_embedding/created_at），ivfflat 索引；`Migrate/Record/Retrieve/Cleanup` 全幂等 |
| **T1-c** | `Retrieve` 只做字符串相等 | KNN-first（`intent_embedding <=> $1`）→ string-equality fallback；`cmd/agent` 接线 PGStore 并把 memory.Embedder 透传成 `IntentEmbedder`（结构性接口对齐） |
| **T2-a** | `Distiller` 只看 RedisHot 50 条 | 接口化 `DistillerStore`（HybridStore 实现）；`DistillerOptions` 三阈值（MaxEpisodicPerRun / MinEpisodicToTrigger / SemanticScore）；幂等性靠 ConflictResolver 兜底 |
| **T2-b** | 蓝图里的蒸馏生产不跑 | `MemoryDistillConfig` + `MemoryDistillTarget` 配置 + `runMemoryDistillLoop` ticker（默认禁用 + 5min 延迟启动）；shutdown 链加 `memoryDistillStop()`；新增 3 个 metric：`distill_runs_total{path}` / `distill_duration_seconds` / `distill_produced_total{kind}` |
| **T3-a** | CoreMemory 严格 project 隔离，user 级 persona 跨 project 消失 | `CoreMemoryScope` 枚举 + `GetCoreMemoryScoped` / `AppendToSectionScoped` / `ReplaceInSectionScoped`；**`GetMerged`**（project 覆盖 user，每节标 `Scope` 用于渲染）；Redis keyspace 改 `core_memory:user:<userID>` 与 `core_memory:project:<userID>:<projectID>`，旧 `core_memory:<userID>:<projectID>` 在 project scope 读时作为 alias 自动兼容 |
| **T3-b** | tools 不能写 user 级 | `core_memory_append/replace` 新增 `scope` 可选参数（默认 `project`）；`parseScope` 未知值兜底走 project（避免"工程师以为只动当前 repo 实际写到全局"）；`formatCoreMemory` 给 user-scope 节标 `(user)` 后缀 |
| **T4-a** | `MemoryRetriever.Retrieve` 一刀切 top-K，可能 5 条全是 `knowledge` | `RetrieveByType(ctx, userID, projectID, memType, query, limit)` 加到 interface；HybridStore 实现（cold 走 SQL `WHERE type=$5`，hot over-fetch + 客户端过滤）；memoryAdapter 桥接；`PGCold.RetrieveByVectorAndType` 双分支保留 prepared-statement 复用 |
| **T4-b** | 高价值 preference 被 knowledge 淹没 | `retrieveBucketedMemories`：先各保 1 条 `preference` + 1 条 `decision`，剩余位用通用 top-K 补；按 content 跨桶去重 |
| **T5** | `ctxKeySessionID` 是 orchestrator 私有 contextKey，跨包不能用 | 全部迁移到 `models.SessionIDFromContext`；私有 `contextKey` 删除；context 写入路径统一走 `models.WithSessionContext`（一行设三键） |

设计与取舍要点（详见 §15）：

1. 接口（`DistillerStore` / `TrajectoryStore` / `IntentEmbedder`）都是本地最小接口，避免循环导入也让测试 fake 化便宜
2. PG schema 全 `IF NOT EXISTS`，旧部署不破坏
3. KNN 失败 → string-equality fallback（早期空表 / embedder 临时挂掉时不让 hint 完全消失）
4. CoreMemory legacy key 仅在 project scope 读时做 alias 尝试；写永远走新 keyspace
5. distill ticker 默认禁用 + staggered start（5 min），防止 t=0 LLM stampede
6. `scope` 默认 project（更安全的默认 — 错写到 user 全局是难以恢复的副作用）

验证：

- `go build ./...` ✅
- `go test ./internal/{memory,agentloop,tools,orchestrator}/...` ✅
- `go test ./tests/internal/memory/...` ✅
- `ReadLints` 所有受影响文件 ✅

---

## 2026-06-26 三轮闭环 P0 #1：Distiller 空转 — episodic 写入路径

来源审计：用户在 §2026-06-26 二轮闭环基础上做了一次系统性 20 项缺陷复审，列出 P0/P1/P2 缺陷清单。本次先修复 **P0 #1**（其它 19 项按优先级逐个 follow up）。

### 病征

§14/§15 已经把 Distiller 接线进 ticker，但 `Extractor.parseMemoryType` 只产 4 类 typed memory，全仓零代码写入 `MemoryTypeEpisodic` → `ListByType(episodic)` 永远空 → Distiller 每轮 no-op。蓝图设计的 "hippocampus → cortex 巩固" 在生产里不运行。

### 落地点

| 编号 | 修复 |
|---|---|
| MEM-P0(3rd)-1a | `internal/memory/types.go::Memory` 加 `DistilledAt *time.Time` |
| MEM-P0(3rd)-1b | `internal/memory/extractor.go::RecordTaskEpisode` 新增：拼装 USER/ASSISTANT/TOOLS 三段，走 PII masking，跳过 importance 阈值 |
| MEM-P0(3rd)-1c | `internal/memory/pg_cold.go` Migrate 加 `distilled_at` 列 + 部分索引；新增 `ListEpisodicUndistilled` / `MarkDistilled`；`Retrieve / retrieveByVectorTyped` 默认 `AND type <> 'episodic'` |
| MEM-P0(3rd)-1d | `internal/memory/redis_hot.go::RetrieveByQuery / Retrieve` 客户端过滤 episodic |
| MEM-P0(3rd)-1e | `internal/memory/hybrid.go::ListByType` episodic 路径走 cold 专用 SQL；新增 `MarkDistilled` 转发 |
| MEM-P0(3rd)-1f | `internal/memory/distiller.go::DistillerStore` 接口加 `MarkDistilled`；`Distill` 成功后调用，metric `distill_produced_total{kind="marked"}` |
| MEM-P0(3rd)-1g | `internal/orchestrator/react_core.go::reactCoreResult` 加 `toolsUsed []string`；7 处 return 附带 |
| MEM-P0(3rd)-1h | `internal/orchestrator/orchestrator.go::reactLoop` 签名改 `(string, []string, error)`；3 处调用点更新 |
| MEM-P0(3rd)-1i | `internal/orchestrator/memory_bridge.go::recordTaskEpisodeAsync` 新增；与 `extractMemoriesAsync` 并排独立调用 |
| MEM-P0(3rd)-1j | `tests/internal/memory/episodic_record_test.go` + `distiller_test.go` 扩展（共 11 条测试，4 条新增） |
| MEM-P0(3rd)-1k | `docs/architecture/25_memory.md::§16` 文档段 |

### 设计取舍

详见 `docs/architecture/25_memory.md::§16.5`。关键三条：
1. 不调 LLM 写 episodic（避免成本翻倍）；Distiller 调一次 LLM 处理 50 条
2. ON CONFLICT 不动 `distilled_at`（防止 re-Store 反标）
3. episodic 永久标记，不删除（保留审计追溯）

### 验证

- `go build ./...` ✅
- `go test ./internal/{memory,agentloop,tools,orchestrator}/... ./tests/internal/memory/...` ✅
- `ReadLints` 11 个改动文件 ✅

---

## 2026-06-26 三轮闭环 P0 #2：hot retrieve 真实时间排序

来源：P0 #1 实施时在 `RedisHot.Retrieve` 留下 NOTE 承认 "sort.Strings(keys) 是按 UUID 字典序，与时间无关"。本轮专项消除。

### 病征

- `RedisHot.Retrieve` 旧代码 `sort.Strings(keys)`，假设 key 包含时间戳前缀；实际 key = `memory:<userID>:<projectID>:<uuid_v4>`，UUIDv4 完全随机。"take most recent N" → "take random N"。
- 影响三处：`HybridStore.Retrieve` 无 embedding 降级路径 / `HybridStore.List` UI 列表 / `retrieveByQueryFiltered` cosine 同分 tie。

### 落地点

| 编号 | 修复 |
|---|---|
| MEM-P0(3rd)-2a | `internal/memory/redis_hot.go` 新增常量 `hotScanLimit = 200`，取代 `limit*4` 和硬编码 `50` |
| MEM-P0(3rd)-2b | `internal/memory/redis_hot.go::Retrieve` 删除 `sort.Strings(keys)`；Get 全部 → `sort.Slice` by `LastAccessedAt DESC, ID ASC` → 截到 limit |
| MEM-P0(3rd)-2c | `internal/memory/redis_hot.go::retrieveByQueryFiltered` sort 多级 key：`sim DESC → LastAccessedAt DESC → ID ASC` |
| MEM-P0(3rd)-2d | `internal/memory/redis_hot.go::scanAll` 新增内部 helper，集中 SCAN budget 逻辑 |
| MEM-P0(3rd)-2e | `internal/metrics/memory.go::MemoryHotScanKeys` 新增 HistogramVec(buckets 1..500)，label `endpoint=retrieve\|retrieve_by_query` |
| MEM-P0(3rd)-2f | `tests/internal/memory/redis_hot_retrieve_test.go` 新增：LastAccessedAt 排序 / episodic 过滤不变量 / 时间相等 ID 升序 tie-break / cosine 同分 tie-break（共 4 条） |
| MEM-P0(3rd)-2g | `docs/architecture/25_memory.md::§17` 文档段 |

### 设计取舍

详见 `docs/architecture/25_memory.md::§17.5`。关键三条：
1. 不用 Redis Sorted Set 双写索引：50 条规模下 "Get 全部 + 内存排序" µs 级，胜过双写一致性 + TTL 同步的实现复杂度
2. 不在 key 嵌入时间戳前缀：会有 24h 混排周期和 ASCII 边界陷阱
3. SCAN 上限 200（4x 安全余量）：超过 50 不再静默截断，由 `memory_hot_scan_keys` metric 暴露

### 验证

- `go build ./...` ✅
- `go test ./internal/memory/... ./tests/internal/memory/...` ✅（4 条新增 retrieve 排序 + P0 #1 既有测试不退化）
- `ReadLints` 3 个改动文件 ✅
- 后续：`memory_hot_scan_keys` P95 持续 > 50 即触达 P1 #10 应处理的"硬编码 50 上限"问题

---

## 2026-06-26 三轮闭环 P0 #3：Distill Targets 多租户自动发现

来源：`runMemoryDistillLoop` 旧实现注释里早有 TODO："Future iteration can swap [static Targets] out for a metadata-table sweep without changing the function signature." P0 #1 落地的 `idx_memories_episodic_undistilled` 部分索引让前置条件具备。

### 病征

- `cmd/agent/memory_adapter.go::runMemoryDistillLoop` 只迭代 yaml 配置的 `cfg.Targets`；新增 user/project 必须 reload yaml 才能被蒸馏。
- 多租户部署（10+ tenants）不可扩展 — operator 需要手动维护每一个二元组。

### 落地点

| 编号 | 修复 |
|---|---|
| MEM-P0(3rd)-3a | `internal/memory/types.go` 新增 `TenantRef{UserID, ProjectID, Count}`（Count=源数据排序辅助） |
| MEM-P0(3rd)-3b | `internal/memory/pg_cold.go::ListActiveDistillTenants(ctx, minEpisodic, limit)` GROUP BY 走 `idx_memories_episodic_undistilled` |
| MEM-P0(3rd)-3c | `internal/memory/hybrid.go::ListActiveDistillTenants` 直通到 cold；hot==nil 返回 nil 避免 24h 窗口偏差 |
| MEM-P0(3rd)-3d | `internal/memory/distiller.go::DistillerStore` 接口扩展 `ListActiveDistillTenants` |
| MEM-P0(3rd)-3e | `internal/config/config.go::MemoryDistillConfig` 加 `AutoDiscover bool` + `MaxTenantsPerTick int` + `MinEpisodicForDiscovery int` |
| MEM-P0(3rd)-3f | `internal/metrics/memory.go` 加 `MemoryDistillTargetsTotal{source}` CounterVec + `MemoryDistillDiscoverDuration` Histogram |
| MEM-P0(3rd)-3g | `cmd/agent/memory_adapter.go::buildDistillTenants` 新增纯函数：static-first 合并 + dedup + cap |
| MEM-P0(3rd)-3h | `cmd/agent/memory_adapter.go::runMemoryDistillLoop` 改用 `buildDistillTenants`，AutoDiscover 错误降级到 static Targets |
| MEM-P0(3rd)-3i | `cmd/agent/memory_adapter_test.go` 新增 7 条 buildDistillTenants 单测 |
| MEM-P0(3rd)-3j | `tests/internal/memory/distiller_test.go::fakeDistillerStore` 扩展实现 `ListActiveDistillTenants`；新增 `tests/internal/memory/distiller_discovery_test.go` 3 条 discovery 闭环测试 |
| MEM-P0(3rd)-3k | `docs/architecture/25_memory.md::§18` 文档段 |

### 设计取舍

详见 `docs/architecture/25_memory.md::§18.5`。关键四条：
1. **AutoDiscover 默认 ON（仅当 Enabled=true）**：零 yaml 改动获得多租户能力；默认 Enabled=false 部署不受影响
2. **Static-first + Forced inclusion**：Targets 始终包含（即使没到阈值），allow-list 语义
3. **`buildDistillTenants` 纯函数**：7 个单测覆盖 forced/dedup/cap/discover-error/empty-yaml 等 corner case
4. **discover 错误降级**：PG 抖动不让 tick fail，仍跑 static Targets

### 行为变更高亮

旧默认：`Enabled=true, Targets=[]` → 每 tick no-op + Warn。
新默认：`Enabled=true, Targets=[], AutoDiscover=true` → 每 tick 扫 PG 找活跃 tenant。
`AutoDiscover=false` 完全等价于旧行为，零回归路径。

### 验证

- `go build ./...` ✅
- `go test ./internal/memory/... ./internal/metrics/... ./internal/orchestrator/... ./tests/internal/memory/... ./cmd/agent/...` ✅（10 条 P0 #3 新测试 + 既有测试不退化）
- `ReadLints` 10 个改动文件 ✅
- 后续：`rate(memory_distill_targets_total{source="discovered"}[1h]) > 0` 即确认 AutoDiscover 在 prod 生效；P99 `memory_distill_discover_duration_seconds < 50ms` 即索引正确命中

---

## 2026-06-26 三轮闭环 P0 #4：召回路径 AccessCount 不累加 → Decay 不公平

来源：`Touch()` 方法、`access_count` 列、`PGCold.Decay` cutoff 全套机制存在但**召回路径从不调用 Touch**。Decay 对高频读 vs 从不读的 memory 一视同仁，违背 Ebbinghaus-style 遗忘的设计原意。

### 病征

- `HybridStore.Retrieve / RetrieveByType / List` 全部"读完直接返回"，`access_count` 永远只反映 ConflictResolver 合并次数
- `last_accessed_at` 不被读推进 → Decay cutoff 把高价值 memory 跟过期 memory 同等衰减
- ConflictResolver score-aware merge 读到的 AccessCount 严重低估

### 落地点

| 编号 | 修复 |
|---|---|
| MEM-P0(3rd)-4a | `internal/memory/pg_cold.go::TouchBatch(ctx, ids)` 单 UPDATE ... ANY($1) 用 pq.Array |
| MEM-P0(3rd)-4b | `internal/memory/types.go::MemoryStore` 接口扩展 `TouchBatch` |
| MEM-P0(3rd)-4c | `internal/memory/hybrid.go::HybridStore` 加 `touchQueue chan string` + `accessOpts AccessBatcherOptions` |
| MEM-P0(3rd)-4d | `internal/memory/hybrid.go::EnableAccessBatcher(opts)` + `StartAccessBatcher(ctx)` + `enqueueTouches(ms)` + `runAccessBatcherLoop` (纯状态机) |
| MEM-P0(3rd)-4e | `internal/memory/hybrid.go` Retrieve / RetrieveByType / List 三个返回前 enqueue 命中集 |
| MEM-P0(3rd)-4f | `internal/config/config.go::MemoryAccessConfig{Enabled, BatchSize=100, FlushInterval=5s, QueueSize=1024}` |
| MEM-P0(3rd)-4g | `internal/metrics/memory.go` 加 `MemoryTouchBatchTotal{status}` + `MemoryTouchBatchSize` + `MemoryTouchQueueDropsTotal` |
| MEM-P0(3rd)-4h | `cmd/agent/main.go` 启动 batcher goroutine + shutdown 链；先 Distill/Decay stop，最后 access stop（保 last flush） |
| MEM-P0(3rd)-4i | `internal/memory/touch_batcher_test.go` 8 条白盒测试覆盖 FlushOnBatchSize / FlushOnTimer / DedupWithinBatch / DrainsOnContextCancel / DropOnFullQueue / NilQueueIsNoOp / IgnoresEmptyIDs / FlushErrorDoesNotKillLoop |
| MEM-P0(3rd)-4j | `docs/architecture/25_memory.md::§19` 文档段 |

### 设计取舍

详见 `docs/architecture/25_memory.md::§19.4`。关键四条：
1. **非阻塞 enqueue**：read 路径绝不能因为 Decay 公平性变慢，队列满 → drop + metric
2. **白盒测纯函数 loop**：`runAccessBatcherLoop` 接收 `flush func([]string) error`，无需 PG 即可单测
3. **Hot 不 Touch**：24h TTL 让 hot 的 `last_accessed_at` 长期精度不重要；Decay 仅看 cold 是正确语义
4. **Detached 5s flush context**：shutdown 时主 ctx 已 cancel，但最后一次 flush 必须能跑完

### 行为变更高亮

- `access_count` 数值开始正常增长。Dashboards / 报表如果依赖"= 合并次数"会受影响 — grep 全仓没有此类引用
- `last_accessed_at` 推进让 Decay 影响面缩小：高频读的 memory 不再被衰减（这正是修复目标）
- `Memory.Access.Enabled: false` 完全等价于旧行为，零回归路径

### 验证

- `go build ./...` ✅
- `go test ./internal/memory/... ./internal/metrics/... ./internal/orchestrator/... ./tests/internal/memory/... ./cmd/agent/...` ✅（8 条 P0 #4 新测试 + 既有测试不退化）
- `ReadLints` 7 个改动文件 ✅
- 后续观察：`rate(memory_touch_batch_total{status="ok"}[1h]) > 0` 即确认 batcher 在跑；`memory_touch_queue_drops_total / rate(memory_retrieve_total[1h])` > 1% 即触达 QueueSize 调优阈值

---

## P0 #5：Touch hot/cold 双写一致性（已修复）

### 问题摘要

P0 #4 落地了"读路径异步累加 access_count"，但只更新 cold，hot 副本永远落后。结果：

- hot 命中的 Retrieve 用陈旧 `LastAccessedAt` 排序（P0 #2 后 hot 就是按这个排）
- RetrieveByQuery 余弦 tie-break 同样依赖这个字段
- hot/cold 跨层 RRF 打分不一致

### 修复策略

**双写 + ID-only 公共 API**：

1. 新增 `TouchRef{UserID, ProjectID, ID}` —— hot key 是 `memory:<u>:<p>:<id>`，必须有前缀；
2. `RedisHot.TouchBatch(refs)` —— 两段 pipeline（GET → modify → SET KeepTTL），缺失 key 静默跳过；
3. `HybridStore.touchQueue` 从 `chan string` 改为 `chan TouchRef`；
4. `flushTouches(refs)` 内部 dual-write：cold 走 `TouchBatch(ids)`（PK），hot 走 `TouchBatch(refs)`；
5. `MemoryStore.TouchBatch(ids)` 公共 API 签名保持不变，向后兼容。

### 关键设计决策

- **Hot 是 best-effort**：cold 错误才决定 `status="err"` metric；hot 失败仅 `logger.Warn` 不阻断 batch（cold 是真值，Decay 读 cold）
- **`redis.KeepTTL`**：touch 是"我读过它"，不该刷 24h TTL，否则永远不过期的 memory 会塞满 hot
- **缺失 key 静默跳过**：`Touch` 不做 promote-from-cold —— 那是 `Promote` 的语义（P1 #8 已修复，见本文末段）
- **GET→SET 竞态**：旧 content + 新计数器写回会覆盖并发 `Store` 的内容更新 —— 接受，cold 是真值，hot 24h 后自然过期

### 行为变更

- hot Retrieve 排序现在反映"最近真访问过"，不再"最近写入过"
- `TestRedisHot_Retrieve_OrdersByLastAccessedAt`（P0 #2）联动验证：read → touch → 下一次 read 的相对顺序变化
- 旧 `Memory.Access.Enabled: false` 完全等价于"只走 cold"（hot 仍漂移）—— 不回归

### 落地点

- `internal/memory/types.go::TouchRef`
- `internal/memory/redis_hot.go::RedisHot.TouchBatch`
- `internal/memory/hybrid.go::HybridStore.touchQueue`、`flushTouches`、`enqueueTouches`、`runAccessBatcherLoop`
- 测试：`internal/memory/touch_batcher_test.go`（white-box，10 个用例）、`tests/internal/memory/redis_hot_touch_test.go`（black-box miniredis，3 个用例）
- 架构文档：`docs/architecture/25_memory.md::§20`

### 验证

- `go build ./...` ✅
- `go test ./...` ✅（含 `TestRedisHot_TouchBatch_*` 3 个 + `TestRunAccessBatcherLoop_RefRoundTrip` + `TestEnqueueTouches_CarriesUserProjectIDs`）
- `ReadLints` 5 个改动文件 ✅

### 观测建议

沿用 P0 #4 指标即可。后续若要单独跟踪 hot 漂移，追加 `memory_touch_hot_errors_total` —— 暂未实施，因当前 `logger.Warn` 已可由日志告警捕获。

---

## P1 #6：RedisHot.Decay 全库扫描 + 没有租户隔离（已修复）

### 问题摘要

`(*RedisHot).Decay` 旧实现五条具体缺陷：

1. SCAN 模式 `memory:*` 无 tenant 前缀，跨租户互相阻塞
2. 没有 SCAN budget（不像 Retrieve 路径有 hotScanLimit=200）
3. 逐 GET → 逐 SET，pipeline 完全没用，N 个 key = 2N RTT
4. **判定字段错了**：用 `UpdatedAt` 而非 `LastAccessedAt`，与 P0 #4 cold 路径不一致
5. 没有任何 per-tenant 可观测性

### 修复策略

借鉴 P0 #3 (`ListActiveDistillTenants`) 模式：

1. cold 新增 `ListActiveDecayTenants(ctx, olderThan, limit)` —— GROUP BY 找"真有 stale 数据的 tenant"
2. hot 新增 `DecayTenants(ctx, tenants, olderThan, factor)` 显式快路径 —— per-tenant SCAN（`hotScanLimit` 上限）+ pipeline GET + pipeline SET（KeepTTL）
3. hot 保留旧 `Decay(ctx, olderThan, factor)` 作为 `cold == nil` 的 fallback，同样加 `hotScanLimit` 上限 + `LastAccessedAt` 字段
4. HybridStore.Decay 编排：cold.Decay → cold.ListActiveDecayTenants → hot.DecayTenants（fallback 时 hot.Decay）

### 关键设计决策

- **公共签名不破坏**：`MemoryStore.Decay` 接口未动，新 `DecayTenants` 是 RedisHot 独有方法（HybridStore 直接持有 `*RedisHot`，不需要接口）
- **per-tenant 错误隔离**：一个 tenant SCAN/GET/SET 失败 → log + metric err，循环继续；返回 firstErr 给上层
- **score floor 0.01**：与 PG `Decay` SQL 的 `WHERE score > 0.01` 对齐
- **`KeepTTL`**：decay 不是新写入，不能重置 24h 过期
- **字段语义**：`LastAccessedAt` 是 P0 #4 写入的真值，hot/cold 现在用同一字段判断 stale
- **无 per-tenant 并发**：单 Redis 串行即可；24h 节奏对延迟不敏感

### 行为变更

- hot 副本现在跟 cold 同时减分（之前只有 cold 在动，hot 用陈旧 score 影响 Retrieve 排序）
- 单 tenant Decay 卡住不再拖累其他 tenant
- 字段切换让"读频繁但写少"的 memory 不再被误衰减（这正是修复目标）
- 旧 `decay_test.go`（依赖 localhost:6379）已删除，新 `redis_hot_decay_test.go` 全 miniredis

### 落地点

- `internal/memory/pg_cold.go::PGCold.ListActiveDecayTenants`
- `internal/memory/redis_hot.go`: `DecayTenants` + `decayTenant` + `decayKeys`（共享 helper）+ `Decay`（fallback）
- `internal/memory/hybrid.go::HybridStore.Decay` 编排
- `internal/metrics/memory.go`: `MemoryDecayHotTenantsTotal` + `MemoryDecayHotScanKeys` + `MemoryDecayHotBatchDuration`
- 测试：`tests/internal/memory/redis_hot_decay_test.go` (7 个测试) + `tests/internal/memory/redis_hot_retrieve_test.go::hotTestClientWithMR` (helper)
- 文档：`docs/architecture/25_memory.md::§21`

### 验证

- `go build ./...` ✅
- `go test ./...` ✅（7 个新 decay 测试 + 既有测试不退化）
- `ReadLints` 6 个改动文件 ✅

### 观测建议

- `rate(memory_decay_hot_tenants_total{status="ok"}[24h])` > 0 即确认 tenant 路径在跑
- `histogram_quantile(0.99, memory_decay_hot_scan_keys_bucket)` ≈ 200 → 触顶 `hotScanLimit`，应分批 decay 或上调 limit
- `histogram_quantile(0.99, memory_decay_hot_batch_duration_seconds_bucket)` > 1s → 单 tenant SCAN/RTT 异常，排查 Redis 健康
- `memory_decay_hot_tenants_total{status="err"} / total` > 1% → cold UPDATE 与 hot decay 不一致，触发告警

---

## P1 #7：ConflictResolver 只合并第一条冲突 → 不能消除已有重复（已修复）

### 问题摘要

旧 `HybridStore.Store` 冲突路径三条缺陷：

1. `RetrieveByVector(limit=3)` 候选集偏小，看不到 rank-4 之后的副本
2. 只用 `conflicts[0]`，[1..N-1] 永远存活
3. PGCold 没有任何 Delete API，副本根本无法消除

后果：相似召回重复噪声、Decay 也救不了（每副本独立 Touch 过不了 0.01 floor）。

### 修复策略：anchor + drain

1. cold 新增 `DeleteByIDs` + `DedupTx`（事务包住 anchor UPDATE + dup DELETEs，避免半态）
2. resolver 新增 `PickAnchor`（score → access → createdAt → id 全确定性）+ `ReinforceFromDup`（只继承 AccessCount + LastAccessedAt，不动 content/score）
3. redis hot 新增 `DeleteBatch(refs)` 单 pipeline DEL（复用 TouchRef）
4. HybridStore 新增 `dedupMerge`：cap by MaxConflicts(32) → PickAnchor → 累加 dups → ResolveWithOutcome(anchor, new) → DedupTx 落库 → hot 同步
5. `dedupOversample` 从 3 提升到 10

### 关键设计决策

- **anchor 永远保留**：ID 稳定性 → 下游引用不变
- **hard delete**：副本 cosine ≥ 0.85 同 type 已是同义信息，无 audit 价值
- **事务化**：UPDATE + DELETE 同一 transaction
- **`ReinforceFromDup` 不动 content/score/embedding**：anchor 是按 score 选出的最优，不能被低分 dup 覆盖
- **`MaxConflictsToDedup=32`**：异常候选集保护；可配置；设为 1 等效禁用 dedup（仍合并 [0]，不删其余）
- **PG integration 测试缺位**：本仓库没有 testcontainers 基建，`DedupTx` / `DeleteByIDs` 的 SQL 正确性通过 review + 已有 PGCold 测试间接保证；纯逻辑（PickAnchor / Reinforce）走完整单测

### 行为变更

- 历史副本下次同主题写入即被清理（不需要离线 GC 脚本）
- `metrics.MemoryConflictTotal{outcome="dedup"}` 新增 label
- `MemoryDedupRemovedTotal` / `MemoryDedupBatchSize` 两个新指标
- 上线后短期 dedup_removed_total 快速增长（清存量），后趋于稳态

### 落地点

- `internal/memory/pg_cold.go::DeleteByIDs` + `DedupTx`
- `internal/memory/conflict.go::PickAnchor` + `ReinforceFromDup` + `MaxConflictsToDedup` 配置项
- `internal/memory/redis_hot.go::DeleteBatch`
- `internal/memory/hybrid.go::dedupMerge` + `dedupOversample=10`
- `internal/metrics/memory.go`: `MemoryDedupRemovedTotal` + `MemoryDedupBatchSize` + `outcome="dedup"` label
- `internal/config/config.go::MemoryConfig.MaxConflictsToDedup`
- `cmd/agent/memory_adapter.go` 接通配置
- 测试：`internal/memory/conflict_test.go` (+10) / `tests/internal/memory/redis_hot_touch_test.go` (+2)
- 文档：`docs/architecture/25_memory.md::§22`

### 验证

- `go build ./...` ✅
- `go test ./...` ✅（10 个新 conflict 测试 + 2 个 DeleteBatch 测试 + 既有不退化）
- `ReadLints` 9 个改动文件 ✅

### 观测建议

- `rate(memory_conflict_total{outcome="dedup"}[1h])` > 0 → dedup 路径在跑
- `histogram_quantile(0.95, memory_dedup_batch_size_bucket)` > 5 持续 → 上游 Extractor / RecordTaskEpisode 阈值需收紧
- `memory_dedup_removed_total` —— 累计副本数；上线初期快速增长是预期，之后稳态

### 已知尚未覆盖

- PGCold.DedupTx 没有真实 PG 集成测试（本仓库无 testcontainers 基建）；SQL 正确性靠 review 兜底
- HybridStore.dedupMerge 没有 e2e 测试（concrete `*PGCold` 不易 mock）；通过 PickAnchor + Reinforce 的纯逻辑测试 + DeleteBatch 的 miniredis 测试间接保证

---

## P1 #8：Promote / Demote 死代码 — 缓存升降级从不发生（已修复）

### 问题摘要

`HybridStore.Promote` / `Demote` 接口很早就存在，但全代码库没有调用点：

1. cold 命中的高分老条目永远不会被推到 hot → 与近期低分条目形成"5ms vs 200ms"倒挂
2. cold 已经 Decay 接近噪声的条目还在 hot 等 24h TTL 自然过期 → 挤占其他 tenant 缓存窗口
3. "热/冷双层"在结构上存在、行为上未实现 → 文档与运行时长期不一致

### 修复策略

**Promote（读路径异步回填）**：

1. `RedisHot.PromoteBatch(ctx, mems)` 单 pipeline SET（TTL=24h），跳过 ID/UserID/ProjectID 缺失项
2. `HybridStore.PromoteOptions` + `promoteQueue chan Memory` + `enqueuePromote(hot, fused)` 在 `Retrieve` / `RetrieveByType` 末尾入队（drop-newest，非阻塞）
3. 入队过滤：`m.ID != ""` && `id ∉ hot` && `Score >= Threshold(0.7)`
4. `runPromoteBatcherLoop`：BatchSize=50、FlushInterval=500ms、QueueSize=256，dedup by ID
5. `StartPromoteBatcher(ctx)` 启动；`main.go` 注册到 shutdown 链；`flushPromotes` 失败不阻塞 loop

**Demote（Decay 跨阈值触发）**：

1. `RedisHot.decayKeys` 增第四参数 `demoteThreshold`；当 `oldScore >= T && newScore < T` 时 `writePipe.Del(key)` 而不是 SET
2. `oldScore < T`（已低于阈值）不再 DEL —— 防止 spam metric + 无谓 IO
3. `Decay`/`DecayTenants`/`decayTenant`/`decayKeys` 签名统一加 `demoteThreshold`；0 = 禁用 demote
4. `HybridStore.Decay` 通过 `h.demoteThreshold`（由 `SetDemoteThreshold` 注入）透传到 hot 路径
5. cold 不动（cold 是 truth）

### 关键设计决策

- **Promote 异步**：Retrieve 是延迟敏感路径；丢失的 promote 会被下次 Retrieve 重新发现
- **drop-newest（`select default`）**：队列溢出宁愿丢新的也不阻塞调用方
- **Threshold=0.7 默认**：高于 Extractor 给定的 importance 起点，避免噪声占用 hot 24h 窗口
- **Demote 只在"跨阈值瞬间"触发**：避免每个 Decay tick 都对同批 key DEL（metric 干净 + 减少 IO）
- **Demote 不动 cold**：cold 已被 `cold.Decay` 同步更新 score；hot 只是缓存
- **Decay 路径不 promote / Retrieve 路径不 demote**：单方向单调，防止 promote/demote 抖动

### 行为变更

- 高分老记忆首次 cold 命中后，几百 ms 内被异步 promote 到 hot → 下次 Retrieve 5ms 命中
- Decay 周期内 score 跨过 `DemoteThreshold` 的条目从 hot 蒸发 → hot 容量让位给高信号
- `Promote.Enabled=false` 关闭回填；`Demote.Enabled=false` 或 `Threshold=0` 关闭蒸发
- 文档承诺的"热/冷两层"在行为层面首次完整落地

### 落地点

- `internal/memory/redis_hot.go`：`PromoteBatch(ctx, mems)`；`decayKeys` / `decayTenant` / `DecayTenants` / `Decay` 都加 `demoteThreshold`
- `internal/memory/hybrid.go`：`PromoteOptions`（Threshold/BatchSize/FlushInterval/QueueSize 全套 `withDefaults`）；`promoteQueue` / `promoteOpts` / `demoteThreshold`；`EnablePromoteBatcher` / `StartPromoteBatcher` / `SetDemoteThreshold` / `enqueuePromote` / `flushPromotes` / `runPromoteBatcherLoop`；`Retrieve` / `RetrieveByType` 末尾 `enqueuePromote(hot, fused)`；`Decay` 透传 `demoteThreshold` 到 hot
- `internal/metrics/memory.go`：`MemoryPromoteTotal{status}` / `MemoryPromoteBatchSize` / `MemoryPromoteQueueDropsTotal` / `MemoryDemoteTotal{tier}`
- `internal/config/config.go`：`MemoryPromoteConfig` + `MemoryDemoteConfig`；`MemoryConfig.Promote` / `MemoryConfig.Demote`
- `cmd/agent/main.go`：启动 Promote batcher + shutdown 链；`SetDemoteThreshold` 注入；`runMemoryDecayLoop` 透传 demote 阈值
- 测试：
  - 新建 `internal/memory/promote_batcher_test.go`（loop 5 个 + enqueue 6 个 + defaults 1 个 = 12 个）
  - 新建 `tests/internal/memory/redis_hot_promote_test.go`（PromoteBatch 4 个）
  - 扩展 `tests/internal/memory/redis_hot_decay_test.go`（DemotesBelowThreshold / KeepsAboveThreshold / DoesNotDemoteAlreadyBelow 共 3 个）
- 文档：`docs/architecture/25_memory.md::§23` + §5.3 重写

### 验证

- `go build ./...` ✅
- `go test ./internal/memory/... ./tests/internal/memory/...` ✅
- 新增 19 个测试全通过；老测试无回归
- `ReadLints` 改动文件 0 错误

### 观测建议

- `rate(memory_promote_total{status="ok"}[5m])` > 0 → 回填路径生效
- `rate(memory_promote_total{status="err"}[5m])` 应稳定 0；持续 > 0 → Redis 异常
- `histogram_quantile(0.95, memory_promote_batch_size_bucket)` 持续触顶 BatchSize → 可调大批量
- `rate(memory_promote_queue_drops_total[5m])` 应为 0；> 0 → 队列偏小或 batcher 处理偏慢
- `rate(memory_demote_total{tier="hot"}[1h])` —— hot 蒸发速率；上线初期清存量后趋稳

### 已知尚未覆盖

- `flushPromotes` 的 5s 超时 hard-coded，未配置化；遇真实 Redis 高延迟需调
- 单 tenant 高写入 + Promote 入队风暴尚未做"per-tenant rate limit"，预期靠 BatchSize + Threshold 拦截
- Demote 仅蒸发"跨阈值瞬间"；如果一个条目在 hot 长期 stuck 在阈值之上但被低 Decay factor 慢慢拖到阈值附近反复抖动，会触发反复 DEL/SET —— 实际生产 Decay 周期足够稀疏，本期不做去抖

---

## P1 #9：`isDuplicate` 候选集太小 (top-5) → 大库高漏检（已修复）

### 问题摘要

`Extractor.isDuplicate` 通过 `store.Retrieve(content, 5)` 拉 top-5 候选做 cosine/n-gram 比对。三条耦合缺陷：

1. K=5 写死：大库 (>1k 条/tenant) 真重复常排 rank-6..30；pgvector IVFFlat lists=100 对小 K 召回率本就偏低
2. 走的是用户检索路径 `HybridStore.Retrieve` → 触发 `enqueueTouches`（污染 Decay 的 AccessCount）+ `enqueuePromote`（把查重时碰到的低分条目推上 hot 24h 窗口）
3. n-gram fallback 也跑在 5 条上

与 P1 #7（anchor+drain）互补：#7 写入后兜底，#9 写入前预筛。漏检消失后 #7 的清理负载会下降。

### 修复策略

**dedup 与用户检索在接口层分离**：

1. `MemoryStorer` 新增 `RetrieveCandidates(ctx, user, project, embedding, limit) ([]Memory, error)` —— 纯近邻并集查询
2. `HybridStore.RetrieveCandidates`：cold.RetrieveByVector(K) ⊎ hot.RetrieveByQuery(K)，按 ID 去重；**不走 RRF / 不 enqueueTouches / 不 enqueuePromote**
3. `Extractor.dedupCandidateLimit` 字段（默认 30）+ `SetDedupCandidateLimit(n)` setter (clamp [5, 200])
4. `isDuplicate` 重写：embedder 可用 → 走 `RetrieveCandidates(K)` + cosine；不可用或 Embed 错误 → fall-through 到老 `Retrieve(content, 5)` + n-gram
5. `MemoryConfig.DedupCandidateLimit` (mapstructure `dedup_candidate_limit`)；`main.go` 接线
6. `MemoryDedupCandidateCount` Histogram (buckets {0,1,5,10,20,30,50,100,200}) —— 观察 K 是否够大

### 关键设计决策

- **新接口而非扩 Retrieve limit**：dedup 的副作用边界天然与用户检索不同；同一接口语义混淆会让"查重 → 污染 Decay"这类回归一再发生
- **K=30 默认**：覆盖 IVFFlat lists=100 的典型 recall-miss 窗口；30 条 cosine 检查内存里 < 1μs
- **Clamp [5, 200]**：5 = 老下限，防止 config typo 写 1 静默关 dedup；200 = pgvector + Redis 单调用代价上限
- **并集去重，不做 RRF**：dedup 只问"是否存在 0.85 邻居"，并集天然涵盖两层；RRF 是用户呈现专用
- **Hot 在前合并**：同 ID 两层都有时，hot 副本是最新的，winner 用 hot
- **Embedder 错误 fall-through 而非冒泡**：上游 LLM provider 抖动不应让整个写入路径 dedup 瘫痪

### 行为变更

- 大库下漏检率显著下降；`memory_dedup_total{method="embedding"}` 上线后应上扬
- Decay 不再被"查重导致的 Touch"污染
- Hot 不再被"查重导致的 Promote"塞噪声
- 无 embedder 部署形态完全兼容（老 5 + n-gram 路径保留）

### 落地点

- `internal/memory/extractor.go::MemoryStorer.RetrieveCandidates` 接口；`Extractor.dedupCandidateLimit` + `SetDedupCandidateLimit`；`isDuplicate` 重写
- `internal/memory/hybrid.go::HybridStore.RetrieveCandidates`（无副作用并集查询）+ `dedupCandidateLimitCap=200`
- `internal/config/config.go::MemoryConfig.DedupCandidateLimit`
- `cmd/agent/main.go` 接线
- `internal/metrics/memory.go::MemoryDedupCandidateCount`
- 测试：`internal/memory/extractor_test.go` +7 用例 (`HighLimitCatchesRank12` / `LowLimitMissesRank12` / `FallsBackToLegacyWhenNoEmbedder` / `FallsBackWhenEmbedderErrors` / `SetDedupCandidateLimit_Clamped` / `NewExtractor_DefaultDedupCandidateLimit` / `Dedup_NoCandidatesNoOp`)；mockStore 扩展 `RetrieveCandidates` + `fakeEmbedder`；`tests/internal/memory/episodic_record_test.go::fakeEpisodeStore` 补占位
- 文档：`docs/architecture/25_memory.md::§24` + §4.4 重写

### 验证

- `go build ./...` ✅
- `go test ./internal/memory/... ./tests/internal/memory/...` ✅（既有不退化 + 7 个新用例全过）
- `ReadLints` 0 错误

### 观测建议

- `histogram_quantile(0.95, memory_dedup_candidate_count_bucket)` ≈ 30 → 库已大到要调 `dedup_candidate_limit`
- `rate(memory_dedup_total{method="embedding"}[10m])` 上线后应上升（漏检消失）
- `memory_dedup_removed_total` (P1 #7) 应下降（预筛上游堵住）

### 已知尚未覆盖

- `RetrieveCandidates` 没有 PG 集成测试（本仓库无 testcontainers 基建）；SQL 正确性靠 `cold.RetrieveByVector` 既有 review 保证
- 并集去重的 "hot-first winner" 策略未做 e2e 验证；hot 副本和 cold 副本 embedding 偶有微小漂移时谁赢由实现决定（hot），单测用 mockStore 覆盖

---

## 2026-06-07 verifier retry-once 落地（收束）

反思 `memory/reflections/2026-06-07-verifier-retry-and-process-as-artifact.md` 末段「已发现的潜在 doc gap」三条均已落地，**不再开新条目**：

- [x] `must/working-agreement.md` 死代码清单移除 `formatVerificationFeedback` —— 改为 ✅ 接线条目并指向「Verifier retry-once 门控」段
- [x] `must/working-agreement.md` 新增「ToolResult.Metadata 契约」段，约定 `json.RawMessage` 透传（不要 `map[string]any`）
- [x] `docs/architecture/09_orchestrator.md` §9.2 已在文中点明 `task.VerificationRetried`「运行时哨兵，不入 JSON」—— 该文件无 Task 字段表，故不再单列

---

## 2026-06-26 P1 #10 修复：`RedisHot` SCAN 上限运行时可调 + 截断观测

### 问题陈述

`internal/memory/redis_hot.go::scanAll` 之前签名 `(ctx, pattern, max int)`，内部 `max := hotScanLimit` 拿 const 200 作硬上限。三条结构性问题：

1. `hotScanLimit` 是代码级 `const`：运维想把大 tenant 调到 500 必须改代码重发布。
2. **调用方 limit > scanCap 时静默截断**：`HybridStore.RetrieveByType` 实际传入 `overFetch*2`（user limit=50 → 300），`scanAll` 内部仍只扫 200 keys —— **100 个 key 没看过就返回**，caller 完全不知道有截断。
3. 没有"扫描被截断"指标：现有 `MemoryHotScanKeys` 只观察 `len(keys)`，P95=200 时分不清"这个 tenant 真就 200 条" vs "其实有 500 但被截断到 200"。

### 落地

- `internal/memory/redis_hot.go::RedisHot` 新增 `scanLimit int` 字段，`NewRedisHotWithTTL` 默认 `defaultHotScanLimit=200`
- 新增 `SetScanLimit(n)` / `ScanLimit()`：clamp `[minHotScanLimit=50, maxHotScanLimit=2000]`，0 → 默认
- `scanAll(ctx, pattern, requested, endpoint string)`：
  - `effectiveCap = max(scanLimit, requested)`，clamp 到 `maxHotScanLimit`
  - 命中 cap 时 `metrics.MemoryHotScanTruncated.WithLabelValues(endpoint).Inc()`
- 调用点：
  - `Retrieve` / `retrieveByQueryFiltered`：传 `budget = limit*2`，endpoint = `retrieve` / `retrieve_by_query`
  - `Decay` / `decayTenant`：传 `requested=0`（稳态预算），endpoint = `decay` / `decay_tenant`
- `internal/config/config.go::MemoryConfig` 新增 `HotScanLimit int` (mapstructure `hot_scan_limit`)
- `cmd/agent/memory_adapter.go::NewMemoryAdapter` 在 `NewRedisHotWithTTL` 后 `if memCfg.HotScanLimit != 0 { hot.SetScanLimit(...) }`
- `internal/metrics/memory.go` 新增 `MemoryHotScanTruncated` CounterVec（endpoint 标签）；`MemoryHotScanKeys` buckets 扩到 2000

### 测试

`tests/internal/memory/redis_hot_retrieve_test.go` 新增 5 个用例（miniredis 真实 SCAN 流）：

| 维度 | 测试 |
|------|------|
| 默认值锁定 200 | `TestRedisHot_ScanLimit_DefaultsTo200` |
| Clamp 表驱动 (0 / -100 / 1 / 49 / 50 / 500 / 2000 / 5000) | `TestRedisHot_SetScanLimit_Clamped` |
| Caller limit 自动放大窗口（seed 250 / limit 500 → 250 returned） | `TestRedisHot_RetrieveByQuery_CallerLimitGrowsScanBudget` |
| `maxHotScanLimit=2000` 兜底（seed 2500 / limit 10000 → ≤2000） | `TestRedisHot_RetrieveByQuery_HitsMaxHotScanLimitCeiling` |
| `SetScanLimit(1000)` 让 Retrieve 看到 600 entries | `TestRedisHot_ScanLimit_RespectsCustomFloor` |

完整命令：`go test ./internal/memory/ ./tests/internal/memory/ -run 'ScanLimit|RetrieveByQuery_CallerLimit|RetrieveByQuery_HitsMax' -count=1`

### 已知尚未覆盖

- `MemoryHotScanTruncated` counter 没有专门的 `assert metric inc` 用例（go-redis miniredis 不暴露 Prometheus collector hook，需要 `testutil.ToFloat64` 这类全量基建；当前 `CallerLimitGrowsScanBudget` 通过"未截断 → 全数据返回"间接断言反向链路）
- 真实 redis-cluster 场景下 SCAN 行为略有差异（cursor 跨 shard 切换），本测试基于单实例 miniredis

---

## 2026-06-26 memory 子系统未修复缺陷专项审计

→ 详见 `llmdoc/memory/reflections/2026-06-26-memory-system-audit.md`

该文件以 `AUDIT-P0-N` / `AUDIT-P1-N` / `AUDIT-P2-N` 编号记录三轮闭环（§14/§15/§16–§24 of `25_memory.md`）之后仍然存在的 4 项 P0 + 7 项 P1 + 5 项 P2 缺陷。每项含 file:line 级证据 + 影响评估 + 与既有修复时间线的交叉引用。后续 PR 修复任一项时，请同步在本表追加一行「AUDIT-X 已闭环 → 见 25_memory.md::§N」。
