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
