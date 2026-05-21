package pool

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestByteSlicePool(t *testing.T) {
	p := NewByteSlicePool(1024)

	// Get and verify initial capacity
	bp := p.Get()
	if cap(*bp) < 1024 {
		t.Errorf("expected cap >= 1024, got %d", cap(*bp))
	}
	if len(*bp) != 0 {
		t.Errorf("expected len 0, got %d", len(*bp))
	}

	// Write data and put back
	*bp = append(*bp, "hello"...)
	p.Put(bp)

	// Get again — should be reset to len 0
	bp2 := p.Get()
	if len(*bp2) != 0 {
		t.Errorf("expected len 0 after reuse, got %d", len(*bp2))
	}
	p.Put(bp2)
}

func TestByteSlicePool_OversizedDiscarded(t *testing.T) {
	p := NewByteSlicePool(64)

	// Create oversized slice (>8x default) — should be discarded on Put
	huge := make([]byte, 0, 64*9)
	huge = append(huge, "data"...)
	p.Put(&huge)

	bp := p.Get()
	// Should get a fresh buffer from New(), not the oversized one
	if cap(*bp) >= 64*9 {
		t.Errorf("oversized buffer should not be pooled, got cap %d", cap(*bp))
	}
	p.Put(bp)
}

func TestBufferPool(t *testing.T) {
	buf := GlobalBufferPool.Get()
	if buf == nil {
		t.Fatal("expected non-nil buffer")
	}
	buf.WriteString("test data")
	if buf.Len() != 9 {
		t.Errorf("expected len 9, got %d", buf.Len())
	}
	GlobalBufferPool.Put(buf)

	// Get again — should be reset
	buf2 := GlobalBufferPool.Get()
	if buf2.Len() != 0 {
		t.Errorf("expected empty buffer after reuse, got len %d", buf2.Len())
	}
	GlobalBufferPool.Put(buf2)
}

func TestJSONPool_Encode(t *testing.T) {
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	input := testStruct{Name: "test", Value: 42}
	data, err := GlobalJSONPool.Encode(input)
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	if !bytes.Contains(data, []byte(`"name":"test"`)) {
		t.Errorf("encoded data missing expected content: %s", string(data))
	}

	// Verify it's valid JSON by decoding with standard library
	var output testStruct
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("standard json.Unmarshal failed on pool-encoded data: %v", err)
	}

	if output.Name != input.Name || output.Value != input.Value {
		t.Errorf("round-trip mismatch: got %+v, want %+v", output, input)
	}
}

func BenchmarkByteSlicePool_GetPut(b *testing.B) {
	p := NewByteSlicePool(4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bp := p.Get()
		*bp = append(*bp, "benchmark data for pool testing"...)
		p.Put(bp)
	}
}

func BenchmarkBufferPool_GetPut(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := GlobalBufferPool.Get()
		buf.WriteString("benchmark data for pool testing")
		GlobalBufferPool.Put(buf)
	}
}

func BenchmarkJSONPool_Encode(b *testing.B) {
	type payload struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Count   int    `json:"count"`
	}
	input := payload{ID: "bench-id", Content: "some content here", Count: 100}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, _ := GlobalJSONPool.Encode(input)
		_ = data
	}
}
