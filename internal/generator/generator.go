// Package generator implements the multi-phase project generation pipeline.
// It orchestrates: Blueprint → Scaffold → Implementation → Validation → Polish.
package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/workspace"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ─── Data Structures ────────────────────────────────────────────────────────

// ProjectBlueprint is the structured plan produced by Phase 1.
type ProjectBlueprint struct {
	Name        string     `json:"name"`
	Language    string     `json:"language"`
	Framework   string     `json:"framework"`
	Description string     `json:"description"`
	Files       []FileSpec `json:"files"`
}

// FileSpec describes a single file to be generated.
type FileSpec struct {
	Path         string   `json:"path"`
	Type         string   `json:"type"` // "code", "test", "config", "doc", "template"
	Description  string   `json:"description"`
	Dependencies []string `json:"dependencies,omitempty"` // paths of files this depends on
	Priority     int      `json:"priority"`               // topological order (computed)
	Generated    bool     `json:"generated"`
}

// ProjectStatus tracks the overall generation progress.
type ProjectStatus struct {
	ID             string            `json:"id"`
	Phase          string            `json:"phase"` // "blueprint", "scaffold", "implement", "validate", "polish", "done", "failed"
	Blueprint      *ProjectBlueprint `json:"blueprint,omitempty"`
	FilesTotal     int               `json:"files_total"`
	FilesGenerated int               `json:"files_generated"`
	Errors         []string          `json:"errors,omitempty"`
	WorkspaceID    string            `json:"workspace_id"`
	StartedAt      time.Time         `json:"started_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
}

// ProgressEvent is sent via callback to notify watchers of progress changes.
type ProgressEvent struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
}

// ─── Generator ──────────────────────────────────────────────────────────────

// Generator orchestrates the 5-phase project generation pipeline.
type Generator struct {
	llmClient    *llm.Client
	sandboxMgr   *sandbox.Manager
	workspaceMgr *workspace.Manager
	logger       *zap.Logger

	mu       sync.RWMutex
	projects map[string]*ProjectStatus
}

// NewGenerator creates a new project generator.
func NewGenerator(llmClient *llm.Client, sandboxMgr *sandbox.Manager, wsMgr *workspace.Manager, logger *zap.Logger) *Generator {
	return &Generator{
		llmClient:    llmClient,
		sandboxMgr:   sandboxMgr,
		workspaceMgr: wsMgr,
		logger:       logger.With(zap.String("component", "generator")),
		projects:     make(map[string]*ProjectStatus),
	}
}

// GetStatus retrieves a project's generation status.
func (g *Generator) GetStatus(projectID string) (*ProjectStatus, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.projects[projectID]
	return s, ok
}

// Generate kicks off the full project generation pipeline.
// The onProgress callback receives real-time progress events (can be nil).
func (g *Generator) Generate(ctx context.Context, description string, onProgress func(ProgressEvent)) (*ProjectStatus, error) {
	projectID := uuid.New().String()
	status := &ProjectStatus{
		ID:        projectID,
		Phase:     "blueprint",
		StartedAt: time.Now(),
	}
	g.mu.Lock()
	g.projects[projectID] = status
	g.mu.Unlock()

	emit := func(phase, msg, file string, done, total int) {
		status.Phase = phase
		if onProgress != nil {
			onProgress(ProgressEvent{Phase: phase, Message: msg, File: file, Done: done, Total: total})
		}
	}

	// ─── Phase 1: Blueprint ──────────────────────────────────────────
	emit("blueprint", "Generating project blueprint...", "", 0, 0)
	blueprint, err := g.generateBlueprint(ctx, description)
	if err != nil {
		status.Phase = "failed"
		status.Errors = append(status.Errors, "blueprint: "+err.Error())
		return status, fmt.Errorf("blueprint generation failed: %w", err)
	}
	topoSort(blueprint)
	status.Blueprint = blueprint
	status.FilesTotal = len(blueprint.Files)
	g.logger.Info("blueprint generated",
		zap.String("project", blueprint.Name),
		zap.Int("files", len(blueprint.Files)),
	)

	// ─── Phase 2: Scaffold ───────────────────────────────────────────
	emit("scaffold", "Creating workspace and scaffolding...", "", 0, status.FilesTotal)
	ws, err := g.workspaceMgr.Create(projectID, blueprint.Name)
	if err != nil {
		status.Phase = "failed"
		return status, fmt.Errorf("workspace creation failed: %w", err)
	}
	status.WorkspaceID = ws.ID

	if err := g.scaffold(ws, blueprint); err != nil {
		status.Phase = "failed"
		return status, fmt.Errorf("scaffolding failed: %w", err)
	}

	// ─── Phase 3: Implementation ─────────────────────────────────────
	emit("implement", "Generating code files...", "", 0, status.FilesTotal)
	for i := range blueprint.Files {
		select {
		case <-ctx.Done():
			status.Phase = "failed"
			return status, ctx.Err()
		default:
		}

		f := &blueprint.Files[i]
		if f.Generated {
			continue // scaffold already generated this
		}

		emit("implement", fmt.Sprintf("Generating %s...", f.Path), f.Path, status.FilesGenerated, status.FilesTotal)

		content, err := g.generateFile(ctx, blueprint, f, ws)
		if err != nil {
			g.logger.Warn("file generation failed, continuing",
				zap.String("file", f.Path), zap.Error(err))
			status.Errors = append(status.Errors, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}

		if err := g.workspaceMgr.WriteFile(ws, f.Path, content); err != nil {
			status.Errors = append(status.Errors, fmt.Sprintf("write %s: %v", f.Path, err))
			continue
		}
		f.Generated = true
		status.FilesGenerated++

		// Validate every 5 files for early error detection
		if status.FilesGenerated > 0 && status.FilesGenerated%5 == 0 {
			g.validateAndFix(ctx, blueprint, ws, status)
		}
	}

	// ─── Phase 4: Final Validation ───────────────────────────────────
	emit("validate", "Running final validation...", "", status.FilesGenerated, status.FilesTotal)
	g.validateAndFix(ctx, blueprint, ws, status)

	// ─── Phase 5: Polish ─────────────────────────────────────────────
	emit("polish", "Generating documentation...", "", status.FilesGenerated, status.FilesTotal)
	g.generateDocs(ctx, blueprint, ws)

	now := time.Now()
	status.CompletedAt = &now
	status.Phase = "done"
	emit("done", "Project generation complete!", "", status.FilesGenerated, status.FilesTotal)

	g.logger.Info("project generation complete",
		zap.String("project", blueprint.Name),
		zap.Int("files", status.FilesGenerated),
		zap.Int("errors", len(status.Errors)),
		zap.Duration("duration", time.Since(status.StartedAt)),
	)

	return status, nil
}

// ─── Phase 1: Blueprint Generation ──────────────────────────────────────────

const blueprintPrompt = `You are a senior software architect. Generate a project blueprint as a JSON object.

User requirement: %s

Respond with ONLY a valid JSON object (no markdown fences) with this exact structure:
{
  "name": "project-name",
  "language": "go",
  "framework": "gin",
  "description": "Brief description",
  "files": [
    {
      "path": "cmd/server/main.go",
      "type": "code",
      "description": "Main entry point with HTTP server setup",
      "dependencies": []
    },
    {
      "path": "internal/models/user.go",
      "type": "code",
      "description": "User data model",
      "dependencies": []
    }
  ]
}

Rules:
- Generate 10-30 files for a realistic project structure
- Include: source code, tests, config files, Dockerfile, Makefile, README.md, go.mod
- For Go projects: follow standard layout (cmd/, internal/, pkg/, configs/, deployments/)
- "dependencies" lists OTHER file paths that this file imports/depends on
- Test files depend on the file they test
- Type is one of: "code", "test", "config", "doc", "template"
- Do NOT include any explanation, only the JSON object`

func (g *Generator) generateBlueprint(ctx context.Context, description string) (*ProjectBlueprint, error) {
	prompt := fmt.Sprintf(blueprintPrompt, description)

	resp, err := g.llmClient.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	// Parse the JSON response
	content := strings.TrimSpace(resp.Content)
	// Strip markdown fences if present
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var blueprint ProjectBlueprint
	if err := json.Unmarshal([]byte(content), &blueprint); err != nil {
		return nil, fmt.Errorf("failed to parse blueprint JSON: %w\nRaw response: %s", err, content[:min(len(content), 500)])
	}

	if len(blueprint.Files) == 0 {
		return nil, fmt.Errorf("blueprint has no files")
	}

	return &blueprint, nil
}

// ─── Topology Sort ──────────────────────────────────────────────────────────

// topoSort assigns Priority values to FileSpecs based on dependency order.
// Files with no dependencies get Priority 0 (generated first).
func topoSort(bp *ProjectBlueprint) {
	pathIdx := make(map[string]int)
	for i, f := range bp.Files {
		pathIdx[f.Path] = i
	}

	// Kahn's algorithm
	inDegree := make(map[int]int)
	adj := make(map[int][]int)
	for i, f := range bp.Files {
		for _, dep := range f.Dependencies {
			if j, ok := pathIdx[dep]; ok {
				adj[j] = append(adj[j], i)
				inDegree[i]++
			}
		}
	}

	var queue []int
	for i := range bp.Files {
		if inDegree[i] == 0 {
			queue = append(queue, i)
		}
	}

	priority := 0
	for len(queue) > 0 {
		nextQueue := []int{}
		for _, idx := range queue {
			bp.Files[idx].Priority = priority
			for _, neighbor := range adj[idx] {
				inDegree[neighbor]--
				if inDegree[neighbor] == 0 {
					nextQueue = append(nextQueue, neighbor)
				}
			}
		}
		queue = nextQueue
		priority++
	}

	// Sort by priority (stable to preserve original order within same priority)
	sort.SliceStable(bp.Files, func(i, j int) bool {
		return bp.Files[i].Priority < bp.Files[j].Priority
	})
}

// ─── Phase 2: Scaffold ──────────────────────────────────────────────────────

func (g *Generator) scaffold(ws *workspace.Workspace, bp *ProjectBlueprint) error {
	// Create all directories first
	dirs := make(map[string]bool)
	for _, f := range bp.Files {
		dir := strings.TrimSuffix(f.Path, "/"+lastSegment(f.Path))
		if dir != f.Path && dir != "" {
			dirs[dir] = true
		}
	}
	for dir := range dirs {
		if err := g.workspaceMgr.MkdirAll(ws, dir); err != nil {
			return err
		}
	}

	// Generate deterministic template files (no LLM needed)
	for i := range bp.Files {
		f := &bp.Files[i]
		if f.Type == "template" || f.Type == "config" {
			content := generateTemplate(bp, f)
			if content != "" {
				if err := g.workspaceMgr.WriteFile(ws, f.Path, content); err != nil {
					return err
				}
				f.Generated = true
			}
		}
	}

	return nil
}

// generateTemplate produces deterministic content for well-known files.
func generateTemplate(bp *ProjectBlueprint, f *FileSpec) string {
	name := lastSegment(f.Path)
	switch {
	case name == "go.mod" && bp.Language == "go":
		return fmt.Sprintf("module github.com/example/%s\n\ngo 1.23\n", bp.Name)
	case name == ".gitignore":
		return "# Build\nbin/\n*.exe\n*.o\n\n# IDE\n.idea/\n.vscode/\n*.swp\n\n# OS\n.DS_Store\nThumbs.db\n\n# Deps\nvendor/\nnode_modules/\n"
	case name == "Dockerfile" && bp.Language == "go":
		return fmt.Sprintf(`FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/bin/server ./cmd/server

FROM alpine:3.20
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/bin/server /usr/local/bin/server
ENTRYPOINT ["server"]
`)
	case name == "Makefile" && bp.Language == "go":
		return fmt.Sprintf(`.PHONY: build test lint run
build:
	go build -o bin/server ./cmd/server

test:
	go test ./... -v -race

lint:
	golangci-lint run

run:
	go run ./cmd/server
`)
	}
	return ""
}

// ─── Phase 3: File Generation ───────────────────────────────────────────────

const fileGenPrompt = `You are a senior %s developer. Generate the file: %s

Project: %s (%s using %s)
Purpose of this file: %s

%s

Rules:
- Produce ONLY the file content, no markdown fences, no explanations
- Follow idiomatic %s conventions and best practices
- Include proper error handling
- Add doc comments for all exported symbols
- If this is a test file, use table-driven tests`

func (g *Generator) generateFile(ctx context.Context, bp *ProjectBlueprint, f *FileSpec, ws *workspace.Workspace) (string, error) {
	// Build context from dependencies
	depCtx := g.buildDependencyContext(bp, f, ws)

	prompt := fmt.Sprintf(fileGenPrompt,
		bp.Language, f.Path,
		bp.Name, bp.Language, bp.Framework,
		f.Description,
		depCtx,
		bp.Language,
	)

	resp, err := g.llmClient.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		return "", err
	}

	content := strings.TrimSpace(resp.Content)
	// Strip markdown code fences if present
	if strings.HasPrefix(content, "```") {
		lines := strings.SplitN(content, "\n", 2)
		if len(lines) > 1 {
			content = lines[1]
		}
		if idx := strings.LastIndex(content, "```"); idx > 0 {
			content = content[:idx]
		}
		content = strings.TrimSpace(content)
	}

	return content, nil
}

