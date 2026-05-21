package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent/code_agent/internal/generator"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ProjectGenerator is the interface for the project generation pipeline.
type ProjectGenerator interface {
	Generate(ctx context.Context, description string, onProgress func(generator.ProgressEvent)) (*generator.ProjectStatus, error)
	GetStatus(projectID string) (*generator.ProjectStatus, bool)
}

// SetGenerator injects the project generator into the API server.
func (s *Server) SetGenerator(gen ProjectGenerator) {
	s.generator = gen
}

// handleGenerateProject starts a new project generation task.
// POST /api/v1/projects/generate
func (s *Server) handleGenerateProject(c *gin.Context) {
	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "description is required"})
		return
	}

	if s.generator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "project generator not available"})
		return
	}

	s.logger.Info("project generation requested",
		zap.String("description", req.Description[:min(len(req.Description), 100)]),
	)

	// Start async generation
	var projectID string
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		status, err := s.generator.Generate(ctx, req.Description, nil)
		if err != nil {
			s.logger.Error("project generation failed", zap.Error(err))
			return
		}
		s.logger.Info("project generation done",
			zap.String("project_id", status.ID),
			zap.String("phase", status.Phase),
		)
		projectID = status.ID
		_ = projectID
	}()

	// Return immediately with accepted status
	// The client should poll /projects/:id/status for updates
	c.JSON(http.StatusAccepted, gin.H{
		"status":  "generation_started",
		"message": "Project generation has started. Use the SSE endpoint for real-time progress.",
	})
}

// handleGenerateProjectSSE starts project generation with Server-Sent Events streaming progress.
// POST /api/v1/projects/generate/stream
func (s *Server) handleGenerateProjectSSE(c *gin.Context) {
	var req struct {
		Description string `json:"description" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "description is required"})
		return
	}

	if s.generator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "project generator not available"})
		return
	}

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("Transfer-Encoding", "chunked")

	ctx := c.Request.Context()
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	onProgress := func(evt generator.ProgressEvent) {
		data, _ := json.Marshal(evt)
		fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		flusher.Flush()
	}

	status, err := s.generator.Generate(ctx, req.Description, onProgress)
	if err != nil {
		errEvt, _ := json.Marshal(gin.H{"phase": "error", "message": err.Error()})
		fmt.Fprintf(c.Writer, "data: %s\n\n", errEvt)
		flusher.Flush()
		return
	}

	// Send final complete event
	finalData, _ := json.Marshal(status)
	fmt.Fprintf(c.Writer, "data: %s\n\n", finalData)
	flusher.Flush()
}

// handleGetProjectStatus returns the current status of a project generation task.
// GET /api/v1/projects/:id/status
func (s *Server) handleGetProjectStatus(c *gin.Context) {
	projectID := c.Param("id")
	if s.generator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "project generator not available"})
		return
	}

	status, ok := s.generator.GetStatus(projectID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}

	c.JSON(http.StatusOK, status)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
