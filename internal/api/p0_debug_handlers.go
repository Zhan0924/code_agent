// Package api —— P0 优化项调试 / 可观测端点
//
// 本文件提供 4 个 P0 优化的运行期可观测接口，用于集成测试（端口级）证明优化
// 真正在 HTTP 服务中生效，而不仅仅是单元测试隔离环境下正确。
//
//	GET /api/v1/debug/p0           —— 聚合四项指标（一个请求看全貌）
//	GET /api/v1/debug/p0/schema    —— 返回当前工具 schema 快照（含 ETag）
//	GET /api/v1/debug/p0/spec-cache—— Speculative Tool Cache 命中指标
//	POST /api/v1/debug/p0/spec-cache—— 允许测试向 cache 写一条记录，然后复查
//
// 设计原则：
//   - 仅暴露**聚合指标 + 只读快照**，不泄漏 session 内容；
//   - 所有字段在所有 P0 开关状态下都能返回（未启用时返回 enabled=false）；
//   - 专供内部集成测试 / Prometheus 抓取 / 开发自检；生产建议通过网关限制。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/agent/code_agent/internal/models"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/skill"
	"github.com/gin-gonic/gin"
)

// P0Probes 聚合四项 P0 优化的运行期探针；Server 可选择性注入，未设置时返回 enabled=false。
//
// 注入方式（见 Server.SetP0Probes）：
//
//	server.SetP0Probes(&P0Probes{
//	    SpecCache: specCache,
//	    WarmPool:  warmPool,
//	    EmbedCache: rag.MemLRU, // 可选
//	})
type P0Probes struct {
	SpecCache  *orchestrator.SpeculativeToolCache
	WarmPool   *sandbox.WarmPool
	EmbedCache EmbedCacheProbe // interface 让不同 cache 实现都可以接入
}

// EmbedCacheProbe 是对 rag.EmbeddingCache 的最小探针接口，便于注入。
// rag.EmbeddingCache 返回 CacheStats 结构体，这里只暴露聚合计数避免包依赖扩散。
type EmbedCacheProbe interface {
	EmbedStats() (hits, misses uint64)
}

// EmbedCacheAdapter 把 rag.EmbeddingCache 的 Stats() CacheStats 适配成 EmbedCacheProbe。
// 让 main.go 可以这样注入：
//
//	api.EmbedCacheAdapterFunc(func() (uint64, uint64) {
//	    st := cache.Stats()
//	    return st.Hits, st.Misses
//	})
type EmbedCacheAdapterFunc func() (hits, misses uint64)

// EmbedStats 实现 EmbedCacheProbe.
func (f EmbedCacheAdapterFunc) EmbedStats() (uint64, uint64) { return f() }

// SetP0Probes 注入 P0 运行期探针。
func (s *Server) SetP0Probes(p *P0Probes) { s.p0 = p }

// ═══════════════════════════════════════════════════════════════════════════
//  /api/v1/debug/p0  —— 聚合端点
// ═══════════════════════════════════════════════════════════════════════════

type p0AggregateResp struct {
	SchemaSnapshot schemaSection     `json:"schema_snapshot"`
	SpecCache      specCacheSection  `json:"spec_cache"`
	WarmPool       warmPoolSection   `json:"warm_pool"`
	EmbeddingCache embedCacheSection `json:"embedding_cache"`
}

type schemaSection struct {
	Enabled    bool   `json:"enabled"`
	Generation uint64 `json:"generation"`
	ETag       string `json:"etag"`
	ToolCount  int    `json:"tool_count"`
}

type specCacheSection struct {
	Enabled bool    `json:"enabled"`
	Hits    uint64  `json:"hits"`
	Misses  uint64  `json:"misses"`
	Bypass  uint64  `json:"bypass"`
	HitRate float64 `json:"hit_rate"`
}

type warmPoolSection struct {
	Enabled  bool   `json:"enabled"`
	Created  uint64 `json:"created"`
	Acquired uint64 `json:"acquired"`
	Recycled uint64 `json:"recycled"`
	Fallback uint64 `json:"fallback"`
}

