//go:build !tree_sitter

package treesitter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestFallbackParser_New(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)
	require.NotNil(t, parser)

	langs := parser.SupportedLanguages()
	assert.NotEmpty(t, langs)
	assert.Contains(t, langs, "go")
	assert.Contains(t, langs, "python")
	assert.Contains(t, langs, "typescript")
}

func TestFallbackParser_ExtractSymbols_Go(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

type MyStruct struct {
	Field string
}

func PublicFunc() {}

func privateFunc() {}
`

	symbols, err := parser.ExtractSymbols("go", code)
	require.NoError(t, err)
	assert.NotEmpty(t, symbols)

	hasStruct := false
	hasPublicFunc := false
	for _, s := range symbols {
		if s.Name == "MyStruct" && s.Kind == "struct" {
			hasStruct = true
			assert.Equal(t, "public", s.Visibility)
		}
		if s.Name == "PublicFunc" && s.Kind == "function" {
			hasPublicFunc = true
			assert.Equal(t, "public", s.Visibility)
		}
		if s.Name == "privateFunc" {
			assert.Equal(t, "private", s.Visibility)
		}
	}

	assert.True(t, hasStruct, "should extract MyStruct")
	assert.True(t, hasPublicFunc, "should extract PublicFunc")
}

func TestFallbackParser_ExtractSymbols_Python(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `class MyClass:
    def __init__(self):
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
	hasPublicFunc := false
	for _, s := range symbols {
		if s.Name == "MyClass" && s.Kind == "class" {
			hasClass = true
		}
		if s.Name == "public_function" && s.Kind == "function" {
			hasPublicFunc = true
			assert.Equal(t, "public", s.Visibility)
		}
		if s.Name == "_private_function" {
			assert.Equal(t, "private", s.Visibility)
		}
	}

	assert.True(t, hasClass, "should extract MyClass")
	assert.True(t, hasPublicFunc, "should extract public_function")
}

func TestFallbackParser_ExtractSymbols_TypeScript(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `export class MyClass {
    constructor() {}
}

export function myFunction(): void {}

export interface MyInterface {
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

func TestFallbackParser_ChunkByAST_Go(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

type MyStruct struct {
	Field string
}

func PublicFunc() {
	// implementation
}
`

	chunks, err := parser.ChunkByAST("go", code)
	require.NoError(t, err)
	assert.NotEmpty(t, chunks)

	for _, c := range chunks {
		assert.NotEmpty(t, c.Content)
		assert.Greater(t, c.StartLine, 0)
		assert.GreaterOrEqual(t, c.EndLine, c.StartLine)
	}
}

func TestFallbackParser_UnsupportedLanguage(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	_, err := parser.ExtractSymbols("cobol", "some code")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported language")
}

func TestFallbackParser_EmptyCode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	symbols, err := parser.ExtractSymbols("go", "")
	require.NoError(t, err)
	assert.Empty(t, symbols)
}

func TestFallbackParser_ChunkByAST_EmptyCode(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	chunks, err := parser.ChunkByAST("go", "")
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

func TestFallbackParser_ChunkByAST_NoSymbols(t *testing.T) {
	logger := zaptest.NewLogger(t)
	parser := NewCGOParser(logger)

	code := `package main

// just a comment
var x = 1
`

	chunks, err := parser.ChunkByAST("go", code)
	require.NoError(t, err)
	// Should return a single file-level chunk as fallback
	assert.NotEmpty(t, chunks)
	assert.Equal(t, "<file>", chunks[0].SymbolName)
}
