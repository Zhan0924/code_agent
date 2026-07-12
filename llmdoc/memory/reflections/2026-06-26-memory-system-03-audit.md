# 反思：memory 系统审计（第三轮） — 2026-06-29 基于代码层全量复审后的「设计性偏差与产品体验」清单

日期：2026-06-29
作者：基于第三轮代码复审，对 `internal/memory/*`（13 个 `hybrid_*.go` + `extractor.go` + `core_memory.go` + `pg_cold.go` + `distiller.go`）+ `internal/orchestrator/memory_bridge.go` + `internal/api/memory_handlers.go` + `cmd/agent/memory_adapter.go` 的全量过审，与 `docs/architecture/25_memory.md::§14–§38` / 第一轮 audit (`2026-06-26-memory-system-audit.md`) / 第二轮 audit (`2026-06-26-memory-system-02-audit.md`) 的交叉比对得出。

> 本文与前两轮的关系：
>
> - **第一轮**（6/26）：13 项 AUDIT-P0/P1/P2 一次性标 ✅。
> - **第二轮**（6/29 上午）：复核第一轮 ✅，发现 3 项假 ✅（MarkDistilled DELETE/UPDATE 二义性、CoreMemory PII 缺口、HybridStore 拆分未发生）+ 5 项新缺陷；REAUDIT-P0-1 / P0-2 / P0-4 / P1-1 / P1-2 / P1-3 / P2-1 等已实际闭环（13 个 `hybrid_*.go` 已拆分、PIIMasker 接入 CoreMemory、MarkDistilled 改回 UPDATE、embedder degrade 监控、tenant 归一化统一）。
> - **本轮（第三轮）**：在第二轮闭环结果之上，再做一次"假设功能性 bug 已基本收敛"的代码复审，核心价值是**把视角从'修 bug'抬升到'修设计性偏差与产品体验'**——找出"代码逻辑全对、指标全绿，但生产里仍然不工作或体验差"的硬伤。

## 1. 背景

`docs/architecture/25_memory.md` 现已记录 §14–§38 共 38 个修复段（P0/P1/P2 共 14+13+11+ 项）；第二轮 audit 后又有 7 项 REAUDIT- 闭环。当前 `internal/memory/*` 的结构：

| 子系统 | 代码位置 | 状态 |
|---|---|---|
| HybridStore（hot+cold） | `hybrid.go` (94 行核心) + 12 个 `hybrid_*.go` 领域文件 | ✅ 按 REAUDIT-P1-1 拆分完成 |
| Extractor（LLM 蒸馏 + 启发式） | `extractor.go:67-92` | ✅ 双消息哨兵 + PII 双层 + dedup K=30 |
| Distiller（episodic → semantic） | `distiller.go:83-211` + `cmd/agent/memory_adapter.go::runMemoryDistillLoop` | ✅ AutoDiscover + MarkDistilled UPDATE 语义 |
| CoreMemory（MemGPT persona） | `core_memory.go::maskForPersist` | ✅ PIIMasker 接入；REAUDIT-P1-3 字符串等值去重 |
| 反馈闭环 | `memory_bridge.go::boostCitedMemories` + `session_handlers.go` feedback | ✅ 机制就位但**实际效果存疑**（见 REAUDIT3-P0-1） |
| 治理面 | `failures_total{tier,op,severity}` 三档 + `retrieve_degraded_total{reason}` | ✅ 接线齐 |

**本次复审的目的**：
1. **回到代码层**，对第二轮闭环的 7 项 REAUDIT 做最后一轮"是否产品级可用"验证；
2. 找出"代码全对、指标全绿，但生产体验差"的设计性偏差；
3. 把"修 bug"模式过渡到"修设计与体验"模式。

> 范围：`internal/memory/*` 13 文件 + 直接接线层（`cmd/agent/memory_adapter.go`、`internal/orchestrator/memory_bridge.go`、`internal/api/memory_handlers.go`、`internal/tools/memory_tools.go`）。

## 2. 总体评价

正面：

