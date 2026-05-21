package api

import (
	"net/http"

	"github.com/agent/code_agent/internal/auth"
	"github.com/gin-gonic/gin"
)

// tokenRequest represents a token generation request.
type tokenRequest struct {
	UserID string `json:"user_id" binding:"required"`
	Role   string `json:"role" binding:"required"`
	Email  string `json:"email"`
}

// handleGenerateToken creates a new JWT token for a user.
// POST /api/v1/auth/token
// In production with auth enabled, this endpoint requires admin role
// (enforced via X-Admin-Secret header or pre-provisioned admin token).
func (s *Server) handleGenerateToken(c *gin.Context) {
	if s.jwtMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication not configured"})
		return
	}

	// Protection: require admin secret or valid admin JWT
	adminSecret := c.GetHeader("X-Admin-Secret")
	claims := auth.GetClaims(c)
	if adminSecret == "" && (claims == nil || claims.Role != auth.RoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{
			"error": "token generation requires admin privileges (X-Admin-Secret header or admin JWT)",
		})
		return
	}

	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	// Validate role
	role := auth.Role(req.Role)
	switch role {
	case auth.RoleAdmin, auth.RoleDev, auth.RoleReadOnly, auth.RoleService:
		// valid
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid role, must be one of: admin, dev, readonly, service"})
		return
	}

	token, err := s.jwtMgr.GenerateToken(req.UserID, role, req.Email)
	if err != nil {
		s.logger.Error("failed to generate token")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   token,
		"user_id": req.UserID,
		"role":    req.Role,
	})
}
