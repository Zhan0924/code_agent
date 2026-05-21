// Package orchestrator: auto_test_runner.go implements automatic test triggering
// after file edits. This is the core of the TDD self-verification loop.
//
// Key behaviors:
//   - After any file write/edit, find and run the corresponding test file
//   - Inject test results back into the ReAct loop for self-healing
//   - Support Go, Python, TypeScript, and JavaScript test conventions
package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent/code_agent/internal/workspace"
	"go.uber.org/zap"
)

// AutoTestRunner automatically discovers and runs tests related to edited files.
type AutoTestRunner struct {
	workspaceMgr *workspace.Manager
	logger       *zap.Logger
}

// NewAutoTestRunner creates a new AutoTestRunner.
func NewAutoTestRunner(wm *workspace.Manager, logger *zap.Logger) *AutoTestRunner {
	return &AutoTestRunner{
		workspaceMgr: wm,
		logger:       logger.With(zap.String("component", "auto_test")),
	}
}

// TestResult holds the outcome of an automatic test run.
type TestResult struct {
	TestFiles  []string `json:"test_files"`  // Which test files were run
	Command    string   `json:"command"`     // The command that was executed
	Passed     bool     `json:"passed"`      // Whether all tests passed
	ExitCode   int      `json:"exit_code"`   // Process exit code
	Output     string   `json:"output"`      // Combined stdout + stderr
	Duration   string   `json:"duration"`    // How long the test took
	SkipReason string   `json:"skip_reason"` // Why tests were skipped (if applicable)
}

// AfterEdit finds and runs tests related to the edited files.
// Returns nil if no relevant tests were found or tests are not applicable.
func (r *AutoTestRunner) AfterEdit(ctx context.Context, ws *workspace.Workspace, editedFiles []string) *TestResult {
	if ws == nil || len(editedFiles) == 0 {
		return nil
	}

	// Skip test files themselves, config files, docs, etc.
	var codeFiles []string
	for _, f := range editedFiles {
		if r.isTestableCodeFile(f) {
			codeFiles = append(codeFiles, f)
		}
	}
	if len(codeFiles) == 0 {
		return nil
	}

	// Find related test files
	testFiles := r.findRelatedTests(ws, codeFiles)

	// Determine the best test command based on the file type
	lang := r.detectLanguage(codeFiles[0])
	cmd, testTargets := r.buildTestCommand(ws, lang, codeFiles, testFiles)
	if cmd == "" {
		return &TestResult{
			SkipReason: fmt.Sprintf("No test runner available for %s files", lang),
		}
	}

	// Execute the test command
	result := r.runTest(ctx, ws.RootDir, cmd)
	result.TestFiles = testTargets

	r.logger.Info("auto-test completed",
		zap.Strings("edited", codeFiles),
		zap.Strings("tests", testTargets),
		zap.Bool("passed", result.Passed),
		zap.String("duration", result.Duration))

	return result
}

// isTestableCodeFile returns true if the file is source code worth testing.
func (r *AutoTestRunner) isTestableCodeFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	// Skip non-code files
	switch ext {
	case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs":
		// Skip test files themselves
		if r.isTestFile(path) {
			return false
		}
		return true
	}
	return false
}

// isTestFile returns true if the file is itself a test file.
func (r *AutoTestRunner) isTestFile(path string) bool {
	base := filepath.Base(path)
	lower := strings.ToLower(base)

	// Go: *_test.go
	if strings.HasSuffix(lower, "_test.go") {
		return true
	}
	// Python: test_*.py or *_test.py
	if strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py") {
		return true
	}
	if strings.HasSuffix(lower, "_test.py") {
		return true
	}
	// JavaScript/TypeScript: *.test.ts, *.spec.ts, *.test.js, *.spec.js
	for _, suffix := range []string{".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx", ".test.js", ".test.jsx", ".spec.js", ".spec.jsx"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// findRelatedTests finds test files corresponding to the given source files.
func (r *AutoTestRunner) findRelatedTests(ws *workspace.Workspace, sourceFiles []string) []string {
	var testFiles []string
	seen := make(map[string]bool)

	for _, src := range sourceFiles {
		candidates := r.testCandidates(src)
		for _, candidate := range candidates {
			if seen[candidate] {
				continue
			}
			absPath := filepath.Join(ws.RootDir, candidate)
			if _, err := os.Stat(absPath); err == nil {
				testFiles = append(testFiles, candidate)
				seen[candidate] = true
			}
		}
	}
	return testFiles
}

// testCandidates generates possible test file paths for a given source file.
func (r *AutoTestRunner) testCandidates(srcPath string) []string {
	ext := filepath.Ext(srcPath)
	base := strings.TrimSuffix(srcPath, ext)
	dir := filepath.Dir(srcPath)
	name := filepath.Base(base)

	switch ext {
	case ".go":
		return []string{base + "_test.go"}
	case ".py":
		return []string{
			filepath.Join(dir, "test_"+name+".py"),
			base + "_test.py",
			filepath.Join(dir, "tests", "test_"+name+".py"),
		}
	case ".ts", ".tsx":
		return []string{
			base + ".test" + ext,
			base + ".spec" + ext,
			filepath.Join(dir, "__tests__", name+ext),
		}
	case ".js", ".jsx":
		return []string{
			base + ".test" + ext,
			base + ".spec" + ext,
			filepath.Join(dir, "__tests__", name+ext),
		}
	case ".rs":
		// Rust tests are usually in the same file
		return nil
	}
	return nil
}

// detectLanguage determines the language from a file extension.
func (r *AutoTestRunner) detectLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	default:
		return "unknown"
	}
}