- **第二轮 audit 列的 8 项 REAUDIT 已基本闭环**：`hybrid.go` 现 106 行 core-only + 12 个领域文件；CoreMemory 写入路径全过 `maskForPersist` 并带 `audit_id="REAUDIT-P0-2"` 结构化日志；`MarkDistilled` SQL 是 `UPDATE ... SET distilled_at = NOW() WHERE id = ANY($1) AND distilled_at IS NULL` 真正幂等；embedder degrade 有独立 `memory_failures_total{tier="embedder"}` + `memory_retrieve_degraded_total{reason}` 两条指标；orchestrator 与 API 层共用 `session.NormalizeTenantIDs`；`dedupOversample` 可通过 `SetDedupOversample` 调整；`CorePromoter` 接口 + `DefaultCorePromoteThreshold=0.9` 接线齐备。
- **写入安全性达标**：cold-first（`hybrid_store.go:73-92`）+ `severity="critical"` 让"丢数据"路径有可告警条件。
- **召回路径无 short-circuit**：`hybrid_retrieve.go:69-99` 真 RRF 融合，长尾 cold-only 高分条目不再被 hot 噪声掩盖。
- **反馈闭环机制层面齐备**：`[mem:<id>]` 引用 +0.05 / 用户点踩 -0.2 / Core auto-promote 0.9 三件套都接通。

负面：

本轮没有发现新的"功能性 bug"；但**找出 13 项设计性偏差与工程债**，分布如下：

- **5 项设计性硬伤**（A 类，REAUDIT3-P0/P1）：citation 反馈对低成本模型失效、embedder degraded 路径中文召回崩塌、反馈解析正则脆弱、Distiller 蒸馏价值无量化、CoreMemory 自动晋升无"持续观察"机制。
- **5 项工程债**（B 类，REAUDIT3-P1/P2）：dedup 路径双 tier 失败"伪通过"、List 契约不显、embedding 维度切换无 RRF 跨模型支持、dedupCandidateLimitCap 仍是 const、Extractor LLM 解析失败不重试。
- **3 项测试与流程**（C 类，REAUDIT3-P2）：testcontainers 覆盖窄、仓库未跟踪脚本噪声、25_memory.md 已 2000+ 行。

## 3. 已知未修复缺陷一览（按优先级 + 严重程度排序）

### 3.1 P0 — 设计性硬伤（直接影响生产体验）

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **REAUDIT3-P0-1** | **Citation 反馈闭环对低成本模型形同虚设** — §35 依赖 LLM 自觉在回答里 emit `[mem:<id>]` 标签才触发 +0.05 boost，但 GPT-4o-mini / Claude Haiku / 国产模型遵守该 prompt 指令的比例经验值 < 30%，意味着整个 RLHF-lite 闭环在生产最常见的成本敏感部署形态下 silent 失效 | `internal/orchestrator/memory_bridge.go:222` instruct 文本 `"If you use a memory, explicitly cite its ID like '[mem:<id>]' in your response to boost its relevance."`；§35 设计取舍自白 "LLM 引用非强制：机制就绪，是否 cite 取决于模型；无 citation 时不 boost"；+0.05 boost vs Decay factor=0.95 / 30d → 一次 cite 仅勉强抵一次 decay，需要持续 cite 才能形成正反馈 | 生产环境最常见的成本敏感部署形态（用 mini/haiku/国产）→ 整条 P0-1 反馈闭环 silent 失效；指标 `memory_citation_boost_total{source=auto}` 长期为 0 既无告警条件也很难被运维识别。Decay 与 Score 调整失去可信信号 |
| **REAUDIT3-P0-2** | **Embedder degraded 路径下中文 query 召回质量崩塌** — §37/REAUDIT-P0-4 只补了监控指标，没补降级路径的搜索能力。embedder 不可用时 `Retrieve` 走 `cold.Retrieve` 的 ILIKE 文本搜索，对中英文混合用户的中文 query 几乎完全失效 | `internal/memory/hybrid_retrieve.go:48-61` 降级到 `cold.Retrieve(userID, projectID, query, limit)`；`internal/memory/pg_cold.go::Retrieve` 用 `content ILIKE '%' || $3 || '%'`；中文 query `"我喜欢用 tabs"` 与已存 memory `"user prefers tabs not spaces"` 在 ILIKE 下零交集 | embedder 服务挂 1 小时，retrieve 全降级，用户中文回忆功能彻底失效；只有 `memory_retrieve_degraded_total` counter 上涨——运维知道"质量在降"但**用户已经感受到了**，等运维介入就晚了 |

