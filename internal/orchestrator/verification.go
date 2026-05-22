package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent/code_agent/internal/agentloop"
	"github.com/agent/code_agent/internal/llm"
	"github.com/agent/code_agent/internal/models"
)

// VerificationResult holds the outcome of output verification.
type VerificationResult struct {
	Passed      bool     `json:"passed"`
	Score       float64  `json:"score"`        // 0.0-1.0
	Issues      []string `json:"issues"`       // Problems found
	Suggestions []string `json:"suggestions"`  // Improvement recommendations
	Reasoning   string   `json:"reasoning"`    // Why this score
}

// verifyOutput uses LLM to evaluate if the agent's output satisfies the user's request.
func (o *Orchestrator) verifyOutput(ctx context.Context, userRequest, agentOutput string) (*VerificationResult, error) {
	return verifyOutputWithLLM(ctx, o.llmClient, userRequest, agentOutput)
}

// verifyOutputWithLLM is the testable implementation that accepts any LLMCaller.
func verifyOutputWithLLM(ctx context.Context, caller agentloop.LLMCaller, userRequest, agentOutput string) (*VerificationResult, error) {
	prompt := buildVerificationPrompt(userRequest, agentOutput)

	resp, err := caller.ChatCompletion(ctx, &llm.ChatRequest{
		Messages: []models.Message{
			{Role: models.RoleUser, Content: prompt},
		},
		Temperature: 0.1,
		MaxTokens:   1024,
	})
	if err != nil {
		return nil, fmt.Errorf("verification LLM call: %w", err)
	}

	return parseVerificationResponse(resp.Content)
}

const verificationPromptTemplate = `You are an output quality evaluator. Assess whether the agent's response adequately addresses the user's request.

User's original request:
%s

Agent's response:
%s

Evaluate on these criteria:
1. **Completeness**: Does it address all parts of the request?
2. **Correctness**: Is the information/code accurate?
3. **Clarity**: Is it well-explained and understandable?
4. **Actionability**: Can the user act on this response?

Respond with ONLY a JSON object:
{
  "passed": true/false,
  "score": 0.0-1.0,
  "issues": ["issue1", "issue2"],
  "suggestions": ["suggestion1"],
  "reasoning": "brief explanation"
}

Score guide:
- 0.9-1.0: Excellent, fully addresses request
- 0.7-0.8: Good, minor gaps
- 0.5-0.6: Acceptable, some issues
- 0.3-0.4: Poor, significant problems
- 0.0-0.2: Fails to address request

Set "passed" to true if score >= 0.7.`

func buildVerificationPrompt(userRequest, agentOutput string) string {
	// Truncate to avoid excessive token usage
	userTrunc := truncateText(userRequest, 1500)
	outputTrunc := truncateText(agentOutput, 3000)

	return fmt.Sprintf(verificationPromptTemplate, userTrunc, outputTrunc)
}

func parseVerificationResponse(content string) (*VerificationResult, error) {
	content = strings.TrimSpace(content)
	// Strip markdown code fences
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(content, "\n")
		if len(lines) > 2 {
			content = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var result VerificationResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("parse verification JSON: %w", err)
	}

	// Clamp score
	if result.Score < 0 {
		result.Score = 0
	}
	if result.Score > 1 {
		result.Score = 1
	}

	return &result, nil
}

func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// shouldVerifyOutput decides if verification is needed based on task complexity.
func (o *Orchestrator) shouldVerifyOutput(intent models.TaskIntent, stepsUsed int) bool {
	// Always verify for high-stakes intents
	switch intent {
	case models.IntentDeploy, models.IntentCodeExecute, models.IntentDiagnose:
		return true
	}

	// Verify if the task took significant effort (>5 steps)
	if stepsUsed > 5 {
		return true
	}

	return false
}

// formatVerificationFeedback creates a system message from verification results.
func formatVerificationFeedback(result *VerificationResult) string {
	var parts []string

	parts = append(parts, fmt.Sprintf("[OUTPUT VERIFICATION — Score: %.0f%%]", result.Score*100))

	if result.Passed {
		parts = append(parts, "✅ Output quality check passed.")
	} else {
		parts = append(parts, "⚠️ Output quality check failed.")
	}

	if len(result.Issues) > 0 {
		parts = append(parts, "\nIssues found:")
		for _, issue := range result.Issues {
			parts = append(parts, "- "+issue)
		}
	}

	if len(result.Suggestions) > 0 {
		parts = append(parts, "\nSuggestions:")
		for _, sug := range result.Suggestions {
			parts = append(parts, "- "+sug)
		}
	}

	if result.Reasoning != "" {
		parts = append(parts, "\nReasoning: "+result.Reasoning)
	}

	if !result.Passed {
		parts = append(parts, "\nPlease address the issues above before finalizing your response.")
	}

	return strings.Join(parts, "\n")
}