type embedCacheSection struct {
	Enabled bool   `json:"enabled"`
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`
}

// handleP0Aggregate 返回四项优化的运行期一瞥。
// GET /api/v1/debug/p0
func (s *Server) handleP0Aggregate(c *gin.Context) {
	resp := p0AggregateResp{
		SchemaSnapshot: s.probeSchema(),
		SpecCache:      s.probeSpecCache(),
		WarmPool:       s.probeWarmPool(),
		EmbeddingCache: s.probeEmbedCache(),
	}
	// 暴露 ETag 到 HTTP 响应头；客户端可用作 If-None-Match
	if resp.SchemaSnapshot.ETag != "" {
		c.Header("X-Tools-Etag", resp.SchemaSnapshot.ETag)
	}
	c.JSON(http.StatusOK, resp)
}

// ═══════════════════════════════════════════════════════════════════════════
//  /api/v1/debug/p0/schema  —— 拉取工具 schema 快照
// ═══════════════════════════════════════════════════════════════════════════

// handleP0SchemaSnapshot 返回当前 skill.Registry 的稳定快照。
// 同一 generation 内字节完全相同；客户端可通过 If-None-Match 命中 304。
//
// GET /api/v1/debug/p0/schema
func (s *Server) handleP0SchemaSnapshot(c *gin.Context) {
	snap := s.currentSchemaSnapshot()
	if snap == nil {
		c.Header("X-Tools-Etag", "none")
		c.JSON(http.StatusOK, gin.H{
			"enabled": false,
			"tools":   []any{},
		})
		return
	}

	// 标准 HTTP ETag 协议：相同 ETag → 304 Not Modified。
	// 这是 P0-1 优化的 HTTP 层体现：同 generation 跨请求零拷贝、零序列化。
	c.Header("ETag", snap.ETag)
	c.Header("X-Tools-Etag", snap.ETag)

	if inm := c.GetHeader("If-None-Match"); inm == snap.ETag {
		c.Status(http.StatusNotModified)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":    true,
		"generation": snap.Generation,
		"etag":       snap.ETag,
		"tool_count": len(snap.Tools),
		"tools":      snap.Tools,
	})
}

// ═══════════════════════════════════════════════════════════════════════════
//  /api/v1/debug/p0/spec-cache  —— 投放 + 查询
// ═══════════════════════════════════════════════════════════════════════════

// handleP0SpecCacheGet 返回当前 cache 指标（hits/misses/hit_rate）。
// GET /api/v1/debug/p0/spec-cache
func (s *Server) handleP0SpecCacheGet(c *gin.Context) {
	c.JSON(http.StatusOK, s.probeSpecCache())
}

// handleP0SpecCachePut 允许集成测试直接向 cache 注入一条记录，再读回。
// 这样可在无 LLM / Orchestrator 的测试环境下，验证 cache 走的是 HTTP 层注入的
// 同一对象（而不是测试里另起的实例）。
//
// POST /api/v1/debug/p0/spec-cache
//
//	{
//	  "session_id": "s1",
//	  "tool":       "read_file",
//	  "args":       "{\"path\":\"main.go\"}",
//	  "content":    "hello world"
//	}
func (s *Server) handleP0SpecCachePut(c *gin.Context) {
	if s.p0 == nil || s.p0.SpecCache == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "spec cache not wired"})
		return
	}
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
		Tool      string `json:"tool"       binding:"required"`
		Args      string `json:"args"       binding:"required"`
		Content   string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 只允许幂等白名单，避免通过 debug 端点污染缓存
	if !orchestrator.IsIdempotentTool(req.Tool) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "tool is not idempotent / not cacheable",
			"tool":  req.Tool,
		})
		return
	}
	ok := s.p0.SpecCache.Put(req.SessionID, req.Tool,
		json.RawMessage(req.Args),
		&models.ToolResult{Content: req.Content})
	c.JSON(http.StatusOK, gin.H{"cached": ok})
}

// handleP0SpecCacheQuery 根据 (session, tool, args) 查询 cache hit。
// GET /api/v1/debug/p0/spec-cache/query?session_id=s1&tool=read_file&args={...}
func (s *Server) handleP0SpecCacheQuery(c *gin.Context) {
	if s.p0 == nil || s.p0.SpecCache == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "spec cache not wired"})
		return
	}
	sid := c.Query("session_id")
	tool := c.Query("tool")
	args := c.Query("args")
	if sid == "" || tool == "" || args == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id, tool, args are required"})
		return
	}
	res, hit := s.p0.SpecCache.Get(sid, tool, json.RawMessage(args))
	if !hit {
		c.JSON(http.StatusOK, gin.H{"hit": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"hit":      true,
		"content":  res.Content,
		"is_error": res.IsError,
	})
}

// ═══════════════════════════════════════════════════════════════════════════
//  探针辅助
// ═══════════════════════════════════════════════════════════════════════════

// currentSchemaSnapshot 返回当前 skill.Registry 的 Snapshot；未设 registry 返回 nil。
func (s *Server) currentSchemaSnapshot() *skill.ToolSchemaSnapshot {
	if s.skillRegistry == nil {
		return nil
	}
	return s.skillRegistry.Snapshot()
}

func (s *Server) probeSchema() schemaSection {
	snap := s.currentSchemaSnapshot()
	if snap == nil {
		return schemaSection{Enabled: false}
	}
	return schemaSection{
		Enabled:    true,
		Generation: snap.Generation,
		ETag:       snap.ETag,
		ToolCount:  len(snap.Tools),
	}
}

func (s *Server) probeSpecCache() specCacheSection {
	if s.p0 == nil || s.p0.SpecCache == nil {
		return specCacheSection{Enabled: false}
	}
	hits, misses, bypass, rate := s.p0.SpecCache.Metrics()
	return specCacheSection{
		Enabled: true,
		Hits:    hits,
		Misses:  misses,
		Bypass:  bypass,
		HitRate: rate,
	}
}

func (s *Server) probeWarmPool() warmPoolSection {
	if s.p0 == nil || s.p0.WarmPool == nil {
		return warmPoolSection{Enabled: false}
	}
	created, acquired, recycled, fallback := s.p0.WarmPool.Metrics()
	return warmPoolSection{
		Enabled:  true,
		Created:  created,
		Acquired: acquired,
		Recycled: recycled,
		Fallback: fallback,
	}
}

func (s *Server) probeEmbedCache() embedCacheSection {
	if s.p0 == nil || s.p0.EmbedCache == nil {
		return embedCacheSection{Enabled: false}
	}
	hits, misses := s.p0.EmbedCache.EmbedStats()
	return embedCacheSection{Enabled: true, Hits: hits, Misses: misses}
}
