package llm

import (
	"testing"
)

// TestEstimateTokens pins expected behaviour of the rune-class estimator.
// The values here are not literal tiktoken counts — they are the
// deterministic output of the heuristic documented on EstimateTokens and
// are chosen to catch regressions if someone reverts to len(text)/4.
func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"empty", "", 0},
		{"short_word", "hello", 2}, // 5 ASCII letters → ceil(5/4)=2
		// English sentence: 36 letters/digits (ASCII word) + 8 spaces +
		// 1 trailing period. ceil((36+8)/4)=11 word tokens + 1 punct = 12.
		{"english_sentence", "The quick brown fox jumps over the lazy dog.", 12},
		// CJK: 12 runes, each non-ASCII → 12 tokens. Prior heuristic
		// returned 9 by dividing UTF-8 byte length by 4 (undercount by 25%).
		{"chinese", "你好世界测试一下中文字符", 12},
		// JSON-ish content: 5 alphanumerics (k,v,n,4,2) → ceil(5/4)=2
		// word tokens; 11 punctuation characters ({, ", :, ", ", ,, ", :,
		// ", }) each counted as one → total 13. Dense punct is the
		// whole point of tracking it separately.
		{"json_fragment", `{"k":"v","n":42}`, 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.input)
			if got != tt.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestEstimateTokens_CJKDenserThanASCII encodes the key invariant of the
// new heuristic: a string of N Chinese characters must cost noticeably
// more tokens than N ASCII letters. The prior len/4 heuristic inverted
// this in bytes-per-rune terms and was the root of the budget error.
func TestEstimateTokens_CJKDenserThanASCII(t *testing.T) {
	const n = 20
	asciiCount := EstimateTokens(stringRepeat("a", n))
	cjkCount := EstimateTokens(stringRepeat("你", n))
	if cjkCount <= asciiCount {
		t.Errorf("CJK must cost more tokens per rune than ASCII letters; got cjk=%d ascii=%d", cjkCount, asciiCount)
	}
}

// stringRepeat avoids importing "strings" just for Repeat.
func stringRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestBuildToolDefinitions(t *testing.T) {
	// Empty tools
	result := BuildToolDefinitions(nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d items", len(result))
	}
}