// buildDependencyContext reads generated dependency files to include in the prompt.
func (g *Generator) buildDependencyContext(bp *ProjectBlueprint, f *FileSpec, ws *workspace.Workspace) string {
	if len(f.Dependencies) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Related files already generated:\n\n")
	for _, dep := range f.Dependencies {
		content, err := g.workspaceMgr.ReadFile(ws, dep)
		if err != nil {
			continue
		}
		// Truncate very long files to just signatures
		if len(content) > 3000 {
			content = content[:3000] + "\n// ... truncated ..."
		}
		sb.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", dep, content))
	}
	return sb.String()
}

// ─── Phase 4: Validation ────────────────────────────────────────────────────

func (g *Generator) validateAndFix(ctx context.Context, bp *ProjectBlueprint, ws *workspace.Workspace, status *ProjectStatus) {
	if g.sandboxMgr == nil {
		return
	}

	// Determine the build command based on language
	var buildCmd string
	switch bp.Language {
	case "go":
		buildCmd = "cd /workspace && go build ./... 2>&1"
	case "python":
		buildCmd = "cd /workspace && python3 -m py_compile *.py 2>&1"
	case "typescript", "javascript":
		buildCmd = "cd /workspace && npx tsc --noEmit 2>&1"
	default:
		return
	}

	// Select the right image for the language
	lang := bp.Language
	if lang == "typescript" || lang == "javascript" {
		lang = "node"
	}

	result, err := g.sandboxMgr.ExecuteWithVolume(ctx, lang, buildCmd, ws.RootDir)
	if err != nil {
		g.logger.Warn("validation execution failed", zap.Error(err))
		return
	}

	if result.ExitCode != 0 {
		g.logger.Info("validation found errors, attempting fix",
			zap.Int("exit_code", result.ExitCode),
			zap.String("output", result.Stdout[:min(len(result.Stdout), 500)]),
		)
		status.Errors = append(status.Errors, "build: "+result.Stdout[:min(len(result.Stdout), 300)])

		// Attempt auto-fix via LLM (one round)
		g.attemptAutoFix(ctx, bp, ws, result.Stdout)
	} else {
		g.logger.Info("validation passed")
	}
}

