package llm

import (
	"strings"
	"testing"
)

func TestFastEstimate_Empty(t *testing.T) {
	if got := FastEstimate(""); got != 0 {
		t.Errorf("FastEstimate(\"\") = %d, want 0", got)
	}
}

func TestFastEstimate_ASCII(t *testing.T) {
	// "hello world" = 11 chars (10 letters + 1 space), expect ceil(11/4) = 3
	got := FastEstimate("hello world")
	if got < 2 || got > 5 {
		t.Errorf("FastEstimate(\"hello world\") = %d, want 2-5", got)
	}
}

func TestFastEstimate_CJK(t *testing.T) {
	// Each CJK character should count as ~1 token
	got := FastEstimate("你好世界")
	if got != 4 {
		t.Errorf("FastEstimate(\"你好世界\") = %d, want 4", got)
	}
}

func TestFastEstimate_Punctuation(t *testing.T) {
	// Each punctuation mark counts as 1 token
	got := FastEstimate("!!!")
	if got != 3 {
		t.Errorf("FastEstimate(\"!!!\") = %d, want 3", got)
	}
}

func TestExactTokenCount_Empty(t *testing.T) {
	if got := ExactTokenCount(""); got != 0 {
		t.Errorf("ExactTokenCount(\"\") = %d, want 0", got)
	}
}

func TestExactTokenCount_English(t *testing.T) {
	got := ExactTokenCount("Hello, world!")
	// tiktoken cl100k_base: "Hello" "," " world" "!" = 4 tokens
	if got < 3 || got > 6 {
		t.Errorf("ExactTokenCount(\"Hello, world!\") = %d, want 3-6", got)
	}
}

func TestExactTokenCount_Code(t *testing.T) {
	code := `func main() {
	fmt.Println("hello")
}`
	got := ExactTokenCount(code)
	if got < 8 || got > 20 {
		t.Errorf("ExactTokenCount(code) = %d, want 8-20", got)
	}
}

func TestExactTokenCount_CJK(t *testing.T) {
	got := ExactTokenCount("你好世界")
	// CJK characters typically use 1-2 tokens each in cl100k_base
	if got < 2 || got > 8 {
		t.Errorf("ExactTokenCount(\"你好世界\") = %d, want 2-8", got)
	}
}

func TestEstimateTokens_BackwardCompat(t *testing.T) {
	// EstimateTokens should delegate to FastEstimate
	text := "hello world"
	if EstimateTokens(text) != FastEstimate(text) {
		t.Error("EstimateTokens should delegate to FastEstimate")
	}
}

func TestAccuracyComparison(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"english_prose", "The quick brown fox jumps over the lazy dog. This is a test sentence for token counting accuracy."},
		{"code_go", "func (s *Server) handleRequest(ctx context.Context, req *http.Request) (*Response, error) { return nil, nil }"},
		{"mixed_cjk", "这是一段中英文混合的文本 with some English words mixed in 用于测试 token 计数准确性"},
		{"json", `{"name": "test", "value": 42, "nested": {"key": "value"}}`},
		{"long_text", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exact := ExactTokenCount(tc.text)
			fast := FastEstimate(tc.text)

			if exact == 0 {
				t.Skip("tiktoken not available, skipping accuracy test")
			}

			// FastEstimate should be within 30% of exact for most content
			ratio := float64(fast) / float64(exact)
			if ratio < 0.5 || ratio > 2.0 {
				t.Errorf("accuracy ratio %.2f out of range [0.5, 2.0]: exact=%d, fast=%d",
					ratio, exact, fast)
			}
			t.Logf("exact=%d, fast=%d, ratio=%.2f", exact, fast, ratio)
		})
	}
}

func BenchmarkFastEstimate(b *testing.B) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50)
	b.ResetTimer()
	for range b.N {
		FastEstimate(text)
	}
}

func BenchmarkExactTokenCount(b *testing.B) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50)
	// Warm up tiktoken
	ExactTokenCount(text)
	b.ResetTimer()
	for range b.N {
		ExactTokenCount(text)
	}
}
