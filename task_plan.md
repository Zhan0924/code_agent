# Task Plan: 闭环 memory-system-02-audit.md 中的 11 条 REAUDIT 缺陷

## Goal
按 audit-driven-fix-loop 五步循环逐条闭环 `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md` 里 11 条未闭环 REAUDIT-ID，每条独立 Plan + 实现 + Docker 三件套校验 + safe-push。

## Current Phase
Phase 8

## Phases

### Phase 1: REAUDIT-P0-1 — MarkDistilled DELETE/UPDATE 二义性
- **Status:** complete

### Phase 2: REAUDIT-P0-2 — CoreMemory PII 缺口
- **Status:** complete

### Phase 3: REAUDIT-P0-3 — Citation 反馈可观测性
- **Status:** complete

### Phase 4: REAUDIT-P0-4 — Embedder 失效监控盲区
- **Status:** complete

### Phase 5: REAUDIT-P1-1 — HybridStore 拆分
- **Status:** complete

### Phase 6: REAUDIT-P1-2 — 租户 ID 兜底语义统一
- **Status:** complete

### Phase 7: REAUDIT-P1-3 — Core Memory 自动晋升阈值与去重
- [x] Step 4: `verify-reaudit-p1-3.sh` PASS
- **Status:** complete

### Phase 8: REAUDIT-P1-4 — Session feedback 依赖正则匹配 [mem:<id>]
- [ ] 五步循环（cited_memory_ids 持久化 + 正则 fallback + miss 指标）
- **Status:** pending

### Phase 9: REAUDIT-P2-1 — dedupOversample 可配置
- **Status:** pending

### Phase 10: REAUDIT-P2-2 — testcontainers 集成测试拆分
- **Status:** pending

### Phase 11: REAUDIT-P2-3 — 仓库卫生
- **Status:** pending
