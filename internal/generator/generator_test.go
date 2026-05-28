package generator

import (
	"testing"

	"github.com/agent/code_agent/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ─── Pure Function Tests ───────────────────────────────────────────────

func TestTopoSort_Linear(t *testing.T) {
	// A → B → C 线性依赖
	bp := &ProjectBlueprint{
		Files: []FileSpec{
			{Path: "c.go", Dependencies: []string{"b.go"}},
			{Path: "b.go", Dependencies: []string{"a.go"}},
			{Path: "a.go"},
		},
	}
	topoSort(bp)
	paths := extractPaths(bp.Files)
	assert.Equal(t, []string{"a.go", "b.go", "c.go"}, paths)
}

func TestTopoSort_Diamond(t *testing.T) {
	// A → B, A → C, B → D, C → D 菱形依赖
	bp := &ProjectBlueprint{
		Files: []FileSpec{
			{Path: "d.go", Dependencies: []string{"b.go", "c.go"}},
			{Path: "c.go", Dependencies: []string{"a.go"}},
			{Path: "b.go", Dependencies: []string{"a.go"}},
			{Path: "a.go"},
		},
	}
	topoSort(bp)
	paths := extractPaths(bp.Files)
	assert.Equal(t, "a.go", paths[0]) // A 必须第一
	assert.Equal(t, "d.go", paths[3]) // D 必须最后
	// B 和 C 顺序不定，但都在 A 之后 D 之前
	assert.Contains(t, paths[1:3], "b.go")
	assert.Contains(t, paths[1:3], "c.go")
}

func TestTopoSort_NoDependencies(t *testing.T) {
	// 所有文件无依赖，保持原始顺序
	bp := &ProjectBlueprint{
		Files: []FileSpec{
			{Path: "z.go"},
			{Path: "a.go"},
			{Path: "m.go"},
		},
	}
	topoSort(bp)
	// 所有文件 Priority 应该为 0
	for _, f := range bp.Files {
		assert.Equal(t, 0, f.Priority)
	}
}

func TestTopoSort_MissingDependency(t *testing.T) {
	// 依赖不存在的文件（应该被忽略）
	bp := &ProjectBlueprint{
		Files: []FileSpec{
			{Path: "b.go", Dependencies: []string{"nonexistent.go"}},
			{Path: "a.go"},
		},
	}
	topoSort(bp)
	// 不应该 panic，b.go 的不存在依赖被忽略
	paths := extractPaths(bp.Files)
	assert.Len(t, paths, 2)
}

func TestGenerateTemplate_GoMod(t *testing.T) {
	bp := &ProjectBlueprint{
		Name:     "test-project",
		Language: "go",
	}
	f := &FileSpec{Path: "go.mod", Type: "config"}
	tmpl := generateTemplate(bp, f)
	assert.Contains(t, tmpl, "module github.com/example/test-project")
	assert.Contains(t, tmpl, "go 1.23")
}

func TestGenerateTemplate_Gitignore(t *testing.T) {
	bp := &ProjectBlueprint{Language: "go"}
	f := &FileSpec{Path: ".gitignore", Type: "config"}
	tmpl := generateTemplate(bp, f)
	assert.Contains(t, tmpl, "bin/")
	assert.Contains(t, tmpl, "*.exe")
	assert.Contains(t, tmpl, "vendor/")
	assert.Contains(t, tmpl, ".DS_Store")
}

func TestGenerateTemplate_Dockerfile(t *testing.T) {
	bp := &ProjectBlueprint{Language: "go"}
	f := &FileSpec{Path: "Dockerfile", Type: "config"}
	tmpl := generateTemplate(bp, f)
	assert.Contains(t, tmpl, "FROM golang:1.23-alpine")
	assert.Contains(t, tmpl, "WORKDIR /app")
	assert.Contains(t, tmpl, "ENTRYPOINT")
}

func TestGenerateTemplate_Makefile(t *testing.T) {
	bp := &ProjectBlueprint{Language: "go"}
	f := &FileSpec{Path: "Makefile", Type: "config"}
	tmpl := generateTemplate(bp, f)
	assert.Contains(t, tmpl, ".PHONY:")
	assert.Contains(t, tmpl, "build:")
	assert.Contains(t, tmpl, "test:")
	assert.Contains(t, tmpl, "go test")
}

func TestGenerateTemplate_UnknownFile(t *testing.T) {
	bp := &ProjectBlueprint{Language: "go"}
	f := &FileSpec{Path: "unknown.txt", Type: "config"}
	tmpl := generateTemplate(bp, f)
	assert.Empty(t, tmpl)
}

func TestLastSegment(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"a/b/c", "c"},
		{"single", "single"},
		{"", ""},
		{"/absolute/path", "path"},
		{"trailing/slash/", ""},
		{"a/b/c.go", "c.go"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, lastSegment(tt.input))
		})
	}
}

