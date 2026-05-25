// session_handlers.go — Session-specific handlers for message pinning.

package api

import (
	"net/http"

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
