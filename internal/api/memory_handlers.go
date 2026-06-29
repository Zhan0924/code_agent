package api

import (
	"net/http"
	"strconv"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/session"
	"github.com/gin-gonic/gin"
)

// handleListMemory enumerates long-term memories for (user_id, project_id).
// Reads hot + cold layers and merges by ID, sorted by LastAccessedAt desc.
//
// Query params:
//
//	user_id     (required) — owner of the memories
//	project_id  (optional, default "") — project scope
//	type        (optional) — preference|decision|knowledge|pattern
//	limit       (optional, default 50) — max items returned
//
// Returns 503 if memoryStore is not wired.
func (s *Server) handleListMemory(c *gin.Context) {
	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory store not configured"})
		return
	}

	// (user_id, project_id) 一起归一化 —— 跟 session.Manager.Create 入口对齐。
	// session.Create 把空 project_id 写成 "default",memory.RedisHot.keyPrefix
	// 用的是 "memory:<user>:<project>",所以前端不传 project_id 时,扫描模式会
	// 变成 "memory:anonymous::*",永远扫不到真实存的 "memory:anonymous:default:*"。
	userID := c.Query("user_id")
	if userID == "" {
		userID = session.AnonymousUserID
	}
	projectID := c.Query("project_id")
	if projectID == "" {
		projectID = "default"
	}
	typeFilter := c.Query("type")
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	mems, hotCount, coldCount, err := s.memoryStore.List(c.Request.Context(), userID, projectID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if typeFilter != "" {
		filtered := make([]memory.Memory, 0, len(mems))
		for _, m := range mems {
			if string(m.Type) == typeFilter {
				filtered = append(filtered, m)
			}
		}
		mems = filtered
	}

	c.JSON(http.StatusOK, gin.H{
		"memories":   mems,
		"hot_count":  hotCount,
		"cold_count": coldCount,
		"total":      len(mems),
	})
}

// handleMemoryStats returns per-type counts for (user_id, project_id).
//
// Query params: same as handleListMemory (without `type`).
//
// Response: { user_id, project_id, by_type: {preference: N, ...}, total }
func (s *Server) handleMemoryStats(c *gin.Context) {
	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory store not configured"})
		return
	}

	// 同 handleListMemory:user_id + project_id 一起归一化,使前端不带任何 query
	// 也能在「默认空间」拿到 stats。
	userID := c.Query("user_id")
	if userID == "" {
		userID = session.AnonymousUserID
	}
	projectID := c.Query("project_id")
	if projectID == "" {
		projectID = "default"
	}

	// 500 is a deliberate upper bound for the stats roll-up; visualization
	// histograms rarely benefit from more granularity. Adjust if/when needed.
	mems, _, _, err := s.memoryStore.List(c.Request.Context(), userID, projectID, 500)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	byType := map[string]int{
		string(memory.MemoryPreference): 0,
		string(memory.MemoryDecision):   0,
		string(memory.MemoryKnowledge):  0,
		string(memory.MemoryPattern):    0,
	}
	for _, m := range mems {
		byType[string(m.Type)]++
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":    userID,
		"project_id": projectID,
		"by_type":    byType,
		"total":      len(mems),
	})
}

// handleDeleteMemoryByUser handles GDPR deletion requests.
// It deletes all memories belonging to the specified user_id.
func (s *Server) handleDeleteMemoryByUser(c *gin.Context) {
	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory store not configured"})
		return
	}

	userID := c.Param("user_id")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id is required"})
		return
	}

	deletedCount, err := s.memoryStore.DeleteByUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":       userID,
		"deleted_count": deletedCount,
	})
}