// buildTestCommand returns the shell command to run and the list of test targets.
func (r *AutoTestRunner) buildTestCommand(ws *workspace.Workspace, lang string, srcFiles, testFiles []string) (string, []string) {
	switch lang {
	case "go":
		return r.buildGoTestCmd(ws, srcFiles, testFiles)
	case "python":
		return r.buildPythonTestCmd(ws, testFiles)
	case "typescript", "javascript":
		return r.buildJSTestCmd(ws, testFiles)
	case "rust":
		return r.buildRustTestCmd(ws)
	default:
		return "", nil
	}
}

func (r *AutoTestRunner) buildGoTestCmd(ws *workspace.Workspace, srcFiles, testFiles []string) (string, []string) {
	// Check if go.mod exists
	if _, err := os.Stat(filepath.Join(ws.RootDir, "go.mod")); os.IsNotExist(err) {
		return "", nil
	}

	if len(testFiles) > 0 {
		// Run specific package tests
		pkgs := make(map[string]bool)
		for _, tf := range testFiles {
			pkg := "./" + filepath.Dir(tf)
			pkgs[pkg] = true
		}
		var pkgList []string
		for pkg := range pkgs {
			pkgList = append(pkgList, pkg)
		}
		cmd := fmt.Sprintf("go test -v -count=1 -timeout=60s %s", strings.Join(pkgList, " "))
		return cmd, testFiles
	}

	// No specific test files found — run go vet as a minimum check
	pkgs := make(map[string]bool)
	for _, src := range srcFiles {
		pkg := "./" + filepath.Dir(src)
		pkgs[pkg] = true
	}
	var pkgList []string
	for pkg := range pkgs {
		pkgList = append(pkgList, pkg)
	}
	cmd := fmt.Sprintf("go vet %s", strings.Join(pkgList, " "))
	return cmd, pkgList
}

func (r *AutoTestRunner) buildPythonTestCmd(ws *workspace.Workspace, testFiles []string) (string, []string) {
	if len(testFiles) == 0 {
		return "", nil
	}
	// Prefer pytest, fallback to unittest
	cmd := fmt.Sprintf("python3 -m pytest -v %s", strings.Join(testFiles, " "))
	return cmd, testFiles
}

func (r *AutoTestRunner) buildJSTestCmd(ws *workspace.Workspace, testFiles []string) (string, []string) {
	if len(testFiles) == 0 {
		return "", nil
	}
	// Check for vitest/jest
	pkgJSON := filepath.Join(ws.RootDir, "package.json")
	if _, err := os.Stat(pkgJSON); os.IsNotExist(err) {
		return "", nil
	}
	cmd := fmt.Sprintf("npx vitest run %s --reporter=verbose 2>/dev/null || npx jest --verbose %s",
		strings.Join(testFiles, " "), strings.Join(testFiles, " "))
	return cmd, testFiles
}

func (r *AutoTestRunner) buildRustTestCmd(ws *workspace.Workspace) (string, []string) {
	if _, err := os.Stat(filepath.Join(ws.RootDir, "Cargo.toml")); os.IsNotExist(err) {
		return "", nil
	}
	return "cargo test", []string{"(all)"}
}

// runTest executes a test command and captures the result.
func (r *AutoTestRunner) runTest(ctx context.Context, workDir, command string) *TestResult {
	cmdCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if cmdCtx.Err() != nil {
			exitCode = 137
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Combine output
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	// Smart truncation for large outputs
	if len(output) > 15000 {
		output = smartTruncateOutput(output, 15000)
	}

	return &TestResult{
		Command:  command,
		Passed:   exitCode == 0,
		ExitCode: exitCode,
		Output:   output,
		Duration: duration.Round(time.Millisecond).String(),
	}
}

// FormatForLLM formats the test result as a message suitable for injection
// into the ReAct conversation loop.
func (r *TestResult) FormatForLLM() string {
	if r == nil {
		return ""
	}
	if r.SkipReason != "" {
		return fmt.Sprintf("ℹ️ Auto-test skipped: %s", r.SkipReason)
	}

	var sb strings.Builder
	if r.Passed {
		sb.WriteString("✅ **Auto-test PASSED**\n")
	} else {
		sb.WriteString("❌ **Auto-test FAILED** — please fix the issues before proceeding\n")
	}

	sb.WriteString(fmt.Sprintf("Command: `%s`\n", r.Command))
	sb.WriteString(fmt.Sprintf("Duration: %s | Exit code: %d\n", r.Duration, r.ExitCode))

	if len(r.TestFiles) > 0 {
		sb.WriteString(fmt.Sprintf("Test files: %s\n", strings.Join(r.TestFiles, ", ")))
	}

	if r.Output != "" {
		sb.WriteString("\n```\n")
		sb.WriteString(r.Output)
		sb.WriteString("\n```\n")
	}

	if !r.Passed {
		sb.WriteString("\n⚠️ Your edit caused test failures. Please review the errors above, " +
			"use read_file to re-examine the code, and use edit_file to fix the issues. " +
			"Then I will automatically re-run the tests.")
	}

	return sb.String()
}
