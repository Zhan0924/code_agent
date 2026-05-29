package main

import (
	"github.com/agent/code_agent/internal/rag"
	"github.com/agent/code_agent/internal/treesitter"
)

// tsParserAdapter adapts treesitter.Parser to rag.TreeSitterParser interface.
type tsParserAdapter struct {
	parser treesitter.Parser
}

func (a *tsParserAdapter) ChunkByAST(language, content string) ([]rag.TreeSitterChunk, error) {
	chunks, err := a.parser.ChunkByAST(language, content)
	if err != nil {
		return nil, err
	}
	result := make([]rag.TreeSitterChunk, len(chunks))
	for i, c := range chunks {
		result[i] = rag.TreeSitterChunk{
			SymbolName:   c.SymbolName,
			SymbolType:   c.SymbolType,
			Content:      c.Content,
			StartLine:    c.StartLine,
			EndLine:      c.EndLine,
			Dependencies: c.Dependencies,
			Signature:    c.Signature,
		}
	}
	return result, nil
}

func (a *tsParserAdapter) SupportedLanguages() []string {
	return a.parser.SupportedLanguages()
}
