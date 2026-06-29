# REAUDIT-P1-2: 匿名/默认值兜底语义统一

**Audit ID:** REAUDIT-P1-2
**来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:53`
**Goal:** API 与 orchestrator 对空 user_id/project_id 使用同一套 fallback

## Step 1 复核

**判定：** 真实存在

- API `memory_handlers.go` 空值 → `anonymous` + `default`
- orchestrator `extractMemoriesAsync` / `recordTaskEpisodeAsync` 空 userID 直接 skip

## 修复

- `session.NormalizeTenantIDs` 单一真相源
- orchestrator `resolveTenantIDs`：context → session → NormalizeTenantIDs（不再 skip）
- dev endpoint `POST /api/v1/test_tenant_normalize`
- 校验脚本 `scripts/verify-reaudit-p1-2.sh`
