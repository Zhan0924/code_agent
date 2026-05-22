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
