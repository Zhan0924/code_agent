// Package api —— P0 优化项「端口级」集成测试
//
// ═══════════════════════════════════════════════════════════════════════════
// 测试目标：证明 4 项 P0 优化在**真实 HTTP 端口**（而非单元测试内存调用）上
// 确实生效、可观测、与用户看到的行为一致。
//
//	P0-1  LLM Prompt Cache（Schema Snapshot / ETag）
//	      → GET /api/v1/debug/p0/schema 返回稳定 ETag；同 generation 多次请求
//	        ETag 完全一致；注册新 skill 后 generation++、ETag 改变；
//	        If-None-Match 携带旧 ETag 时返回 304 Not Modified。
//
//	P0-2  RAG Embedding Cache
//	      → GET /api/v1/debug/p0 暴露 embedding_cache.hits/misses；
//	        注入 Adapter 后跨 HTTP 观察到计数单调递增。
//
//	P0-3  Speculative Tool Execution
//	      → POST /api/v1/debug/p0/spec-cache 注入一条 read_file 缓存；
//	        GET /spec-cache/query 返回 hit=true；
//	        GET /debug/p0 的 spec_cache.hits=1、misses=1、hit_rate=0.5；
//	        非幂等工具名被 400 拒绝。
//
//	P0-4  Sandbox Warm Pool
//	      → GET /api/v1/debug/p0 返回 warm_pool 节（enabled=false / true）。
//
// ═══════════════════════════════════════════════════════════════════════════
//
// 实现要点：
//  1. 采用 net/http/httptest.NewServer —— 分配真实端口，使用 http.Client
//     发请求，完全复现生产网络链路（Gin Router + middleware + response
//     headers），而不是用 ServeHTTP(ResponseRecorder)。
//  2. 不依赖外部 Docker / Redis / Postgres；miniredis 提供内存 Redis。
//  3. 所有 P0 探针通过 Server.SetP0Probes 注入真实对象——spec cache 与
//     warm pool 直接构造，embedding cache 通过 Adapter 包装 fake stats。
//
// ═══════════════════════════════════════════════════════════════════════════
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent/code_agent/internal/config"
	"github.com/agent/code_agent/internal/mcp"
	"github.com/agent/code_agent/internal/orchestrator"
	"github.com/agent/code_agent/internal/sandbox"
	"github.com/agent/code_agent/internal/session"
	"github.com/agent/code_agent/internal/skill"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// ─── 测试工具 ────────────────────────────────────────────────────────────────

// p0Harness 把真实 httptest.Server + 所有 P0 探针打包，供各测试共享。
type p0Harness struct {
	t          *testing.T
	server     *Server
	http       *httptest.Server
	skillReg   *skill.Registry
	specCache  *orchestrator.SpeculativeToolCache
	warmPool   *sandbox.WarmPool // 可能为 nil（disabled 场景）
	embedHits  *atomic.Uint64
	embedMiss  *atomic.Uint64
	sessionMgr *session.Manager
	miniredis  *miniredis.Miniredis
}

