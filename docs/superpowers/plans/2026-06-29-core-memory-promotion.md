# Core Memory Auto-Promotion Plan (AUDIT-P1-4)

## Context
From `2026-06-26-memory-system-audit.md` (AUDIT-P1-4):
> **CoreMemory 完全 tool-driven，没有自动晋升**。LLM 必须主动调 `core_memory_append` 才写；Extractor 产出的高 importance preference 不会自动晋升到 core

Currently, CoreMemory relies entirely on the LLM actively invoking `core_memory_append`. If the LLM forgets to call the tool, a critical user persona constraint ("I like Chinese responses") is lost from Core Memory, and it might eventually decay from Long-Term Memory (LTM). However, the `Extractor` passively identifies high-importance memories from the conversation. 

## Proposed Solution
We need an "Auto-Promotion" mechanism connecting the `Extractor` to the `CoreMemoryManager`.

### Architecture Updates
1. **Define Core Memory Threshold**: If the Extractor generates a memory with `Type == "preference"` (or `decision`) and `Importance >= 9`, it qualifies for auto-promotion to Core Memory.
2. **Interface Injection**: 
   - Extracting memory happens in `extractor.go`.
   - Core Memory is managed by `CoreMemoryManager` (`internal/session/core_memory.go`).
   - We should inject a `CorePromoter` interface into `Extractor`, which `CoreMemoryManager` implements, or inject it via a callback `OnHighImportance(ctx, projectID, content)`.
   Wait, the `Extractor` sits in `internal/memory`, while `CoreMemoryManager` sits in `internal/session` (or `internal/orchestrator` / `internal/tools`). To avoid circular dependencies, `Extractor` can accept an interface:
   ```go
   type CoreMemoryPromoter interface {
       AppendCoreMemory(ctx context.Context, userID, projectID, content string) error
   }
   ```
   Or `Extractor` can return a flag indicating "ShouldPromoteToCore", and the orchestrator handles it.
   Since `extractor.ExtractFromInteraction` does not return the extracted memories (it asynchronously saves them to LTM via the blackboard or hybrid store), it's better to pass a callback or interface to the `Extractor`. 

3. **Modify `extractor.go`**:
   - Add `corePromoter CoreMemoryPromoter` to `Extractor` struct.
   - In `ExtractFromInteraction`, after extracting memories:
     ```go
     for _, mem := range validMemories {
         if mem.Importance >= 9.0 && mem.Type == "preference" {
             if e.corePromoter != nil {
                 // Promote to core memory asynchronously
                 go e.corePromoter.AppendCoreMemory(context.Background(), userID, projectID, mem.Content)
             }
         }
     }
     ```

4. **Wiring in Main / Orchestrator**:
   - Ensure that when the `Extractor` is initialized, it receives the `CoreMemoryManager` (which implements `AppendCoreMemory`).

## Testing Strategy
1. **Unit Test**: Mock `CoreMemoryPromoter` and verify it is called when `Importance >= 9.0` and `Type == preference`.
2. **Integration / Manual Test**: Deploy with Docker, hit the API with a strong preference ("You MUST always speak to me in Chinese"), and observe if it appears in `CoreMemory` alongside `LongTermMemory`.
