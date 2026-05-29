package main

import (
	"github.com/agent/code_agent/internal/repomap"
	"github.com/agent/code_agent/internal/treesitter"
)

// tsRepomapAdapter adapts treesitter.Parser to repomap.TreeSitterParser interface.
type tsRepomapAdapter struct {
	parser treesitter.Parser
}

func (a *tsRepomapAdapter) ExtractSymbols(language, content string) ([]repomap.TreeSitterSymbol, error) {
	symbols, err := a.parser.ExtractSymbols(language, content)
	if err != nil {
		return nil, err
	}
	result := make([]repomap.TreeSitterSymbol, len(symbols))
	for i, s := range symbols {
		result[i] = repomap.TreeSitterSymbol{
			Name:       s.Name,
			Kind:       s.Kind,
			StartLine:  s.StartLine,
			Visibility: s.Visibility,
		}
	}
	return result, nil
}

func (a *tsRepomapAdapter) SupportedLanguages() []string {
	return a.parser.SupportedLanguages()
}
