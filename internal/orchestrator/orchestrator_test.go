package orchestrator

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// ─── Unit Tests for Orchestrator internals ───────────────────────────────────

func TestContainsSensitiveContent(t *testing.T) {
	cfg := testSecurityConfig()
	o := &Orchestrator{
		sensitiveRules: compileRules(cfg.SensitivePatterns),
		intentCache:    make(map[string]intentCacheEntry),
		approvalCh:     make(map[string]chan models.ApprovalResponse),
		logger:         zap.NewNop(),
	}

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"drop database detected", "please run DROP DATABASE users", true},
		{"kubectl apply detected", "kubectl apply -f deploy.yaml", true},
		{"safe query", "what does the auth service do?", false},
		{"case insensitive", "drop database Test", true},
		{"partial match no trigger", "I dropped my phone", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := o.containsSensitiveContent(tt.input)
			if got != tt.expected {
				t.Errorf("containsSensitiveContent(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestIntentCacheExpiry(t *testing.T) {
	o := &Orchestrator{
		intentCache: make(map[string]intentCacheEntry),
	}

	// Add a cache entry that expires in 1ms
	o.intentCache["sess1"] = intentCacheEntry{
		intent:    "code_query",
		expiresAt: time.Now().Add(-1 * time.Second), // Already expired
	}

	// Check that expired entry is not returned
	o.intentCacheMu.RLock()
	entry, ok := o.intentCache["sess1"]
	isValid := ok && time.Now().Before(entry.expiresAt)
	o.intentCacheMu.RUnlock()

	if isValid {
		t.Error("expired cache entry should not be valid")
	}

	// Add a valid entry
	o.intentCache["sess2"] = intentCacheEntry{
		intent:    "code_execute",
		expiresAt: time.Now().Add(5 * time.Minute),
	}

	o.intentCacheMu.RLock()
	entry2, ok2 := o.intentCache["sess2"]
	isValid2 := ok2 && time.Now().Before(entry2.expiresAt)
	o.intentCacheMu.RUnlock()

	if !isValid2 {
		t.Error("valid cache entry should be valid")
	}
	if entry2.intent != "code_execute" {
		t.Errorf("expected intent code_execute, got %s", entry2.intent)
	}
}

func TestPruneMessages(t *testing.T) {
	o := &Orchestrator{}

	tests := []struct {
		name      string
		msgCount  int
		maxTokens int
		wantMin   int // minimum messages after pruning
	}{
		{"few messages no prune", 2, 10000, 2},
		{"exactly 3 no prune", 3, 10000, 3},
		{"many messages pruned", 20, 100, 6}, // system + pruned-notice + 4 tail
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := make([]models.Message, tt.msgCount)
			msgs[0] = models.Message{Role: models.RoleSystem, Content: "system prompt"}
			for i := 1; i < tt.msgCount; i++ {
				msgs[i] = models.Message{Role: models.RoleUser, Content: "hello world message"}
			}

			result := o.pruneMessages(msgs, tt.maxTokens)
			if len(result) < tt.wantMin {
				t.Errorf("pruneMessages: got %d messages, want at least %d", len(result), tt.wantMin)
			}
			// First message should always be system
			if result[0].Role != models.RoleSystem {
				t.Errorf("first message should be system, got %s", result[0].Role)
			}
		})
	}
}

func TestBuildSystemMessage(t *testing.T) {
	o := &Orchestrator{}

	intents := []models.TaskIntent{
		models.IntentCodeQuery,
		models.IntentCodeExecute,
		models.IntentDiagnose,
		models.IntentMCPCall,
		models.IntentDeploy,
		models.IntentConversation,
		"unknown_intent",
	}

	for _, intent := range intents {
		t.Run(string(intent), func(t *testing.T) {
			msg := o.buildSystemMessage(intent)
			if msg.Role != models.RoleSystem {
				t.Errorf("expected system role, got %s", msg.Role)
			}
			if msg.Content == "" {
				t.Error("system message content should not be empty")
			}
		})
	}
}

func TestStatusLabel(t *testing.T) {
	if statusLabel(nil) != "success" {
		t.Error("nil error should return success")
	}
	if statusLabel(fmt.Errorf("fail")) != "error" {
		t.Error("non-nil error should return error")
	}
}

