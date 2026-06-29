# Session Memory Feedback (RLHF-lite) Plan (AUDIT-P0-4)

## Context
As stated in `2026-06-26-memory-system-audit.md` (AUDIT-P0-4):
> **`session` 与 `memory` 之间没有持续学习的桥**。session 的"用户对 agent 回复的反馈"（点踩/重新生成/纠正）没有反向训练 memory 重要性。缺少 RLHF-lite 能力：用户多次说"不要这样回答"，下次召回时不会自动降权"造成该回答的 memory"

## Goal
Implement a feedback loop where user feedback (thumbs down, correction) on an assistant's message penalizes the memories cited in that message, providing a continuous learning mechanism.

## Implementation Steps

### Task 1: Session Manager Update
1. In `internal/session/manager.go`, add a method `GetMessage(ctx context.Context, sessionID, messageID string) (*models.Message, error)` to retrieve a specific message.

### Task 2: Regex for Citation Parsing
1. In `internal/api/session_handlers.go`, define a regex `\[mem:([^\]]+)\]` to extract memory IDs from the assistant's message content.

### Task 3: API Endpoint for Feedback
1. Add `POST /api/v1/sessions/:id/messages/:message_id/feedback` to `internal/api/router.go`.
2. Implement `handleMessageFeedback(c *gin.Context)` in `internal/api/session_handlers.go`.
   - Parse request body: `{ "score": -1 }` (or `+1`).
   - Fetch the message via `s.sessionManager.GetMessage(ctx, sessionID, messageID)`.
   - Ensure the message is from `assistant`.
   - Extract `[mem:<id>]` from its content.
   - For each extracted memory ID, call `s.memoryStore.BoostScoreBatch(ctx, refs, -0.2)` if `score < 0`, and `+0.1` if `score > 0`. (Need to fetch `projectID` from session).
   - If `s.memoryStore` interface is not exported or accessible directly from the handler, make sure it is injected. Wait, `s.memoryStore` is already used in `memory_handlers.go`. It should be available in `Server` struct.

### Task 4: Deployment & Verification
1. Run `podman-compose build && up`.
2. Test the endpoint using `curl`.
3. Verify memory score drops.
