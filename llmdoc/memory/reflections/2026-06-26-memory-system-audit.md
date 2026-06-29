# 反思：memory 系统审计 — 2026-06-26 第三轮闭环后的未修复缺陷清单

日期：2026-06-26
作者：基于对 `internal/memory/*` + `cmd/agent/memory_adapter.go` + `internal/orchestrator/memory_bridge.go` 的代码复审与 `docs/architecture/25_memory.md` §14–§24 修复时间线的交叉比对得出。

## 1. 背景

`docs/architecture/25_memory.md::§14–§24` 记录了三轮共 30+ 处 P0/P1/P2 缺陷的闭环修复（双写漂移、PII、Prompt Injection、SCAN 截断、Distiller 接线、anchor+drain dedup、Promote/Demote 死代码…）。本文件的目的不是再重复那些已修复的项，而是**审计当前代码状态下仍然存在的问题**，以便后续 PR 按优先级取用。

> 范围：仅 `internal/memory/*` + 直接接线层（`cmd/agent`、`internal/orchestrator/memory_bridge.go`、`internal/tools/memory_tools.go`、`internal/agentloop/trajectory_memory.go`、`internal/agentloop/pg_trajectory_store.go`）。
>
> 与 `doc-gaps.md` 的关系：`doc-gaps.md` 是跨包死代码/接线缺失清单（已修复条目沉淀），本文件是**未修复缺陷专项**。

## 2. 总体评价

正面：

- 多层语义模型（4 类 typed + episodic + core + trajectory）覆盖了 Agent 长期记忆的主要分类维度。
- 热/冷双层 + RRF k=60 融合（`internal/memory/hybrid.go::Retrieve`）+ Promote/Demote/Touch/Decay 四套异步队列已全部跑通。
- 抗 Prompt Injection（双消息哨兵）+ PII 双重遮蔽 + 多租户 (userID, projectID) 隔离全链路落地。
- 20+ Prometheus metrics 覆盖 store/retrieve/conflict/dedup/decay/extractor/distill/blackboard/promote/touch/scan_truncated 等关键路径。
- 4 个后台 goroutine 都接入 shutdown 链，detached flush context 保证最后一次落库。

负面：

- 子系统已经处于「过度工程」的临界点：`hybrid.go` 单文件 1200+ 行，4 个异步队列 + 双写一致性 + RRF + Promote/Demote/Touch/Decay 都耦合在 `HybridStore` 上，新人理解成本陡峭。
- "召回 → LLM 是否真的用了" 的反馈闭环缺失，Decay/Score 调整缺乏可信信号。
- Distiller 默认 `Enabled=false`，蒸馏后的 episodic 不删除，长期会让 PG 表无限膨胀。

## 3. 已知未修复缺陷一览（按优先级 + 严重程度排序）

