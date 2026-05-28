package rag

import (
	"context"
	"testing"

	"github.com/agent/code_agent/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestKeywordExpander(t *testing.T) {
	logger := zap.NewNop()
	expander := NewKeywordExpander(logger)

	tests := []struct {
		name     string
		query    string
		contains []string
	}{
		{
			name:     "camelCase expansion",
			query:    "getUserName",
			contains: []string{"getUserName", "get_user_name", "get-user-name"},
		},
		{
			name:     "snake_case expansion",
			query:    "get_user_name",
			contains: []string{"get_user_name", "getUserName", "get-user-name"},
		},
		{
			name:     "kebab-case expansion",
			query:    "get-user-name",
			contains: []string{"get-user-name", "get_user_name", "getUserName"},
		},
		{
			name:     "mixed query",
			query:    "how to getUserName from database",
			contains: []string{"getUserName", "get_user_name"},
		},
		{
			name:     "no expansion for short words",
			query:    "get id",
			contains: []string{"get", "id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := expander.Rewrite(context.Background(), tt.query)
			require.NoError(t, err)
			for _, word := range tt.contains {
				assert.Contains(t, result, word, "expanded query should contain %s", word)
			}
		})
	}
}

func TestHyDERewriter(t *testing.T) {
	logger := zap.NewNop()

	// Mock LLM function that returns a hypothetical code snippet
	mockLLM := func(ctx context.Context, messages []models.Message) (string, error) {
		return "func GetUserName(id int) string {\n    return db.Query(id).Name\n}", nil
	}

	rewriter := NewHyDERewriter(mockLLM, logger)

	query := "how to get user name from database"
	result, err := rewriter.Rewrite(context.Background(), query)
	require.NoError(t, err)
	assert.Contains(t, result, "GetUserName")
	assert.Contains(t, result, "db.Query")
}

func TestHyDERewriter_Fallback(t *testing.T) {
	logger := zap.NewNop()

	// Mock LLM function that fails
	mockLLM := func(ctx context.Context, messages []models.Message) (string, error) {
		return "", assert.AnError
	}

	rewriter := NewHyDERewriter(mockLLM, logger)

	query := "how to get user name"
	result, err := rewriter.Rewrite(context.Background(), query)
	require.NoError(t, err)
	assert.Equal(t, query, result, "should fallback to original query on LLM failure")
}

func TestCompositeRewriter(t *testing.T) {
	logger := zap.NewNop()

	// Mock LLM that returns a simple code snippet
	mockLLM := func(ctx context.Context, messages []models.Message) (string, error) {
		return "getUserName", nil
	}

	hyde := NewHyDERewriter(mockLLM, logger)
	expander := NewKeywordExpander(logger)

	composite := NewCompositeRewriter([]QueryRewriter{hyde, expander}, logger)

	query := "how to get user name"
	result, err := composite.Rewrite(context.Background(), query)
	require.NoError(t, err)

	// Should contain both HyDE output and its expansions
	assert.Contains(t, result, "getUserName")
	assert.Contains(t, result, "get_user_name")
}

func TestGenerateVariants(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{
			input:    "getUserName",
			expected: []string{"get_user_name", "get-user-name"},
		},
		{
			input:    "get_user_name",
			expected: []string{"getUserName", "get-user-name"},
		},
		{
			input:    "get-user-name",
			expected: []string{"get_user_name", "getUserName"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			variants := generateVariants(tt.input)
			for _, exp := range tt.expected {
				assert.Contains(t, variants, exp)
			}
		})
	}
}
