package sandbox

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestWarmPool_DisabledStart 当 Enabled=false 时 Start 必须立即返回且不启动 goroutine。
// 这保证在没有 Docker 的 CI 环境中也能安全单测。
func TestWarmPool_DisabledStart(t *testing.T) {
	p := NewWarmPool(nil, nil, &WarmPoolConfig{Enabled: false}, zap.NewNop())
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start disabled pool: %v", err)
	}
	// Stop 应同样 no-op 完成
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.Stop(ctx)
}

// TestWarmPool_AcquireFallbackWhenEmpty 未 Start 或未知语言应立即返回 nil。
func TestWarmPool_AcquireFallbackWhenEmpty(t *testing.T) {
	p := NewWarmPool(nil, nil, &WarmPoolConfig{Enabled: false}, zap.NewNop())
	got := p.Acquire("python")
	if got != nil {
		t.Fatalf("expected nil on empty pool, got %+v", got)
	}
	_, _, _, fb := p.Metrics()
	if fb != 1 {
		t.Fatalf("fallback counter want 1 got %d", fb)
	}
}

// TestWarmPool_AcquireZeroWait 零 MaxWaitMs 下 Acquire 必须立刻返回 nil 不阻塞。
func TestWarmPool_AcquireZeroWait(t *testing.T) {
	p := NewWarmPool(nil, nil, &WarmPoolConfig{
		Enabled:   true,
		PerLang:   map[string]int{"python": 1},
		MaxWaitMs: 0,
	}, zap.NewNop())
	// 手动注入空 queue（不创建 goroutine，避免调 Docker）
	p.queues["python"] = make(chan *PooledContainer, 1)

	start := time.Now()
	got := p.Acquire("python")
	elapsed := time.Since(start)
	if got != nil {
		t.Fatalf("unexpected container from empty queue: %+v", got)
	}
	if elapsed > 10*time.Millisecond {
		t.Fatalf("zero-wait Acquire blocked for %v", elapsed)
	}
}

// TestWarmPool_AcquireWithTimeout MaxWaitMs>0 时应至少等待接近指定时长。
func TestWarmPool_AcquireWithTimeout(t *testing.T) {
	p := NewWarmPool(nil, nil, &WarmPoolConfig{
		Enabled:   true,
		PerLang:   map[string]int{"python": 1},
		MaxWaitMs: 50,
	}, zap.NewNop())
	p.queues["python"] = make(chan *PooledContainer, 1)

	start := time.Now()
	got := p.Acquire("python")
	elapsed := time.Since(start)
	if got != nil {
		t.Fatalf("expected nil after timeout")
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Acquire returned too fast: %v", elapsed)
	}
}

// TestWarmPool_AcquireHit 预塞一个容器到 channel，Acquire 应立即拿到且指标 +1。
func TestWarmPool_AcquireHit(t *testing.T) {
	p := NewWarmPool(nil, nil, &WarmPoolConfig{
		Enabled:   true,
		PerLang:   map[string]int{"python": 1},
		MaxWaitMs: 50,
	}, zap.NewNop())
	ch := make(chan *PooledContainer, 1)
	c := &PooledContainer{ID: "abc", Language: "python", StartedAt: time.Now()}
	ch <- c
	p.queues["python"] = ch

	got := p.Acquire("python")
	if got == nil || got.ID != "abc" {
		t.Fatalf("Acquire should return the prepared container")
	}
	created, acquired, _, _ := p.Metrics()
	if acquired != 1 {
		t.Fatalf("acquired counter want 1 got %d", acquired)
	}
	_ = created // warm path unused here
}

func TestDefaultWarmPoolConfig(t *testing.T) {
	c := DefaultWarmPoolConfig()
	if c == nil || c.Enabled {
		t.Fatalf("default config must exist and be disabled by default")
	}
}
