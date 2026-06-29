# REAUDIT-P0-4: Embedder 失效监控盲区

**Audit ID:** REAUDIT-P0-4
**来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:46`
**Goal:** embedder 失败时上报告警指标，retrieve 降级到 ILIKE 可观测

## Step 1 复核

**判定：** 真实存在

- `hybrid.go::embedText` 失败仅 Warn 日志，无 `memory_failures_total{tier="embedder"}`
- `Retrieve` 降级 ILIKE 路径无独立 counter，监控全绿

## 修复

- `embedText` 失败 → `MemoryFailuresTotal{embedder,embed,warn|error}`
- `Retrieve`/`RetrieveByType` ILIKE 降级 → `MemoryRetrieveDegradedTotal{reason}`
- 结构化日志 `audit_id=REAUDIT-P0-4`
- dev endpoint `POST /api/v1/test_embedder_degrade` + `verify-reaudit-p0-4.sh`