### 3.1 P0 — 架构原理层面硬伤

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| ✅ **AUDIT-P0-1** | **召回质量没有反馈闭环**。`access_count` 累加只能证明"被召回"，无法证明"被 LLM 实际使用"。Decay 把"召回但 LLM 忽略"和"召回且被采纳"同等对待 | `internal/memory/hybrid.go::enqueueTouches` (line ~855) 只看返回集；prompt 注入路径 `internal/orchestrator/memory_bridge.go::buildLongTermMemory` 不携带 memory ID，LLM 也无回写接口 | Score 信号长期失真，Decay 公平性悖论：高质量 memory 与噪声 memory 的衰减节奏只取决于"是否被检索 hit"，与"是否真的有用"无关 |
| ✅ **AUDIT-P0-2** | **Distiller 实际"半死状态"**。默认 `Enabled=false`；6h 周期；蒸馏后 episodic 不删除，PG 表会无限膨胀（部分索引在但表本身在涨） | `cmd/agent/memory_adapter.go:223-225` `if !cfg.Enabled { return }`；`docs/architecture/25_memory.md::§16.5#4` 明确"不删 episodic 作为审计"；grep `DELETE FROM memories WHERE type='episodic'` 全仓 0 命中 | 单 tenant 每天 ~20 个任务 × 365 天 ≈ 7300 行/年，IVFFlat `lists=100` 在 episodic 长期占主表的情况下召回率会进一步下降；磁盘成本和查询代价随时间线性涨 |
| ✅ **AUDIT-P0-3** | **小库召回 IVFFlat lists=100 反而比 brute force 差**。Memory 表预期是"每 tenant 几十到几百条"，但 IVFFlat 的 100 个聚类对 < 1k 行严重欠拟合 | `internal/memory/pg_cold.go::Migrate` 写死 `WITH (lists = 100)`；没有自动切到 brute force / hnsw 的策略；pgvector 官方推荐 `lists ≈ rows/1000` 对小库应该是 1-5 | 早期部署（< 10k 行）召回率可能只有 70-80%，被 P1 #9 的 K=30 候选放大也不一定能弥补 |
| **AUDIT-P0-4** | **`session` 与 `memory` 之间没有持续学习的桥**。session 的"用户对 agent 回复的反馈"（点踩/重新生成/纠正）没有反向训练 memory 重要性 | `docs/architecture/25_memory.md::§10.3` 自白"故意解耦"是设计选择；`internal/session/manager.go` 与 `internal/memory/*` 之间没有数据通道 | 缺少 RLHF-lite 能力：用户多次说"不要这样回答"，下次召回时不会自动降权"造成该回答的 memory" |

### 3.2 P1 — 功能正确性 / 数据治理

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **AUDIT-P1-1** | **跨 Type 重复无法识别**。ConflictResolver 强制同 Type 才视为冲突，但实际上同一句"我喜欢 tabs"可能被 LLM 第一次标为 `preference`，第二次标为 `knowledge` → 两条独立存活 | `internal/memory/conflict.go::FindConflicts` 显式 `c.Type != newMem.Type → skip` | RRF 召回 top-K 出现"看起来重复但 Type 不同"的条目，token 浪费 + score 分散 |
| **AUDIT-P1-2** | **PII 全部基于 regex**，对企业内部的非标准 token / RBAC role / 业务 secret 完全失效；blackboard pub/sub 广播 `Memory.Content`，没有第二道遮蔽 | `internal/memory/extractor.go::piiMasker` 是固定 regex；`internal/memory/blackboard.go::Publish` 整条 Memory JSON 广播 | 真实生产中的"内部 token"格式 LLM 抓不到也写进库，blackboard 订阅方拿到原文（用于多 agent 协作） |
| ✅ **AUDIT-P1-3** | **GDPR/删除接口缺失**。`docs/architecture/25_memory.md::§12` TODO 中提到"用户能查看/删除自己的 Memory"——但代码里没有 user-facing delete API；`DeleteByIDs` 是 dedup 唯一调用方 | grep `DeleteByIDs` 命中点：`internal/memory/pg_cold.go` 实现 + `internal/memory/hybrid.go::dedupMerge` 调用 → 仅此两处 | 合规硬需求未满足；用户无法主动清除"agent 记住的关于我的事" |
| **AUDIT-P1-4** | **CoreMemory 完全 tool-driven，没有自动晋升**。LLM 必须主动调 `core_memory_append` 才写；Extractor 产出的高 importance preference 不会自动晋升到 core | `internal/tools/memory_tools.go::handleCoreMemoryAppend` 是唯一写入路径；Extractor 写入路径 `extractor.go::ExtractFromInteraction` 与 CoreMemoryManager 完全无连接 | 「最稳定的 user persona」必须依赖 LLM 主动调 tool，但 LLM 经常忽略；用户 30 次说"我喜欢中文回答"，core memory 可能一次都没写 |
| **AUDIT-P1-5** | **维度迁移路径缺失**。`embedding vector(1536)` 硬编码进 schema；切换 `text-embedding-3-large` (3072d) 需要重建表 → 没有"双 column 渐进迁移"支持 | `internal/memory/pg_cold.go::Migrate` 拼 `vector(%d)` 但只在 CREATE TABLE 时生效；ALTER COLUMN 不支持向量维度变更（pgvector 限制） | 升级 embedding 模型即等于 schema migration + 全量回填 embedding，是高成本风险点 |
| **AUDIT-P1-6** | **召回是 per-turn 重算，KV-cache 友好性形同虚设**。`memory_bridge.go:158-165` 说 [Long-Term Memory] 块放后部以保前缀稳定，但 query 每 turn 不同 → 召回结果每 turn 不同 → KV-cache 的 prompt suffix 一直在变 | `internal/orchestrator/memory_bridge.go::buildLongTermMemory` 每次 react 循环都跑 `retrieveBucketedMemories` | 实际 KV-cache 命中率被吃掉一大块，但日志没体现 |
| ✅ **AUDIT-P1-7** | **Episodic 永久保留，GC 缺失**。`DistilledAt` 标记后无清理逻辑；表会无限膨胀，部分索引（`idx_memories_episodic_undistilled`）只过滤未蒸馏，主表索引仍全量扫 | `internal/memory/pg_cold.go::MarkDistilled` 只 UPDATE 不 DELETE；没有 `runMemoryEpisodicGCLoop` | 上线 1 年后 episodic 占主表 90%+，pgvector IVFFlat 查询 + GROUP BY 都会被拖慢 |

