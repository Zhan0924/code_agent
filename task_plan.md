# Task Plan: 闭环 memory-system-03-audit.md 中的 13 条 REAUDIT3 缺陷

## Goal

按 audit-driven-fix-loop 五步循环（代码复核 → plan → 实现 → Docker 三件套校验 → safe-push）逐条闭环 `llmdoc/memory/reflections/2026-06-26-memory-system-03-audit.md` 里 13 条 REAUDIT3-ID。每个 Phase 单独 safe-push，一笔提交一个 ID。

## Current Phase

**Phase 1 待用户决策**（见 Notes：13 条 ID 中含设计性变更，需用户先确认方向再启动）

## Phases

| Phase | ID | 现象一句话 | 类别 | Status |
|-------|-----|-----------|------|--------|
| 1 | REAUDIT3-P0-1 | Citation 反馈对低成本模型 silent 失效，需机制性兜底（structured tool 或 post-hoc 余弦匹配） | 设计性硬伤 | pending |
| 2 | REAUDIT3-P0-2 | embedder degraded 路径中文 ILIKE 召回崩塌，需补分词 / BM25 / pg_trgm 兜底 | 设计性硬伤 | pending |
| 3 | REAUDIT3-P1-1 | Session feedback 反馈解析依赖正则，需把 cited_memory_ids 持久化为消息结构化字段 | 设计性偏差 | pending |
| 4 | REAUDIT3-P1-2 | Distiller 蒸馏粒度无量化挂钩，需 episode 聚类 + score 按 log(episode_count) 挂钩 | 设计性偏差 | pending |
| 5 | REAUDIT3-P1-3 | CoreMemory 自动晋升无"持续观察"机制，需 N 次确认 + cosine 去重 | 设计性偏差 | pending |
| 6 | REAUDIT3-P1-4 | RetrieveCandidates 双 tier 失败"伪通过"，需返回 ErrDedupUnavailable 或加计数器 | 设计性偏差 | pending |
| 7 | REAUDIT3-P1-5 | handleListMemory 的 episodic 过滤契约不显，需 `?include_episodic` 开关或文档化 | 工程债 | pending |
| 8 | REAUDIT3-P1-6 | embedding 维度切回老模型时老数据死锁，需 `embedding_model` 列 + per-model RRF | 工程债 | pending |
| 9 | REAUDIT3-P1-7 | dedupCandidateLimitCap 仍是 const，需可配置 + §34 调优表补一行 | 工程债 | pending |
| 10 | REAUDIT3-P1-8 | Extractor LLM 解析失败不重试 + 无计数器，需一次 retry-with-backoff + `extractor_parse_errors_total` | 工程债 | pending |
| 11 | REAUDIT3-P2-1 | testcontainers 拆分 case，覆盖 BoostScoreBatch / MarkDistilled / DeleteByUser / cross-type | 测试与流程 | pending |
| 12 | REAUDIT3-P2-2 | 仓库卫生：清理未跟踪的 `split_hybrid.py` / `deploy-p0.sh` / 4 个 plans 文件去留 | 测试与流程 | pending |
| 13 | REAUDIT3-P2-3 | `docs/architecture/25_memory.md` 2000+ 行拆出 §14–§38 到 CHANGELOG.md | 测试与流程 | pending |

## Decisions Made

| Decision | Rationale |
|----------|-----------|
| 按"设计性硬伤 → 设计性偏差 → 工程债 → 测试流程"排序 | P0-1 / P0-2 直接影响生产用户体验，先动；P2 的 testcontainers 是 REAUDIT3-P0-2/REAUDIT-P0-1 同源问题的工程基础，但短期靠 PR review 拦得住 |
| 每 Phase 单独 safe-push | AGENTS.md 强制"一笔提交一个 AUDIT-ID"，便于回滚 |
| Phase 1 / Phase 2 需用户先确认设计方向再启动 | 涉及机制性变更（structured tool vs post-hoc cosine / 分词器选型 jieba vs pg_trgm vs BM25），不能 LLM 单边决定 |

## Errors Encountered

| Error | Attempt | Resolution |
|-------|---------|------------|
|       | 1       |            |

## Notes

- **Phase 1 (REAUDIT3-P0-1)** 需要先和用户对齐 citation 反馈机制选型：
  - 选项 A：让 LLM 调 `cite_memory(ids=[...])` structured tool（要改 prompt + tool schema + 反馈解析）
  - 选项 B：post-hoc 余弦匹配（assistant 回复 embed → 与候选 memory 算 cosine → 过阈值视为 used）
  - 选项 C：A + B 叠加（结构化优先，余弦兜底）
- **Phase 2 (REAUDIT3-P0-2)** 需要先和用户对齐降级路径搜索方案：
  - 选项 A：PG `pg_trgm` 模糊匹配（部署最简单，跨语言）
  - 选项 B：PG `tsvector` + 中英文双 dictionary（生产质量更好但运维成本高）
  - 选项 C：BM25 索引（最强但要引外部依赖）
  - 选项 D：jieba/简单切词 + 多关键词 OR ILIKE（折中方案）
- **Phase 11 (REAUDIT3-P2-1)** 是基础工程能力，可与 Phase 1/2 并行启动（不冲突）
- 每完成一个 Phase 都要：
  1. 在本文件把对应 Status 改 `complete`
  2. 在 `llmdoc/memory/reflections/2026-06-26-memory-system-03-audit.md` 把对应 REAUDIT3-ID 加 ✅ + 指向 plan 路径
  3. 在 `docs/architecture/25_memory.md` 末尾追加 §39/§40… 修复段（结构与 §35–§38 对齐）
  4. safe-push（commit message: `REAUDIT3-Px-y: <一句话现象>`）
