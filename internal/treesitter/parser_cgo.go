//go:build tree_sitter

package treesitter

import (
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/cpp"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/java"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	"go.uber.org/zap"
)

var langMap = map[string]*sitter.Language{
	"go":         golang.GetLanguage(),
	"python":     python.GetLanguage(),
	"typescript": typescript.GetLanguage(),
	"javascript": javascript.GetLanguage(),
	"rust":       rust.GetLanguage(),
	"java":       java.GetLanguage(),
	"cpp":        cpp.GetLanguage(),
	"c":          cpp.GetLanguage(),
}

// cgoParser implements Parser using CGO tree-sitter bindings.
type cgoParser struct {
	logger *zap.Logger
}

// NewCGOParser creates a tree-sitter parser using CGO bindings.
func NewCGOParser(logger *zap.Logger) Parser {
	return &cgoParser{logger: logger.With(zap.String("component", "treesitter"))}
}

func (p *cgoParser) SupportedLanguages() []string {
	langs := make([]string, 0, len(langMap))
	for k := range langMap {
		langs = append(langs, k)
	}
	return langs
}

func (p *cgoParser) ExtractSymbols(language, content string) ([]Symbol, error) {
	lang, ok := langMap[strings.ToLower(language)]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s", language)
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(nil, nil, []byte(content))
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	defer tree.Close()

	root := tree.RootNode()
	var symbols []Symbol

	extractSymbolsFromNode(root, content, language, "", &symbols)

	return symbols, nil
}

func (p *cgoParser) ChunkByAST(language, content string) ([]Chunk, error) {
	symbols, err := p.ExtractSymbols(language, content)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(content, "\n")
	var chunks []Chunk

	for _, sym := range symbols {
		startIdx := sym.StartLine - 1
		endIdx := sym.EndLine
		if startIdx < 0 {
			startIdx = 0
		}
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		chunk := Chunk{
			SymbolName: sym.Name,
			SymbolType: sym.Kind,
			Content:    strings.Join(lines[startIdx:endIdx], "\n"),
			StartLine:  sym.StartLine,
			EndLine:    sym.EndLine,
			Signature:  sym.Signature,
		}
		chunks = append(chunks, chunk)
	}

	// If no symbols found, return the whole file as one chunk
	if len(chunks) == 0 && len(content) > 0 {
		chunks = append(chunks, Chunk{
			SymbolName: "<file>",
			SymbolType: "file",
			Content:    content,
			StartLine:  1,
			EndLine:    len(lines),
		})
	}

	return chunks, nil
}

func extractSymbolsFromNode(node *sitter.Node, source, language, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch language {
	case "go":
		extractGoSymbol(node, source, parent, symbols)
	case "python":
		extractPythonSymbol(node, source, parent, symbols)
	case "typescript", "javascript":
		extractTSSymbol(node, source, parent, symbols)
	case "rust":
		extractRustSymbol(node, source, parent, symbols)
	case "java":
		extractJavaSymbol(node, source, parent, symbols)
	case "c", "cpp":
		extractCppSymbol(node, source, parent, symbols)
	}

	// Determine new parent for nested traversal
	newParent := parent
	if isContainerNode(nodeType, language) {
		if name := findChildName(node, source); name != "" {
			newParent = name
		}
	}

	// Recurse into children
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		extractSymbolsFromNode(child, source, language, newParent, symbols)
	}
}

func extractGoSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "function_declaration":
		name := findChildByType(node, "identifier")
		if name != nil {
			sym := Symbol{
				Name:       nodeText(name, source),
				Kind:       "function",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: goVisibility(nodeText(name, source)),
			}
			if params := findChildByType(node, "parameter_list"); params != nil {
				sym.Signature = nodeText(node, source)
				if idx := strings.Index(sym.Signature, "{"); idx > 0 {
					sym.Signature = strings.TrimSpace(sym.Signature[:idx])
				}
			}
			*symbols = append(*symbols, sym)
		}
	case "method_declaration":
		name := findChildByType(node, "field_identifier")
		if name != nil {
			sym := Symbol{
				Name:       nodeText(name, source),
				Kind:       "method",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: goVisibility(nodeText(name, source)),
			}
			sym.Signature = nodeText(node, source)
			if idx := strings.Index(sym.Signature, "{"); idx > 0 {
				sym.Signature = strings.TrimSpace(sym.Signature[:idx])
			}
			*symbols = append(*symbols, sym)
		}
	case "type_declaration":
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child.Type() == "type_spec" {
				typeName := findChildByType(child, "type_identifier")
				if typeName == nil {
					continue
				}
				kind := "type"
				for j := 0; j < int(child.ChildCount()); j++ {
					cc := child.Child(j)
					switch cc.Type() {
					case "struct_type":
						kind = "struct"
					case "interface_type":
						kind = "interface"
					}
				}
				*symbols = append(*symbols, Symbol{
					Name:       nodeText(typeName, source),
					Kind:       kind,
					StartLine:  int(node.StartPoint().Row) + 1,
					EndLine:    int(node.EndPoint().Row) + 1,
					Visibility: goVisibility(nodeText(typeName, source)),
				})
			}
		}
	}
}

func extractPythonSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "function_definition":
		name := findChildByType(node, "identifier")
		if name != nil {
			kind := "function"
			if parent != "" {
				kind = "method"
			}
			vis := "public"
			nameStr := nodeText(name, source)
			if strings.HasPrefix(nameStr, "_") {
				vis = "private"
			}
			*symbols = append(*symbols, Symbol{
				Name:       nameStr,
				Kind:       kind,
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: vis,
			})
		}
	case "class_definition":
		name := findChildByType(node, "identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "class",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	}
}

func extractTSSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "function_declaration":
		name := findChildByType(node, "identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "function",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: "public",
			})
		}
	case "class_declaration":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "class",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "interface_declaration":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "interface",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "method_definition":
		name := findChildByType(node, "property_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "method",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: "public",
			})
		}
	case "arrow_function", "function":
		// Check if it's assigned to a variable
		if p := node.Parent(); p != nil && p.Type() == "variable_declarator" {
			nameNode := findChildByType(p, "identifier")
			if nameNode != nil {
				*symbols = append(*symbols, Symbol{
					Name:       nodeText(nameNode, source),
					Kind:       "function",
					StartLine:  int(node.StartPoint().Row) + 1,
					EndLine:    int(node.EndPoint().Row) + 1,
					Parent:     parent,
					Visibility: "public",
				})
			}
		}
	}
}

func extractRustSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "function_item":
		name := findChildByType(node, "identifier")
		if name != nil {
			vis := "private"
			for i := 0; i < int(node.ChildCount()); i++ {
				if node.Child(i).Type() == "visibility_modifier" {
					vis = "public"
					break
				}
			}
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "function",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: vis,
			})
		}
	case "struct_item":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "struct",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "impl_item":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "type",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "trait_item":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "interface",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "enum_item":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "type",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	}
}

func extractJavaSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "method_declaration":
		name := findChildByType(node, "identifier")
		if name != nil {
			vis := javaVisibility(node, source)
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "method",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: vis,
			})
		}
	case "class_declaration":
		name := findChildByType(node, "identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "class",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: javaVisibility(node, source),
			})
		}
	case "interface_declaration":
		name := findChildByType(node, "identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "interface",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: javaVisibility(node, source),
			})
		}
	}
}

func extractCppSymbol(node *sitter.Node, source, parent string, symbols *[]Symbol) {
	nodeType := node.Type()

	switch nodeType {
	case "function_definition":
		name := findChildByType(node, "identifier")
		if name == nil {
			name = findChildByType(node, "field_identifier")
		}
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "function",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Parent:     parent,
				Visibility: "public",
			})
		}
	case "class_specifier":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "class",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	case "struct_specifier":
		name := findChildByType(node, "type_identifier")
		if name != nil {
			*symbols = append(*symbols, Symbol{
				Name:       nodeText(name, source),
				Kind:       "struct",
				StartLine:  int(node.StartPoint().Row) + 1,
				EndLine:    int(node.EndPoint().Row) + 1,
				Visibility: "public",
			})
		}
	}
}

// Helpers

func nodeText(node *sitter.Node, source string) string {
	return source[node.StartByte():node.EndByte()]
}

func findChildByType(node *sitter.Node, childType string) *sitter.Node {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == childType {
			return child
		}
	}
	return nil
}

func findChildName(node *sitter.Node, source string) string {
	for _, t := range []string{"identifier", "type_identifier", "field_identifier", "property_identifier"} {
		if n := findChildByType(node, t); n != nil {
			return nodeText(n, source)
		}
	}
	return ""
}

func isContainerNode(nodeType, language string) bool {
	containers := map[string]bool{
		"class_declaration":    true,
		"class_definition":     true,
		"class_specifier":      true,
		"struct_specifier":     true,
		"interface_declaration": true,
		"impl_item":            true,
		"type_declaration":     true,
	}
	return containers[nodeType]
}

func goVisibility(name string) string {
	if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
		return "public"
	}
	return "private"
}

func javaVisibility(node *sitter.Node, source string) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "modifiers" {
			text := nodeText(child, source)
			if strings.Contains(text, "public") {
				return "public"
			}
			if strings.Contains(text, "private") {
				return "private"
			}
			if strings.Contains(text, "protected") {
				return "protected"
			}
		}
	}
	return "package"
}