### 3.3 P2 — 工程治理 / 可演进性

| ID | 现象 | 关键证据 | 影响 |
|---|---|---|---|
| **AUDIT-P2-1** | **认知复杂度爆表**。`hybrid.go` 单文件 1200+ 行，4 个异步队列 + 双写一致性 + RRF + Promote/Demote/Touch/Decay 全在 HybridStore 一个类里 | `internal/memory/hybrid.go` 总行数（参考实际）；`HybridStore` struct 字段 10+ | 新人入门成本极高；单元测试在但回归 PR 经常触发"我以为这块跟那块无关"的微妙 bug |
| ✅ **AUDIT-P2-2** | **PG 真实集成测试缺位**。`DedupTx` / `RetrieveByVectorAndType` / `ListActiveDecayTenants` / `MarkDistilled` 等 SQL 关键路径用 fake / mock 覆盖，没有 testcontainers | `tests/internal/memory/*` 多用 `miniredis` / `fakeStore`；grep `testcontainers` 全仓 0 命中 | SQL 语法或事务行为变化（pgvector 升级、PG 主版本切换）会绕过 CI |
| **AUDIT-P2-3** | **配置项默认值是经验值，缺压测**。`Threshold=0.7` / `dedupOversample=10` / `MaxConflictsToDedup=32` / `QueueSize=256` 都是文档里写"经验值"，但没有数据支撑 | `internal/memory/hybrid.go:82-95` + `internal/memory/extractor.go:39-57` 全是注释说"经验值"；`MEM-P2-3` 仅做了"可配置"而未做"知道该配多少" | 真实流量 profile 出来后大概率要调；目前 dashboard 也没有"建议下调到 X" 的明示线 |
| **AUDIT-P2-4** | **错误分级不清**。hot Store 失败只 Warn，blackboard Publish 失败只 Debug；缺乏"持久层失败需要 alert + DLQ 重试" 的分级 | `internal/memory/hybrid.go::Store` line ~267 仅 Warn；`publishEvent` 只 Debug | 关键失败被淹没在日志里，运维很难第一时间知道 |
| **AUDIT-P2-5** | **可解释性缺失**。用户问"agent 为什么知道我用 tabs"，没有 audit trail 显示注入了哪条 memory ID + 何时蒸馏 | `internal/orchestrator/memory_bridge.go::buildLongTermMemory` 拼 `[type] content` 但不带 ID；prompt 注入路径不带 ID 反射 | 既不可解释也不可追溯，AUDIT-P0-1 的反馈闭环要先解决这个 |

