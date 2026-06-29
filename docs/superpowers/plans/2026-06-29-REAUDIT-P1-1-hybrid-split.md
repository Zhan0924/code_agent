# REAUDIT-P1-1: HybridStore 拆分（AUDIT-P2-1 假闭环）

**Audit ID:** REAUDIT-P1-1
**来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:52`
**Goal:** `hybrid.go` 仅保留 struct/ctor/setters，领域方法拆到 `hybrid_*.go`

## Step 1 复核

**判定：** 真实存在（部分拆分已做但未闭环）

- `hybrid_queues/decay/dedup/rrf.go` 已存在，但 `hybrid.go` 仍 ~781 行
- `AUDIT-P2-1` 在 01-audit 标 ✅ 与仓库事实不符

## 修复

| 文件 | 职责 |
|---|---|
| `hybrid.go` | struct、NewHybridStore、setter（≤120 行） |
| `hybrid_embed.go` | embedText、降级观测、TestFailingEmbedder |
| `hybrid_store.go` | Store、publishEvent |
| `hybrid_retrieve.go` | Retrieve、RetrieveByType、RetrieveCandidates |
| `hybrid_list.go` | List、ListByType |
| `hybrid_admin.go` | GDPR、distill、touch、boost |
| `hybrid_queues/decay/dedup/rrf.go` | 已有 |

- 契约测试 `hybrid_structure_test.go`
- 校验脚本 `scripts/verify-reaudit-p1-1.sh`