func (g *Generator) attemptAutoFix(ctx context.Context, bp *ProjectBlueprint, ws *workspace.Workspace, buildErrors string) {
	prompt := fmt.Sprintf(`The %s project "%s" has build errors:

%s

Analyze the errors and respond with a JSON array of fixes:
[{"path": "file/path.go", "content": "full corrected file content"}]

Only include files that need changes. Respond with ONLY the JSON array.`,
		bp.Language, bp.Name, buildErrors[:min(len(buildErrors), 2000)])

	resp, err := g.llmClient.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		g.logger.Warn("auto-fix LLM call failed", zap.Error(err))
		return
	}

	content := strings.TrimSpace(resp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var fixes []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(content), &fixes); err != nil {
		g.logger.Warn("failed to parse auto-fix response", zap.Error(err))
		return
	}

	for _, fix := range fixes {
		if err := g.workspaceMgr.WriteFile(ws, fix.Path, fix.Content); err != nil {
			g.logger.Warn("failed to apply fix", zap.String("path", fix.Path), zap.Error(err))
		} else {
			g.logger.Info("auto-fix applied", zap.String("path", fix.Path))
		}
	}
}

// ─── Phase 5: Polish ────────────────────────────────────────────────────────

func (g *Generator) generateDocs(ctx context.Context, bp *ProjectBlueprint, ws *workspace.Workspace) {
	// Check if README.md was already generated
	if _, err := g.workspaceMgr.ReadFile(ws, "README.md"); err == nil {
		return // already exists
	}

	tree := g.workspaceMgr.TreeString(ws)
	prompt := fmt.Sprintf(`Generate a comprehensive README.md for this %s project:

Project: %s
Description: %s
Framework: %s

Directory structure:
%s

Include: project description, features, prerequisites, installation, usage, project structure, and license sections.
Respond with ONLY the markdown content.`,
		bp.Language, bp.Name, bp.Description, bp.Framework, tree)

	resp, err := g.llmClient.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, nil)
	if err != nil {
		g.logger.Warn("README generation failed", zap.Error(err))
		return
	}

	_ = g.workspaceMgr.WriteFile(ws, "README.md", strings.TrimSpace(resp.Content))
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func lastSegment(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
