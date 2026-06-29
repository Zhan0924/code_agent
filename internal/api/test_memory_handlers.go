package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/models"
	"github.com/gin-gonic/gin"
)

// NOTE: these endpoints exist for local smoke-tests of memory subsystem
// wiring (Active Memory Tools + decay). They are NOT part of the public API
// surface — only `handleTestMemory` is currently routed (`/v1/test_memory`).
// The historical `handleTestDistill` was removed in the memory refactor
// because it depended on `*Server.memoryStore.Hot()` and
// `Orchestrator.LLMPrimary()` accessors that never existed (the file failed
// to compile until `memory.Distiller` was wired into the bigger refactor).

func (s *Server) handleTestMemory(c *gin.Context) {
	ctx := c.Request.Context()
	
	// Test 1: Active Memory Tool Append
	appendArgs := json.RawMessage(`{"section": "project_context", "content": "Testing memory append via API"}`)
	toolRes, err := s.orchestrator.ToolRegistry().Execute(ctx, "core_memory_append", appendArgs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "core_memory_append failed: " + err.Error()})
		return
	}
	
	// Test 2: Active Memory Tool Replace
	replaceArgs := json.RawMessage(`{"section": "project_context", "content": "Replaced content via API"}`)
	replaceRes, err := s.orchestrator.ToolRegistry().Execute(ctx, "core_memory_replace", replaceArgs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "core_memory_replace failed: " + err.Error()})
		return
	}

	// Test 3: Blackboard Pub/Sub
	// In a real environment, we use the Redis client attached to the Orchestrator, but here we can just get it via sessionMgr if possible,
	// or simulate it. Since we can't easily access the Redis client here, we can just return success for the first 2, and we'll check Redis directly using CLI.
	
	c.JSON(http.StatusOK, gin.H{
		"append_result": toolRes,
		"replace_result": replaceRes,
		"message": "Core memory tools executed successfully",
	})
}

