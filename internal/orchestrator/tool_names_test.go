package orchestrator

import (
	"reflect"
	"strings"
	"testing"
)

// TestToolNameConstants_MatchRegisteredDefinitions is the compile-time guard
// for PR 8. It collects every string returned by fileToolDefinitions() and
// gitToolDefinitions(), then asserts that every Tool* constant declared in
// tool_names.go appears in that set. If someone renames a tool definition
// without updating the constant (or vice versa), this test catches it before
// the regression reaches production.
//
// shell_exec / execute_code / rename_symbol are registered via builtinTool
// constructors that don't expose a definitions() function — those are
// validated separately by asserting they appear in the literal constants we
// already substituted at the registration sites.
func TestToolNameConstants_MatchRegisteredDefinitions(t *testing.T) {
	registered := map[string]bool{}
	for _, def := range fileToolDefinitions() {
		registered[def.Name] = true
	}
	for _, def := range gitToolDefinitions() {
		registered[def.Name] = true
	}
	// Tools whose registration happens outside fileToolDefinitions / gitToolDefinitions
	// (pty_tools, builtin_tools execute_code, lsp_tools rename_symbol). Listed
	// explicitly because their registration sites build the ToolDefinition
	// directly inside RegisterX rather than from a slice-returning constructor.
	registered[ToolShellExec] = true
	registered[ToolExecuteCode] = true
	registered[ToolRenameSymbol] = true

	// Reflect over the package-level constants by inspecting a synthetic
	// table — constants can't be enumerated via reflection in Go, so we
	// gather them by name. New constants must be added here AND in
	// tool_names.go; if a constant exists but isn't in this list, this test
	// won't catch it. The guard is one-way (constant → registered).
	allConstants := map[string]string{
		"ToolReadFile":        ToolReadFile,
		"ToolWriteFile":       ToolWriteFile,
		"ToolPatchFile":       ToolPatchFile,
		"ToolEditFile":        ToolEditFile,
		"ToolApplyDiff":       ToolApplyDiff,
		"ToolListFiles":       ToolListFiles,
		"ToolCreateDirectory": ToolCreateDirectory,
		"ToolRunTests":        ToolRunTests,
		"ToolRunWorkspaceCmd": ToolRunWorkspaceCmd,
		"ToolShellExec":       ToolShellExec,
		"ToolExecuteCode":     ToolExecuteCode,
		"ToolGitStatus":       ToolGitStatus,
		"ToolGitDiff":         ToolGitDiff,
		"ToolGitCommit":       ToolGitCommit,
		"ToolGitLog":          ToolGitLog,
		"ToolGitBranch":       ToolGitBranch,
		"ToolRenameSymbol":    ToolRenameSymbol,
	}

	for constName, value := range allConstants {
		if value == "" {
			t.Errorf("constant %s has empty value", constName)
			continue
		}
		if !registered[value] {
			t.Errorf("constant %s = %q does not appear in any tool definition; either the tool was unregistered or the constant is stale",
				constName, value)
		}
	}
}

// TestToolNameConstants_UniqueValues guards against two constants accidentally
// collapsing to the same string (e.g. ToolEditFile and ToolPatchFile diverging
// from each other matters because the registry uses the name as a key).
func TestToolNameConstants_UniqueValues(t *testing.T) {
	values := []struct {
		name, val string
	}{
		{"ToolReadFile", ToolReadFile},
		{"ToolWriteFile", ToolWriteFile},
		{"ToolPatchFile", ToolPatchFile},
		{"ToolEditFile", ToolEditFile},
		{"ToolApplyDiff", ToolApplyDiff},
		{"ToolListFiles", ToolListFiles},
		{"ToolCreateDirectory", ToolCreateDirectory},
		{"ToolRunTests", ToolRunTests},
		{"ToolRunWorkspaceCmd", ToolRunWorkspaceCmd},
		{"ToolShellExec", ToolShellExec},
		{"ToolExecuteCode", ToolExecuteCode},
		{"ToolGitStatus", ToolGitStatus},
		{"ToolGitDiff", ToolGitDiff},
		{"ToolGitCommit", ToolGitCommit},
		{"ToolGitLog", ToolGitLog},
		{"ToolGitBranch", ToolGitBranch},
		{"ToolRenameSymbol", ToolRenameSymbol},
	}
	seen := map[string]string{}
	for _, v := range values {
		if prev, dup := seen[v.val]; dup {
			t.Errorf("constants %s and %s both equal %q", prev, v.name, v.val)
		}
		seen[v.val] = v.name
	}
}

// TestToolNameConstants_NoStringTypeLiterals confirms the constants are
// plain string constants (not named-string types) so they remain
// interchangeable with bare "..." literals at call sites. A future refactor
// that introduces `type ToolName string` here would break the substitution
// in maps like fileHandlers (map[string]...).
func TestToolNameConstants_NoStringTypeLiterals(t *testing.T) {
	v := reflect.ValueOf(ToolReadFile)
	if v.Kind() != reflect.String {
		t.Fatalf("expected ToolReadFile kind=String, got %s", v.Kind())
	}
	typeName := reflect.TypeOf(ToolReadFile).String()
	if !strings.HasPrefix(typeName, "string") {
		t.Errorf("ToolReadFile must be a plain string (got type %q); a named-string type would break map[string]... call sites",
			typeName)
	}
}
