# REAUDIT-P0-3: Citation 反馈闭环可观测性

**Audit ID:** REAUDIT-P0-3
**来源:** `llmdoc/memory/reflections/2026-06-26-memory-system-02-audit.md:45`
**Goal:** 当 memory 已注入 prompt 但 LLM 未 cite `[mem:id]` 时，运维可从指标/日志识别 silent 失效

## Step 1 复核

**判定：** 真实存在

- `memory_bridge.go:214` 仅 instruct LLM cite，无强制机制
- `boostCitedMemories` 在零 citation 时直接 return，无 miss 信号
- `memory_citation_boost_total` 长期为 0 无法区分「未注入」与「注入但未 cite」

## 修复

- `buildDynamicMemory` 返回 `(prompt, injectedIDs)` 供 ReAct 结束路径对比
- `recordCitationFeedback`：`injected|missed|cited|partial` 指标 + 结构化日志 `audit_id=REAUDIT-P0-3`
- `MemoryCitationFeedbackTotal{outcome}` Prometheus counter
- dev endpoint `POST /api/v1/test_citation_feedback`
- 校验脚本 `scripts/verify-reaudit-p0-3.sh`