## 4. 推荐优先级（如果只能动 3 项）

按"修复 ROI / 实施成本"权衡：

1. **AUDIT-P0-1（反馈闭环）** —— 在 `buildLongTermMemory` 注入时给每条 memory 一个 `[mem:<id>]` 标签，让 LLM 在 reasoning trace 里引用；ReAct 结束后扫一遍引用集，给"被引用的 memory" +0.05 score boost。这是把"召回"和"使用"分开统计的唯一可信信号，**也是 P2-5 可解释性的前置条件**。
2. **AUDIT-P1-7 + AUDIT-P1-3（episodic GC + GDPR）** —— 加 `runMemoryEpisodicGCLoop` 定期删 `distilled_at IS NOT NULL AND distilled_at < now() - 30d`；同时暴露 `DELETE /api/v1/memory/user/{userID}` 接口（含 cold + hot + blackboard 广播 deletion 事件）。前者解决数据膨胀，后者是合规硬需求。
3. **AUDIT-P2-2（PG 真实集成测试）** —— 用 `testcontainers-go` 起 pgvector 容器，至少覆盖 `DedupTx` 事务回滚、`RetrieveByVectorAndType` 的 type filter、`Decay` 的 score floor 三条关键路径。当前 30+ 处修复都没碰真 PG，是稳定性的最大盲点。

## 5. 附录：与既有时间线的交叉引用

| 本文 ID | 与 25_memory.md 时间线的关系 |
|---|---|
| AUDIT-P0-1 | 与 §19 (P0 #4 AccessCount 累加) 互补：§19 解决了"Touch 不累加"，但没解决"Touch ≠ Use" |
| AUDIT-P0-2 | 与 §16 (Distiller 空转) + §18 (AutoDiscover) 互补：那两轮让 Distiller 能跑起来，但没解决"跑起来之后数据怎么收摊" |
| AUDIT-P0-3 | 文档 §7.2 自己讨论了 ivfflat vs hnsw，但没给"小库切 brute force"路径 |
| AUDIT-P1-1 | 文档 §8.1 明确"同 Type 才算冲突"是 by design，本审计提出这个 design choice 在跨 Type LLM 标签噪声下会失效 |
| AUDIT-P1-2 | 与 §14::MEM-P1-3 (PII regex masking) 同源；本审计指出 regex 防线对企业 token 不够 |
| AUDIT-P1-5 | 文档 §11 "1536d hard-coded schema" 自己列为权衡，本审计建议升级为可演进点 |
| AUDIT-P1-7 | §16.5#4 显式选择"不删 episodic"，本审计提出 GC 是必须的 |
| AUDIT-P2-1 | 多轮 P0/P1 都加在 HybridStore 上，没有人停下来做拆分；可能要为 HybridStore 抽出 `BatcherCoordinator` / `Tier` 子类 |
| AUDIT-P2-2 | §22 + §23 的"已知尚未覆盖"段已经自白这一点 |

## 6. 验证方法（用于将来 PR 自检）

后续若有 PR 修复任一项，请在 `docs/architecture/25_memory.md` 末尾追加 §25/§26… 章节，并按以下结构写：

```
## §25. AUDIT-P0-N: <现象一句话>

### 病征
（修复前的具体证据，最好 file:line）

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

完成后回到本文件，把对应行的"P0/P1/P2" 标记加 ✅，并指向 §N 修复段。

---

**与 `llmdoc/memory/doc-gaps.md` 协同**：
- `doc-gaps.md` 是"全局/跨包"的死代码与接线缺失主索引
- 本文件是 memory 子系统专项审计快照
- 修复后双源同步：本文件标 ✅ + doc-gaps 末尾追加一行"AUDIT-X 已闭环 → 见 25_memory.md::§N"