// newP0Harness 构建一个已启动 TCP 端口的测试 Server，并注入全部 P0 探针。
// warmPoolEnabled 控制 P0-4 的 enabled 字段。
func newP0Harness(t *testing.T, warmPoolEnabled bool) *p0Harness {
	t.Helper()
	logger := zaptest.NewLogger(t)

	// ── 1. Redis ──
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// ── 2. Session manager（用 config 包里 SessionConfig 的最小可用字段）──
	sessionMgr := session.NewManager(rdb, &config.SessionConfig{
		TTL:                    30 * time.Minute,
		MaxHistoryTokens:       8000,
		SummaryThresholdTokens: 4000,
	}, logger)

	// ── 3. Skill registry，预先塞一个 skill 让 ETag 非 empty ──
	skillReg := skill.NewRegistry(logger)
	_ = skillReg.Register(&skill.Definition{
		Name:        "weather",
		Description: "Query weather",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Executor:    skill.ExecutorConfig{Type: "webhook", URL: "http://example/w"},
	})

	// ── 4. MCP gateway（空）──
	gw, _ := mcp.NewGateway(&config.MCPConfig{}, nil, logger)

	// ── 5. P0 探针 ──
	specCache := orchestrator.NewSpeculativeToolCache(60*time.Second, logger)

	var wp *sandbox.WarmPool
	if warmPoolEnabled {
		// 不启动 Start() —— 只是让 enabled=true、Metrics() 可读即可；
		// PerLang 为空保证 replenish goroutine 不会真的去连 docker。
		cfg := sandbox.DefaultWarmPoolConfig()
		cfg.Enabled = true
		cfg.PerLang = map[string]int{} // 空池子；不真启动容器
		wp = sandbox.NewWarmPool(nil, nil, cfg, logger)
	}

	hits := &atomic.Uint64{}
	miss := &atomic.Uint64{}
	embedAdapter := EmbedCacheAdapterFunc(func() (uint64, uint64) {
		return hits.Load(), miss.Load()
	})

	// ── 6. API server ──
	srv := NewServer(nil, sessionMgr, logger)
	srv.SetMCPGateway(gw)
	srv.SetSkillRegistry(skillReg)
	srv.SetP0Probes(&P0Probes{
		SpecCache:  specCache,
		WarmPool:   wp,
		EmbedCache: embedAdapter,
	})

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	return &p0Harness{
		t:          t,
		server:     srv,
		http:       httpSrv,
		skillReg:   skillReg,
		specCache:  specCache,
		warmPool:   wp,
		embedHits:  hits,
		embedMiss:  miss,
		sessionMgr: sessionMgr,
		miniredis:  mr,
	}
}

// getJSON 发起 GET 请求，解析 JSON body；返回 (status, headers, parsed)
func (h *p0Harness) getJSON(path string, reqHeaders map[string]string) (int, http.Header, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+path, nil)
	if err != nil {
		h.t.Fatalf("new req: %v", err)
	}
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	return resp.StatusCode, resp.Header, parsed
}

// postJSON 发起 POST JSON 请求。
func (h *p0Harness) postJSON(path string, body any) (int, http.Header, map[string]any) {
	h.t.Helper()
	var reqBody io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reqBody = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.http.URL+path, reqBody)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	bs, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(bs, &parsed)
	return resp.StatusCode, resp.Header, parsed
}

// ═══════════════════════════════════════════════════════════════════════════
//  P0-1：Schema Snapshot / ETag —— 端口级验证
// ═══════════════════════════════════════════════════════════════════════════

// TestP0_Schema_ETagIsStable 同一 generation 下，多次 HTTP 请求 ETag 不变；
// 证明 Prompt Cache 在网络层也确实字节一致。
func TestP0_Schema_ETagIsStable(t *testing.T) {
	h := newP0Harness(t, false)

	var first string
	for i := 0; i < 5; i++ {
		st, hdr, body := h.getJSON("/api/v1/debug/p0/schema", nil)
		if st != 200 {
			t.Fatalf("GET /debug/p0/schema #%d → %d  body=%v", i, st, body)
		}
		etag := hdr.Get("ETag")
		xetag := hdr.Get("X-Tools-Etag")
		if etag == "" || etag != xetag {
			t.Fatalf("iter %d: ETag=%q X-Tools-Etag=%q want non-empty equal", i, etag, xetag)
		}
		if i == 0 {
			first = etag
			continue
		}
		if etag != first {
			t.Fatalf("iter %d: ETag changed without registry mutation: first=%q now=%q", i, first, etag)
		}
	}
}

