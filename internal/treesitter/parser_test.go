//go:build tree_sitter

package treesitter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestNewCGOParser(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)
	require.NotNil(t, parser)

	langs := parser.SupportedLanguages()
	assert.NotEmpty(t, langs)
	assert.Contains(t, langs, "go")
	assert.Contains(t, langs, "python")
	assert.Contains(t, langs, "typescript")
}

func TestParser_ExtractSymbols_Go(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

type MyStruct struct {
	Field string
}

func PublicFunc() {}

func privateFunc() {}

const PublicConst = 42

var PublicVar = "test"
`

	symbols, err := parser.ExtractSymbols("go", code)
	require.NoError(t, err)
	assert.NotEmpty(t, symbols)

	// Check for public symbols
	hasStruct := false
	hasFunc := false
	hasConst := false
	for _, s := range symbols {
		if s.Name == "MyStruct" && s.Kind == "struct" {
			hasStruct = true
		}
		if s.Name == "PublicFunc" && s.Kind == "function" {
			hasFunc = true
		}
		if s.Name == "PublicConst" && s.Kind == "const" {
			hasConst = true
		}
		// Private symbols should not be included
		assert.NotEqual(t, "privateFunc", s.Name)
	}

	assert.True(t, hasStruct, "should extract MyStruct")
	assert.True(t, hasFunc, "should extract PublicFunc")
	assert.True(t, hasConst, "should extract PublicConst")
}

func TestParser_ExtractSymbols_Python(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `class MyClass:
    def __init__(self):
        pass

    def public_method(self):
        pass

    def _private_method(self):
        pass

def public_function():
    pass

def _private_function():
    pass
`

	symbols, err := parser.ExtractSymbols("python", code)
	require.NoError(t, err)
	assert.NotEmpty(t, symbols)

	hasClass := false
	hasPublicMethod := false
	hasPublicFunc := false
	for _, s := range symbols {
		if s.Name == "MyClass" && s.Kind == "class" {
			hasClass = true
		}
		if s.Name == "public_method" && s.Kind == "method" {
			hasPublicMethod = true
		}
		if s.Name == "public_function" && s.Kind == "function" {
			hasPublicFunc = true
		}
		// Private symbols should not be included
		assert.NotEqual(t, "_private_method", s.Name)
		assert.NotEqual(t, "_private_function", s.Name)
	}

	assert.True(t, hasClass, "should extract MyClass")
	assert.True(t, hasPublicMethod, "should extract public_method")
	assert.True(t, hasPublicFunc, "should extract public_function")
}

func TestParser_ExtractSymbols_TypeScript(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `export class MyClass {
    constructor() {}

    public method(): void {}
}

export function myFunction(): void {}

export const MY_CONST = 42;

interface MyInterface {
    prop: string;
}
`

	symbols, err := parser.ExtractSymbols("typescript", code)
	require.NoError(t, err)
	assert.NotEmpty(t, symbols)

	hasClass := false
	hasFunc := false
	hasInterface := false
	for _, s := range symbols {
		if s.Name == "MyClass" && s.Kind == "class" {
			hasClass = true
		}
		if s.Name == "myFunction" && s.Kind == "function" {
			hasFunc = true
		}
		if s.Name == "MyInterface" && s.Kind == "interface" {
			hasInterface = true
		}
	}

	assert.True(t, hasClass, "should extract MyClass")
	assert.True(t, hasFunc, "should extract myFunction")
	assert.True(t, hasInterface, "should extract MyInterface")
}

func TestParser_ChunkByAST_Go(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

type MyStruct struct {
	Field string
}

func (m *MyStruct) Method() {
	// implementation
}

func PublicFunc() {
	// implementation
}
`

	chunks, err := parser.ChunkByAST("go", code)
	require.NoError(t, err)
	assert.NotEmpty(t, chunks)

	// Should have chunks for struct and functions
	hasStruct := false
	hasMethod := false
	hasFunc := false
	for _, c := range chunks {
		if c.SymbolName == "MyStruct" && c.SymbolType == "struct" {
			hasStruct = true
			assert.Contains(t, c.Content, "Field string")
		}
		if c.SymbolName == "Method" && c.SymbolType == "method" {
			hasMethod = true
		}
		if c.SymbolName == "PublicFunc" && c.SymbolType == "function" {
			hasFunc = true
		}
	}

	assert.True(t, hasStruct, "should chunk MyStruct")
	assert.True(t, hasMethod, "should chunk Method")
	assert.True(t, hasFunc, "should chunk PublicFunc")
}

func TestParser_ChunkByAST_Python(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `class MyClass:
    def __init__(self):
        self.value = 0

    def method(self):
        return self.value

def standalone_function():
    return 42
`

	chunks, err := parser.ChunkByAST("python", code)
	require.NoError(t, err)
	assert.NotEmpty(t, chunks)

	hasClass := false
	hasFunc := false
	for _, c := range chunks {
		if c.SymbolName == "MyClass" && c.SymbolType == "class" {
			hasClass = true
			assert.Contains(t, c.Content, "def __init__")
		}
		if c.SymbolName == "standalone_function" && c.SymbolType == "function" {
			hasFunc = true
		}
	}

	assert.True(t, hasClass, "should chunk MyClass")
	assert.True(t, hasFunc, "should chunk standalone_function")
}

func TestParser_UnsupportedLanguage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	_, err := parser.ExtractSymbols("cobol", "some code")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported language")

	_, err = parser.ChunkByAST("fortran", "some code")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported language")
}

func TestParser_EmptyCode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	symbols, err := parser.ExtractSymbols("go", "")
	require.NoError(t, err)
	assert.Empty(t, symbols)

	chunks, err := parser.ChunkByAST("go", "")
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

func TestParser_InvalidSyntax(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	// Invalid Go code
	code := `package main
func {{{{{ invalid
`

	// Parser should handle gracefully (tree-sitter is error-tolerant)
	symbols, err := parser.ExtractSymbols("go", code)
	// Should not panic, may return empty or partial results
	assert.NoError(t, err)
	_ = symbols
}

func TestParser_LineNumbers(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

func FirstFunc() {
	// line 3
}

func SecondFunc() {
	// line 7
}
`

	symbols, err := parser.ExtractSymbols("go", code)
	require.NoError(t, err)

	for _, s := range symbols {
		if s.Name == "FirstFunc" {
			assert.Equal(t, 3, s.StartLine)
		}
		if s.Name == "SecondFunc" {
			assert.Equal(t, 7, s.StartLine)
		}
	}
}
