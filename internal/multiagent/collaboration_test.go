package multiagent

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestConflictResolver_NoConflict(t *testing.T) {
	r := NewConflictResolver(StrategyLastWriter, zap.NewNop())

	edit1 := FileEdit{AgentID: "code-1", FilePath: "/src/main.go", Action: "edit", Timestamp: time.Now()}
	edit2 := FileEdit{AgentID: "code-1", FilePath: "/src/utils.go", Action: "edit", Timestamp: time.Now()}

	if c := r.RecordEdit(edit1); c != nil {
		t.Error("expected no conflict for first edit")
	}
	if c := r.RecordEdit(edit2); c != nil {
		t.Error("expected no conflict for different file")
	}
}

func TestConflictResolver_DetectsConflict(t *testing.T) {
	r := NewConflictResolver(StrategyLastWriter, zap.NewNop())

	edit1 := FileEdit{AgentID: "code-1", FilePath: "/src/main.go", Action: "edit", Timestamp: time.Now()}
	edit2 := FileEdit{AgentID: "test-1", FilePath: "/src/main.go", Action: "write", Timestamp: time.Now()}

	r.RecordEdit(edit1)
	conflict := r.RecordEdit(edit2)

	if conflict == nil {
		t.Fatal("expected conflict for same file from different agents")
	}
	if conflict.Type != ConflictConcurrentWrite {
		t.Errorf("expected concurrent_write, got %s", conflict.Type)
	}
}

func TestConflictResolver_ResolvesLastWriter(t *testing.T) {
	r := NewConflictResolver(StrategyLastWriter, zap.NewNop())

	edit1 := FileEdit{AgentID: "code-1", FilePath: "/src/main.go", Action: "edit", Timestamp: time.Now()}
	edit2 := FileEdit{AgentID: "test-1", FilePath: "/src/main.go", Action: "write", Timestamp: time.Now()}

	r.RecordEdit(edit1)
	conflict := r.RecordEdit(edit2)

	winner := r.Resolve(conflict)
	if winner.AgentID != "test-1" {
		t.Errorf("expected last writer (test-1) to win, got %s", winner.AgentID)
	}
	if !conflict.Resolved {
		t.Error("expected conflict to be marked resolved")
	}
}

func TestConflictResolver_ResolvesPriority(t *testing.T) {
	r := NewConflictResolver(StrategyPriority, zap.NewNop())

	edit1 := FileEdit{AgentID: "review-1", FilePath: "/src/main.go", Action: "edit", Timestamp: time.Now()}
	edit2 := FileEdit{AgentID: "code-2", FilePath: "/src/main.go", Action: "write", Timestamp: time.Now()}

	r.RecordEdit(edit1)
	conflict := r.RecordEdit(edit2)

	winner := r.Resolve(conflict)
	if winner.AgentID != "code-2" {
		t.Errorf("expected code agent to win by priority, got %s", winner.AgentID)
	}
}

func TestRoleSelector_SelectBest_NoHistory(t *testing.T) {
	rs := NewRoleSelector(zap.NewNop())

	// With no history, should use default affinity
	best := rs.SelectBest("write_file", []AgentType{AgentCode, AgentTest, AgentReview})
	if best != AgentCode {
		t.Errorf("expected AgentCode for write_file, got %s", best)
	}

	best = rs.SelectBest("run_tests", []AgentType{AgentCode, AgentTest, AgentReview})
	if best != AgentTest {
		t.Errorf("expected AgentTest for run_tests, got %s", best)
	}
}

func TestRoleSelector_SelectBest_WithHistory(t *testing.T) {
	rs := NewRoleSelector(zap.NewNop())

	// AgentReview has been very successful at read_file
	for range 10 {
		rs.RecordResult(AgentReview, true, 100*time.Millisecond)
	}
	// AgentCode has been failing
	for range 10 {
		rs.RecordResult(AgentCode, false, 500*time.Millisecond)
	}

	best := rs.SelectBest("read_file", []AgentType{AgentCode, AgentReview})
	if best != AgentReview {
		t.Errorf("expected AgentReview (high success rate), got %s", best)
	}
}

func TestRoleSelector_RecordResult(t *testing.T) {
	rs := NewRoleSelector(zap.NewNop())

	rs.RecordResult(AgentCode, true, 100*time.Millisecond)
	rs.RecordResult(AgentCode, true, 200*time.Millisecond)
	rs.RecordResult(AgentCode, false, 300*time.Millisecond)

	m := rs.GetMetrics(AgentCode)
	if m == nil {
		t.Fatal("expected non-nil metrics")
	}
	if m.TotalTasks != 3 {
		t.Errorf("expected 3 total tasks, got %d", m.TotalTasks)
	}
	if m.SuccessCount != 2 {
		t.Errorf("expected 2 successes, got %d", m.SuccessCount)
	}
}

func TestCandidatesForAction(t *testing.T) {
	candidates := CandidatesForAction("write_file")
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate for write_file")
	}
	found := false
	for _, c := range candidates {
		if c == AgentCode {
			found = true
		}
	}
	if !found {
		t.Error("expected AgentCode in candidates for write_file")
	}
}
