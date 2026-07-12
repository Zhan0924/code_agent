# Progress: memory-system-03-audit 闭环跟踪

## 当前状态

**审计文档**：`llmdoc/memory/reflections/2026-06-26-memory-system-03-audit.md` 已写入（13 条 REAUDIT3-P0/P1/P2 ID）。

**task_plan**：`task_plan.md` 已立项 13 个 Phase，全部 `pending`。

**audit-driven-fix-loop 启动状态**：**暂缓**（等待用户决策）。

## 暂缓启动原因

2026-06-29 18:17（UTC+8）：assistant 已按 `AGENTS.md` 强制规则在写入审计文档后立即向用户出示 `AskQuestion`，给出 4 个选项：

1. 先并行启动 Phase 11 (REAUDIT3-P2-1, testcontainers 拆分)，同时讨论 Phase 1/2 选型 **(推荐)**
2. 先讨论 Phase 1/2 设计方向，方向确定后再串行启动
3. 立即按 task_plan.md 顺序串行启动
4. skip audit-driven-fix-loop

**用户跳过了问题**（"Questions skipped by the user"），未给出明确指令。

assistant 解读：用户本次的明确请求只是"将上述缺陷分析添加到 03-audit.md 中"，跳过选项视为**不希望立刻进入修复循环**。文档已经归档，task_plan 已经立项，下次用户主动说"开始闭环 REAUDIT3-Px-y" / "按 task_plan 走"时再启动五步循环。

## 已识别的关键决策依赖（启动前必须先对齐）

| Phase | ID | 决策项 | 候选 |
|-------|-----|-------|------|
| 1 | REAUDIT3-P0-1 | citation 反馈机制选型 | A) structured tool `cite_memory(ids=[...])` / B) post-hoc 余弦匹配 / C) A+B 叠加 / D) 仅补观测性 |
| 2 | REAUDIT3-P0-2 | embedder degraded 路径降级方案 | A) PG `pg_trgm` / B) PG `tsvector` + jieba_pg / C) 应用层 jieba + 多关键词 ILIKE / D) 外部 BM25 |
| 5 | REAUDIT3-P1-3 | CoreMemory 自动晋升"持续观察"机制 | 阈值 0.9 + N 次观察 → N 取值？时间窗口取值？ |
| 11 | REAUDIT3-P2-1 | testcontainers Podman 兼容方案 | 现有 `TESTCONTAINERS_RYUK_DISABLED=true` 是否够，是否需要 testcontainers-go v0.30+ 的 Podman provider |

## 可并行启动的 Phase（无决策依赖）

- **Phase 7 (REAUDIT3-P1-5)**：handleListMemory 加 `?include_episodic` 开关或文档化（纯工程）
- **Phase 9 (REAUDIT3-P1-7)**：`dedupCandidateLimitCap` 改为可配置 + §34 调优表补一行（纯工程）
- **Phase 10 (REAUDIT3-P1-8)**：Extractor LLM 解析失败加 retry + `extractor_parse_errors_total`（纯工程）
- **Phase 12 (REAUDIT3-P2-2)**：仓库卫生清理（纯流程）
- **Phase 13 (REAUDIT3-P2-3)**：`25_memory.md` 拆出 CHANGELOG.md（纯文档）

## 下一步

等用户指令。可能的触发关键词：
- "开始闭环 REAUDIT3-P0-1" → 进入 Phase 1（先用 AskQuestion 收选型）
- "按 task_plan 走" → 从 Phase 7/9/10 这种无决策依赖的开始并行启动
- "改 task_plan 顺序" → 调整 Phases 表格
- "skip audit-driven-fix-loop" → 把所有 Phase 状态改为 `cancelled` 并归档
