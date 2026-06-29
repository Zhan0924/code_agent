# REAUDIT-P0-1: MarkDistilled DELETE/UPDATE 二义性

**Audit ID:** REAUDIT-P0-1
**审计文档来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:38`
**优先级:** P0
**Goal:** 让 `MarkDistilled` 兑现注释与 §36 文档承诺：蒸馏后 SET `distilled_at`，由 `DeleteOldEpisodic` 在 30d 观察窗后物理删除。

## 1. Step 1 复核结论

**判定：** 真实存在

**证据：**
- `internal/memory/pg_cold.go:542-551` — 注释写幂等 UPDATE，SQL 为 `DELETE FROM memories`
- `internal/memory/pg_cold.go:563-575` — `DeleteOldEpisodic` 依赖 `distilled_at IS NOT NULL`，MarkDistilled 从不 SET 该列
- `internal/api/memory_handlers.go:164` — explain API 的 `distilled_at` 对已蒸馏 episodic 恒 null
- `tests/internal/memory/distiller_test.go:202-211` — 单测曾断言"蒸馏后 DELETE"，与 §36 契约矛盾

## 2. 修复策略

采用 **UPDATE 路径**（保留 30d 观察窗）：
- `MarkDistilled`: `UPDATE ... SET distilled_at = NOW() WHERE id = ANY($1) AND distilled_at IS NULL`
- 单测 fake store 同步改为 SET `DistilledAt`
- 集成测试新增 `MarkDistilledAndDeleteOldEpisodic` 子用例

## 3. 可观测性

| 位置 | 字段 | Step 4 grep |
|---|---|---|
| `pg_cold.go::MarkDistilled` Info | `audit_id=REAUDIT-P0-1, op=mark_distilled, before.count, after.marked, result` | `rg 'audit_id=REAUDIT-P0-1.*mark_distilled'` |

## 4. 验证

- `go test ./internal/memory/... ./tests/internal/memory/...`
- `scripts/verify-reaudit-p0-1.sh`（Docker 三件套）
