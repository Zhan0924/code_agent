package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/lsp"
	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/tools"
	"go.uber.org/zap"
)

// RegisterLSPTools registers LSP-backed code intelligence tools.
func (o *Orchestrator) RegisterLSPTools(reg *tools.Registry) error {
	if o.lspClient == nil {
		return nil
	}

	lspTools := []struct {
		name    string
		desc    string
		params  json.RawMessage
		handler func(context.Context, json.RawMessage) (*models.ToolResult, error)
	}{
		{
			name:    "goto_definition",
			desc:    "Jump to the definition of a symbol at the given position. Returns file path and line number.",
			params:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path"},"line":{"type":"integer","description":"Line number (1-based)"},"column":{"type":"integer","description":"Column number (1-based)"}},"required":["file","line","column"]}`),
			handler: o.toolGotoDefinition,
		},
		{
			name:    "find_references",
			desc:    "Find all references to the symbol at the given position across the workspace.",
			params:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path"},"line":{"type":"integer","description":"Line number (1-based)"},"column":{"type":"integer","description":"Column number (1-based)"}},"required":["file","line","column"]}`),
			handler: o.toolFindReferences,
		},
		{
			name:    "hover_info",
			desc:    "Get type signature and documentation for the symbol at the given position.",
			params:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path"},"line":{"type":"integer","description":"Line number (1-based)"},"column":{"type":"integer","description":"Column number (1-based)"}},"required":["file","line","column"]}`),
			handler: o.toolHoverInfo,
		},
		{
			name:    ToolRenameSymbol,
			desc:    "Semantically rename a symbol across all files in the workspace. Returns the list of changes.",
			params:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path"},"line":{"type":"integer","description":"Line number (1-based)"},"column":{"type":"integer","description":"Column number (1-based)"},"new_name":{"type":"string","description":"New name for the symbol"}},"required":["file","line","column","new_name"]}`),
			handler: o.toolRenameSymbol,
		},
		{
			name:    "document_symbols",
			desc:    "List all symbols (functions, types, methods, variables) declared in a file, with their line ranges. Useful for understanding a file's structure before navigating.",
			params:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"File path"}},"required":["file"]}`),
			handler: o.toolDocumentSymbols,
		},
	}

	for _, lt := range lspTools {
		def := models.ToolDefinition{
			Name:        lt.name,
			Description: lt.desc,
			Parameters:  lt.params,
			Source:      "builtin",
			RiskLevel:   0,
		}
		switch lt.name {
		case "goto_definition", "find_references", "hover_info", "document_symbols":
			def.IsIdempotentRead = true
		case ToolRenameSymbol:
			def.RiskLevel = 2 // high risk: cross-file modification
			def.IsFileWrite = true
			def.TriggersAutoTest = true
			def.InvalidatesCache = true
		}
		tool := &builtinTool{def: def, handler: lt.handler}
		if err := reg.Register(tool); err != nil {
			return err
		}
	}

	return nil
}

func (o *Orchestrator) toolGotoDefinition(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		File   string `json:"file"`
		Line   int    `json:"line"`
		Column int    `json:"column"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	uri := fileToURI(params.File)
	locations, err := o.lspClient.GotoDefinition(ctx, uri, params.Line-1, params.Column-1)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}

	if len(locations) == 0 {
		return &models.ToolResult{Content: "No definition found"}, nil
	}

	var sb strings.Builder
	for _, loc := range locations {
		sb.WriteString(fmt.Sprintf("%s:%d:%d\n", uriToFile(loc.URI), loc.StartLine+1, loc.StartCol+1))
	}

	o.logger.Info("tool:goto_definition", zap.String("file", params.File), zap.Int("results", len(locations)))
	return &models.ToolResult{Content: sb.String()}, nil
}

