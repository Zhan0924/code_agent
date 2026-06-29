// session_handlers.go — Session-specific handlers for message pinning.

package api

import (
	"net/http"
	"regexp"

	"github.com/agent/code_agent/internal/memory"
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

var memoryCitationRe = regexp.MustCompile(`\[mem:([^\]]+)\]`)

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
		s.logger.Warn("memoryStore is not initialized, skipping feedback boost")
		c.JSON(http.StatusOK, gin.H{"status": "recorded_locally_only"})
		return
	}

	msg, err := s.sessionMgr.GetMessage(c.Request.Context(), sessionID, messageID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}

	if msg.Role != "assistant" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "feedback is only supported on assistant messages"})
		return
	}

	// Parse cited memories
	matches := memoryCitationRe.FindAllStringSubmatch(msg.Content, -1)
	if len(matches) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "recorded", "memories_affected": 0})
		return
	}

	sess, err := s.sessionMgr.Get(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get session details"})
		return
	}

	// Calculate boost (e.g., if score is -1, boost is -0.2; if +1, boost is +0.1)
	boost := 0.1
	if req.Score < 0 {
		boost = -0.2
	}

	var refs []memory.TouchRef
	for _, match := range matches {
		if len(match) == 2 {
			refs = append(refs, memory.TouchRef{
				ID:        match[1],
				UserID:    sess.UserID,
				ProjectID: sess.ProjectID,
			})
		}
	}

	if len(refs) > 0 {
		if err := s.memoryStore.BoostScoreBatch(c.Request.Context(), refs, boost); err != nil {
			s.logger.Error("failed to boost memories on feedback", zap.Error(err))
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"status":            "recorded",
		"memories_affected": len(refs),
	})
}
