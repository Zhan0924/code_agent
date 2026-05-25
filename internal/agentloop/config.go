package agentloop

// Config controls the behavior of a ReAct loop Runner.
type Config struct {
	MaxSteps         int
	MaxContextTokens int
	EnableReflection bool
	LLMRetries       int
}

// DefaultConfig returns the standard orchestrator-level config.
func DefaultConfig() Config {
	return Config{
		MaxSteps:         50,
		MaxContextTokens: 128000,
		EnableReflection: true,
		LLMRetries:       3,
	}
}

// DefaultSubAgentConfig returns a lightweight config for sub-agent loops.
func DefaultSubAgentConfig() Config {
	return Config{
		MaxSteps:         8,
		MaxContextTokens: 32000,
		EnableReflection: false,
		LLMRetries:       2,
	}
}

// WithContextWindow returns a copy of the config with MaxContextTokens set
// to the given context window size (with a 5% safety margin).
func (c Config) WithContextWindow(contextWindow int) Config {
	if contextWindow > 0 {
		c.MaxContextTokens = contextWindow * 95 / 100
	}
	return c
}

// ContextBudget computes proportional token allocations for different prompt regions.
type ContextBudget struct {
	Total       int // Total available tokens (95% of context window)
	System      int // System prompt: 10%
	RAG         int // RAG context: 20%
	History     int // Conversation history: 60%
	CurrentMsg  int // Current message + response: 10%
}

// ComputeBudget returns proportional token allocations based on the total context window.
func ComputeBudget(contextWindow int) ContextBudget {
	total := contextWindow * 95 / 100 // 5% safety margin
	return ContextBudget{
		Total:      total,
		System:     total * 10 / 100,
		RAG:        total * 20 / 100,
		History:    total * 60 / 100,
		CurrentMsg: total * 10 / 100,
	}
}
