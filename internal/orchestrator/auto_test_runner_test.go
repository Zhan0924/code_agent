package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestAutoTestRunner_IsTestableCodeFile(t *testing.T) {
	logger := zap.NewNop()
	runner := &AutoTestRunner{logger: logger}

	tests := []struct {
		path     string
		expected bool
	}{
		{"main.go", true},
		{"handler.py", true},
		{"app.ts", true},
		{"index.tsx", true},
		{"lib.rs", true},
		{"main_test.go", false},    // test file
		{"test_handler.py", false}, // test file
		{"app.test.ts", false},     // test file
		{"config.yaml", false},     // not code
		{"README.md", false},       // not code
		{".gitignore", false},      // not code
		{"app.spec.tsx", false},    // test file
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := runner.isTestableCodeFile(tt.path)
			if got != tt.expected {
				t.Errorf("isTestableCodeFile(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}

func TestAutoTestRunner_IsTestFile(t *testing.T) {
	logger := zap.NewNop()
	runner := &AutoTestRunner{logger: logger}

	tests := []struct {
		path     string
		expected bool
	}{
		{"main_test.go", true},
		{"handler_test.go", true},
		{"test_handler.py", true},
		{"handler_test.py", true},
		{"app.test.ts", true},
		{"app.spec.ts", true},
		{"app.test.tsx", true},
		{"app.spec.jsx", true},
		{"main.go", false},
		{"handler.py", false},
		{"app.ts", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := runner.isTestFile(tt.path)
			if got != tt.expected {
				t.Errorf("isTestFile(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}

func TestAutoTestRunner_TestCandidates(t *testing.T) {
	logger := zap.NewNop()
	runner := &AutoTestRunner{logger: logger}

	tests := []struct {
		srcPath        string
		expectContains string
	}{
		{"internal/auth/jwt.go", "internal/auth/jwt_test.go"},
		{"src/utils/parse.py", "src/utils/test_parse.py"},
		{"src/app.ts", "src/app.test.ts"},
		{"lib/index.js", "lib/index.test.js"},
	}

	for _, tt := range tests {
		t.Run(tt.srcPath, func(t *testing.T) {
			candidates := runner.testCandidates(tt.srcPath)
			found := false
			for _, c := range candidates {
				if c == tt.expectContains {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("testCandidates(%q) = %v, expected to contain %q", tt.srcPath, candidates, tt.expectContains)
			}
		})
	}
}

func TestAutoTestRunner_FindRelatedTests(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Create a source file and its test file
	if err := wm.WriteFile(ws, "handler.go", "package main\n"); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(ws, "handler_test.go", "package main\n"); err != nil {
		t.Fatal(err)
	}

	tests := runner.findRelatedTests(ws, []string{"handler.go"})
	if len(tests) != 1 || tests[0] != "handler_test.go" {
		t.Errorf("expected [handler_test.go], got %v", tests)
	}
}

func TestAutoTestRunner_FindRelatedTests_NoTestFile(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Create a source file WITHOUT a test file
	if err := wm.WriteFile(ws, "orphan.go", "package main\n"); err != nil {
		t.Fatal(err)
	}

	tests := runner.findRelatedTests(ws, []string{"orphan.go"})
	if len(tests) != 0 {
		t.Errorf("expected empty, got %v", tests)
	}
}

func TestAutoTestRunner_DetectLanguage(t *testing.T) {
	logger := zap.NewNop()
	runner := &AutoTestRunner{logger: logger}

	tests := []struct {
		path     string
		expected string
	}{
		{"main.go", "go"},
		{"app.py", "python"},
		{"index.ts", "typescript"},
		{"index.tsx", "typescript"},
		{"app.js", "javascript"},
		{"lib.rs", "rust"},
		{"file.txt", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := runner.detectLanguage(tt.path)
			if got != tt.expected {
				t.Errorf("detectLanguage(%q) = %q, want %q", tt.path, got, tt.expected)
			}
		})
	}
}

func TestAutoTestRunner_BuildGoTestCmd(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Create go.mod so Go test detection works
	goMod := "module example.com/test\n\ngo 1.21\n"
	if err := wm.WriteFile(ws, "go.mod", goMod); err != nil {
		t.Fatal(err)
	}

	t.Run("with test files", func(t *testing.T) {
		cmd, targets := runner.buildGoTestCmd(ws, []string{"pkg/handler.go"}, []string{"pkg/handler_test.go"})
		if cmd == "" {
			t.Error("expected a test command")
		}
		if !strings.Contains(cmd, "go test") {
			t.Errorf("expected 'go test' in command, got: %s", cmd)
		}
		if len(targets) == 0 {
			t.Error("expected test targets")
		}
	})

	t.Run("without test files", func(t *testing.T) {
		cmd, _ := runner.buildGoTestCmd(ws, []string{"pkg/handler.go"}, nil)
		if cmd == "" {
			t.Error("expected a vet command as fallback")
		}
		if !strings.Contains(cmd, "go vet") {
			t.Errorf("expected 'go vet' fallback, got: %s", cmd)
		}
	})

	t.Run("no go.mod", func(t *testing.T) {
		// Use a different workspace without go.mod
		noModDir := t.TempDir()
		noModWs := &wm.ListWorkspaces()[0]
		_ = noModWs // use the workspace with go.mod removed
		noModWm, _ := wm.Create("nomod", "nomod")
		// Don't write go.mod
		cmd, _ := runner.buildGoTestCmd(noModWm, []string{"main.go"}, nil)
		if cmd != "" {
			t.Errorf("expected empty command without go.mod, got: %s", cmd)
		}
		_ = noModDir
	})
}

func TestAutoTestRunner_BuildGoTestCmd_NoGoMod(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// No go.mod in workspace
	cmd, _ := runner.buildGoTestCmd(ws, []string{"main.go"}, nil)
	if cmd != "" {
		t.Errorf("expected empty command without go.mod, got: %s", cmd)
	}
}

func TestTestResult_FormatForLLM(t *testing.T) {
	t.Run("passed", func(t *testing.T) {
		r := &TestResult{
			TestFiles: []string{"handler_test.go"},
			Command:   "go test -v ./...",
			Passed:    true,
			ExitCode:  0,
			Output:    "PASS\nok  example.com/pkg 0.1s",
			Duration:  "123ms",
		}
		msg := r.FormatForLLM()
		if !strings.Contains(msg, "✅") {
			t.Error("expected success emoji")
		}
		if !strings.Contains(msg, "PASSED") {
			t.Error("expected PASSED in message")
		}
	})

	t.Run("failed", func(t *testing.T) {
		r := &TestResult{
			TestFiles: []string{"handler_test.go"},
			Command:   "go test -v ./...",
			Passed:    false,
			ExitCode:  1,
			Output:    "--- FAIL: TestHandler",
			Duration:  "200ms",
		}
		msg := r.FormatForLLM()
		if !strings.Contains(msg, "❌") {
			t.Error("expected failure emoji")
		}
		if !strings.Contains(msg, "FAILED") {
			t.Error("expected FAILED in message")
		}
		if !strings.Contains(msg, "fix the issues") {
			t.Error("expected fix instruction")
		}
	})

	t.Run("skipped", func(t *testing.T) {
		r := &TestResult{SkipReason: "No test runner available"}
		msg := r.FormatForLLM()
		if !strings.Contains(msg, "skipped") {
			t.Error("expected 'skipped' in message")
		}
	})

	t.Run("nil result", func(t *testing.T) {
		var r *TestResult
		msg := r.FormatForLLM()
		if msg != "" {
			t.Errorf("expected empty for nil result, got: %s", msg)
		}
	})
}

func TestAutoTestRunner_AfterEdit_SkipsNonCodeFiles(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Non-code files should be skipped
	result := runner.AfterEdit(nil, ws, []string{"config.yaml", "README.md"})
	if result != nil {
		t.Errorf("expected nil for non-code files, got: %+v", result)
	}
}

func TestAutoTestRunner_AfterEdit_SkipsTestFiles(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Test files themselves should be skipped
	result := runner.AfterEdit(nil, ws, []string{"handler_test.go"})
	if result != nil {
		t.Errorf("expected nil for test files, got: %+v", result)
	}
}

func TestAutoTestRunner_AfterEdit_NilWorkspace(t *testing.T) {
	wm, _ := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	result := runner.AfterEdit(nil, nil, []string{"main.go"})
	if result != nil {
		t.Error("expected nil for nil workspace")
	}
}

func TestAutoTestRunner_PythonTestCandidates(t *testing.T) {
	wm, ws := setupTestWorkspace(t)
	logger := zap.NewNop()
	runner := NewAutoTestRunner(wm, logger)

	// Create test file in expected location
	testDir := filepath.Join(ws.RootDir, "src")
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(ws, "src/handler.py", "def hello(): pass\n"); err != nil {
		t.Fatal(err)
	}
	if err := wm.WriteFile(ws, "src/test_handler.py", "def test_hello(): pass\n"); err != nil {
		t.Fatal(err)
	}

	tests := runner.findRelatedTests(ws, []string{"src/handler.py"})
	if len(tests) == 0 {
		t.Error("expected to find test_handler.py")
	}
	if len(tests) > 0 && tests[0] != "src/test_handler.py" {
		t.Errorf("expected src/test_handler.py, got: %v", tests)
	}
}