// TestP0_Schema_ETagChangesOnRegister 注册新 skill 后 generation++、ETag 必须改变。
func TestP0_Schema_ETagChangesOnRegister(t *testing.T) {
	h := newP0Harness(t, false)

	// 初始 ETag
	_, hdr, body := h.getJSON("/api/v1/debug/p0/schema", nil)
	before := hdr.Get("ETag")
	genBefore, _ := body["generation"].(float64)

	// 通过 HTTP API 注册新 skill —— 模拟外部热插拔
	st, _, reg := h.postJSON("/api/v1/skills", map[string]any{
		"name":        "translate",
		"description": "Translate text between languages",
		"parameters":  map[string]any{"type": "object"},
		"executor":    map[string]any{"type": "webhook", "url": "http://example/t"},
	})
	if st != 200 {
		t.Fatalf("POST /skills → %d body=%v", st, reg)
	}

	// 再取 snapshot
	_, hdr2, body2 := h.getJSON("/api/v1/debug/p0/schema", nil)
	after := hdr2.Get("ETag")
	genAfter, _ := body2["generation"].(float64)

	if before == after {
		t.Fatalf("ETag unchanged after Register: %q", before)
	}
	if genAfter <= genBefore {
		t.Fatalf("generation did not advance: before=%v after=%v", genBefore, genAfter)
	}
	if tc, _ := body2["tool_count"].(float64); int(tc) != 2 {
		t.Fatalf("tool_count want 2, got %v", tc)
	}
}

// TestP0_Schema_IfNoneMatch304 客户端带 If-None-Match 且匹配时返回 304。
// 这是 P0-1 在 HTTP 层的**可直接被 CDN / 浏览器缓存**的证据。
func TestP0_Schema_IfNoneMatch304(t *testing.T) {
	h := newP0Harness(t, false)

	_, hdr, _ := h.getJSON("/api/v1/debug/p0/schema", nil)
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first request")
	}

	st, hdr2, _ := h.getJSON("/api/v1/debug/p0/schema", map[string]string{"If-None-Match": etag})
	if st != http.StatusNotModified {
		t.Fatalf("If-None-Match did not trigger 304, got %d", st)
	}
	// 304 body 应为空，但 ETag 头应该仍然存在（RFC 7232 §4.1 建议）
	_ = hdr2
}

