package orchestrator

import (
	"context"
	"testing"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

func TestParseVerificationResponse_Valid(t *testing.T) {
	json := `{
		"passed": true,
		"score": 0.85,
		"issues": [],
		"suggestions": ["Consider adding error handling"],
		"reasoning": "Response is complete and correct"
	}`

	result, err := parseVerificationResponse(json)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Passed {
		t.Error("expected passed=true")
	}
	if result.Score != 0.85 {
		t.Errorf("expected score 0.85, got %f", result.Score)
	}
	if len(result.Suggestions) != 1 {
		t.Errorf("expected 1 suggestion, got %d", len(result.Suggestions))
	}
}

func TestParseVerificationResponse_WithCodeFence(t *testing.T) {
	json := "```json\n" + `{"passed": false, "score": 0.4, "issues": ["incomplete"], "suggestions": [], "reasoning": "missing key parts"}` + "\n```"

	result, err := parseVerificationResponse(json)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Passed {
		t.Error("expected passed=false")
	}
	if result.Score != 0.4 {
		t.Errorf("expected score 0.4, got %f", result.Score)
	}
}

func TestParseVerificationResponse_ClampsScore(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{`{"passed": true, "score": -0.5, "issues": [], "suggestions": [], "reasoning": ""}`, 0.0},
		{`{"passed": true, "score": 1.5, "issues": [], "suggestions": [], "reasoning": ""}`, 1.0},
	}

	for _, tt := range tests {
		result, err := parseVerificationResponse(tt.input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Score != tt.want {
			t.Errorf("expected score %f, got %f", tt.want, result.Score)
		}
	}
}

func TestShouldVerifyOutput(t *testing.T) {
	o := &Orchestrator{}

	tests := []struct {
		intent    models.TaskIntent
		stepsUsed int
		want      bool
	}{
		{models.IntentDeploy, 1, true},        // high-stakes always
		{models.IntentCodeExecute, 2, true},   // high-stakes always
		{models.IntentDiagnose, 3, true},      // high-stakes always
		{models.IntentCodeQuery, 10, true},    // many steps
		{models.IntentCodeQuery, 3, false},    // simple query, few steps
		{models.IntentConversation, 2, false}, // simple ops
	}

	for _, tt := range tests {
		got := o.shouldVerifyOutput(tt.intent, tt.stepsUsed)
		if got != tt.want {
			t.Errorf("shouldVerifyOutput(%v, %d) = %v, want %v", tt.intent, tt.stepsUsed, got, tt.want)
		}
	}
}

func TestFormatVerificationFeedback_Passed(t *testing.T) {
	result := &VerificationResult{
		Passed:      true,
		Score:       0.9,
		Issues:      []string{},
		Suggestions: []string{"Consider adding tests"},
		Reasoning:   "Complete and correct",
	}

	feedback := formatVerificationFeedback(result)

	if !contains(feedback, "✅") {
		t.Error("expected checkmark for passed")
	}
	if !contains(feedback, "90%") {
		t.Error("expected score percentage")
	}
	if !contains(feedback, "Consider adding tests") {
		t.Error("expected suggestion in feedback")
	}
}

func TestFormatVerificationFeedback_Failed(t *testing.T) {
	result := &VerificationResult{
		Passed:      false,
		Score:       0.5,
		Issues:      []string{"Missing error handling", "Incomplete implementation"},
		Suggestions: []string{"Add try-catch"},
		Reasoning:   "Several gaps",
	}

	feedback := formatVerificationFeedback(result)

	if !contains(feedback, "⚠️") {
		t.Error("expected warning for failed")
	}
	if !contains(feedback, "Missing error handling") {
		t.Error("expected issue in feedback")
	}
	if !contains(feedback, "address the issues") {
		t.Error("expected remediation instruction")
	}
}

// mockVerifierLLM implements agentloop.LLMCaller for testing.
type mockVerifierLLM struct {
	response string
	err      error
}

var _ agentloop.LLMCaller = (*mockVerifierLLM)(nil)

func (m *mockVerifierLLM) ChatCompletion(_ context.Context, _ *llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &llm.ChatResponse{Content: m.response}, nil
}

// ─── P2: retry-once gate ─────────────────────────────────────────────────────

// TestDecideVerificationFollowup_RetryOnLowScore covers the happy retry path:
// the first failed verification (retried=false, plenty of step budget) flips
// the retry switch on and reports `retrying: true` in the SSE payload so the
// front-end can show "Reviewing your answer…" instead of going silent.
func TestDecideVerificationFollowup_RetryOnLowScore(t *testing.T) {
	vResult := &VerificationResult{
		Score:     0.2,
		Issues:    []string{"No actual code provided", "non-actionable summary"},
		Reasoning: "The agent described the work but never produced files.",
	}

	payload, retry := decideVerificationFollowup(false, 5, 200, vResult)

	if !retry {
		t.Fatal("expected retry=true on first verification failure")
	}
	if !payload.Retrying {
		t.Error("payload.Retrying must mirror retry decision")
	}
	if payload.Score != 0.2 {
		t.Errorf("payload.Score=%v, want 0.2", payload.Score)
	}
	if len(payload.Issues) != 2 {
		t.Errorf("payload.Issues lost data: %v", payload.Issues)
	}
	if payload.Reasoning == "" {
		t.Error("payload.Reasoning dropped")
	}
}

// TestDecideVerificationFollowup_NoDoubleRetry — second failure on the same
// task must NOT retry again. The orchestrator sets task.VerificationRetried
// on the first retry, and this helper is the only authority that decides
// whether to re-enter the ReAct loop, so the invariant lives here.
func TestDecideVerificationFollowup_NoDoubleRetry(t *testing.T) {
	vResult := &VerificationResult{Score: 0.2, Issues: []string{"still bad"}}

	payload, retry := decideVerificationFollowup(true /* already retried */, 5, 200, vResult)

	if retry {
		t.Fatal("expected retry=false after one prior retry — verifier loop must terminate")
	}
	if payload.Retrying {
		t.Error("payload.Retrying must be false when the gate refuses retry")
	}
}

// TestDecideVerificationFollowup_StepBudgetGuard — even on the first failure,
// if the loop is within 3 steps of the absolute cap, retrying would land on
// the step-exhausted exit path and produce nothing useful. The gate must
// refuse retry in that case.
func TestDecideVerificationFollowup_StepBudgetGuard(t *testing.T) {
	vResult := &VerificationResult{Score: 0.2}

	tests := []struct {
		name         string
		retried      bool
		globalStep   int
		absoluteMax  int
		expectRetry  bool
	}{
		{"plenty of room", false, 10, 200, true},
		{"exactly at guard line", false, 197, 200, false}, // 200-3=197, NOT <
		{"one step past guard", false, 198, 200, false},
		{"at absolute cap", false, 200, 200, false},
		{"retried + plenty of room", true, 10, 200, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, retry := decideVerificationFollowup(tt.retried, tt.globalStep, tt.absoluteMax, vResult)
			if retry != tt.expectRetry {
				t.Errorf("retry = %v, want %v", retry, tt.expectRetry)
			}
		})
	}
}

func TestVerifyOutput_Integration(t *testing.T) {
	mock := &mockVerifierLLM{
		response: `{"passed": true, "score": 0.85, "issues": [], "suggestions": [], "reasoning": "Good response"}`,
	}

	result, err := verifyOutputWithLLM(context.Background(), mock, "Fix the bug", "I fixed the bug by changing line 42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Passed {
		t.Error("expected passed=true")
	}
	if result.Score != 0.85 {
		t.Errorf("expected score 0.85, got %f", result.Score)
	}
}