### 3.2 P1 — 设计性偏差（机制层缺一道环节）

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **REAUDIT3-P1-1** | **Session feedback 反馈解析依赖正则匹配 `[mem:<id>]`，未把 cited_memory_ids 作为消息结构化字段持久化** — §28 的 RLHF-lite 链路如果上层做任何 trim/sanitize（剥除 markdown 引用块、改写格式）就会 ID 丢失 | `§28` 描述 "解析助手回复中的 `[mem:<id>]` 引用"；`internal/session.Message` 元数据没有 `cited_memory_ids []string` 字段；解析错误目前无独立计数器 | 用户点踩时，原本能降权的 memory 因 ID 抽取失败漏报；不可观测——指标只能看到 `memory_citation_boost_total{status=ok/err}`，看不到"用户点踩但找不到 cited_id" 这种空转。是 REAUDIT3-P0-1 的姐妹问题 |
| **REAUDIT3-P1-2** | **Distiller 的"准入门槛"和"产出价值"未量化挂钩** — `DistillerOptions` 只用 `MinEpisodicToTrigger=3` 和 `MaxEpisodicPerRun=50` 控量，没有按相似性聚类后再分桶蒸馏；蒸馏成功后 `SemanticScore=1.2` 是常数，无法体现"这条 rule 是从 50 条 episode 凝出来的"还是"只从 3 条凝出来的" | `internal/memory/distiller.go:60-71` 默认值；`distiller.go:144-147` LLM prompt 固定要求"ONE concise semantic rule"；`distiller.go:160-171` semantic memory 的 Score 取自 `opts.SemanticScore` 常数 | 蒸馏产出的 semantic rule "粒度粗 + 权重无差异" —— 50 条不同主题的 episode 会被强行揉成 1 条噪声大的 rule；而 50 条同主题 episode 与 3 条同主题 episode 蒸馏出的 rule 拿到的 score 完全相同，Decay 衰减后无法保留高凝聚度的 rule |
| **REAUDIT3-P1-3** | **CoreMemory 自动晋升阈值 0.9 全局硬编码，且没有"持续观察确认 → 才晋升"机制** — LLM 在一次蒸馏里给 `importance=0.9` 就直接写入 persona section，section 内去重是字符串等值（`sectionContainsLine`），换措辞就重复 | `extractor.go:54` `DefaultCorePromoteThreshold = 0.9`；`core_memory.go:213-225::sectionContainsLine` 用 `strings.TrimSpace == ` 等值判定；LLM 把"我喜欢 tabs"和"用户偏好 tabs"会写两条 | 一次"用户激动地说我以后都要 X"就会写进 persona 并存活 30 天；persona section 长期累积冗余措辞；user-scope persona 跨 project 共享后噪声放大 |
| **REAUDIT3-P1-4** | **`HybridStore.RetrieveCandidates` 在 hot+cold 全失败时返回 `(nil, nil)`，dedup 路径"伪通过"** — 双 tier 同时故障时，`Extractor.isDuplicate` 拿到空候选集判定"不重复"，直接 Store；故障期间会绕过 dedup 制造一批伪重复让 P1 #7 anchor+drain 兜底 | `internal/memory/hybrid_retrieve.go:131-189` hot/cold 都失败时 `out` 为空切片、`error` 为 nil；`extractor.go::isDuplicate` 看到 `len(existing) == 0` 即视为不重复 | 双 tier 同时故障概率小，但故障期间**写入路径绕过 dedup**；anchor+drain 后续修但有时间窗口；当前没有 `memory_dedup_unavailable_total` 计数器，故障是"看不见的吞错误" |

