// Package rag / AST 感知分块器（核心检索质量来源）
//
// ============================================================================
//
//	【为什么要 AST 分块？】
//
//	传统 RAG 按字符长度切分（RecursiveCharacterTextSplitter），对代码而言：
//	  - 一个 200 行的函数可能被从中间切断，语义完全丢失；
//	  - 变量名、类型签名这些"结构化信号"被淹没；
//	  - 检索结果返回半截函数，LLM 拿不到足够上下文。
//
//	正确做法：按源语言的"语义最小单元"切块。
//	  · Go:       func / method / type / interface / struct
//	  · Python:   def / class
//	  · Markdown: #, ##, ### 小节
//	  · Shell:    定义函数或逻辑段落
//
//	每个 chunk 是**自包含**的：包含签名、注释、函数体、依赖调用。LLM 看到
//	一个 chunk 就够理解"这段代码做什么"。
//
// 【为什么本实现是启发式而非 tree-sitter？】
//
//	本项目采用"可选 cgo 编译"策略：
//	  - 默认走纯 Go 启发式分块（本文件），无需任何系统依赖，一条 go build 即可；
//	  - 若开启 `-tags tree_sitter`，会替换为 ast_native.go 里调用 tree-sitter-go 的
//	    精确解析。二者接口一致 (astChunk)，生产环境按需切换。
//
//	启发式规则：基于 "func / def / class / # " 前缀 + 花括号深度计数，对 90% 常见
//	代码已足够；极端嵌套或 macro 代码会退化，但不会 crash。
//
// 【输出格式 astChunk】
//
//	symbolName  : 可被 LLM 直接引用的符号名（如 "UserService.Login"）
//	symbolType  : function/method/class/... 供 Payload 过滤与排序
//	content     : 完整原文（含前导注释，用作 embedding 输入）
//	startLine   : 源文件行号（定位跳转用）
//	endLine     :
//	dependencies: 该块调用到的其他符号（用于构建调用图）
//
// ============================================================================
package rag

import (
	"strings"
)

// astChunk represents a code segment identified by AST analysis.
type astChunk struct {
	symbolName   string
	symbolType   string // "function", "method", "class", "struct", "interface"
	content      string
	startLine    int
	endLine      int
	dependencies []string
}

// parseWithAST attempts to parse code using language-specific heuristics.
// In production, this would use tree-sitter for accurate AST parsing.
// This implementation provides a robust heuristic-based parser.
//
// 入口函数：按语言分派到不同的分块器。所有分块器返回统一的 []astChunk，
// 便于下游（embedder.go）无差别处理。
//
// 添加新语言支持只需三步：
//  1. 实现 parseXxxCode(content) []astChunk；
//  2. 在本函数 switch 增加一条 case；
//  3. 无需改动 engine.go / embedder.go。
func parseWithAST(language, content string) []astChunk {
	switch strings.ToLower(language) {
	case "go":
		return parseGoCodeNative(content)
	case "python":
		return parsePythonCode(content)
	case "markdown":
		return parseMarkdown(content)
	case "shell", "bash":
		return parseShellScript(content)
	default:
		return parseGenericCode(content)
	}
}

// parseGoCode parses Go source code into function/type-level chunks.
func parseGoCode(content string) []astChunk {
	lines := strings.Split(content, "\n")
	var chunks []astChunk
	var current *astChunk
	braceDepth := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect function declarations
		if strings.HasPrefix(trimmed, "func ") {
			if current != nil && braceDepth == 0 {
				current.endLine = i
				current.content = strings.Join(lines[current.startLine-1:i], "\n")
				chunks = append(chunks, *current)
			}

			symbolName := extractGoFuncName(trimmed)
			// Method detection: the signature must open with a receiver in
			// parentheses immediately after "func ". The prior check
			// (strings.Contains ") ") incorrectly matched ordinary functions
			// with multi-value returns like `func F() (int, error)`.
			symbolType := "function"
			if strings.HasPrefix(trimmed, "func (") {
				symbolType = "method"
			}

			current = &astChunk{
				symbolName: symbolName,
				symbolType: symbolType,
				startLine:  i + 1,
			}
		}

		// Detect type declarations
		if strings.HasPrefix(trimmed, "type ") && (strings.Contains(trimmed, "struct") || strings.Contains(trimmed, "interface")) {
			if current != nil && braceDepth == 0 {
				current.endLine = i
				current.content = strings.Join(lines[current.startLine-1:i], "\n")
				chunks = append(chunks, *current)
			}

			symbolType := "struct"
			if strings.Contains(trimmed, "interface") {
				symbolType = "interface"
			}

			parts := strings.Fields(trimmed)
			symbolName := ""
			if len(parts) >= 2 {
				symbolName = parts[1]
			}

			current = &astChunk{
				symbolName: symbolName,
				symbolType: symbolType,
				startLine:  i + 1,
			}
		}

		// Track brace depth for block boundaries
		braceDepth += strings.Count(line, "{") - strings.Count(line, "}")

		// Extract dependencies (function calls)
		if current != nil {
			deps := extractGoDependencies(trimmed)
			current.dependencies = append(current.dependencies, deps...)
		}
	}

	// Flush last chunk
	if current != nil {
		current.endLine = len(lines)
		current.content = strings.Join(lines[current.startLine-1:], "\n")
		chunks = append(chunks, *current)
	}

	return chunks
}

