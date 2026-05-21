package skill

import (
	"encoding/json"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// TestSnapshot_Deterministic 验证 Snapshot 在未修改时返回同一指针 & 同一 ETag。
func TestSnapshot_Deterministic(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	for _, name := range []string{"zeta", "alpha", "mike"} {
		err := r.Register(&Definition{
			Name:        name,
			Description: "desc",
			Parameters:  json.RawMessage(`{"type":"object"}`),
			Executor:    ExecutorConfig{Type: "webhook", URL: "http://x"},
		})
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	s1 := r.Snapshot()
	s2 := r.Snapshot()
	if s1 != s2 {
		t.Fatalf("expected pointer equality between snapshots")
	}
	if s1.ETag != s2.ETag {
		t.Fatalf("ETag mismatch: %s vs %s", s1.ETag, s2.ETag)
	}
	// 排序断言
	if s1.Tools[0].Name != "alpha" || s1.Tools[1].Name != "mike" || s1.Tools[2].Name != "zeta" {
		t.Fatalf("snapshot not sorted: %+v", s1.Tools)
	}
}

// TestSnapshot_InvalidatedOnChange Register/Unregister 后必须拿到新的快照。
func TestSnapshot_InvalidatedOnChange(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	_ = r.Register(&Definition{
		Name:        "tool1",
		Description: "d",
		Parameters:  json.RawMessage(`{}`),
		Executor:    ExecutorConfig{Type: "webhook", URL: "http://x"},
	})
	first := r.Snapshot()

	_ = r.Register(&Definition{
		Name:        "tool2",
		Description: "d",
		Parameters:  json.RawMessage(`{}`),
		Executor:    ExecutorConfig{Type: "webhook", URL: "http://x"},
	})
	second := r.Snapshot()
	if first == second {
		t.Fatalf("snapshot should change after Register")
	}
	if first.ETag == second.ETag {
		t.Fatalf("ETag should differ after schema change")
	}
	if second.Generation <= first.Generation {
		t.Fatalf("generation not bumped: %d → %d", first.Generation, second.Generation)
	}

	_ = r.Unregister("tool2")
	third := r.Snapshot()
	if third.ETag != first.ETag {
		t.Fatalf("ETag should match first after unregistering the added tool; got %s want %s", third.ETag, first.ETag)
	}
}

// TestSnapshot_ConcurrentRead 1000 并发读下不应发生数据竞争，且多数读应返回同一指针。
func TestSnapshot_ConcurrentRead(t *testing.T) {
	r := NewRegistry(zap.NewNop())
	_ = r.Register(&Definition{
		Name:        "x",
		Description: "d",
		Parameters:  json.RawMessage(`{}`),
		Executor:    ExecutorConfig{Type: "webhook", URL: "http://x"},
	})
	// 触发一次构建
	_ = r.Snapshot()

	const N = 1000
	var wg sync.WaitGroup
	wg.Add(N)
	var mu sync.Mutex
	seen := make(map[*ToolSchemaSnapshot]int)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			s := r.Snapshot()
			mu.Lock()
			seen[s]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Fatalf("expected single shared snapshot, got %d distinct", len(seen))
	}
}
