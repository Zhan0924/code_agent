// Package workspace manages isolated project workspaces for code generation.
// It provides safe file I/O operations with path traversal protection
// and lifecycle management for temporary project directories.
package workspace

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Workspace represents an isolated project directory with safe file operations.
type Workspace struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id,omitempty"` // Tied to a chat session for isolation
	RootDir   string    `json:"root_dir"`
	Project   string    `json:"project_name"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager handles workspace lifecycle (create, read, write, archive, cleanup).
type Manager struct {
	baseDir    string
	workspaces sync.Map // id → *Workspace
	logger     *zap.Logger
}

// NewManager creates a workspace manager rooted at baseDir.
// It automatically restores previously persisted workspaces from disk.
func NewManager(baseDir string, logger *zap.Logger) (*Manager, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create workspace base dir: %w", err)
	}
	m := &Manager{
		baseDir: baseDir,
		logger:  logger.With(zap.String("component", "workspace")),
	}
	// Restore persisted workspaces from disk
	m.restore()
	return m, nil
}

// Create provisions a new isolated workspace directory.
func (m *Manager) Create(id, projectName string) (*Workspace, error) {
	return m.CreateForSession(id, "", projectName)
}

// CreateForSession provisions a new isolated workspace tied to a specific session.
func (m *Manager) CreateForSession(id, sessionID, projectName string) (*Workspace, error) {
	// If a workspace already exists for this ID, return it
	if existing, ok := m.Get(id); ok {
		return existing, nil
	}
	dir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace dir: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace dir: %w", err)
	}
	ws := &Workspace{
		ID:        id,
		SessionID: sessionID,
		RootDir:   realDir,
		Project:   projectName,
		CreatedAt: time.Now(),
	}
	m.workspaces.Store(id, ws)
	m.saveManifest(ws)
	m.logger.Info("workspace created", zap.String("id", id), zap.String("session_id", sessionID), zap.String("dir", dir))
	return ws, nil
}

// GetBySession returns the workspace tied to a specific session ID.
func (m *Manager) GetBySession(sessionID string) (*Workspace, bool) {
	var found *Workspace
	m.workspaces.Range(func(_, v interface{}) bool {
		ws := v.(*Workspace)
		if ws.SessionID == sessionID {
			found = ws
			return false
		}
		return true
	})
	return found, found != nil
}

// saveManifest persists workspace metadata to a JSON file inside the workspace directory.
func (m *Manager) saveManifest(ws *Workspace) {
	manifestPath := filepath.Join(ws.RootDir, ".workspace.json")
	data, err := json.Marshal(ws)
	if err != nil {
		m.logger.Error("failed to marshal workspace manifest", zap.Error(err))
		return
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		m.logger.Error("failed to write workspace manifest", zap.String("path", manifestPath), zap.Error(err))
	}
}

// restore scans the baseDir for existing workspace directories with manifests
// and re-registers them in-memory. This ensures workspaces survive restarts.
func (m *Manager) restore() {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		m.logger.Warn("failed to scan workspace base dir for restore", zap.Error(err))
		return
	}
	restored := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(m.baseDir, e.Name(), ".workspace.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue // No manifest = not a managed workspace
		}
		var ws Workspace
		if err := json.Unmarshal(data, &ws); err != nil {
			m.logger.Warn("corrupt workspace manifest", zap.String("dir", e.Name()), zap.Error(err))
			continue
		}
		// Fix root dir to match current baseDir (in case of mount changes)
		ws.RootDir = filepath.Join(m.baseDir, e.Name())
		// Resolve symlinks to match CreateForSession behavior (P0-1 fix)
		if realDir, err := filepath.EvalSymlinks(ws.RootDir); err == nil {
			ws.RootDir = realDir
		}
		m.workspaces.Store(ws.ID, &ws)
		restored++
	}
	if restored > 0 {
		m.logger.Info("workspaces restored from disk", zap.Int("count", restored))
	}
}

// Get retrieves an existing workspace by ID.
func (m *Manager) Get(id string) (*Workspace, bool) {
	v, ok := m.workspaces.Load(id)
	if !ok {
		return nil, false
	}
	return v.(*Workspace), true
}

// WriteFile writes content to a file inside the workspace with path traversal protection.
func (m *Manager) WriteFile(ws *Workspace, relPath, content string) error {
	absPath, err := m.safePath(ws, relPath)
	if err != nil {
		return err
	}
	// Defense-in-depth: refuse to overwrite an existing directory. safePath
	// already rejects "" and ".", but a relative path like "subdir" pointing
	// at an existing directory would still slip through and fail with EISDIR
	// deep inside os.WriteFile.
	if info, statErr := os.Stat(absPath); statErr == nil && info.IsDir() {
		return fmt.Errorf("refusing to write: %q is an existing directory", relPath)
	}
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("create parent dirs for %s: %w", relPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", relPath, err)
	}
	m.logger.Debug("file written", zap.String("workspace", ws.ID), zap.String("path", relPath), zap.Int("bytes", len(content)))
	return nil
}

// DeleteFile removes a file from the workspace with path traversal protection.
func (m *Manager) DeleteFile(ws *Workspace, relPath string) error {
	absPath, err := m.safePath(ws, relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", relPath, err)
	}
	m.logger.Debug("file deleted", zap.String("workspace", ws.ID), zap.String("path", relPath))
	return nil
}

// ReadFile reads a file from the workspace with path traversal protection.
func (m *Manager) ReadFile(ws *Workspace, relPath string) (string, error) {
	absPath, err := m.safePath(ws, relPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", relPath, err)
	}
	return string(data), nil
}

// ListFiles returns all files in the workspace as relative paths.
func (m *Manager) ListFiles(ws *Workspace) ([]string, error) {
	var files []string
	err := filepath.Walk(ws.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(ws.RootDir, path)
		files = append(files, rel)
		return nil
	})
	return files, err
}

// MkdirAll creates a directory tree inside the workspace.
//
// safePath()'s single EvalSymlinks pass fails on nested-and-missing inputs
// like `internal/shortcode` when `internal/` also doesn't exist yet. We can't
// just drop the symlink check though — a pre-existing intermediate component
// could be a symlink pointing outside ws.RootDir, and os.MkdirAll would
// silently follow it (CVE-class path-traversal). So: walk the cleaned path
// segment by segment, EvalSymlinks every *existing* component, abort if it
// leaves the workspace, stop the check at the first missing component, then
// hand the rest to os.MkdirAll which only ever creates real directories.
func (m *Manager) MkdirAll(ws *Workspace, relPath string) error {
	cleaned := filepath.Clean(relPath)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("absolute paths not allowed: %s", relPath)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path traversal not allowed: %s", relPath)
	}
	absPath := filepath.Join(ws.RootDir, cleaned)
	if !strings.HasPrefix(absPath, ws.RootDir+string(filepath.Separator)) && absPath != ws.RootDir {
		return fmt.Errorf("path traversal not allowed: %s", relPath)
	}

	// Walk each existing ancestor and verify symlinks (if any) still resolve
	// within ws.RootDir.
	rootReal, err := filepath.EvalSymlinks(ws.RootDir)
	if err != nil {
		return fmt.Errorf("workspace root invalid: %w", err)
	}
	parts := strings.Split(cleaned, string(filepath.Separator))
	curr := ws.RootDir
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		curr = filepath.Join(curr, part)
		info, err := os.Lstat(curr)
		if err != nil {
			if os.IsNotExist(err) {
				break // first missing component — remaining segments will be created by MkdirAll
			}
			return fmt.Errorf("stat %s: %w", curr, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			realCurr, err := filepath.EvalSymlinks(curr)
			if err != nil {
				return fmt.Errorf("symlink resolve %s: %w", curr, err)
			}
			if !strings.HasPrefix(realCurr, rootReal+string(filepath.Separator)) && realCurr != rootReal {
				return fmt.Errorf("path traversal detected (symlink %s → %s)", curr, realCurr)
			}
		}
	}

	return os.MkdirAll(absPath, 0o755)
}

// Archive creates a tar.gz of the workspace and writes it to the given writer.
func (m *Manager) Archive(ws *Workspace, w io.Writer) error {
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(ws.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(ws.RootDir, path)
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.Join(ws.Project, rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// Cleanup removes a workspace directory.
func (m *Manager) Cleanup(id string) error {
	ws, ok := m.Get(id)
	if !ok {
		return nil
	}
	m.workspaces.Delete(id)
	m.logger.Info("workspace cleaned up", zap.String("id", id))
	return os.RemoveAll(ws.RootDir)
}

// safePath resolves a relative path within the workspace and prevents path traversal.
func (m *Manager) safePath(ws *Workspace, relPath string) (string, error) {
	// Reject empty or whitespace-only paths. filepath.Clean("") returns ".",
	// which would resolve to ws.RootDir itself — callers like WriteFile would
	// then try to open the workspace directory as a file (EISDIR).
	if strings.TrimSpace(relPath) == "" {
		return "", fmt.Errorf("empty path not allowed")
	}

	// Clean and normalize
	cleaned := filepath.Clean(relPath)
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths not allowed: %s", relPath)
	}

	// Reject paths that resolve to the workspace root itself (e.g. ".", "./",
	// "foo/.."). These would let WriteFile/ReadFile target the directory node
	// rather than a file inside it.
	if cleaned == "." {
		return "", fmt.Errorf("path must reference a file inside the workspace, not the workspace root: %q", relPath)
	}

	// Construct initial path
	absPath := filepath.Join(ws.RootDir, cleaned)

	// Resolve all symlinks to real path
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// EvalSymlinks fails if file doesn't exist (e.g., WriteFile creating new file)
		// In this case, check the parent directory
		parentDir := filepath.Dir(absPath)
		realParent, err2 := filepath.EvalSymlinks(parentDir)
		if err2 != nil {
			return "", fmt.Errorf("parent directory invalid: %w", err2)
		}
		// Reconstruct path: real parent + filename
		realPath = filepath.Join(realParent, filepath.Base(absPath))
	}

	// Verify resolved real path is still within workspace
	if !strings.HasPrefix(realPath, ws.RootDir+string(filepath.Separator)) && realPath != ws.RootDir {
		return "", fmt.Errorf("path traversal detected (symlink resolved to %s)", realPath)
	}

	return realPath, nil
}

// ListWorkspaces returns all currently tracked workspaces.
func (m *Manager) ListWorkspaces() []*Workspace {
	var result []*Workspace
	m.workspaces.Range(func(_, v interface{}) bool {
		result = append(result, v.(*Workspace))
		return true
	})
	return result
}

// ListDir returns entries (files and dirs) in a directory within the workspace.
func (m *Manager) ListDir(ws *Workspace, relPath string) ([]string, error) {
	if relPath == "" || relPath == "." {
		relPath = "."
	}
	absPath, err := m.safePath(ws, relPath)
	if err != nil {
		// For root, use RootDir directly
		if relPath == "." {
			absPath = ws.RootDir
		} else {
			return nil, err
		}
	}
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return nil, fmt.Errorf("list dir %s: %w", relPath, err)
	}
	var result []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		result = append(result, name)
	}
	return result, nil
}

// TreeString returns an indented directory tree representation of the workspace.
func (m *Manager) TreeString(ws *Workspace) string {
	var sb strings.Builder
	sb.WriteString(ws.Project + "/\n")

	_ = filepath.Walk(ws.RootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(ws.RootDir, path)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		prefix := "├── "
		if info.IsDir() {
			sb.WriteString(indent + prefix + info.Name() + "/\n")
		} else {
			sb.WriteString(indent + prefix + info.Name() + "\n")
		}
		return nil
	})
	return sb.String()
}
