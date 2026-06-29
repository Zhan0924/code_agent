# REAUDIT-P2-1 — dedupOversample 可配置

## 方案
- `MemoryConfig.dedup_oversample` + `HybridStore.SetDedupOversample`
- 启动日志 `memory subsystem effective config` 输出 `dedup_oversample`
- §34 调优表追加一行

## 验收
`scripts/verify-reaudit-p2-1.sh` 检查启动日志含 `dedup_oversample` 与 `"audit_id":"REAUDIT-P2-1"`（memory_adapter 配置日志）。
