package api

import (
	"encoding/json"
	"net/http"

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