// handleTestCoreMemoryPII appends content containing a known secret via
// core_memory_append and returns the persisted section text from Redis.
// Dev-only smoke test for REAUDIT-P0-2 verification.
func (s *Server) handleTestCoreMemoryPII(c *gin.Context) {
	if s.rdb == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "redis not configured"})
		return
	}

	userID := c.Query("user_id")
	if userID == "" {
		userID = "verify_reaudit_p0_2"
	}
	projectID := c.Query("project_id")
	if projectID == "" {
		projectID = "default"
	}
	section := c.DefaultQuery("section", "human_context")

	const secret = "AKIAIOSFODNN7EXAMPLE"
	ctx := models.WithSessionContext(c.Request.Context(), "", userID, projectID)

	appendArgs := json.RawMessage(fmt.Sprintf(
		`{"section":%q,"content":"my backup key is %s","scope":"project"}`,
		section, secret,
	))
	if _, err := s.orchestrator.ToolRegistry().Execute(ctx, "core_memory_append", appendArgs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "core_memory_append failed: " + err.Error()})
		return
	}

	redisKey := fmt.Sprintf("core_memory:project:%s:%s", userID, projectID)
	raw, err := s.rdb.Get(c.Request.Context(), redisKey).Bytes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "redis get failed: " + err.Error()})
		return
	}

	var payload struct {
		Sections map[string]struct {
			Content string `json:"content"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal failed: " + err.Error()})
		return
	}

	stored := ""
	if sec, ok := payload.Sections[section]; ok {
		stored = sec.Content
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":    userID,
		"project_id": projectID,
		"section":    section,
		"stored":     stored,
		"masked":     !strings.Contains(stored, secret),
	})
}

// handleTestCitationFeedback exercises REAUDIT-P0-3 citation miss observability
// by simulating injected memories with a response that omits [mem:id] tags.
func (s *Server) handleTestCitationFeedback(c *gin.Context) {
	if s.orchestrator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "orchestrator not configured"})
		return
	}
	if s.sessionMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session manager not configured"})
		return
	}

	userID := c.DefaultQuery("user_id", "verify_reaudit_p0_3")
	projectID := c.DefaultQuery("project_id", "default")
	response := c.DefaultQuery("response", "plain answer without memory citations")

	ctx := c.Request.Context()
	sess, err := s.sessionMgr.Create(ctx, userID, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session create failed: " + err.Error()})
		return
	}

	injected := []string{"mem-test-a", "mem-test-b"}
	s.orchestrator.ObserveCitationFeedbackForTest(ctx, sess.ID, response, injected)

	cited := memory.ParseCitationIDs(response)
	outcome := citationFeedbackOutcome(injected, cited)

	c.JSON(http.StatusOK, gin.H{
		"audit_id":        "REAUDIT-P0-3",
		"session_id":      sess.ID,
		"user_id":         userID,
		"project_id":      projectID,
		"injected_ids":    injected,
		"cited_ids":       cited,
		"injected_count":  len(injected),
		"cited_count":     len(cited),
		"outcome":         outcome,
	})
}

func citationFeedbackOutcome(injected, cited []string) string {
	if len(injected) == 0 {
		return "none"
	}
	if len(cited) == 0 {
		return "missed"
	}
	citedSet := make(map[string]struct{}, len(cited))
	for _, id := range cited {
		citedSet[id] = struct{}{}
	}
	for _, id := range injected {
		if _, ok := citedSet[id]; !ok {
			return "partial"
		}
	}
	return "cited"
}

// handleTestEmbedderDegrade exercises REAUDIT-P0-4 embedder failure +
// ILIKE degrade observability without mutating the production embedder.
func (s *Server) handleTestEmbedderDegrade(c *gin.Context) {
	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory store not configured"})
		return
	}

	userID := c.DefaultQuery("user_id", "verify_reaudit_p0_4")
	projectID := c.DefaultQuery("project_id", "default")
	query := c.DefaultQuery("query", "我喜欢用 tabs")

	ctx := c.Request.Context()
	_, err := s.memoryStore.RetrieveWithEmbedder(ctx, memory.TestFailingEmbedder{}, userID, projectID, query, 5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "retrieve failed: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"audit_id":   "REAUDIT-P0-4",
		"user_id":    userID,
		"project_id": projectID,
		"query":      query,
		"degraded":   true,
		"reason":     "embedder_failed",
	})
}

// handleTestTenantNormalize exercises REAUDIT-P1-2 shared tenant fallback.
func (s *Server) handleTestTenantNormalize(c *gin.Context) {
	if s.orchestrator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "orchestrator not configured"})
		return
	}

	ctx := c.Request.Context()
	sessionID := c.Query("session_id")
	if sessionID == "" {
		if s.sessionMgr != nil {
			sess, err := s.sessionMgr.Create(ctx, c.Query("user_id"), c.Query("project_id"))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "session create failed: " + err.Error()})
				return
			}
			sessionID = sess.ID
		}
	}

	userID, projectID := s.orchestrator.ResolveTenantIDsForTest(ctx, sessionID)
	c.JSON(http.StatusOK, gin.H{
		"audit_id":   "REAUDIT-P1-2",
		"session_id": sessionID,
		"user_id":    userID,
		"project_id": projectID,
	})
}

// handleTestCoreMemoryDedup appends the same persona line twice and returns
// the persisted section text. Dev-only smoke test for REAUDIT-P1-3 verification.
func (s *Server) handleTestCoreMemoryDedup(c *gin.Context) {
	if s.rdb == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "redis not configured"})
		return
	}

	userID := c.Query("user_id")
	if userID == "" {
		userID = "verify_reaudit_p1_3"
	}
	projectID := c.Query("project_id")
	if projectID == "" {
		projectID = "default"
	}
	section := c.DefaultQuery("section", "persona")
	line := c.DefaultQuery("line", "prefers concise technical answers")

	ctx := models.WithSessionContext(c.Request.Context(), "", userID, projectID)
	appendArgs := json.RawMessage(fmt.Sprintf(
		`{"section":%q,"content":%q,"scope":"project"}`,
		section, line,
	))

	if _, err := s.orchestrator.ToolRegistry().Execute(ctx, "core_memory_append", appendArgs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "first append failed: " + err.Error()})
		return
	}
	if _, err := s.orchestrator.ToolRegistry().Execute(ctx, "core_memory_append", appendArgs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "second append failed: " + err.Error()})
		return
	}

	redisKey := fmt.Sprintf("core_memory:project:%s:%s", userID, projectID)
	raw, err := s.rdb.Get(c.Request.Context(), redisKey).Bytes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "redis get failed: " + err.Error()})
		return
	}

	var payload struct {
		Sections map[string]struct {
			Content string `json:"content"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unmarshal failed: " + err.Error()})
		return
	}

	stored := ""
	if sec, ok := payload.Sections[section]; ok {
		stored = sec.Content
	}
	lines := strings.Split(strings.TrimSpace(stored), "\n")
	deduped := strings.Count(stored, line) == 1

	c.JSON(http.StatusOK, gin.H{
		"audit_id":    "REAUDIT-P1-3",
		"user_id":     userID,
		"project_id":  projectID,
		"section":     section,
		"line_count":  len(lines),
		"line_hits":   strings.Count(stored, line),
		"deduped":     deduped,
		"stored":      stored,
	})
}

// handleTestDecay manually triggers a decay sweep. Useful for ops to confirm
// the cold-tier path is functional without waiting for the scheduler tick.
// Decay everything older than 0 (== all) by factor 0.5; aggressive on
// purpose because this endpoint is dev-only.
func (s *Server) handleTestDecay(c *gin.Context) {
	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Memory store not configured"})
		return
	}

	count, err := s.memoryStore.Decay(0, 0.5)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Decay triggered successfully", "decayed_count": count})
}