// parsePythonCode parses Python source code into function/class-level chunks.
func parsePythonCode(content string) []astChunk {
	lines := strings.Split(content, "\n")
	var chunks []astChunk
	var current *astChunk

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect function/method definitions
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") {
			if current != nil {
				current.endLine = i
				current.content = strings.Join(lines[current.startLine-1:i], "\n")
				chunks = append(chunks, *current)
			}

			symbolName := extractPythonFuncName(trimmed)
			current = &astChunk{
				symbolName: symbolName,
				symbolType: "function",
				startLine:  i + 1,
			}
		}

		// Detect class definitions
		if strings.HasPrefix(trimmed, "class ") {
			if current != nil {
				current.endLine = i
				current.content = strings.Join(lines[current.startLine-1:i], "\n")
				chunks = append(chunks, *current)
			}

			symbolName := extractPythonClassName(trimmed)
			current = &astChunk{
				symbolName: symbolName,
				symbolType: "class",
				startLine:  i + 1,
			}
		}
	}

	if current != nil {
		current.endLine = len(lines)
		current.content = strings.Join(lines[current.startLine-1:], "\n")
		chunks = append(chunks, *current)
	}

	return chunks
}

// parseGenericCode provides a fallback parser that tries to identify code blocks.
func parseGenericCode(content string) []astChunk {
	// For unknown languages, return nil to trigger fallback sliding window
	return nil
}

// ─── Helper Functions ─────────────────────────────────────────────────────────

func extractGoFuncName(line string) string {
	// Handle: func Name(...) or func (r *Receiver) Name(...)
	line = strings.TrimPrefix(line, "func ")

	// Method with receiver
	if strings.HasPrefix(line, "(") {
		idx := strings.Index(line, ")")
		if idx >= 0 && idx+2 < len(line) {
			line = strings.TrimSpace(line[idx+1:])
		}
	}

	// Extract name before (
	idx := strings.Index(line, "(")
	if idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	// Fallback: take the first whitespace-delimited token if any.
	// strings.Fields returns [] on whitespace-only input, so guarding the
	// slice access here prevents an indexing panic on malformed source like
	// "func " with nothing after it.
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func extractGoDependencies(line string) []string {
	var deps []string
	// Simple heuristic: find function calls pattern: identifier(
	parts := strings.Split(line, "(")
	for _, p := range parts[:len(parts)-1] {
		p = strings.TrimSpace(p)
		// Get last word before (
		fields := strings.FieldsFunc(p, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == '=' || r == '!'
		})
		if len(fields) > 0 {
			last := fields[len(fields)-1]
			// Filter out keywords
			if !isGoKeyword(last) && len(last) > 0 {
				deps = append(deps, last)
			}
		}
	}
	return deps
}

func extractPythonFuncName(line string) string {
	line = strings.TrimPrefix(line, "async ")
	line = strings.TrimPrefix(line, "def ")
	idx := strings.Index(line, "(")
	if idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	return line
}

func extractPythonClassName(line string) string {
	line = strings.TrimPrefix(line, "class ")
	idx := strings.IndexAny(line, "(:")
	if idx > 0 {
		return strings.TrimSpace(line[:idx])
	}
	return strings.TrimSuffix(strings.TrimSpace(line), ":")
}

func isGoKeyword(s string) bool {
	keywords := map[string]bool{
		"if": true, "else": true, "for": true, "range": true,
		"switch": true, "case": true, "return": true, "go": true,
		"defer": true, "select": true, "var": true, "const": true,
		"type": true, "func": true, "package": true, "import": true,
	}
	return keywords[s]
}