### 3.3 P1 — 工程债

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **REAUDIT3-P1-5** | **`handleListMemory` / `HybridStore.List` 与 `RetrieveByVector` 的 episodic 过滤契约不显** — `RetrieveByVector` 默认排除 episodic、`hot.RetrieveByQuery` 客户端过滤 episodic，但 `List` 调的 `cold.Retrieve("")` 行为依赖 SQL 内部 `AND type <> 'episodic'`，API 调用方需要知道"episodic 不会出现在列表/召回里" | `internal/memory/hybrid_list.go:15-65` 不带 type filter 直通 `cold.Retrieve("")`；`internal/api/memory_handlers.go::handleListMemory` 没有 `?include_episodic=true` 开关；`MemoryRetriever` 接口注释未声明 episodic 排除契约 | 调用方（前端 / 第三方集成）查不到 episodic 但读不到任何提示；要看 episodic 必须单独走 `/api/v1/memory/explain/:id`；"为什么我的 List 里没有蒸馏前的原始数据"会变成支持工单 |
| **REAUDIT3-P1-6** | **embedding 维度迁移是"双 column"而非"动态 schema"，模型切回会让老数据死锁** — §32/REAUDIT-AUDIT-P1-5 用约定列名 `embedding_<dim>`，但 embedder 模型切回 1536 时，新数据写 `embedding` 列、老的 3072 数据死在 `embedding_3072` 列里，召回时永远不会被混合 | `pg_cold.go` 用约定列名 `embedding_<dim>`；schema 没有 `embedding_model TEXT` 列 + per-model 索引；§32 没有"两套维度并存时如何 RRF" 的设计 | 切模型这种事产品演进里几乎一定会发生；切完模型后老 memory **永远召回不到**（除非走 ILIKE 兜底，那是 P0-2 的问题）；迁移期间没有 batch job 用新模型 re-embed 老数据的流程 |
| **REAUDIT3-P1-7** | **`dedupCandidateLimitCap=200` 仍是 const；§34 调优表也没把 cap 列入** — `dedupOversample` 已可配置（REAUDIT-P2-1 闭环），但 cap 不可调；大库下 30 已偏紧，operator 想加 K 没法加 | `internal/memory/hybrid_retrieve.go:106` `const dedupCandidateLimitCap = 200`；`docs/architecture/25_memory.md:1793-1809` §34 调优表覆盖 11 参数但漏 `dedupCandidateLimitCap` | 流量增长后 operator 发现 `memory_dedup_candidate_count` P95 = 200 但调不动；与 REAUDIT-P2-1 形成"半完成"状态 |
| **REAUDIT3-P1-8** | **Extractor LLM 解析失败不重试，且无解析错误计数器** — §4.2 文档自白"两种失败都不重试"，但当前 prompt 已加 `<<<INTERACTION_BEGIN/END>>>` 哨兵格式错误率应已很低；保留"不重试"更多是历史遗留，且生产里看不到该路径有多脆 | `internal/memory/extractor.go::extractWithLLM` 解析失败直接返回 nil；无 `memory_extractor_parse_errors_total` 计数器 | 关键蒸馏路径的可靠性盲区——LLM 偶发输出格式错误 → 用户的一次重要对话被静默丢弃；运维看不到该路径在生产里的真实失败率 |

