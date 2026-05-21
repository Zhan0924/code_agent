// Package pool provides sync.Pool-based object reuse for hot-path allocations.
// By pooling frequently allocated objects (byte slices, JSON buffers, AST nodes,
// RPC message structs), we dramatically reduce GC pressure under high concurrency,
// yielding smoother latency percentiles (P99) for the Agent system.
package pool

import (
	"bytes"
	"encoding/json"
	"sync"
)

// ─── Byte Slice Pool ─────────────────────────────────────────────────────────
// Used in RAG parsing, sandbox I/O streaming, and MCP JSON-RPC communication
// where transient byte buffers are allocated at extremely high frequency.

// ByteSlicePool manages reusable byte slices of a given initial capacity.
type ByteSlicePool struct {
	pool sync.Pool
	size int
}

// NewByteSlicePool creates a pool for byte slices with the given initial capacity.
func NewByteSlicePool(initialCap int) *ByteSlicePool {
	return &ByteSlicePool{
		size: initialCap,
		pool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, 0, initialCap)
				return &b
			},
		},
	}
}

// Get retrieves a byte slice from the pool, reset to zero length.
func (p *ByteSlicePool) Get() *[]byte {
	bp := p.pool.Get().(*[]byte)
	*bp = (*bp)[:0]
	return bp
}

// Put returns a byte slice to the pool. Oversized slices are discarded
// to prevent the pool from holding onto excessively large allocations.
func (p *ByteSlicePool) Put(bp *[]byte) {
	if cap(*bp) > p.size*8 {
		// Discard oversized slices to prevent memory bloat
		return
	}
	*bp = (*bp)[:0]
	p.pool.Put(bp)
}

// ─── Buffer Pool ─────────────────────────────────────────────────────────────
// Used for JSON marshaling/unmarshaling in the orchestrator, MCP client,
// and API handlers.

// BufferPool manages reusable bytes.Buffer instances.
type BufferPool struct {
	pool sync.Pool
}

// NewBufferPool creates a new buffer pool.
func NewBufferPool() *BufferPool {
	return &BufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				return bytes.NewBuffer(make([]byte, 0, 4096))
			},
		},
	}
}

// Get retrieves a buffer from the pool, reset and ready to use.
func (p *BufferPool) Get() *bytes.Buffer {
	buf := p.pool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// Put returns a buffer to the pool. Oversized buffers are discarded.
func (p *BufferPool) Put(buf *bytes.Buffer) {
	if buf.Cap() > 1<<20 { // 1MB limit
		return
	}
	buf.Reset()
	p.pool.Put(buf)
}

// ─── JSON Encoder/Decoder Pool ───────────────────────────────────────────────
// MCP JSON-RPC 2.0 communication produces massive amounts of temporary
// encoder/decoder objects. Pooling them eliminates per-request GC overhead.

// JSONEncoderPool manages reusable JSON encoders backed by pooled buffers.
type JSONEncoderPool struct {
	bufferPool *BufferPool
}

// NewJSONEncoderPool creates a new JSON encoder pool.
func NewJSONEncoderPool() *JSONEncoderPool {
	return &JSONEncoderPool{
		bufferPool: NewBufferPool(),
	}
}

// Encode marshals the given value to JSON using a pooled buffer, returning
// the JSON bytes. The caller does not need to manage the buffer.
func (p *JSONEncoderPool) Encode(v interface{}) ([]byte, error) {
	buf := p.bufferPool.Get()
	defer p.bufferPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	// Copy out of buffer before returning it to pool
	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// ─── RPC Message Pool ────────────────────────────────────────────────────────
// Pool for JSON-RPC request/response structs used in high-frequency MCP communication.

// RPCRequestPool manages reusable JSON-RPC request objects.
type RPCRequestPool struct {
	pool sync.Pool
}

// RPCRequest represents a poolable JSON-RPC 2.0 request.
type RPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// NewRPCRequestPool creates a pool for RPC request objects.
func NewRPCRequestPool() *RPCRequestPool {
	return &RPCRequestPool{
		pool: sync.Pool{
			New: func() interface{} {
				return &RPCRequest{JSONRPC: "2.0"}
			},
		},
	}
}

// Get retrieves an RPC request from the pool.
func (p *RPCRequestPool) Get() *RPCRequest {
	req := p.pool.Get().(*RPCRequest)
	req.JSONRPC = "2.0"
	req.ID = 0
	req.Method = ""
	req.Params = nil
	return req
}

// Put returns an RPC request to the pool.
func (p *RPCRequestPool) Put(req *RPCRequest) {
	req.Params = nil // Clear reference to allow GC of params
	p.pool.Put(req)
}

// ─── Global Singleton Pools ──────────────────────────────────────────────────
// Pre-initialized global pools for the most common allocation hot-spots.

var (
	// SmallBytePool for small buffers (up to 4KB) — sandbox I/O lines, short responses
	SmallBytePool = NewByteSlicePool(4096)

	// LargeBytePool for large buffers (up to 64KB) — AST chunks, code files
	LargeBytePool = NewByteSlicePool(65536)

	// GlobalBufferPool for general-purpose buffered I/O
	GlobalBufferPool = NewBufferPool()

	// GlobalJSONPool for JSON encoding in hot paths
	GlobalJSONPool = NewJSONEncoderPool()

	// GlobalRPCPool for MCP JSON-RPC message objects
	GlobalRPCPool = NewRPCRequestPool()
)
