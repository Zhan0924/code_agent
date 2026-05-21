package multiagent

import (
	"sync"

	"go.uber.org/zap"
)

// AgentPool manages a bounded pool of sub-agents with channel-based semaphore.
type AgentPool struct {
	sem    chan struct{}
	mu     sync.Mutex
	agents map[AgentType][]*SubAgent
	logger *zap.Logger
}

// NewAgentPool creates a pool with the given concurrency limit.
func NewAgentPool(maxParallel int, logger *zap.Logger) *AgentPool {
	if maxParallel <= 0 {
		maxParallel = 3
	}
	return &AgentPool{
		sem:    make(chan struct{}, maxParallel),
		agents: make(map[AgentType][]*SubAgent),
		logger: logger.With(zap.String("component", "multiagent.pool")),
	}
}

// Acquire gets or creates a sub-agent of the given type, blocking if at capacity.
func (p *AgentPool) Acquire(agentType AgentType) *SubAgent {
	p.sem <- struct{}{}

	p.mu.Lock()
	defer p.mu.Unlock()

	pool := p.agents[agentType]
	if len(pool) > 0 {
		agent := pool[len(pool)-1]
		p.agents[agentType] = pool[:len(pool)-1]
		return agent
	}

	return NewSubAgent(agentType, p.logger)
}

// Release returns a sub-agent to the pool.
func (p *AgentPool) Release(agent *SubAgent) {
	p.mu.Lock()
	p.agents[agent.Type] = append(p.agents[agent.Type], agent)
	p.mu.Unlock()

	<-p.sem
}

// Size returns the current number of idle agents in the pool.
func (p *AgentPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, agents := range p.agents {
		total += len(agents)
	}
	return total
}
