package rag

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
)

// slowEmbedder 模拟真实 API 80ms 级延迟，用于展示缓存的加速效果。
type slowEmbedder struct {
	delay time.Duration
}

func (s *slowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	time.Sleep(s.delay)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{0.1, 0.2, 0.3}
	}
	return out, nil
}

// BenchmarkEmbed_NoCache：直接调底层 embedder，模拟优化前。
func BenchmarkEmbed_NoCache(b *testing.B) {
	inner := &slowEmbedder{delay: time.Millisecond} // 1ms，方便测试机可跑
	ctx := context.Background()
	texts := fixedTexts(50)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = inner.Embed(ctx, texts)
	}
}

// BenchmarkEmbed_WithCache_AllHit：稳态全缓存命中，证明 P0-2 的延迟收益。
func BenchmarkEmbed_WithCache_AllHit(b *testing.B) {
	inner := &slowEmbedder{delay: time.Millisecond}
	cache := NewMemoryEmbeddingCache(1000, "m", zap.NewNop())
	ce := NewCachedEmbedder(inner, cache, zap.NewNop())
	ctx := context.Background()
	texts := fixedTexts(50)
	// 预热：第一次全部 miss，之后全 hit
	_, _ = ce.Embed(ctx, texts)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = ce.Embed(ctx, texts)
	}
}

// BenchmarkEmbed_WithCache_HalfHit：50% 命中率（现实增量索引场景）。
func BenchmarkEmbed_WithCache_HalfHit(b *testing.B) {
	inner := &slowEmbedder{delay: time.Millisecond}
	cache := NewMemoryEmbeddingCache(1000, "m", zap.NewNop())
	ce := NewCachedEmbedder(inner, cache, zap.NewNop())
	ctx := context.Background()
	hot := fixedTexts(25)
	_, _ = ce.Embed(ctx, hot)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mixed := make([]string, 0, 50)
		mixed = append(mixed, hot...)
		for j := 0; j < 25; j++ {
			mixed = append(mixed, fmt.Sprintf("cold-%d-%d", i, j))
		}
		_, _ = ce.Embed(ctx, mixed)
	}
}

func fixedTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("func hello_%d() {}", i)
	}
	return out
}
