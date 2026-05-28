package api

import (
	"net/http"
	"path/filepath"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/skill"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ═══════════════════════════════════════════════════════════════════════════════
// MCP Server Management Handlers
// ═══════════════════════════════════════════════════════════════════════════════

// handleAddMCPServer adds a new MCP server at runtime.
// POST /api/v1/mcp/servers
func (s *Server) handleAddMCPServer(c *gin.Context) {
	var req struct {
		Name      string            `json:"name" binding:"required"`
		Transport string            `json:"transport" binding:"required"`
		Command   string            `json:"command" binding:"required"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate command against whitelist to prevent command injection
	cmd := filepath.Base(req.Command)
	if !config.IsAllowedMCPCommand(cmd) {
		s.logger.Warn("MCP server command rejected",
			zap.String("command", req.Command),
			zap.String("ip", c.ClientIP()),
		)
		c.JSON(http.StatusBadRequest, gin.H{"error": "command not allowed: " + cmd})
		return
	}

	if s.mcpGateway == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "MCP gateway not initialized"})
		return
	}

	cfg := &config.MCPServerConfig{
		Name:      req.Name,
		Transport: req.Transport,
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
	}

	status, err := s.mcpGateway.AddServer(cfg)
	if err != nil {
		s.logger.Error("failed to add MCP server", zap.String("name", req.Name), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	s.logger.Info("MCP server added via API", zap.String("name", req.Name))
	c.JSON(http.StatusOK, status)
}

// handleRemoveMCPServer disconnects and removes an MCP server.
// DELETE /api/v1/mcp/servers/:name
func (s *Server) handleRemoveMCPServer(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server name is required"})
		return
	}

	if s.mcpGateway == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "MCP gateway not initialized"})
		return
	}

	if err := s.mcpGateway.RemoveServer(name); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "server disconnected", "name": name})
}

// handleListMCPServers lists all connected MCP servers and their tools.
// GET /api/v1/mcp/servers
func (s *Server) handleListMCPServers(c *gin.Context) {
	if s.mcpGateway == nil {
		c.JSON(http.StatusOK, []mcp.ServerStatus{})
		return
	}

	servers := s.mcpGateway.ListServers()
	if servers == nil {
		servers = []mcp.ServerStatus{}
	}
	c.JSON(http.StatusOK, servers)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Skill Management Handlers
// ═══════════════════════════════════════════════════════════════════════════════

// handleAddSkill registers a new skill (custom tool) at runtime.
// POST /api/v1/skills
func (s *Server) handleAddSkill(c *gin.Context) {
	var def skill.Definition
	if err := c.ShouldBindJSON(&def); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if s.skillRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "skill registry not initialized"})
		return
	}

	if err := s.skillRegistry.Register(&def); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	s.logger.Info("skill added via API", zap.String("name", def.Name))
	c.JSON(http.StatusOK, gin.H{"name": def.Name, "status": "active"})
}

// handleRemoveSkill unregisters a skill.
// DELETE /api/v1/skills/:name
func (s *Server) handleRemoveSkill(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill name is required"})
		return
	}

	if s.skillRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "skill registry not initialized"})
		return
	}

	if err := s.skillRegistry.Unregister(name); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "skill removed", "name": name})
}

// handleListSkills lists all registered skills.
// GET /api/v1/skills
func (s *Server) handleListSkills(c *gin.Context) {
	if s.skillRegistry == nil {
		c.JSON(http.StatusOK, []skill.SkillStatus{})
		return
	}

	skills := s.skillRegistry.List()
	if skills == nil {
		skills = []skill.SkillStatus{}
	}
	c.JSON(http.StatusOK, skills)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Unified Tools List
// ═══════════════════════════════════════════════════════════════════════════════

// handleListTools returns all currently active tools (builtin + MCP + skills).
// GET /api/v1/tools
func (s *Server) handleListTools(c *gin.Context) {
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
	}

	var tools []toolInfo

	// Builtin tools
	builtins := []string{"execute_code", "search_code", "read_file", "write_file",
		"patch_file", "list_files", "create_directory", "run_tests", "run_workspace_cmd",
		"git_status", "git_diff", "git_commit", "git_log", "git_branch", "apply_diff"}
	for _, name := range builtins {
		tools = append(tools, toolInfo{Name: name, Source: "builtin"})
	}

	// MCP tools
	if s.mcpGateway != nil {
		for _, t := range s.mcpGateway.GetAvailableTools() {
			tools = append(tools, toolInfo{
				Name:        t.Name,
				Description: t.Description,
				Source:      t.Source,
			})
		}
	}

	// Skill tools —— 走稳定 Snapshot 路径，顺便把 ETag 写到响应头，
	// 以便客户端（或 Anthropic prompt cache）做精确 cache busting 判断。
	if s.skillRegistry != nil {
		snap := s.skillRegistry.Snapshot()
		c.Header("X-Tools-Etag", snap.ETag)
		for _, t := range snap.Tools {
			tools = append(tools, toolInfo{
				Name:        t.Name,
				Description: t.Description,
				Source:      t.Source,
			})
		}
	}

	// Dynamic tools from orchestrator registry
	if s.orchestrator != nil {
		for _, t := range s.orchestrator.GetAvailableTools() {
			if t.Source == "dynamic" {
				tools = append(tools, toolInfo{
					Name:        t.Name,
					Description: t.Description,
					Source:      t.Source,
				})
			}
		}
	}

	c.JSON(http.StatusOK, tools)
}
