package skill

import (
	"encoding/json"
	"testing"

	"go.uber.org/zap"
)

// BenchmarkSnapshot_Cached 测稳态下（无写操作）Snapshot 的热路径延迟。
// 预期：atomic.Pointer.Load，ns 级，零分配。
func BenchmarkSnapshot_Cached(b *testing.B) {
	r := prepareRegistry(10)
	_ = r.Snapshot() // warm up
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Snapshot()
	}
}

// BenchmarkSnapshot_Rebuild 测每次写操作后首次 Snapshot 的重建成本，
// 即 P0-1 的"最坏路径"开销，用于确认不会被异常放大。
func BenchmarkSnapshot_Rebuild(b *testing.B) {
	r := prepareRegistry(10)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// 强制让快照失效，下一次调用重建
		r.schemaStore.Bump()
		_ = r.Snapshot()
	}
}

// BenchmarkGetToolDefinitions 基准线：对比优化前后调用 GetToolDefinitions
// 的开销（当前内部走 Snapshot，应与 Cached 基本一致）。
func BenchmarkGetToolDefinitions(b *testing.B) {
	r := prepareRegistry(10)
	_ = r.GetToolDefinitions() // warm
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.GetToolDefinitions()
	}
}

func prepareRegistry(n int) *Registry {
	r := NewRegistry(zap.NewNop())
	for i := 0; i < n; i++ {
		_ = r.Register(&Definition{
			Name:        fname(i),
			Description: "tool",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
			Executor:    ExecutorConfig{Type: "webhook", URL: "http://x"},
		})
	}
	return r
}

func fname(i int) string {
	const letters = "abcdefghijklmnopqrst"
	return string(letters[i%len(letters)]) + "_tool"
}
