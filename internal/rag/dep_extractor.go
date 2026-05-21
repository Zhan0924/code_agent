package rag

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// FileDepInfo holds all dependency information extracted from a single file.
type FileDepInfo struct {
	FilePath   string   `json:"file_path"`
	Package    string   `json:"package"`
	Imports    []string `json:"imports"`
	Symbols    []string `json:"symbols"`
	TypeRefs   []string `json:"type_refs"`
	CallRefs   []string `json:"call_refs"`
	Embeds     []string `json:"embeds"`
	Implements []string `json:"implements"`
}

// ExtractGoDeps performs deep dependency extraction from Go source code.
// It extracts imports, type references, interface implementations, and embeddings.
func ExtractGoDeps(filePath, content string) *FileDepInfo {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return &FileDepInfo{FilePath: filePath}
	}

	info := &FileDepInfo{
		FilePath: filePath,
		Package:  f.Name.Name,
	}

	// Extract imports
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		info.Imports = append(info.Imports, path)
	}

	// Walk all declarations for symbols, type refs, embeds
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recvType := typeExprName(d.Recv.List[0].Type)
				name = recvType + "." + name
			}
			info.Symbols = append(info.Symbols, name)

			// Extract type references from params and returns
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					info.TypeRefs = appendTypeRef(info.TypeRefs, field.Type)
				}
			}
			if d.Type.Results != nil {
				for _, field := range d.Type.Results.List {
					info.TypeRefs = appendTypeRef(info.TypeRefs, field.Type)
				}
			}

			// Extract call references from body
			if d.Body != nil {
				info.CallRefs = append(info.CallRefs, extractCallNames(d.Body)...)
			}

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					info.Symbols = append(info.Symbols, s.Name.Name)
					extractStructInfo(s, info)
				}
			}
		}
	}

	// Deduplicate
	info.TypeRefs = dedup(info.TypeRefs)
	info.CallRefs = dedup(info.CallRefs)
	info.Embeds = dedup(info.Embeds)

	return info
}

// extractStructInfo extracts embedding and interface implementation info from a type spec.
func extractStructInfo(spec *ast.TypeSpec, info *FileDepInfo) {
	switch t := spec.Type.(type) {
	case *ast.StructType:
		if t.Fields == nil {
			return
		}
		for _, field := range t.Fields.List {
			// Anonymous field = embedding
			if len(field.Names) == 0 {
				name := typeExprName(field.Type)
				if name != "" {
					info.Embeds = append(info.Embeds, name)
				}
			}
			// All field types are type references
			info.TypeRefs = appendTypeRef(info.TypeRefs, field.Type)
		}

	case *ast.InterfaceType:
		if t.Methods == nil {
			return
		}
		for _, method := range t.Methods.List {
			// Embedded interface
			if len(method.Names) == 0 {
				name := typeExprName(method.Type)
				if name != "" {
					info.Implements = append(info.Implements, name)
				}
			}
		}
	}
}

// extractCallNames extracts all function/method call names from a block.
func extractCallNames(body *ast.BlockStmt) []string {
	seen := make(map[string]bool)
	var names []string

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := typeExprName(call.Fun)
		if name != "" && !seen[name] && !isBuiltin(name) {
			seen[name] = true
			names = append(names, name)
		}
		return true
	})

	return names
}

// typeExprName extracts a readable name from a type expression.
func typeExprName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return typeExprName(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return typeExprName(e.X)
	case *ast.ArrayType:
		return typeExprName(e.Elt)
	case *ast.MapType:
		return typeExprName(e.Value)
	case *ast.IndexExpr:
		return typeExprName(e.X)
	default:
		return ""
	}
}

// appendTypeRef adds a type reference if it's a meaningful named type.
func appendTypeRef(refs []string, expr ast.Expr) []string {
	name := typeExprName(expr)
	if name != "" && !isPrimitive(name) {
		refs = append(refs, name)
	}
	return refs
}

func isPrimitive(name string) bool {
	prims := map[string]bool{
		"bool": true, "byte": true, "int": true, "int8": true,
		"int16": true, "int32": true, "int64": true, "uint": true,
		"uint8": true, "uint16": true, "uint32": true, "uint64": true,
		"float32": true, "float64": true, "string": true, "error": true,
		"any": true, "rune": true, "uintptr": true, "complex64": true,
		"complex128": true,
	}
	return prims[name]
}

func isBuiltin(name string) bool {
	builtins := map[string]bool{
		"len": true, "cap": true, "make": true, "new": true,
		"append": true, "copy": true, "delete": true, "close": true,
		"panic": true, "recover": true, "print": true, "println": true,
	}
	return builtins[name]
}

func dedup(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ss))
	result := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// PopulateDepGraph takes extracted file dependency info and adds it to the graph.
func PopulateDepGraph(graph *DepGraph, info *FileDepInfo) {
	// Register all symbols defined in this file
	for _, sym := range info.Symbols {
		qSym := qualifiedSymbol(info.FilePath, sym)
		graph.RegisterSymbol(info.FilePath, qSym)
	}

	// Add call edges
	for _, sym := range info.Symbols {
		qFrom := qualifiedSymbol(info.FilePath, sym)
		for _, call := range info.CallRefs {
			qTo := qualifyTarget(info.FilePath, call)
			graph.AddEdge(DepEdge{
				From:     qFrom,
				To:       qTo,
				Kind:     DepCall,
				FilePath: info.FilePath,
				Weight:   1.0,
			})
		}
	}

	// Add type reference edges
	for _, sym := range info.Symbols {
		qFrom := qualifiedSymbol(info.FilePath, sym)
		for _, ref := range info.TypeRefs {
			qTo := qualifyTarget(info.FilePath, ref)
			graph.AddEdge(DepEdge{
				From:     qFrom,
				To:       qTo,
				Kind:     DepType,
				FilePath: info.FilePath,
				Weight:   0.6,
			})
		}
	}

	// Add embed edges
	for _, sym := range info.Symbols {
		qFrom := qualifiedSymbol(info.FilePath, sym)
		for _, embed := range info.Embeds {
			qTo := qualifyTarget(info.FilePath, embed)
			graph.AddEdge(DepEdge{
				From:     qFrom,
				To:       qTo,
				Kind:     DepEmbed,
				FilePath: info.FilePath,
				Weight:   0.8,
			})
		}
	}

	// Add interface implementation edges
	for _, sym := range info.Symbols {
		qFrom := qualifiedSymbol(info.FilePath, sym)
		for _, iface := range info.Implements {
			qTo := qualifyTarget(info.FilePath, iface)
			graph.AddEdge(DepEdge{
				From:     qFrom,
				To:       qTo,
				Kind:     DepImplement,
				FilePath: info.FilePath,
				Weight:   0.9,
			})
		}
	}
}

// qualifyTarget qualifies a target symbol: if it contains a dot (cross-package),
// keep as-is; otherwise qualify with the source file's package.
func qualifyTarget(filePath, target string) string {
	if strings.Contains(target, ".") {
		return target
	}
	return qualifiedSymbol(filePath, target)
}