### 3.4 P2 — 测试与流程

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **REAUDIT3-P2-1** | **testcontainers 集成测试仍只覆盖一组用例** — `TestPGCold_Integration` 单一测试函数，未为 `BoostScoreBatch` / `MarkDistilled` / `DeleteByUser` / cross-type 合并等最容易 SQL 反转的路径单独建 case。这是 REAUDIT-P0-1（MarkDistilled DELETE vs UPDATE 二义性）当年能绕过 CI 的根因，本轮**未真正解决** | 仓库 `grep testcontainers` 命中 `pg_cold_integration_test.go` 一个文件；`TestPGCold_Integration` 单测试函数，未拆分 case；新接线的 4 条关键路径都没有"真 PG + 真 pgvector + 真索引"的端到端验证 | 任何 SQL/索引变更不会被 CI 拦截——下次 SQL 语义反转同样会完美绕过；REAUDIT-P2-2 标 ✅ 但实际未达到"每条新接线一个 case"的目标 |
| **REAUDIT3-P2-2** | **仓库卫生**：多个未跟踪的诊断脚本（`split_hybrid.py`、`list_funcs.py`、`proxy_forwarder.py`、`pull_aggressively.sh`、`test_feedback.go`、`test_pii.go`、`deploy-p0.sh`）暗示有未合入的工作；同时 `docs/superpowers/plans/` 下仍有 4 个 2026-06-29 计划 | `git status` 输出 7+ 未跟踪 Python/Go/Shell 脚本；`docs/superpowers/plans/2026-06-29-{AUDIT-P2-1-hybrid-refactor, episodic-gc, gdpr-delete, pg-integration-test}.md` 4 个计划文件存在 | 工程信号噪声：新人难以分辨"已完成"和"在路上"；REAUDIT-P2-3 标 ✅ 但物证仍在 |
| **REAUDIT3-P2-3** | **`docs/architecture/25_memory.md` 已 2000+ 行（§1–§38）**，可读性逼近临界点 | `wc -l docs/architecture/25_memory.md` ≈ 2000+；正文模板 13 节 + 24 个修复时间线段杂糅 | 新人入门需要扫读全部修复段才能理解"当前生效设计"；建议把 §14–§38 拆出到 `docs/architecture/memory/CHANGELOG.md`，正文只保留"当前生效设计" |

## 4. 推荐优先级（如果只能动 3 项）

1. **REAUDIT3-P0-1（Citation 反馈对低成本模型失效）** —— 把"是否引用了 memory"从依赖 LLM 自发性，改为 **structured tool 调用**（让 LLM 调一个 `cite_memory(ids=[...])` 工具）或 **post-hoc 语义匹配**（assistant 回复 vs 召回 memory 算 cosine，过阈值视为 used）。机制性兜底，不再依赖 prompt-following 能力。这是当前生产环境最常见部署形态下整条反馈闭环 silent 失效的根因。
2. **REAUDIT3-P0-2（embedder degraded 路径中文召回崩塌）** —— embedder 失效时至少补一道**关键词分词 + 多关键词 OR ILIKE**（中英文混合用 jieba/简单切词），或保留一份兜底的 BM25 / `pg_trgm` / `tsvector` 索引。这条路径生产部署很常见（embedder 抖动 / 配额），不能只靠"告警让人来救"，要给用户保留可用召回能力。
3. **REAUDIT3-P2-1（testcontainers 拆分 case）** —— 把 `TestPGCold_Integration` 按"每条新接线路径一个 case"拆开，补 `BoostScoreBatch / MarkDistilled / DeleteByUser / cross-type` 四个 PG 级集成 case。第二轮 audit 已经用 REAUDIT-P0-1 证明了缺乏真 PG 测试是 SQL 语义反转无人察觉的根因，本轮把它做实。

## 5. 附录：与既有时间线的交叉引用

