# REAUDIT-P1-3 — Core Memory 自动晋升阈值与去重

## 判定
真实存在：`extractor.go` 硬编码 `importance >= 0.9`；`core_memory.go` append 无重复行检测。

## 方案
1. `MemoryConfig.core_promote_threshold` + `Extractor.SetCorePromoteThreshold`
2. `AppendToSectionScoped` 用 `sectionContainsLine` 跳过重复行，日志 `core_memory_dedup_skip`
3. auto-promote 日志 `audit_id=REAUDIT-P1-3` + `threshold` 字段
4. §34 调优表追加 `core_promote_threshold` 行
5. 单测 `TestRedisCoreMemory_AppendDedupSkipsDuplicateLine`
6. dev endpoint `POST /api/v1/test_core_memory_dedup`

## 回滚
移除 config 字段与 dedup 分支；恢复硬编码 0.9。

## 验收
`scripts/verify-reaudit-p1-3.sh`：API `deduped=true` + 容器日志含 `"audit_id":"REAUDIT-P1-3"`。
