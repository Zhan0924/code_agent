package api

import (
	"net/http"
	"time"

	"github.com/agent/code_agent/internal/store"
	"github.com/agent/code_agent/internal/tools"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// handleRegisterDynamicTool registers a new dynamic tool.
// POST /api/v1/tools
func (s *Server) handleRegisterDynamicTool(c *gin.Context) {
	var config tools.DynamicToolConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	config.CreatedAt = time.Now()

	// Check for conflicts with builtin tools
	builtins := []string{
		"execute_code", "search_code", "read_file", "write_file",
		"patch_file", "list_files", "create_directory", "run_tests",
		"run_workspace_cmd", "git_status", "git_diff", "git_commit",
		"git_log", "git_branch", "edit_file",
	}
	for _, name := range builtins {
		if config.Name == name {
			c.JSON(http.StatusConflict, gin.H{"error": "tool name conflicts with builtin: " + name})
			return
		}
	}

	// Create dynamic tool
	tool, err := tools.NewDynamicTool(config)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to create tool: " + err.Error()})
		return
	}

	// Register to orchestrator
	if err := s.orchestrator.RegisterDynamicTool(tool); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "failed to register tool: " + err.Error()})
		return
	}

	// Persist to database if store is available
	if s.store != nil {
		rec := &store.DynamicToolRecord{
			Name:           config.Name,
			Description:    config.Description,
			Parameters:     config.Parameters,
			ExecutorType:   string(config.ExecutorType),
			ExecutorConfig: config.ExecutorConfig,
			RiskLevel:      config.RiskLevel,
			TTLSeconds:     config.TTLSeconds,
			CreatedAt:      config.CreatedAt,
		}

		if err := s.store.SaveDynamicTool(c.Request.Context(), rec); err != nil {
			s.logger.Error("failed to persist dynamic tool", zap.Error(err))
			// Continue anyway — tool is registered in memory
		}
	}

	s.logger.Info("registered dynamic tool", zap.String("name", config.Name))
	c.JSON(http.StatusCreated, gin.H{"message": "tool registered", "name": config.Name})
}

// handleUnregisterDynamicTool removes a dynamic tool.
// DELETE /api/v1/tools/:name
func (s *Server) handleUnregisterDynamicTool(c *gin.Context) {
	name := c.Param("name")

	// Unregister from orchestrator
	if !s.orchestrator.UnregisterDynamicTool(name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "tool not found: " + name})
		return
	}

	// Delete from database if store is available
	if s.store != nil {
		if err := s.store.DeleteDynamicTool(c.Request.Context(), name); err != nil {
			s.logger.Error("failed to delete dynamic tool from DB", zap.Error(err))
		}
	}

	s.logger.Info("unregistered dynamic tool", zap.String("name", name))
	c.JSON(http.StatusOK, gin.H{"message": "tool unregistered", "name": name})
}

// handleGetDynamicTool retrieves a tool definition by name.
// GET /api/v1/tools/dynamic/:name
func (s *Server) handleGetDynamicTool(c *gin.Context) {
	name := c.Param("name")

	tool, ok := s.orchestrator.GetTool(name)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "tool not found: " + name})
		return
	}

	def := tool.Definition()
	c.JSON(http.StatusOK, def)
}
