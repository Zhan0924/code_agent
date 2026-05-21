package orchestrator

import (
	"fmt"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/models"
	"go.uber.org/zap"
)

// BenchmarkSpecCache_Hit：稳态缓存命中路径，测纯 sync.Map 读开销。
func BenchmarkSpecCache_Hit(b *testing.B) {
	c := NewSpeculativeToolCache(time.Minute, zap.NewNop())
	args := []byte(`{"path":"main.go"}`)
	c.Put("sess1", "read_file", args, &models.ToolResult{Content: "hello"})
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("sess1", "read_file", args)
	}
}

// BenchmarkSpecCache_Miss：未命中路径，对应新工具首次调用。
func BenchmarkSpecCache_Miss(b *testing.B) {
	c := NewSpeculativeToolCache(time.Minute, zap.NewNop())
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		args := []byte(fmt.Sprintf(`{"id":%d}`, i))
		_, _ = c.Get("sess1", "read_file", args)
	}
}

// BenchmarkSpecCache_PutGet：写入 + 命中读全链路；用于评估 ReAct 循环单步开销。
func BenchmarkSpecCache_PutGet(b *testing.B) {
	c := NewSpeculativeToolCache(time.Minute, zap.NewNop())
	args := []byte(`{"path":"main.go"}`)
	res := &models.ToolResult{Content: "hello"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Put("sess1", "read_file", args, res)
		_, _ = c.Get("sess1", "read_file", args)
	}
}

// BenchmarkSpecCache_BypassNonIdempotent：写工具的穿透路径，验证不会被无故耽搁。
func BenchmarkSpecCache_BypassNonIdempotent(b *testing.B) {
	c := NewSpeculativeToolCache(time.Minute, zap.NewNop())
	args := []byte(`{}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("s", "edit_file", args)
	}
}
