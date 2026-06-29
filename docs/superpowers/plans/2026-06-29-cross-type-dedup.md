# Cross-Type Conflict Resolution Plan (AUDIT-P1-1)

## Context
As stated in `2026-06-26-memory-system-audit.md` (AUDIT-P1-1):
> **跨 Type 重复无法识别**。ConflictResolver 强制同 Type 才视为冲突，但实际上同一句"我喜欢 tabs"可能被 LLM 第一次标为 `preference`，第二次标为 `knowledge` → 两条独立存活 | `internal/memory/conflict.go::FindConflicts` 显式 `c.Type != newMem.Type → skip`

This leads to token waste and score dilution when identical semantic memories are classified differently across different interactions.

## Goal
Allow semantic deduplication across memory types. For example, a `preference` and a `knowledge` memory with highly similar embeddings should be considered conflicts and merged.

## Implementation Steps

### Task 1: Update Conflict Resolver
1. In `internal/memory/conflict.go` line 106, remove the `c.Type != newMemory.Type` condition.
2. Allow vector similarity to be the sole determinant of redundancy.

### Task 2: Type Promotion Strategy in dedupMerge
1. In `internal/memory/hybrid.go` `dedupMerge` method, when an anchor memory merges duplicates of different types, we should ensure the anchor retains the most "valuable" type.
2. The type hierarchy can be defined as: `preference` > `knowledge` > `pattern` > `episodic` > `semantic`.
3. If a duplicate has a higher-priority type than the anchor, update the anchor's type.

### Task 3: Unit Testing
1. Update or add a unit test in `internal/memory/conflict_test.go` to simulate a cross-type conflict (e.g., `MemoryKnowledge` vs `MemoryPreference`) and ensure `FindConflicts` returns them.

### Task 4: Deployment & Verification
1. Run `podman-compose up -d --build` to deploy.
2. Run `go test` and verify behavior.
