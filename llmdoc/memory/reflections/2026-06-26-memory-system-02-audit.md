# 反思：memory 系统审计（第二轮） — 2026-06-29 复审 §14–§38 闭环后的"标 ✅ 实未闭环"清单

日期：2026-06-29
作者：基于第二轮代码复审，对 `internal/memory/*` + `internal/orchestrator/memory_bridge.go` + `internal/api/memory_handlers.go` + `internal/tools/memory_tools.go` 的全量过审与 `docs/architecture/25_memory.md::§14–§38` / `llmdoc/memory/reflections/2026-06-26-memory-system-audit.md` 的交叉比对得出。

> 本文与第一轮审计（`2026-06-26-memory-system-audit.md`）的关系：第一轮列出 AUDIT-P0-1 ~ AUDIT-P2-5 共 13 项缺陷并**全部标 ✅**。本轮的核心价值是 **复核每个 ✅，回到代码层验证修复是否真闭环**；同时记录第一轮未涉及的新发现。

## 1. 背景

`docs/architecture/25_memory.md::§14–§38` 记录了三轮共 38 个章节的修复时间线（P0/P1/P2 14+13+? 项）。第一轮反思文档（`2026-06-26-memory-system-audit.md`）在 6/26 把 13 项 AUDIT 缺陷全部标为 ✅。

本次复审的目的：
1. **回到代码层**，对每一个 ✅ 缺陷重新验证 file:line 是否真的实现了承诺；
2. 找出"文档声称已修复，但代码层语义/实现不一致"的真实漏洞；
3. 记录第一轮未覆盖、第二轮新发现的缺陷。

> 范围：`internal/memory/*` + 直接接线层（`cmd/agent/*`、`internal/orchestrator/memory_bridge.go`、`internal/tools/memory_tools.go`、`internal/api/memory_handlers.go`、`internal/agentloop/trajectory_memory.go`）。

## 2. 总体评价

正面：
- 13 项第一轮 AUDIT 缺陷中**约 10 项确实闭环**：feedback boost（§35）、GDPR delete API（§26）、testcontainers（§27）、cross-type dedup（§29）、PII 第二层广播脱敏（§30）、core memory auto-promotion（§31）、embedding dim 动态列（§32）、KV-cache 拆分（§33）、配置调优表（§34）、failures severity（§37）、explain API（§38）——这些路径都有 file:line 级别的真实代码 + 单元测试。
- 反馈闭环 `[mem:<id>]` citation boost + RLHF-lite session feedback（§28）+ Core Auto-Promotion（§31）三件套已成形。

负面：
- **3 项 ✅ 标记实际上没真闭环**（§3.1 详述）：
  - `MarkDistilled` 注释承诺"幂等标记"、SQL 实现是 `DELETE`，连带 `DeleteOldEpisodic` 的 30 天观察窗口承诺全部失效；
  - `HybridStore` 拆分（AUDIT-P2-1）从未发生，`hybrid.go` 仍 733 行；
  - PII 第二层广播脱敏（§30）覆盖了 `HybridStore.Store` 与 `Blackboard.Publish`，**唯独漏掉了 core_memory tool 写入路径**，LLM 通过 `core_memory_append` 写入的内容**完全不过 Mask**。
- 此外发现 2 项第一轮未提到的新缺陷（§3.2 / §3.3）：anonymous + default 默认值在 API 层与 orchestrator 层语义不一致，以及 embedder 失效降级路径监控盲区。

## 3. 已知未修复缺陷一览（按优先级 + 严重程度排序）

