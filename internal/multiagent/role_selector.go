package multiagent

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// AgentMetrics tracks performance history for a specific agent type.
type AgentMetrics struct {
	TotalTasks     int           `json:"total_tasks"`
	SuccessCount   int           `json:"success_count"`
	FailureCount   int           `json:"failure_count"`
	AvgDuration    time.Duration `json:"avg_duration"`
	LastUsed       time.Time     `json:"last_used"`
	SuccessRate    float64       `json:"success_rate"`
	RecentResults  []bool        // sliding window of last 10 results
}

// RoleSelector dynamically assigns agent types to tasks based on
// task characteristics and historical agent performance.
type RoleSelector struct {
	mu      sync.RWMutex
	metrics map[AgentType]*AgentMetrics
	logger  *zap.Logger
}

// NewRoleSelector creates a role selector with empty metrics.
func NewRoleSelector(logger *zap.Logger) *RoleSelector {
	return &RoleSelector{
		metrics: make(map[AgentType]*AgentMetrics),
		logger:  logger.With(zap.String("component", "multiagent.role_selector")),
	}
}

// RecordResult updates metrics for an agent type after task completion.
func (rs *RoleSelector) RecordResult(agentType AgentType, success bool, duration time.Duration) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	m, ok := rs.metrics[agentType]
	if !ok {
		m = &AgentMetrics{RecentResults: make([]bool, 0, 10)}
		rs.metrics[agentType] = m
	}

	m.TotalTasks++
	if success {
		m.SuccessCount++
	} else {
		m.FailureCount++
	}
	m.LastUsed = time.Now()

	// Update average duration
	m.AvgDuration = time.Duration(
		(int64(m.AvgDuration)*int64(m.TotalTasks-1) + int64(duration)) / int64(m.TotalTasks),
	)

	// Sliding window
	m.RecentResults = append(m.RecentResults, success)
	if len(m.RecentResults) > 10 {
		m.RecentResults = m.RecentResults[len(m.RecentResults)-10:]
	}

	// Compute success rate from recent window
	successes := 0
	for _, r := range m.RecentResults {
		if r {
			successes++
		}
	}
	m.SuccessRate = float64(successes) / float64(len(m.RecentResults))
}

// SelectBest picks the best agent type for a given task action,
// considering both the default classification and performance history.
func (rs *RoleSelector) SelectBest(action string, candidates []AgentType) AgentType {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if len(candidates) == 0 {
		return AgentCode // fallback
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	var bestType AgentType
	var bestScore float64 = -1

	for _, candidate := range candidates {
		score := rs.scoreCandidate(candidate, action)
		if score > bestScore {
			bestScore = score
			bestType = candidate
		}
	}

	return bestType
}

func (rs *RoleSelector) scoreCandidate(agentType AgentType, action string) float64 {
	m, ok := rs.metrics[agentType]
	if !ok {
		// No history: use default affinity score
		return defaultAffinity(agentType, action)
	}

	// Weighted score: 60% success rate + 20% affinity + 20% recency
	affinityScore := defaultAffinity(agentType, action)
	recencyScore := rs.recencyScore(m.LastUsed)

	return m.SuccessRate*0.6 + affinityScore*0.2 + recencyScore*0.2
}

func (rs *RoleSelector) recencyScore(lastUsed time.Time) float64 {
	elapsed := time.Since(lastUsed)
	if elapsed < 1*time.Minute {
		return 1.0
	}
	if elapsed < 5*time.Minute {
		return 0.8
	}
	if elapsed < 30*time.Minute {
		return 0.5
	}
	return 0.2
}

// defaultAffinity returns how well-suited an agent type is for a given action.
func defaultAffinity(agentType AgentType, action string) float64 {
	affinities := map[AgentType]map[string]float64{
		AgentCode: {
			"write_file": 1.0, "edit_file": 1.0, "patch_file": 1.0,
			"create_directory": 0.9, "git_commit": 0.9,
			"read_file": 0.5, "run_tests": 0.3,
		},
		AgentTest: {
			"run_tests": 1.0, "execute_code": 1.0,
			"read_file": 0.6, "write_file": 0.3,
		},
		AgentReview: {
			"read_file": 1.0, "search_code": 1.0, "list_files": 0.9,
			"git_diff": 0.9, "git_log": 0.8,
		},
	}

	if typeAffinities, ok := affinities[agentType]; ok {
		if score, exists := typeAffinities[action]; exists {
			return score
		}
	}
	return 0.3 // default low affinity for unknown combinations
}

// GetMetrics returns a copy of metrics for a given agent type.
func (rs *RoleSelector) GetMetrics(agentType AgentType) *AgentMetrics {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	m, ok := rs.metrics[agentType]
	if !ok {
		return nil
	}
	copied := *m
	return &copied
}

// CandidatesForAction returns agent types that have non-zero affinity for an action.
func CandidatesForAction(action string) []AgentType {
	all := []AgentType{AgentCode, AgentTest, AgentReview}
	var candidates []AgentType
	for _, t := range all {
		if defaultAffinity(t, action) > 0.3 {
			candidates = append(candidates, t)
		}
	}
	if len(candidates) == 0 {
		return []AgentType{AgentCode}
	}
	return candidates
}
