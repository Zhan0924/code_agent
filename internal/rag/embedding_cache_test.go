package rag

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// fakeEmbedder 统计 Embed 调用次数，方便断言"被缓存拦截了多少次"。
type fakeEmbedder struct {
	calls atomic.Int64
	mu    sync.Mutex
	// 为每个不同的文本返回一个确定性的单值 vector，便于断言
	history [][]string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.history = append(f.history, append([]string(nil), texts...))
	f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// 用文本长度作为 marker，便于测试确认是否返回了正确条目
		out[i] = []float32{float32(len(t)), 0.1, 0.2}
	}
	return out, nil
}

func TestCachedEmbedder_HitsCache(t *testing.T) {
	inner := &fakeEmbedder{}
	cache := NewMemoryEmbeddingCache(100, "test-model", zap.NewNop())
	ce := NewCachedEmbedder(inner, cache, zap.NewNop())

	// 第一次：全部 miss，底层被调用一次
	out1, err := ce.Embed(context.Background(), []string{"aaa", "bb", "c"})
	if err != nil {
		t.Fatalf("embed1: %v", err)
	}
	if len(out1) != 3 {
		t.Fatalf("want 3 vectors, got %d", len(out1))
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner calls want 1 got %d", got)
	}

	// 第二次：完全相同 → 全部 cache hit，底层调用次数不应增加
	_, _ = ce.Embed(context.Background(), []string{"aaa", "bb", "c"})
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("cache miss: expected still 1 inner call, got %d", got)
	}

	// 第三次：混合（两个旧的 + 一个新的） → 底层应只收到 1 个新文本
	_, _ = ce.Embed(context.Background(), []string{"aaa", "new", "bb"})
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("want 2 inner calls after mixed batch, got %d", got)
	}
	// 校验底层这次确实只收到了 "new"
	inner.mu.Lock()
	last := inner.history[len(inner.history)-1]
	inner.mu.Unlock()
	if len(last) != 1 || last[0] != "new" {
		t.Fatalf("underlying embedder received unexpected batch: %+v", last)
	}

	stats := cache.Stats()
	if stats.Hits == 0 {
		t.Fatalf("expected hits > 0, got %+v", stats)
	}
}

func TestCachedEmbedder_NamespaceIsolation(t *testing.T) {
	inner := &fakeEmbedder{}
	c1 := NewMemoryEmbeddingCache(10, "model-a", zap.NewNop())
	c2 := NewMemoryEmbeddingCache(10, "model-b", zap.NewNop())
	ce1 := NewCachedEmbedder(inner, c1, zap.NewNop())
	ce2 := NewCachedEmbedder(inner, c2, zap.NewNop())

	_, _ = ce1.Embed(context.Background(), []string{"hello"})
	_, _ = ce2.Embed(context.Background(), []string{"hello"})

	// 两个不同 namespace 的 cache 各 miss 一次，inner 应被调用 2 次
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("namespaces must not share cache, want 2 got %d", got)
	}
}

func TestMemLRU_Evicts(t *testing.T) {
	c := NewMemoryEmbeddingCache(2, "n", zap.NewNop())
	ctx := context.Background()
	c.Put(ctx, "a", []float32{1})
	c.Put(ctx, "b", []float32{2})
	c.Put(ctx, "c", []float32{3}) // 应淘汰 "a"

	if _, ok := c.Get(ctx, "a"); ok {
		t.Fatalf("LRU did not evict oldest entry")
	}
	if _, ok := c.Get(ctx, "c"); !ok {
		t.Fatalf("newest entry missing")
	}
}