### 3.1 P0 — 第一轮标 ✅ 实未闭环（含真实数据完整性 bug）

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| ✅ **REAUDIT-P0-1** | **`MarkDistilled` 名实不符 — Distiller 蒸馏后直接 DELETE，AUDIT-P0-2（§36）声称的"30 天观察窗口 + GC 安全契约"**完全失效** | `internal/memory/pg_cold.go:542-551` 函数名 `MarkDistilled` + 注释 "Idempotent: re-marking already-distilled entries is a no-op (the WHERE clause keeps the original timestamp)" + 实际 SQL `DELETE FROM memories WHERE id = ANY($1)`；连带 `pg_cold.go:563-575::DeleteOldEpisodic` 的 `WHERE distilled_at IS NOT NULL AND distilled_at < $1` 永远命中 0 行（因为没有任何代码 SET distilled_at）；`internal/api/memory_handlers.go:163-165` 的 explain API 返回的 `distilled_at` 字段对 episodic 永远为 null；`pg_cold.go:79, 82` 的 schema 仍在维护 `distilled_at` 列 + `idx_memories_episodic_undistilled` 部分索引（后者仍可用，但已等价于 `WHERE type='episodic'`） | 数据完整性风险：蒸馏失败 → 重试时原始 episode 已被删；事故排查时 explain API 显示不出"何时蒸馏"；测试 `tests/internal/memory/episodic_gc_test.go::TestDeleteOldEpisodic_OnlyTouchesDistilled` 验证的是 fake store 语义，CI 永远绿但生产是另一套行为 → 见 `docs/superpowers/plans/2026-06-29-REAUDIT-P0-1-mark-distilled-semantics.md` |
| ✅ **REAUDIT-P0-2** | **PII 防御链路在 core_memory tool 写入处出现缺口**。AUDIT-P1-2（§30）声称在 `HybridStore.Store` + `Blackboard.Publish` 二次 Mask，但 LLM 通过 `core_memory_append` / `core_memory_replace` 工具直接写 `CoreMemoryManager`，**完全不过 PIIMasker** | `internal/tools/memory_tools.go:122` 直接调 `t.coreManager.AppendToSectionScoped(ctx, userID, projectID, scope, section, content)`，`memory_tools.go:187` 同理 `ReplaceInSectionScoped`；`internal/memory/core_memory.go` 全文 `grep Mask\|masker\|PIIMasker` 零命中；`internal/memory/hybrid.go:124` 有 `m.Content = h.masker.Mask(m.Content)`，`internal/memory/blackboard.go:45` 有 `safeM.Content = b.masker.Mask(safeM.Content)`，但 Core Memory 不走这两条路径 | LLM 把"用户手机号 138xxxx1234"或企业 token 写进 persona section，会原文持久化 + 通过 `formatCoreMemory` 注入 system prompt + 通过 `GetMerged` 在跨 project 召回。`AGENT_CUSTOM_PII_REGEX` 在 core memory 路径上失效 → 见 `docs/superpowers/plans/2026-06-29-REAUDIT-P0-2-core-memory-pii.md` |

### 3.2 P0 — 模型行为依赖与监控盲区

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| ✅ **REAUDIT-P0-3** | **Citation 反馈闭环对低成本模型形同虚设**。§35 依赖 LLM 在回答里 emit `[mem:<id>]` 标签触发 +0.05 boost，但 GPT-4o-mini / Claude Haiku / 国产模型遵守该 prompt 指令的比例可能 < 30% | `internal/orchestrator/memory_bridge.go:212` instruct 文本 "If you use a memory, explicitly cite its ID like `[mem:<id>]` in your response to boost its relevance."；§35 设计取舍自白 "LLM 引用非强制：机制就绪，是否 cite 取决于模型；无 citation 时不 boost"；同时 +0.05 boost vs Decay factor=0.95 / 30d → 一次 cite 抵一次 decay，正反馈极度勉强 | 生产环境最常见的成本敏感部署形态（用 mini/haiku/国产）→ 整个 P0-1 反馈闭环 silent 失效，但指标 `memory_citation_boost_total{source=auto}` 长期为 0 也只是 "无信号"而非"告警条件"，运维无法从指标识别该模式 → 见 `docs/superpowers/plans/2026-06-29-REAUDIT-P0-3-citation-feedback-observability.md` |
| ✅ **REAUDIT-P0-4** | **Embedder 失效时降级到 ILIKE 文本搜索，对中文 query 几乎完全失效**，且无独立监控指标 | `internal/memory/hybrid.go:252-264` 降级到 `cold.Retrieve(userID, projectID, query, limit)`（ILIKE 文本搜索）；`pg_cold.go::Retrieve` 用 `content ILIKE '%' || $3 || '%'`；中文 query "我喜欢用 tabs" 与已存 memory "user prefers tabs not spaces" 在 ILIKE 下零交集；`memory_failures_total{tier, op, severity}` 三档（warn/error/critical）只覆盖 hot/cold/blackboard，**embedder 失败归不到任一 tier** → embedder 服务挂 1 小时，retrieve 全降级，监控指标看上去全绿 | 监控盲区：embedder 失效时 retrieve 质量骤降但 alert rule 触发不了；中英文混合用户的"中文回忆"路径在 embedder 失效时彻底丢失能力 → 见 `docs/superpowers/plans/2026-06-29-REAUDIT-P0-4-embedder-degrade-observability.md` |

