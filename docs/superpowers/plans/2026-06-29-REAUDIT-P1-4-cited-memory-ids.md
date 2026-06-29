# REAUDIT-P1-4 — Session feedback 结构化 cited_memory_ids

## 判定
真实存在：feedback 仅 `ParseCitationIDs(msg.Content)`，content 被 sanitize 后 ID 丢失。

## 方案
1. `models.Message.cited_memory_ids` JSON 字段
2. `orchestrator.assistantMessage` 在持久化 assistant 时写入结构化 IDs
3. `memory.ResolveCitedMemoryIDs` 优先 structured、正则 fallback
4. `MemoryFeedbackCitedMissTotal` + 结构化日志
5. dev endpoint `POST /api/v1/test_feedback_cited_resolve`

## 验收
`scripts/verify-reaudit-p1-4.sh`：`cited_source=structured` + 日志 `"audit_id":"REAUDIT-P1-4"`。