// TestP0_Tools_ExposesETag /api/v1/tools 响应头应带 X-Tools-Etag，
// 方便上游 LLM 客户端做 prompt-cache-key。
func TestP0_Tools_ExposesETag(t *testing.T) {
	h := newP0Harness(t, false)

	req, _ := http.NewRequest(http.MethodGet, h.http.URL+"/api/v1/tools", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	etag := resp.Header.Get("X-Tools-Etag")
	if etag == "" {
		t.Fatalf("/tools response missing X-Tools-Etag header (P0-1 regression)")
	}
	if len(etag) != 12 {
		// buildDeterministicSnapshot 取 sha256 hex 前 12 字符
		t.Errorf("X-Tools-Etag length = %d, want 12 (sha256[:12]): %q", len(etag), etag)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
//  P0-2：Embedding Cache —— 端口级指标可观测
// ═══════════════════════════════════════════════════════════════════════════

// TestP0_EmbedCache_HTTPObservable 调整内部计数器后 HTTP /debug/p0 能读到。
// 这是"探针注入链路正确性"的测试：保证 API 层读到的就是业务层同一对象。
func TestP0_EmbedCache_HTTPObservable(t *testing.T) {
	h := newP0Harness(t, false)

	// 初始状态
	_, _, body := h.getJSON("/api/v1/debug/p0", nil)
	ec, _ := body["embedding_cache"].(map[string]any)
	if ec["enabled"] != true {
		t.Fatalf("embedding_cache.enabled want true; got %v", ec)
	}
	if toUint64(ec["hits"]) != 0 || toUint64(ec["misses"]) != 0 {
		t.Fatalf("initial embed stats not zero: %v", ec)
	}

	// 模拟业务层一次 embed：3 次命中 + 2 次 miss
	for i := 0; i < 3; i++ {
		h.embedHits.Add(1)
	}
	for i := 0; i < 2; i++ {
		h.embedMiss.Add(1)
	}

	_, _, body2 := h.getJSON("/api/v1/debug/p0", nil)
	ec2, _ := body2["embedding_cache"].(map[string]any)
	if toUint64(ec2["hits"]) != 3 {
		t.Errorf("hits want 3, got %v", ec2["hits"])
	}
	if toUint64(ec2["misses"]) != 2 {
		t.Errorf("misses want 2, got %v", ec2["misses"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════
//  P0-3：Speculative Tool Cache —— 端口级端到端验证
// ═══════════════════════════════════════════════════════════════════════════

// TestP0_SpecCache_PutAndHit
// 1. POST /debug/p0/spec-cache 注入 read_file 结果；
// 2. GET /spec-cache/query 命中（hit=true，返回 content）；
// 3. 再查不存在的 args，miss；
// 4. /debug/p0 聚合指标 hit_rate 正确。
func TestP0_SpecCache_PutAndHit(t *testing.T) {
	h := newP0Harness(t, false)

	const sid = "sess-A"
	const tool = "read_file"
	args := `{"path":"main.go"}`

	// Put
	st, _, res := h.postJSON("/api/v1/debug/p0/spec-cache", map[string]any{
		"session_id": sid,
		"tool":       tool,
		"args":       args,
		"content":    "package main\nfunc main(){}",
	})
	if st != 200 {
		t.Fatalf("POST put → %d body=%v", st, res)
	}
	if res["cached"] != true {
		t.Fatalf("expect cached=true, got %v", res)
	}

	// Query hit
	q := fmt.Sprintf("/api/v1/debug/p0/spec-cache/query?session_id=%s&tool=%s&args=%s",
		sid, tool, urlEscape(args))
	st, _, body := h.getJSON(q, nil)
	if st != 200 || body["hit"] != true {
		t.Fatalf("query hit expected; got st=%d body=%v", st, body)
	}
	if !strings.Contains(fmt.Sprint(body["content"]), "package main") {
		t.Errorf("content not returned correctly: %v", body["content"])
	}

	// Query miss（改 args）
	q2 := fmt.Sprintf("/api/v1/debug/p0/spec-cache/query?session_id=%s&tool=%s&args=%s",
		sid, tool, urlEscape(`{"path":"other.go"}`))
	_, _, body2 := h.getJSON(q2, nil)
	if body2["hit"] != false {
		t.Errorf("expect hit=false for different args, got %v", body2)
	}

	// 聚合指标
	_, _, agg := h.getJSON("/api/v1/debug/p0", nil)
	sc, _ := agg["spec_cache"].(map[string]any)
	if toUint64(sc["hits"]) != 1 {
		t.Errorf("spec_cache.hits want 1, got %v", sc["hits"])
	}
	if toUint64(sc["misses"]) != 1 {
		t.Errorf("spec_cache.misses want 1, got %v", sc["misses"])
	}
	if rate, _ := sc["hit_rate"].(float64); rate < 0.49 || rate > 0.51 {
		t.Errorf("hit_rate want ~0.5, got %v", rate)
	}
}

// TestP0_SpecCache_RejectsNonIdempotent 写入非幂等工具应当被 400 拒绝；
// 防止通过 debug 端点污染 cache。
func TestP0_SpecCache_RejectsNonIdempotent(t *testing.T) {
	h := newP0Harness(t, false)

	st, _, body := h.postJSON("/api/v1/debug/p0/spec-cache", map[string]any{
		"session_id": "s",
		"tool":       "write_file", // 非幂等
		"args":       `{}`,
		"content":    "whatever",
	})
	if st != 400 {
		t.Fatalf("non-idempotent write_file should 400, got %d body=%v", st, body)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "idempotent") {
		t.Errorf("error message should mention idempotent: %v", body)
	}
}

// TestP0_SpecCache_SessionIsolation 同一 (tool,args) 不同 session 互不可见。
func TestP0_SpecCache_SessionIsolation(t *testing.T) {
	h := newP0Harness(t, false)

	// session-A 写
	_, _, _ = h.postJSON("/api/v1/debug/p0/spec-cache", map[string]any{
		"session_id": "A",
		"tool":       "read_file",
		"args":       `{"path":"a"}`,
		"content":    "A-content",
	})
	// session-B 查应 miss
	q := "/api/v1/debug/p0/spec-cache/query?session_id=B&tool=read_file&args=" + urlEscape(`{"path":"a"}`)
	_, _, body := h.getJSON(q, nil)
	if body["hit"] != false {
		t.Fatalf("session B must not see session A's cache: %v", body)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
//  P0-4：Warm Pool —— 端口级启用状态
// ═══════════════════════════════════════════════════════════════════════════

// TestP0_WarmPool_Disabled 未注入 warm pool 时 enabled=false。
func TestP0_WarmPool_Disabled(t *testing.T) {
	h := newP0Harness(t, false) // warmPoolEnabled=false

	_, _, body := h.getJSON("/api/v1/debug/p0", nil)
	wp, _ := body["warm_pool"].(map[string]any)
	if wp["enabled"] != false {
		t.Errorf("warm_pool.enabled want false, got %v", wp)
	}
}

// TestP0_WarmPool_Enabled 注入 warm pool 时 enabled=true + 计数字段齐全。
func TestP0_WarmPool_Enabled(t *testing.T) {
	h := newP0Harness(t, true) // warmPoolEnabled=true

	_, _, body := h.getJSON("/api/v1/debug/p0", nil)
	wp, _ := body["warm_pool"].(map[string]any)
	if wp["enabled"] != true {
		t.Errorf("warm_pool.enabled want true, got %v", wp)
	}
	for _, k := range []string{"created", "acquired", "recycled", "fallback"} {
		if _, ok := wp[k]; !ok {
			t.Errorf("warm_pool.%s missing", k)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════
//  综合：健康 + 聚合端点
// ═══════════════════════════════════════════════════════════════════════════

// TestP0_Aggregate_AllSectionsPresent 一次请求看四项全貌；
// 字段命名和类型与前端 / Grafana 面板契约一致。
func TestP0_Aggregate_AllSectionsPresent(t *testing.T) {
	h := newP0Harness(t, true)

	st, hdr, body := h.getJSON("/api/v1/debug/p0", nil)
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if got := hdr.Get("X-Tools-Etag"); got == "" {
		t.Errorf("aggregate response must carry X-Tools-Etag (P0-1 header surface)")
	}
	for _, key := range []string{"schema_snapshot", "spec_cache", "warm_pool", "embedding_cache"} {
		if _, ok := body[key]; !ok {
			t.Errorf("aggregate missing section %q; full body=%v", key, body)
		}
	}
}

// TestP0_HealthzStillOK 回归兜底：debug 路由不应破坏 /healthz。
func TestP0_HealthzStillOK(t *testing.T) {
	h := newP0Harness(t, false)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _, body := h.getJSON("/healthz", nil)
		if st == 200 && body["status"] == "ok" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("/healthz never became ok within 2s")
}

// ─── 杂项 ────────────────────────────────────────────────────────────────────

// toUint64 把 json 数字（float64）安全转成 uint64；nil / 负数 / 非数字 → 0。
func toUint64(v any) uint64 {
	f, ok := v.(float64)
	if !ok || f < 0 {
		return 0
	}
	return uint64(f)
}

// urlEscape 简易 query 值编码；避免引入 net/url 只为此处使用。
func urlEscape(s string) string {
	return strings.NewReplacer(
		"{", "%7B", "}", "%7D",
		":", "%3A", ",", "%2C",
		"\"", "%22", " ", "%20",
	).Replace(s)
}

// 让 linter 不抱怨未使用的 logger（在部分分支里可能没直接用）。
var _ = zap.NewNop
