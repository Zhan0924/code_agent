package orchestrator

import (
	"github.com/agent/code_agent/internal/agentloop"
)

// MetacognitiveState is an alias to the shared agentloop implementation.
type MetacognitiveState = agentloop.MetacognitiveState

// NewMetacognitiveState creates a fresh metacognitive tracker.
var NewMetacognitiveState = agentloop.NewMetacognitiveState