// ─── Test helpers ────────────────────────────────────────────────────────────

func testSecurityConfig() *config.SecurityConfig {
	return &config.SecurityConfig{
		SensitivePatterns: []string{
			`drop\s+database`,
			`kubectl\s+apply`,
			`rm\s+-rf\s+/`,
		},
	}
}

func compileRules(patterns []string) []*regexp.Regexp {
	var rules []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			continue
		}
		rules = append(rules, re)
	}
	return rules
}

// TestIntentCacheKey_IncludesMessage asserts that two different user messages
// in the same session produce different cache keys. The prior implementation
// keyed on sessionID alone, which let a benign first message ("show me the
// code") cache-poison a later dangerous one ("deploy to prod") into bypassing
// the HITL-gated deploy intent path.
func TestIntentCacheKey_IncludesMessage(t *testing.T) {
	const sess = "session-abc"

	benign := intentCacheKey(sess, "show me the authentication code")
	dangerous := intentCacheKey(sess, "deploy this to production")

	if benign == dangerous {
		t.Fatalf("cache keys must differ per message; both produced %q", benign)
	}
	// Same message should always produce the same key (determinism).
	if got := intentCacheKey(sess, "show me the authentication code"); got != benign {
		t.Errorf("non-deterministic key for same input: %q vs %q", benign, got)
	}
	// Different session must still segregate the cache.
	other := intentCacheKey("session-xyz", "show me the authentication code")
	if other == benign {
		t.Errorf("different sessions must yield different keys")
	}
}

// TestEvictIntentCacheLocked_BoundsGrowth verifies that the cache stays
// bounded under insertion pressure — before eviction was added, a long-running
// process could accumulate unbounded entries.
func TestEvictIntentCacheLocked_BoundsGrowth(t *testing.T) {
	o := &Orchestrator{
		intentCache: make(map[string]intentCacheEntry),
		logger:      zap.NewNop(),
	}
	// Seed above the cap with already-expired entries so the first pass of
	// eviction clears them out.
	past := time.Now().Add(-time.Hour)
	for i := 0; i < intentCacheMaxEntries+10; i++ {
		o.intentCache[fmt.Sprintf("s-%d", i)] = intentCacheEntry{
			intent:    models.IntentConversation,
			expiresAt: past,
		}
	}
	o.evictIntentCacheLocked()

	if len(o.intentCache) > intentCacheMaxEntries {
		t.Errorf("cache exceeded cap after eviction: got %d, want <= %d",
			len(o.intentCache), intentCacheMaxEntries)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// P7: classifyIntentByKeywords Tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestClassifyIntentByKeywords(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want models.TaskIntent
	}{
		// Deploy
		{"deploy english", "please deploy this to production", models.IntentDeploy},
		{"kubectl", "kubectl apply -f deploy.yaml", models.IntentDeploy},
		{"terraform", "run terraform plan", models.IntentDeploy},
		{"deploy chinese", "帮我部署到线上", models.IntentDeploy},

		// Code execute
		{"run command", "run the tests please", models.IntentCodeExecute},
		{"execute", "execute this script", models.IntentCodeExecute},
		{"chinese run", "运行一下这个程序", models.IntentCodeExecute},

		// Diagnose
		{"debug", "debug this crash", models.IntentDiagnose},
		{"check logs", "check logs for errors", models.IntentDiagnose},
		{"chinese diagnose", "帮我排查这个问题", models.IntentDiagnose},

		// MCP
		{"github issue", "create a github issue for this bug", models.IntentMCPCall},
		{"jira", "update the jira ticket", models.IntentMCPCall},

		// Code query
		{"how does", "how does the auth middleware work?", models.IntentCodeQuery},
		{"explain", "explain this function", models.IntentCodeQuery},
		{"chinese query", "这段代码是什么意思", models.IntentCodeQuery},

		// Ambiguous — should return empty (fall through to LLM)
		{"ambiguous", "fix the login bug", ""},
		{"generic", "hello", ""},
		{"vague", "make it better", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyIntentByKeywords(tt.msg)
			if got != tt.want {
				t.Errorf("classifyIntentByKeywords(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}
