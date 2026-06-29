# REAUDIT-P0-2: CoreMemory PII 缺口

**Audit ID:** REAUDIT-P0-2
**来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:39`
**Goal:** Core Memory 所有写入路径在持久化前过 PIIMasker

## Step 1 复核

**判定：** 真实存在

- `memory_tools.go:122,187` 直写 `AppendToSectionScoped` / `ReplaceInSectionScoped`，无 Mask
- `core_memory.go` 全文无 PIIMasker
- `hybrid.go:124` / `blackboard.go:45` 已有 Mask，Core Memory 路径遗漏

## 修复

- `RedisCoreMemory` 注入 `PIIMasker`（默认 `NewPIIMasker()`）
- `maskForPersist` 在 append/replace 写入前统一 Mask + 结构化日志 `audit_id=REAUDIT-P0-2`
- 单测 `core_memory_test.go`；dev endpoint `POST /api/v1/test_core_memory_pii`
- 校验脚本 `scripts/verify-reaudit-p0-2.sh`