### 3.3 P1 — 工程治理/语义一致性

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| ✅ **REAUDIT-P1-1** | **AUDIT-P2-1 标 ✅ 但 `HybridStore` 拆分从未发生**。`hybrid.go` 仍 733 行，4 个异步队列 + 双写 + RRF + Promote/Demote/Touch/Decay + GDPR delete + explain + bucket 全在一个类里 | `wc -l internal/memory/hybrid.go` → 733；`HybridStore` struct 在 `hybrid.go:32-66` 仍有 12+ 字段；同时仓库存在 `docs/superpowers/plans/2026-06-29-AUDIT-P2-1-hybrid-refactor.md`（暗示有人开了头但没合）+ 未跟踪文件 `split_hybrid.py` | 新人理解成本陡峭；`HybridStore` 修改极易触发"我以为这块跟那块无关"的微妙 bug；测试覆盖也被迫集中在一个超大文件 → 见 `docs/superpowers/plans/2026-06-29-REAUDIT-P1-1-hybrid-split.md`（`hybrid.go` 现 94 行 core-only + 9 个 `hybrid_*.go` 领域文件） |
| **REAUDIT-P1-2** | **匿名/默认值兜底在 API 层与 orchestrator 层两套行为**。API 层把空 user_id 兜底为 `anonymous` + 空 project_id 兜底为 `default`，orchestrator 端无 userID 直接跳过 extract，两条路径行为不一致 | `internal/api/memory_handlers.go:33-40, 86-93` 把空值兜底为 `AnonymousUserID + "default"`；`internal/orchestrator/memory_bridge.go:81-84` 无 userID 直接 `return`；`internal/orchestrator/memory_bridge.go:127-130` `recordTaskEpisodeAsync` 同样直接跳过 | 通过 API 写入的"匿名"memory 全部挤进同一个 (anonymous, default) bucket，导致：（1）跨用户隐私交叉；（2）pgvector 召回质量崩塌（一个 bucket 10K 条 memory，HNSW recall 失效）；同时 orchestrator 自动 extract 一条不存，行为可观测性混乱 |
| **REAUDIT-P1-3** | **Core Memory 自动晋升（§31）阈值 0.9 经验值，且与 tool 主动 append 没有去重协调** | `internal/memory/extractor.go::ExtractFromInteraction` 触发 AutoPromote 条件 `importance >= 0.9 && type ∈ {preference, decision}`；`internal/memory/core_memory.go` 全文 202 行无 `Mask` 也无对"重复内容/标签"的去重逻辑；同一条 preference 既被 LLM tool 写入又被 auto-promote 写入会产生两条 section entry | `persona` / `human_context` section 长期积累冗余；§34 配置调优表也未把 `core_promote_threshold` 纳入观测维度 |
| **REAUDIT-P1-4** | **Session feedback 反馈解析依赖正则匹配 `[mem:<id>]`**，未把 cited_memory_ids 作为消息结构化字段持久化 | `§28` 描述 "解析助手回复中的 `[mem:<id>]` 引用"；如果上层做任何 trim/sanitize（剥除 markdown 引用块、改写格式）→ ID 丢失 | RLHF-lite 信号脆弱：用户点踩时，原本能降权的 memory 因 ID 抽取失败漏报；不可观测——指标只能看到 boost_total ok/err，看不到"用户点踩但找不到 cited_id" 这种空转 |

