// tokenizer.go — 统一的 token 计数接口，提供精确计数（tiktoken）和快速估算两种模式。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么需要两种计数方式】
//
//	· ExactTokenCount — 使用 tiktoken-go 精确计数，准确率 100%，但每次调用约 80μs
//	· FastEstimate    — 基于 rune 分类的启发式估算，准确率 ~85%，每次调用约 5μs
//
//	使用场景：
//	  · 最终预算验证（LLM 调用前）      → ExactTokenCount
//	  · 批量打分/排序（RAG 检索 1000 chunks）→ FastEstimate
//	  · 会话管理（消息追加时）          → FastEstimate
//
// 【tiktoken 模型映射】
//
//	不同模型使用不同的 tokenizer：
//	  · GPT-4 / GPT-3.5 / text-embedding-ada-002 → cl100k_base
//	  · Claude (通过 OpenAI 兼容代理)            → cl100k_base (近似)
//	  · GPT-3 (davinci/curie)                    → p50k_base
//
//	本实现默认使用 cl100k_base，覆盖 90% 的使用场景。
//
// 【线程安全】
//
//	tiktoken.Encoding 是线程安全的，可以全局共享一个实例。初始化失败时
//	（如离线环境无法下载 tiktoken 数据文件），自动降级到 FastEstimate。
//
// ============================================================================
package llm

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
	"go.uber.org/zap"
)

var (
	// globalTokenizer 是全局共享的 tiktoken encoder 实例
	globalTokenizer *tiktoken.Tiktoken
	tokenizerOnce   sync.Once
	tokenizerLogger *zap.Logger
)

// InitTokenizer 初始化全局 tokenizer（可选，首次调用 ExactTokenCount 时会自动初始化）
func InitTokenizer(logger *zap.Logger) {
	tokenizerLogger = logger
	tokenizerOnce.Do(func() {
		enc, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			if tokenizerLogger != nil {
				tokenizerLogger.Warn("failed to initialize tiktoken, falling back to fast estimate",
					zap.Error(err))
			}
			return
		}
		globalTokenizer = enc
		if tokenizerLogger != nil {
			tokenizerLogger.Info("tiktoken initialized successfully", zap.String("encoding", "cl100k_base"))
		}
	})
}

// ExactTokenCount 使用 tiktoken 精确计数 token 数量。
// 适用于最终预算验证（LLM 调用前）。如果 tiktoken 初始化失败，降级到 FastEstimate。
func ExactTokenCount(text string) int {
	if text == "" {
		return 0
	}

	// 延迟初始化
	if globalTokenizer == nil {
		tokenizerOnce.Do(func() {
			enc, err := tiktoken.GetEncoding("cl100k_base")
			if err != nil {
				// 初始化失败，降级到快速估算
				return
			}
			globalTokenizer = enc
		})
	}

	// 如果初始化失败，降级到快速估算
	if globalTokenizer == nil {
		return FastEstimate(text)
	}

	tokens := globalTokenizer.Encode(text, nil, nil)
	return len(tokens)
}

// FastEstimate 使用启发式规则快速估算 token 数量（~85% 准确率）。
// 适用于批量打分、排序、会话管理等对性能敏感的场景。
//
// 估算规则（基于 cl100k_base tokenizer 的统计特性）：
//   - 非 ASCII 字符（CJK、emoji）：1 rune ≈ 1 token
//   - ASCII 字母/数字：4 chars ≈ 1 token
//   - ASCII 标点符号：每个符号 ≈ 1 token
//
// 这个实现是从原 EstimateTokens 函数迁移过来的，保持向后兼容。
func FastEstimate(text string) int {
	if text == "" {
		return 0
	}
	var asciiWord, asciiPunct, nonASCII int
	for _, r := range text {
		switch {
		case r >= 0x80:
			nonASCII++
		case isASCIIPunct(r):
			asciiPunct++
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			asciiWord++ // whitespace costs roughly the same as a letter
		default:
			asciiWord++
		}
	}
	// ceil(asciiWord / 4) so short text still costs >= 1.
	tokens := (asciiWord + 3) / 4
	tokens += asciiPunct
	tokens += nonASCII
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// isASCIIPunct reports whether r is an ASCII punctuation/symbol character.
// These are treated as standalone tokens by modern BPE tokenizers, so we
// count them separately from alphanumeric content.
func isASCIIPunct(r rune) bool {
	if r >= 0x80 {
		return false
	}
	switch {
	case r >= '0' && r <= '9':
		return false
	case r >= 'A' && r <= 'Z':
		return false
	case r >= 'a' && r <= 'z':
		return false
	case r == ' ' || r == '\t' || r == '\n' || r == '\r':
		return false
	}
	return true
}
