package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent/code_agent/internal/workspace"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// SetWorkspaceManager injects the workspace manager into the API server.
func (s *Server) SetWorkspaceManager(wm *workspace.Manager) {
	s.workspaceMgr = wm
}

// ─── Workspace Listing ──────────────────────────────────────────────────────

// handleListWorkspaces returns all tracked workspaces.
// GET /api/v1/workspaces
func (s *Server) handleListWorkspaces(c *gin.Context) {
	if s.workspaceMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace manager not available"})
		return
	}
	workspaces := s.workspaceMgr.ListWorkspaces()
	c.JSON(http.StatusOK, workspaces)
}

// ─── File Tree ──────────────────────────────────────────────────────────────

// FileTreeNode represents a node in the workspace file tree.
type FileTreeNode struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Type     string          `json:"type"` // "file" or "directory"
	Size     int64           `json:"size,omitempty"`
	Children []*FileTreeNode `json:"children,omitempty"`
}

// handleGetWorkspaceTree returns the full file tree of a workspace.
// GET /api/v1/workspaces/:id/tree
func (s *Server) handleGetWorkspaceTree(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	tree, err := buildFileTree(ws.RootDir, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build file tree: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"workspace_id": ws.ID,
		"project":      ws.Project,
		"tree":         tree,
	})
}

// buildFileTree recursively builds a tree structure from the filesystem.
func buildFileTree(rootDir, relPath string) ([]*FileTreeNode, error) {
	absPath := filepath.Join(rootDir, relPath)
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, err
	}

	var nodes []*FileTreeNode
	for _, e := range entries {
		name := e.Name()
		// Skip hidden files/directories and common non-essential dirs
		if strings.HasPrefix(name, ".") && name != ".plan.md" && name != ".progress.json" {
			continue
		}
		if name == "node_modules" || name == "__pycache__" || name == ".git" {
			continue
		}

		childPath := filepath.Join(relPath, name)
		node := &FileTreeNode{
			Name: name,
			Path: childPath,
		}

		info, _ := e.Info()
		if e.IsDir() {
			node.Type = "directory"
			children, err := buildFileTree(rootDir, childPath)
			if err == nil {
				node.Children = children
			}
		} else {
			node.Type = "file"
			if info != nil {
				node.Size = info.Size()
			}
		}

		nodes = append(nodes, node)
	}
	return nodes, nil
}

// ─── File Read ──────────────────────────────────────────────────────────────

// handleReadWorkspaceFile reads a file from the workspace.
// GET /api/v1/workspaces/:id/files?path=relative/path
func (s *Server) handleReadWorkspaceFile(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	relPath := c.Query("path")
	if relPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path query parameter is required"})
		return
	}

	content, err := s.workspaceMgr.ReadFile(ws, relPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found: " + relPath})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	// Detect language from extension for frontend syntax highlighting
	lang := detectLanguage(relPath)

	c.JSON(http.StatusOK, gin.H{
		"path":     relPath,
		"content":  content,
		"language": lang,
		"size":     len(content),
	})
}

// ─── File Write ─────────────────────────────────────────────────────────────

// handleWriteWorkspaceFile writes/updates a file in the workspace.
// PUT /api/v1/workspaces/:id/files
func (s *Server) handleWriteWorkspaceFile(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	var req struct {
		Path    string `json:"path" binding:"required"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	if err := s.workspaceMgr.WriteFile(ws, req.Path, req.Content); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path":    req.Path,
		"message": "file saved",
		"size":    len(req.Content),
	})
}

// ─── File Delete ────────────────────────────────────────────────────────────

// handleDeleteWorkspaceFile deletes a file from the workspace.
// DELETE /api/v1/workspaces/:id/files?path=relative/path
func (s *Server) handleDeleteWorkspaceFile(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	relPath := c.Query("path")
	if relPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path query parameter is required"})
		return
	}

	// Use safePath logic via a delete operation
	absPath := filepath.Join(ws.RootDir, filepath.Clean(relPath))
	if !strings.HasPrefix(absPath, ws.RootDir+string(filepath.Separator)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "path traversal not allowed"})
		return
	}

	if err := os.Remove(absPath); err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "file deleted", "path": relPath})
}

// ─── Create Directory ───────────────────────────────────────────────────────

// handleCreateWorkspaceDir creates a directory in the workspace.
// POST /api/v1/workspaces/:id/directories
func (s *Server) handleCreateWorkspaceDir(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	var req struct {
		Path string `json:"path" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is required"})
		return
	}

	if err := s.workspaceMgr.MkdirAll(ws, req.Path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "directory created", "path": req.Path})
}