### 3.4 P2 — 工程债 / 可演进性

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **REAUDIT-P2-1** | **`dedupOversample = 10` 写死全局常量，且 §34 调优表未列入** | `internal/memory/hybrid.go:22` 注释长篇说明取值由来，但常量本体不可配置；§34 调优表（`docs/architecture/25_memory.md:1793-1804`）覆盖 9 个可调参数，没有 dedupOversample | 真实流量下，多租户 memory 规模差距大，单一 10 值要么过紧（漏夹）要么过松（pgvector 多扫一倍） |
| **REAUDIT-P2-2** | **testcontainers 集成测试只覆盖一组用例**。`TestPGCold_Integration` 一个测试函数，未单独覆盖 §35（BoostScoreBatch）、§36（MarkDistilled — 也正好是 REAUDIT-P0-1）、§26（DeleteByUser）、§29（cross-type 合并）几条最新接线路径 | 仓库 `grep testcontainers` 命中 `pg_cold_integration_test.go` 一个文件；`TestPGCold_Integration` 单测试函数，未拆分 case；新接线的 4 条关键路径都没有"真 PG + 真 pgvector + 真索引"的端到端验证 | 任何 SQL/索引变更不会被 CI 拦截：例如 REAUDIT-P0-1 这种 DELETE vs UPDATE 的语义反转就完美绕过了第一轮 testcontainers 接入 |
| **REAUDIT-P2-3** | **仓库卫生**：多个未跟踪的诊断脚本（`split_hybrid.py`、`list_funcs.py`、`proxy_forwarder.py`、`pull_aggressively.sh`、`test_feedback.go`、`test_pii.go`）暗示有未合入的工作；同时 `docs/superpowers/plans/` 下有 4 个 2026-06-29 计划但代码层只看到部分接线 | `git status` 输出 7+ 未跟踪 Python/Go/Shell 脚本；`docs/superpowers/plans/2026-06-29-{AUDIT-P2-1-hybrid-refactor, episodic-gc, gdpr-delete, pg-integration-test}.md` 4 个未归档计划 | 工程信号噪声：新人难以分辨"已完成"和"在路上"；同时 main 分支可能潜伏多个开了头没合的 PR |

## 4. 推荐优先级（如果只能动 3 项）

1. **REAUDIT-P0-1（MarkDistilled DELETE/UPDATE 二义性）** —— 这是真实的数据完整性 bug，且**与文档承诺直接冲突**。建议方案：要么把 SQL 改回 `UPDATE memories SET distilled_at = NOW() WHERE id = ANY($1)` 兑现 30d 观察窗，运维通过 `DeleteOldEpisodic` 物理清理；要么坦率删掉 `distilled_at` 列 + `DeleteOldEpisodic` + 文档 30d 承诺，把"蒸馏即删"做实并补充"为什么不需要观察窗"的设计取舍。**两套实现都可接受，但当前是两套都没做对的"半死状态"**。同时把 §36 reflections 标 ✅ 改成 ⚠️。
2. **REAUDIT-P0-2（CoreMemory PII 缺口）** —— 在 `internal/tools/memory_tools.go::handleCoreMemoryAppend` / `handleCoreMemoryReplace` 调用 `coreManager.AppendToSectionScoped` 之前过一遍 `PIIMasker.Mask(content)`；同时给 `CoreMemoryManager` 注入 `PIIMasker` 字段，让任何写入路径（包括未来的 backfill / migration）都默认过 Mask；配套测试覆盖"LLM 写入含 phone/JWT/AWS key 的 content → 落库被遮蔽"。
3. **REAUDIT-P0-4（embedder 失效监控盲区）** —— 给 `HybridStore.embedText` 加 `metrics.MemoryFailuresTotal.WithLabelValues("embedder", "embed", "error").Inc()`，并把 retrieve 降级到 ILIKE 时记一个独立的 `memory_retrieve_degraded_total{reason="embedder_failed"}` counter；alert rule 可基于 `rate(...{tier="embedder",severity="error"}[5m]) > 0.1` 触发工单。

## 5. 附录：与既有时间线的交叉引用

