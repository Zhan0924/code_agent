# PII Regex & Blackboard Shielding Plan (AUDIT-P1-2)

## Context
As stated in `2026-06-26-memory-system-audit.md` (AUDIT-P1-2):
> **PII 全部基于 regex**，对企业内部的非标准 token / RBAC role / 业务 secret 完全失效；blackboard pub/sub 广播 `Memory.Content`，没有第二道遮蔽 | `internal/memory/extractor.go::piiMasker` 是固定 regex；`internal/memory/blackboard.go::Publish` 整条 Memory JSON 广播

The problems:
1. `piiMasker` is hardcoded with fixed regexes, making it useless for proprietary tokens (e.g. `sk-corp-*`).
2. PII masking only happens in the `Extractor` before calling LLM. If a memory is injected via tools or bypasses the extractor, it is written to the store and broadcast on the Blackboard verbatim.

## Implementation Steps

### Task 1: Refactor PIIMasker
1. Move `piiMasker` from `extractor.go` to its own file `pii.go` or make it public (`PIIMasker`).
2. Allow `PIIMasker` to parse custom regex rules from a configuration (e.g., `os.Getenv("AGENT_CUSTOM_PII_REGEX")` or via `HybridStore` options).

### Task 2: Second-Layer Shield in HybridStore & Blackboard
1. Pass the `PIIMasker` to `HybridStore` and `Blackboard`.
2. In `HybridStore.Store`, ensure `m.Content = masker.Mask(m.Content)` is applied before persisting and before `Publish`-ing to the blackboard. (Or alternatively, apply it inside `Blackboard.Publish`).
3. This guarantees that no matter how a memory was generated (Extractor or Tool), it gets scrubbed before reaching the long-term DB or other agents via the event bus.

### Task 3: Unit Testing
1. Add tests in `pii_test.go` to verify custom regex injection.
2. Verify `Blackboard.Publish` does not broadcast PII.

### Task 4: Deployment & Verification
1. Run `podman-compose up -d --build` to deploy.
2. Verify the fix.