// ─── Scaffold Tests (with real workspace.Manager) ──────────────────────

func TestScaffold_CreatesDirectoryStructure(t *testing.T) {
	// 使用真实 workspace.Manager + t.TempDir()
	baseDir := t.TempDir()
	mgr, err := workspace.NewManager(baseDir, zap.NewNop())
	require.NoError(t, err)

	ws, err := mgr.CreateForSession("test-session", "test-ws", "test-project")
	require.NoError(t, err)

	bp := &ProjectBlueprint{
		Name:     "test-project",
		Language: "go",
		Files: []FileSpec{
			{Path: "main.go", Type: "source"},
			{Path: "pkg/util.go", Type: "source"},
			{Path: "go.mod", Type: "config"},
		},
	}
	topoSort(bp)

	gen := &Generator{workspaceMgr: mgr, logger: zap.NewNop()}
	err = gen.scaffold(ws, bp)
	require.NoError(t, err)

	// 验证 go.mod 被生成
	content, err := mgr.ReadFile(ws, "go.mod")
	require.NoError(t, err)
	assert.Contains(t, content, "module github.com/example/test-project")

	// 验证目录结构存在
	_, err = mgr.ReadFile(ws, "pkg/util.go")
	// 文件不存在是正常的（scaffold 只创建目录和模板文件）
	// 但目录应该存在，所以写入应该成功
	err = mgr.WriteFile(ws, "pkg/util.go", "package pkg")
	require.NoError(t, err)
}

func TestScaffold_GeneratesTemplateFiles(t *testing.T) {
	baseDir := t.TempDir()
	mgr, err := workspace.NewManager(baseDir, zap.NewNop())
	require.NoError(t, err)

	ws, err := mgr.Create("test-ws", "test-project")
	require.NoError(t, err)

	bp := &ProjectBlueprint{
		Name:     "my-service",
		Language: "go",
		Files: []FileSpec{
			{Path: "go.mod", Type: "config"},
			{Path: ".gitignore", Type: "config"},
			{Path: "Dockerfile", Type: "template"},
			{Path: "Makefile", Type: "template"},
		},
	}

	gen := &Generator{workspaceMgr: mgr, logger: zap.NewNop()}
	err = gen.scaffold(ws, bp)
	require.NoError(t, err)

	// 验证所有模板文件都被生成
	for _, f := range bp.Files {
		content, err := mgr.ReadFile(ws, f.Path)
		require.NoError(t, err, "file %s should exist", f.Path)
		assert.NotEmpty(t, content, "file %s should have content", f.Path)
		assert.True(t, f.Generated, "file %s should be marked as generated", f.Path)
	}
}

func TestScaffold_SkipsNonTemplateFiles(t *testing.T) {
	baseDir := t.TempDir()
	mgr, err := workspace.NewManager(baseDir, zap.NewNop())
	require.NoError(t, err)

	ws, err := mgr.Create("test-ws", "test-project")
	require.NoError(t, err)

	bp := &ProjectBlueprint{
		Name:     "test-project",
		Language: "go",
		Files: []FileSpec{
			{Path: "main.go", Type: "source"},      // 不是 template/config
			{Path: "go.mod", Type: "config"},       // 是 config
			{Path: "README.md", Type: "doc"},       // 不是 template/config
			{Path: "Dockerfile", Type: "template"}, // 是 template
		},
	}

	gen := &Generator{workspaceMgr: mgr, logger: zap.NewNop()}
	err = gen.scaffold(ws, bp)
	require.NoError(t, err)

	// 验证 config/template 文件被生成
	assert.True(t, bp.Files[1].Generated, "go.mod should be generated")
	assert.True(t, bp.Files[3].Generated, "Dockerfile should be generated")

	// 验证 source/doc 文件未被生成
	assert.False(t, bp.Files[0].Generated, "main.go should not be generated")
	assert.False(t, bp.Files[2].Generated, "README.md should not be generated")
}

// ─── Helper Functions ───────────────────────────────────────────────────

func extractPaths(files []FileSpec) []string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return paths
}
