# REAUDIT-P2-3 — 仓库卫生

## 方案
删除未跟踪诊断脚本（`split_hybrid.py`、`list_funcs.py`、`proxy_forwarder.py`、`pull_aggressively.sh`、`test_feedback.go`、`test_pii.go`、`.test_session_id`）。

删除已落地/被 REAUDIT plan 取代的 4 份草稿计划：
- `AUDIT-P2-1-hybrid-refactor` → REAUDIT-P1-1 已完成
- `episodic-gc` / `gdpr-delete` / `pg-integration-test` → 主分支已接线或 REAUDIT-P2-2 覆盖

## 验收
`scripts/verify-reaudit-p2-3.sh` 确认上述路径不存在且 agent healthz 正常。
