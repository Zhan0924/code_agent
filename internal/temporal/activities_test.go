package temporal

import (
	"context"
	"strings"
	"testing"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

func TestClassifyIntent(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    models.TaskIntent
	}{
		{"deploy keyword", "deploy to production", models.IntentDeploy},
		{"chinese deploy", "部署到生产环境", models.IntentDeploy},
		{"release keyword", "release to prod", models.IntentDeploy},
		{"rollout keyword", "rollout the new version", models.IntentDeploy},
		{"conversation", "how does this work?", models.IntentConversation},
		{"normal question", "explain the code", models.IntentConversation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyIntent(tt.message)
			if got != tt.want {
				t.Errorf("classifyIntent(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}
}

func TestSecurityCheckActivity_SensitivePatterns(t *testing.T) {
	logger := zap.NewNop()
	secCfg := &config.SecurityConfig{
		SensitivePatterns: []string{
			`rm\s+-rf`,
			`DROP\s+TABLE`,
			`kubectl\s+delete`,
		},
	}

	activities := NewActivities(nil, secCfg, logger)

	tests := []struct {
		name             string
		message          string
		intent           models.TaskIntent
		wantApproval     bool
		wantRiskLevel    string
	}{
		{
			name:          "rm -rf command",
			message:       "run rm -rf /tmp/data",
			intent:        models.IntentCodeExecute,
			wantApproval:  true,
			wantRiskLevel: "high",
		},
		{
			name:          "DROP TABLE sql",
			message:       "execute DROP TABLE users",
			intent:        models.IntentCodeExecute,
			wantApproval:  true,
			wantRiskLevel: "high",
		},
		{
			name:          "kubectl delete",
			message:       "kubectl delete pod my-pod",
			intent:        models.IntentDeploy,
			wantApproval:  true,
			wantRiskLevel: "critical",
		},
		{
			name:          "safe conversation",
			message:       "what does this function do?",
			intent:        models.IntentConversation,
			wantApproval:  false,
			wantRiskLevel: "low",
		},
		{
			name:          "deploy intent",
			message:       "deploy the app",
			intent:        models.IntentDeploy,
			wantApproval:  true,
			wantRiskLevel: "critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := activities.SecurityCheckActivity(context.Background(), SecurityCheckInput{
				TaskID:      "test-task",
				UserMessage: tt.message,
				Intent:      tt.intent,
			})
			if err != nil {
				t.Fatalf("SecurityCheckActivity: %v", err)
			}
			if result.RequiresApproval != tt.wantApproval {
				t.Errorf("RequiresApproval = %v, want %v", result.RequiresApproval, tt.wantApproval)
			}
			if result.RiskLevel != tt.wantRiskLevel {
				t.Errorf("RiskLevel = %q, want %q", result.RiskLevel, tt.wantRiskLevel)
			}
		})
	}
}

func TestParseIntentActivity_Fallback(t *testing.T) {
	logger := zap.NewNop()
	secCfg := &config.SecurityConfig{}
	activities := NewActivities(nil, secCfg, logger)

	result, err := activities.ParseIntentActivity(context.Background(), AgentTaskInput{
		TaskID:      "test-1",
		UserMessage: "deploy to production",
	})
	if err != nil {
		t.Fatalf("ParseIntentActivity: %v", err)
	}
	if result.Intent != models.IntentDeploy {
		t.Errorf("Intent = %v, want %v", result.Intent, models.IntentDeploy)
	}
}

func TestNewActivities_InvalidPattern(t *testing.T) {
	logger := zap.NewNop()
	secCfg := &config.SecurityConfig{
		SensitivePatterns: []string{
			`valid\s+pattern`,
			`[invalid(`,
			`another\s+valid`,
		},
	}

	activities := NewActivities(nil, secCfg, logger)

	if len(activities.sensitivePatterns) != 2 {
		t.Errorf("expected 2 valid patterns, got %d", len(activities.sensitivePatterns))
	}
}

func TestSecurityCheckActivity_CaseInsensitive(t *testing.T) {
	logger := zap.NewNop()
	secCfg := &config.SecurityConfig{
		SensitivePatterns: []string{`DROP\s+TABLE`},
	}

	activities := NewActivities(nil, secCfg, logger)

	result, err := activities.SecurityCheckActivity(context.Background(), SecurityCheckInput{
		TaskID:      "test",
		UserMessage: "drop table users",
		Intent:      models.IntentCodeExecute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RequiresApproval {
		t.Error("expected approval required for case-insensitive match")
	}
	if !strings.Contains(result.Reason, "sensitive pattern") {
		t.Errorf("expected reason to mention sensitive pattern, got: %s", result.Reason)
	}
}
