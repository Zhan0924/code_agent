// ast_native.go — 用 Go 官方 go/parser / go/ast 做结构化 AST 切块。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么 Go 有两个分支（native + 启发式 fallback）】
//
//	parseGoCodeNative：用 go/parser，能识别完整的 AST 节点（FuncDecl、GenDecl
//	  等），能精准定位 struct / interface / method receiver。但要求**语法合法**。
//
//	parseGoCode (ast_parser.go)：启发式正则解析，容错但粗糙。专门用于语法
//	  有错误的文件（用户正在编辑中、代码生成中间状态）——这时 AST 解析会整个
//	  失败，fallback 能继续切出可检索的块。
//
//	dispatcher：`parseGoCodeNative` 先试，`ParseFile` 报错就 fallback。
//
// 【chunk 粒度】
//
//	每个顶层 FuncDecl / GenDecl 一个块，附带它的 leading comments（doc
//	comment 对召回质量影响很大，GitHub 上大量代码仅靠 doc 就能找到）。
//	嵌套函数 / 闭包不单独分块（语义上属于外层函数）。
//
// 【符号名提取】
//
//	· FuncDecl → fn.Name.Name；methods 会拼成 "Receiver.Method" 便于 BM25 匹配。
//	· GenDecl (TypeSpec) → 每个 spec 的 Name.Name + 加 symbol_type = struct/interface。
//	· GenDecl (ValueSpec) → 顶层 var/const，也单独建块。
//
// ============================================================================
package rag

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// parseGoCodeNative uses Go's native go/parser for accurate AST analysis.
// This replaces the heuristic regex-based parser for Go source files.
func parseGoCodeNative(content string) []astChunk {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "source.go", content, parser.ParseComments)
	if err != nil {
		// Fall back to heuristic parser on parse error
		return parseGoCode(content)
	}

	lines := strings.Split(content, "\n")
	var chunks []astChunk

	// Extract package-level documentation
	if f.Doc != nil {
		chunks = append(chunks, astChunk{
			symbolName: f.Name.Name,
			symbolType: "package",
			content:    f.Doc.Text(),
			startLine:  fset.Position(f.Doc.Pos()).Line,
			endLine:    fset.Position(f.Doc.End()).Line,
		})
	}

	// Process all top-level declarations
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			chunk := extractFuncChunk(fset, d, lines, content)
			if chunk != nil {
				chunks = append(chunks, *chunk)
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					chunk := extractTypeChunk(fset, d, s, lines, content)
					if chunk != nil {
						chunks = append(chunks, *chunk)
					}

				case *ast.ValueSpec:
					chunk := extractValueChunk(fset, d, s, lines, content)
					if chunk != nil {
						chunks = append(chunks, *chunk)
					}
				}
			}
		}
	}

	return chunks
}

// extractFuncChunk extracts an astChunk from a function declaration.
func extractFuncChunk(fset *token.FileSet, fn *ast.FuncDecl, lines []string, _ string) *astChunk {
	startLine := fset.Position(fn.Pos()).Line
	endLine := fset.Position(fn.End()).Line

	// Determine symbol type: function vs method
	symbolType := "function"
	symbolName := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		symbolType = "method"
		recvType := exprToString(fn.Recv.List[0].Type)
		symbolName = recvType + "." + fn.Name.Name
	}

	// Include doc comment if present
	if fn.Doc != nil {
		docStart := fset.Position(fn.Doc.Pos()).Line
		if docStart < startLine {
			startLine = docStart
		}
	}

	// Extract content from lines
	if startLine > 0 && endLine <= len(lines) {
		content := strings.Join(lines[startLine-1:endLine], "\n")

		// Extract dependencies: function calls within the body
		var deps []string
		if fn.Body != nil {
			deps = extractCallDeps(fn.Body)
		}

		return &astChunk{
			symbolName:   symbolName,
			symbolType:   symbolType,
			content:      content,
			startLine:    startLine,
			endLine:      endLine,
			dependencies: deps,
		}
	}
	return nil
}

// extractTypeChunk extracts an astChunk from a type declaration.
func extractTypeChunk(fset *token.FileSet, gen *ast.GenDecl, spec *ast.TypeSpec, lines []string, _ string) *astChunk {
	startLine := fset.Position(gen.Pos()).Line
	endLine := fset.Position(spec.End()).Line

	// Include doc comment
	if gen.Doc != nil {
		docStart := fset.Position(gen.Doc.Pos()).Line
		if docStart < startLine {
			startLine = docStart
		}
	}

	// Determine type kind
	symbolType := "type"
	switch spec.Type.(type) {
	case *ast.StructType:
		symbolType = "struct"
	case *ast.InterfaceType:
		symbolType = "interface"
	}

	if startLine > 0 && endLine <= len(lines) {
		content := strings.Join(lines[startLine-1:endLine], "\n")
		return &astChunk{
			symbolName: spec.Name.Name,
			symbolType: symbolType,
			content:    content,
			startLine:  startLine,
			endLine:    endLine,
		}
	}
	return nil
}

// extractValueChunk extracts an astChunk from var/const declarations.
func extractValueChunk(fset *token.FileSet, gen *ast.GenDecl, spec *ast.ValueSpec, lines []string, _ string) *astChunk {
	startLine := fset.Position(gen.Pos()).Line
	endLine := fset.Position(spec.End()).Line

	symbolType := "var"
	if gen.Tok == token.CONST {
		symbolType = "const"
	}

	name := ""
	for _, n := range spec.Names {
		if name != "" {
			name += ", "
		}
		name += n.Name
	}

	if startLine > 0 && endLine <= len(lines) {
		content := strings.Join(lines[startLine-1:endLine], "\n")
		return &astChunk{
			symbolName: name,
			symbolType: symbolType,
			content:    content,
			startLine:  startLine,
			endLine:    endLine,
		}
	}
	return nil
}

// extractCallDeps extracts function call names from a function body.
func extractCallDeps(body *ast.BlockStmt) []string {
	seen := make(map[string]bool)
	var deps []string

	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			name := exprToString(call.Fun)
			if name != "" && !seen[name] {
				seen[name] = true
				deps = append(deps, name)
			}
		}
		return true
	})

	return deps
}

// exprToString converts an AST expression to its string representation.
func exprToString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprToString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprToString(e.X)
	case *ast.IndexExpr:
		return exprToString(e.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}