func (o *Orchestrator) toolFindReferences(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		File   string `json:"file"`
		Line   int    `json:"line"`
		Column int    `json:"column"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	uri := fileToURI(params.File)
	locations, err := o.lspClient.FindReferences(ctx, uri, params.Line-1, params.Column-1)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}

	if len(locations) == 0 {
		return &models.ToolResult{Content: "No references found"}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d references:\n", len(locations)))
	for _, loc := range locations {
		sb.WriteString(fmt.Sprintf("  %s:%d:%d\n", uriToFile(loc.URI), loc.StartLine+1, loc.StartCol+1))
	}

	o.logger.Info("tool:find_references", zap.String("file", params.File), zap.Int("results", len(locations)))
	return &models.ToolResult{Content: sb.String()}, nil
}

func (o *Orchestrator) toolHoverInfo(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		File   string `json:"file"`
		Line   int    `json:"line"`
		Column int    `json:"column"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	uri := fileToURI(params.File)
	result, err := o.lspClient.Hover(ctx, uri, params.Line-1, params.Column-1)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}

	if result == nil || result.Contents == "" {
		return &models.ToolResult{Content: "No hover information available"}, nil
	}

	return &models.ToolResult{Content: result.Contents}, nil
}

func (o *Orchestrator) toolRenameSymbol(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Column  int    `json:"column"`
		NewName string `json:"new_name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	uri := fileToURI(params.File)
	edit, err := o.lspClient.Rename(ctx, uri, params.Line-1, params.Column-1, params.NewName)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}

	if edit == nil || len(edit.Changes) == 0 {
		return &models.ToolResult{Content: "No changes needed"}, nil
	}

	var sb strings.Builder
	totalEdits := 0
	for fileURI, edits := range edit.Changes {
		sb.WriteString(fmt.Sprintf("%s: %d edits\n", uriToFile(fileURI), len(edits)))
		totalEdits += len(edits)
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d edits across %d files", totalEdits, len(edit.Changes)))

	o.logger.Info("tool:rename_symbol",
		zap.String("file", params.File),
		zap.String("new_name", params.NewName),
		zap.Int("total_edits", totalEdits))

	return &models.ToolResult{Content: sb.String()}, nil
}

func (o *Orchestrator) toolDocumentSymbols(ctx context.Context, args json.RawMessage) (*models.ToolResult, error) {
	var params struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return &models.ToolResult{Content: "Invalid arguments: " + err.Error(), IsError: true}, nil
	}

	uri := fileToURI(params.File)
	symbols, err := o.lspClient.DocumentSymbols(ctx, uri)
	if err != nil {
		return &models.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}

	if len(symbols) == 0 {
		return &models.ToolResult{Content: "No symbols found"}, nil
	}

	var sb strings.Builder
	var walk func(syms []lsp.SymbolInfo, depth int)
	walk = func(syms []lsp.SymbolInfo, depth int) {
		for _, s := range syms {
			indent := strings.Repeat("  ", depth)
			startLine := s.Range.Start.Line + 1
			endLine := s.Range.End.Line + 1
			sb.WriteString(fmt.Sprintf("%s%s (%s) L%d-%d", indent, s.Name, symbolKindName(s.Kind), startLine, endLine))
			if s.Detail != "" {
				sb.WriteString(" — " + s.Detail)
			}
			sb.WriteString("\n")
			if len(s.Children) > 0 {
				walk(s.Children, depth+1)
			}
		}
	}
	walk(symbols, 0)

	o.logger.Info("tool:document_symbols", zap.String("file", params.File), zap.Int("results", len(symbols)))
	return &models.ToolResult{Content: sb.String()}, nil
}

// symbolKindName maps LSP SymbolKind integers to human-readable names.
func symbolKindName(kind int) string {
	names := map[int]string{
		1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
		6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
		11: "interface", 12: "function", 13: "variable", 14: "constant", 15: "string",
		16: "number", 17: "boolean", 18: "array", 19: "object", 20: "key",
		21: "null", 22: "enum_member", 23: "struct", 24: "event", 25: "operator",
		26: "type_parameter",
	}
	if n, ok := names[kind]; ok {
		return n
	}
	return fmt.Sprintf("kind%d", kind)
}

func fileToURI(path string) string {
	if strings.HasPrefix(path, "file://") {
		return path
	}
	return "file://" + path
}

func uriToFile(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}