| 本文 ID | 与前两轮反思 + 25_memory.md 的关系 |
|---|---|
| REAUDIT3-P0-1 | 与 AUDIT-P0-1 / §35（第一轮 ✅）+ REAUDIT-P0-3（第二轮）同源：第二轮已识别"LLM 引用非强制 + 低成本模型 < 30% 比例"风险，本轮量化为"silent 失效"并要求机制性兜底而非仅观测性 |
| REAUDIT3-P0-2 | 与 AUDIT-P2-4 / §37 + REAUDIT-P0-4（第二轮）互补：第二轮补了 `memory_failures_total{tier="embedder"}` + `memory_retrieve_degraded_total{reason}` 监控，本轮发现"监控告警有了不解决问题"，要求补降级路径搜索能力 |
| REAUDIT3-P1-1 | 与 AUDIT-P0-4 / §28 + REAUDIT-P1-4（第二轮）同源：第二轮已识别正则解析脆弱，本轮要求把 `cited_memory_ids` 持久化为消息结构化字段 |
| REAUDIT3-P1-2 | 第一轮 / 第二轮均未涉及；属于 Distiller 设计粒度问题，建议按 `embedding` 先做 k-means 聚类，再按簇分别蒸馏；score 按 `episode_count` 的 log 挂钩 |
| REAUDIT3-P1-3 | 与 AUDIT-P1-4 / §31 + REAUDIT-P1-3（第二轮）互补：第二轮加了 `sectionContainsLine` 字符串等值去重，本轮要求 cosine 阈值去重 + 多次观察确认机制 |
| REAUDIT3-P1-4 | 与 AUDIT-P1-9（dedup 接口隔离）互补：接口隔离做对了但失败模式吞掉了错误；建议返回 `ErrDedupUnavailable` 或加 `memory_dedup_unavailable_total` 计数器 |
| REAUDIT3-P1-5 | 第一轮 / 第二轮均未涉及；属于 API 契约不显问题 |
| REAUDIT3-P1-6 | 与 AUDIT-P1-5 / §32 互补：§32 做了"双 column 渐进迁移"，但缺少"两套维度并存如何 RRF" + "切回老模型如何召回老数据"的设计 |
| REAUDIT3-P1-7 | 与 AUDIT-P2-3 / §34 + REAUDIT-P2-1（第二轮）互补：第二轮把 `dedupOversample` 可配置了，但 `dedupCandidateLimitCap` 仍是 const，调优表未列入 |
| REAUDIT3-P1-8 | 与 AUDIT-P1-2 / §4.2 互补：双消息哨兵已加，但解析失败"不重试 + 无计数器"的设计取舍未重新评估 |
| REAUDIT3-P2-1 | 与 AUDIT-P2-2 / §27 + REAUDIT-P2-2（第二轮）冲突：第二轮标 ✅ 但实际只有单一测试函数；本轮要求按"每条新接线一个 case"拆分 |
| REAUDIT3-P2-2 | 与 REAUDIT-P2-3（第二轮）部分冲突：第二轮标 ✅ 但 `git status` 仍显示 `deploy-p0.sh` 未跟踪，4 个 plans 文件存在 |
| REAUDIT3-P2-3 | 第一轮 / 第二轮均未涉及；属于文档可维护性问题 |

## 6. 验证方法（用于将来 PR 自检）

后续若有 PR 修复任一项，请在 `docs/architecture/25_memory.md` 末尾追加 §39/§40… 章节，结构与 §35–§38 对齐：

```
## §N. REAUDIT3-P0-X: <现象一句话>

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
- Docker 三件套（podman build + deploy + curl + log）

### 设计取舍
（为什么这么做，3-5 条）
```

完成后回到本文件，把对应行的 "REAUDIT3-P0/P1/P2" 标记加 ✅，并指向 §N 修复段；**同时回到 `2026-06-26-memory-system-audit.md` / `2026-06-26-memory-system-02-audit.md`，对受影响的 AUDIT/REAUDIT 项把 ✅ 改成 ⚠️（部分闭环）或 ❌（实未闭环），追加"三次审计已识别 → 见 03-audit.md::REAUDIT3-X"批注**，避免后续审计再次被假 ✅ 误导。

---

**与既有审计文档的协同**：

- `llmdoc/memory/doc-gaps.md` 是跨包死代码与接线缺失主索引
- `2026-06-26-memory-system-audit.md` 是第一轮 memory 子系统专项审计（13 项 AUDIT-P0/P1/P2，已标 ✅，部分被二轮反证为假 ✅）
- `2026-06-26-memory-system-02-audit.md` 是第二轮 memory 子系统专项审计 — **核心价值是"已标 ✅ 实未闭环"的复核**
- 本文件（第三轮）— **核心价值是从"功能性 bug 收敛"过渡到"设计性偏差与产品体验"**，13 项 REAUDIT3-P0/P1/P2 全部待修
- 修复后四源同步：本文标 ✅ + 第一轮 + 第二轮反思更新标记 + doc-gaps 末尾追加一行"REAUDIT3-X 已闭环 → 见 25_memory.md::§N"