// ─── Download Archive ───────────────────────────────────────────────────────

// handleDownloadWorkspace downloads the workspace as a tar.gz archive.
// GET /api/v1/workspaces/:id/download
func (s *Server) handleDownloadWorkspace(c *gin.Context) {
	ws := s.resolveWorkspace(c)
	if ws == nil {
		return
	}

	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", "attachment; filename="+ws.Project+".tar.gz")

	if err := s.workspaceMgr.Archive(ws, c.Writer); err != nil {
		s.logger.Error("failed to archive workspace", zap.Error(err))
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// resolveWorkspace resolves the workspace from the URL parameter :id.
// If the workspace is not found but there's a "default" workspace, return it.
// Sessions live in Redis (durable across container restarts) while workspaces
// live on the container filesystem; a container rebuild leaves the frontend
// pointed at a session_id whose workspace dir was wiped. When that happens
// we lazy-create a fresh workspace tied to the still-existing session, which
// matches handleGetSessionWorkspace's recovery behaviour.
func (s *Server) resolveWorkspace(c *gin.Context) *workspace.Workspace {
	if s.workspaceMgr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace manager not available"})
		return nil
	}

	id := c.Param("id")

	// Try exact match first
	if ws, ok := s.workspaceMgr.Get(id); ok {
		return ws
	}

	// If id is "default" or "active", return the first (or most recent) workspace
	if id == "default" || id == "active" {
		workspaces := s.workspaceMgr.ListWorkspaces()
		if len(workspaces) > 0 {
			return workspaces[0]
		}
		// Auto-create a default workspace
		ws, err := s.workspaceMgr.Create("default", "workspace")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create default workspace"})
			return nil
		}
		return ws
	}

	// Lazy recovery: the frontend passes session_id as workspace_id; if the
	// session is still alive in Redis we recreate its workspace dir rather
	// than 404-ing. Same code path as handleGetSessionWorkspace.
	if s.sessionMgr != nil {
		if _, err := s.sessionMgr.Get(c.Request.Context(), id); err == nil {
			label := "session-" + id
			if len(id) > 8 {
				label = "session-" + id[:8]
			}
			ws, createErr := s.workspaceMgr.CreateForSession(id, id, label)
			if createErr == nil {
				return ws
			}
		}
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found: " + id})
	return nil
}

// detectLanguage returns the Monaco editor language ID for a file path.
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".jsx":
		return "javascript"
	case ".tsx":
		return "typescript"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".html":
		return "html"
	case ".css":
		return "css"
	case ".sh", ".bash":
		return "shell"
	case ".sql":
		return "sql"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".hpp":
		return "cpp"
	case ".rb":
		return "ruby"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	case ".dockerfile":
		return "dockerfile"
	case ".proto":
		return "protobuf"
	case ".tf":
		return "hcl"
	default:
		name := strings.ToLower(filepath.Base(path))
		if name == "dockerfile" || strings.HasPrefix(name, "dockerfile.") {
			return "dockerfile"
		}
		if name == "makefile" {
			return "makefile"
		}
		if name == "go.mod" || name == "go.sum" {
			return "go"
		}
		return "plaintext"
	}
}
