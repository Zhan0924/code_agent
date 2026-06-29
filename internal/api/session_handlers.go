// session_handlers.go — Session-specific handlers for message pinning.

package api

import (
	"net/http"

	"github.com/agent/code_agent/internal/memory"
	"github.com/agent/code_agent/internal/metrics"
	"github.com/agent/code_agent/internal/models"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// handlePinMessage pins a message so it won't be pruned from context.
// POST /sessions/:id/messages/:message_id/pin
func (s *Server) handlePinMessage(c *gin.Context) {
	sessionID := c.Param("id")
	messageID := c.Param("message_id")

	if err := s.sessionMgr.PinMessage(c.Request.Context(), sessionID, messageID); err != nil {
		s.logger.Error("failed to pin message",
			zap.String("session_id", sessionID),
			zap.String("message_id", messageID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pin message"})
		return
	}

	s.logger.Info("message pinned",
		zap.String("session_id", sessionID),
		zap.String("message_id", messageID))

	c.JSON(http.StatusOK, gin.H{
		"status":     "pinned",
		"session_id": sessionID,
		"message_id": messageID,
	})
}

// handleUnpinMessage removes the pin from a message.
// POST /sessions/:id/messages/:message_id/unpin
func (s *Server) handleUnpinMessage(c *gin.Context) {
	sessionID := c.Param("id")
	messageID := c.Param("message_id")

	if err := s.sessionMgr.UnpinMessage(c.Request.Context(), sessionID, messageID); err != nil {
		s.logger.Error("failed to unpin message",
			zap.String("session_id", sessionID),
			zap.String("message_id", messageID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to unpin message"})
		return
	}

	s.logger.Info("message unpinned",
		zap.String("session_id", sessionID),
		zap.String("message_id", messageID))

	c.JSON(http.StatusOK, gin.H{
		"status":     "unpinned",
		"session_id": sessionID,
		"message_id": messageID,
	})
}

type FeedbackRequest struct {
	Score float64 `json:"score" binding:"required"`
}

// handleMessageFeedback receives user feedback on a specific message
// and penalizes or boosts the cited memories.
// POST /sessions/:id/messages/:message_id/feedback
func (s *Server) handleMessageFeedback(c *gin.Context) {
	sessionID := c.Param("id")
	messageID := c.Param("message_id")

	var req FeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if s.memoryStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "memory store not available"})
		return
	}

	msg, err := s.sessionMgr.GetMessage(c.Request.Context(), sessionID, messageID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}

	if msg.Role != models.RoleAssistant {
		c.JSON(http.StatusBadRequest, gin.H{"error": "feedback is only supported on assistant messages"})
		return
	}

	ids, source := memory.ResolveCitedMemoryIDs(msg.CitedMemoryIDs, msg.Content)
	if len(ids) == 0 {
		metrics.MemoryFeedbackCitedMissTotal.Inc()
		s.logger.Warn("feedback with no cited memory ids",
			zap.String("audit_id", "REAUDIT-P1-4"),
			zap.String("op", "feedback_cited_miss"),
			zap.String("session_id", sessionID),
			zap.String("message_id", messageID),
			zap.String("cited_source", source),
			zap.String("result", "miss"))
		c.JSON(http.StatusOK, gin.H{"status": "recorded", "memories_affected": 0, "cited_source": source})
		return
	}
	s.logger.Info("feedback cited memory ids resolved",
		zap.String("audit_id", "REAUDIT-P1-4"),
		zap.String("op", "feedback_cited_resolve"),
		zap.String("cited_source", source),
		zap.Int("cited_count", len(ids)))

	sess, err := s.sessionMgr.Get(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get session details"})
		return
	}

	boost := 0.1
	direction := "positive"
	if req.Score < 0 {
		boost = -0.2
		direction = "negative"
	}

	refs := memory.TouchRefsFromCitationIDs(sess.UserID, sess.ProjectID, ids)
	if err := s.memoryStore.BoostScoreBatch(c.Request.Context(), refs, boost); err != nil {
		s.logger.Error("failed to boost memories on feedback", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply feedback to memories"})
		return
	}

	metrics.MemoryFeedbackTotal.WithLabelValues(direction).Add(float64(len(refs)))

	c.JSON(http.StatusOK, gin.H{
		"status":            "recorded",
		"memories_affected": len(refs),
		"cited_source":      source,
	})
}