| 本文 ID | 与第一轮反思（2026-06-26）+ 25_memory.md 的关系 |
|---|---|
| REAUDIT-P0-1 | 与 AUDIT-P0-2 / §36 直接冲突：第一轮把"GC 安全契约"标 ✅，但本轮证明 SQL 是 DELETE，30d 观察窗承诺为空头支票 |
| REAUDIT-P0-2 | 与 AUDIT-P1-2 / §30 互补：§30 给 `HybridStore.Store` + `Blackboard.Publish` 加了 Mask，但漏了 `core_memory_append/replace` 工具路径 |
| REAUDIT-P0-3 | 与 AUDIT-P0-1 / §35 同源：第一轮设计取舍已承认"LLM 引用非强制"，本轮量化"低成本模型下信号 < 30% 比例" 的风险并建议补可观测性 |
| REAUDIT-P0-4 | 与 AUDIT-P2-4 / §37 互补：§37 三档分级覆盖 hot/cold/blackboard，但漏了 embedder 这个跨 tier 依赖；§37 的"不带 user_id 标签避免高基数"设计正好留出了 `tier="embedder"` 这个空位 |
| REAUDIT-P1-1 | 与 AUDIT-P2-1 直接冲突：第一轮标 ✅ 但实际仓库 `hybrid.go` 仍 733 行；`docs/superpowers/plans/2026-06-29-AUDIT-P2-1-hybrid-refactor.md` 是"开了头没合"的物证 |
| REAUDIT-P1-2 | 第一轮未涉及；属于 GDPR/匿名兜底的次生隐患 |
| REAUDIT-P1-3 | 与 AUDIT-P1-4 / §31 互补：§31 实现了自动晋升，但没解决 tool 主动 append 与 auto-promote 的去重协调 |
| REAUDIT-P1-4 | 与 AUDIT-P0-4 / §28 互补：§28 实现了 session→memory 反馈桥，但解析路径依赖正则匹配，缺乏结构化兜底 |
| REAUDIT-P2-1 | 与 AUDIT-P2-3 / §34 互补：§34 调优表覆盖 9 参数但漏 dedupOversample |
| REAUDIT-P2-2 | 与 AUDIT-P2-2 / §27 互补：§27 接入了 testcontainers，但单测覆盖面狭窄，正好让 REAUDIT-P0-1 这种 SQL 语义反转绕过 CI |
| REAUDIT-P2-3 | 第一轮未涉及；属于工程卫生 / 信号噪声治理 |

## 6. 验证方法（用于将来 PR 自检）

后续若有 PR 修复任一项，请在 `docs/architecture/25_memory.md` 末尾追加 §39/§40… 章节，结构与 §35–§38 对齐：

```
## §39. REAUDIT-P0-N: <现象一句话>

### 病征
（修复前的具体证据，最好 file:line；如本文表格"关键证据"列）

### 修复策略
（接口/数据流/取舍）

### 关键接口变更
| 位置 | 变更 |

### 验证
- go build ./...
- go test ./... 命令 + 新测试函数列表
- 监控/可观测性新指标

### 设计取舍
（为什么这么做，3-5 条）
```

完成后回到本文件，把对应行的"REAUDIT-P0/P1/P2" 标记加 ✅，并指向 §N 修复段；**同时回到 `2026-06-26-memory-system-audit.md`，对受影响的 AUDIT 项把 ✅ 改成 ⚠️（部分闭环）或 ❌（实未闭环），追加"二次审计已识别 → 见 02-audit.md::REAUDIT-X"批注**，避免后续审计再次被假 ✅ 误导。

---

**与 `llmdoc/memory/doc-gaps.md` 协同**：
- `doc-gaps.md` 是跨包死代码与接线缺失主索引
- `2026-06-26-memory-system-audit.md` 是第一轮 memory 子系统专项审计
- 本文件是第二轮 memory 子系统审计 — **核心价值是"已标 ✅ 实未闭环"的复核**
- 修复后三源同步：本文标 ✅ + 第一轮反思更新标记 + doc-gaps 末尾追加一行"REAUDIT-X 已闭环 → 见 25_memory.md::§N"
