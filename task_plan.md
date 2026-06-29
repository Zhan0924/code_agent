# Task Plan: 闭环 memory-system-02-audit.md 中的 11 条 REAUDIT 缺陷

## Goal
按 audit-driven-fix-loop 五步循环逐条闭环 `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md` 里 11 条未闭环 REAUDIT-ID，每条独立 Plan + 实现 + Docker 三件套校验 + safe-push，并同步把第一份 audit doc 里被推翻的 ✅ 标记改为 ⚠️/❌。

## Current Phase
Phase 2

## Phases

> 每个 Phase 内部都跑 `.claude/skills/audit-driven-fix-loop/SKILL.md` 的五步循环：
> Step 1 代码复核 → Step 2 plan 模式生成方案 → Step 3 实现（含可校验日志） →
> Step 4 Docker 部署 + API/日志/DB 三件套校验 → Step 5 safe-push 提交 + 勾 ✅

### Phase 1: REAUDIT-P0-1 — MarkDistilled DELETE/UPDATE 二义性（数据完整性 bug）
- [x] Step 1: 代码复核 `pg_cold.go:542-575` 实际 SQL 与注释一致性
- [x] Step 2: 方案文档 `docs/superpowers/plans/2026-06-29-REAUDIT-P0-1-mark-distilled-semantics.md`
- [x] Step 3: 实现（UPDATE distilled_at + 结构化日志 + 单测/集成测）
- [x] Step 4: Docker + verify-reaudit-p0-1.sh PASS
- [x] Step 5: safe-push → `b332948`
- **Status:** complete

### Phase 2: REAUDIT-P0-2 — CoreMemory PII 缺口
- [x] Step 1: 代码复核 — memory_tools 直写、core_memory 无 Mask
- [x] Step 2: `docs/superpowers/plans/2026-06-29-REAUDIT-P0-2-core-memory-pii.md`
- [x] Step 3: `RedisCoreMemory.maskForPersist` + 单测 + dev endpoint
- [x] Step 4: `verify-reaudit-p0-2.sh` PASS
- [x] Step 5: safe-push → `b332948`
- **Status:** complete

## Current Phase
Phase 4

### Phase 3: REAUDIT-P0-3 — Citation 反馈闭环对低成本模型形同虚设（观测增强）
- [x] Step 1: 代码复核 — 注入与 cite 对比无 miss 信号
- [x] Step 2: `docs/superpowers/plans/2026-06-29-REAUDIT-P0-3-citation-feedback-observability.md`
- [x] Step 3: `recordCitationFeedback` + `MemoryCitationFeedbackTotal` + dev endpoint
- [x] Step 4: `verify-reaudit-p0-3.sh` PASS
- [x] Step 5: safe-push
- **Status:** complete

## Current Phase
Phase 5

### Phase 4: REAUDIT-P0-4 — Embedder 失效监控盲区
- [x] Step 1: 代码复核 — embedText 无 embedder tier 指标
- [x] Step 2: `docs/superpowers/plans/2026-06-29-REAUDIT-P0-4-embedder-degrade-observability.md`
- [x] Step 3: `MemoryFailuresTotal{embedder}` + `MemoryRetrieveDegradedTotal` + dev endpoint
- [x] Step 4: `verify-reaudit-p0-4.sh` PASS
- [x] Step 5: safe-push
- **Status:** complete

### Phase 5: REAUDIT-P1-1 — HybridStore 拆分从未发生（AUDIT-P2-1 假闭环）
- [ ] 五步循环（重点：先完成 `docs/superpowers/plans/2026-06-29-AUDIT-P2-1-hybrid-refactor.md` 已开头的拆分，再 ✅）
- **Status:** pending

### Phase 6: REAUDIT-P1-2 — 匿名/默认值兜底在 API 层与 orchestrator 层语义不一致
- [ ] 五步循环（重点：统一兜底策略；选 reject vs accept；文档明示）
- **Status:** pending

### Phase 7: REAUDIT-P1-3 — Core Memory 自动晋升阈值与去重
- [ ] 五步循环（重点：threshold 改可配置；同 content 去重；§34 调优表加 `core_promote_threshold`）
- **Status:** pending

### Phase 8: REAUDIT-P1-4 — Session feedback 依赖正则匹配 [mem:<id>]
- [ ] 五步循环（重点：消息结构化字段 cited_memory_ids 持久化；正则作为 fallback）
- **Status:** pending

### Phase 9: REAUDIT-P2-1 — dedupOversample = 10 写死，§34 未列入
- [ ] 五步循环（重点：常量改 config 项；§34 调优表追加一行）
- **Status:** pending

### Phase 10: REAUDIT-P2-2 — testcontainers 集成测试覆盖面狭窄
- [ ] 五步循环（重点：单测拆为 BoostScoreBatch / MarkDistilled / DeleteByUser / cross-type 4 个用例）
- **Status:** pending

### Phase 11: REAUDIT-P2-3 — 仓库卫生（未跟踪诊断脚本 + 4 份未归档计划）
- [ ] 五步循环（重点：归档/删除 `split_hybrid.py` `list_funcs.py` 等；4 份 plan 文档完成度判定）
- **Status:** pending

## Notes / Constraints

- **严格串行**：当前 Phase 未到 complete 前禁止开始下一个
- **绝对禁止**：一笔提交合多条 REAUDIT-ID；只跑单测就宣称修完；只标 ✅ 不部署
- **跨文档同步**：每条 Phase 完成时同步更新两份 audit doc（02-audit 加 ✅ + 01-audit 把被推翻的 AUDIT 项 ✅ 改 ⚠️/❌）
- **Hook 兜底**：`.cursor/hooks/stop.sh` 会阻止在仍有未闭环 REAUDIT-ID 时停止
- **会话边界**：单次会话可能只覆盖 1-2 个 Phase，剩余 Phase 由后续会话继续；hook 的 `userPromptSubmit` 会在新会话开头注入当前 task_plan 状态用于恢复
