package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// newTestRedis gives us a Redis talking to miniredis so tests are fast,
// hermetic, and don't need a real Redis sidecar. Also keeps the import
// consistent with the rest of the test suite (miniredis is in go.mod).
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

// TestRedisRateLimiter_AllowsUpToLimit confirms the basic contract: the
// first N requests within a window pass, the (N+1)th is rejected.
func TestRedisRateLimiter_AllowsUpToLimit(t *testing.T) {
	rdb, _ := newTestRedis(t)
	rl := NewRedisRateLimiter(rdb, "test", 3, time.Minute, zap.NewNop())

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if !rl.Allow(ctx, "u1") {
			t.Errorf("request %d should be allowed within limit", i+1)
		}
	}
	if rl.Allow(ctx, "u1") {
		t.Error("4th request must be rejected — limit is 3")
	}
}

// TestRedisRateLimiter_ResetAcrossWindows verifies that advancing time past
// the window boundary frees up capacity — this is the whole point of a
// fixed-window counter. Uses miniredis's FastForward to advance its clock.
func TestRedisRateLimiter_ResetAcrossWindows(t *testing.T) {
	rdb, mr := newTestRedis(t)
	rl := NewRedisRateLimiter(rdb, "test", 2, time.Minute, zap.NewNop())

	ctx := context.Background()
	if !rl.Allow(ctx, "u1") || !rl.Allow(ctx, "u1") {
		t.Fatal("first two requests must be allowed")
	}
	if rl.Allow(ctx, "u1") {
		t.Fatal("third must be rejected in same window")
	}

	// Advance past the window. miniredis expires keys accordingly.
	mr.FastForward(2 * time.Minute)

	if !rl.Allow(ctx, "u1") {
		t.Error("after window advance, new requests must be allowed again")
	}
}

// TestRedisRateLimiter_IsolatesBuckets confirms two users don't share a
// bucket — exhausting one must not block the other.
func TestRedisRateLimiter_IsolatesBuckets(t *testing.T) {
	rdb, _ := newTestRedis(t)
	rl := NewRedisRateLimiter(rdb, "test", 1, time.Minute, zap.NewNop())

	ctx := context.Background()
	if !rl.Allow(ctx, "alice") {
		t.Fatal("alice first request must pass")
	}
	if rl.Allow(ctx, "alice") {
		t.Fatal("alice second request must be rejected")
	}
	if !rl.Allow(ctx, "bob") {
		t.Error("bob must not be affected by alice's exhausted bucket")
	}
}

// TestRedisRateLimiter_FailsOpenOnRedisDown verifies the stated fail-open
// policy: if Redis is unreachable, requests are allowed (with a warning
// log). Better a brief rate-limit gap than a full outage during a Redis
// incident.
func TestRedisRateLimiter_FailsOpenOnRedisDown(t *testing.T) {
	rdb, mr := newTestRedis(t)
	rl := NewRedisRateLimiter(rdb, "test", 1, time.Minute, zap.NewNop())

	mr.Close() // Redis is gone

	if !rl.Allow(context.Background(), "u1") {
		t.Error("must fail open when Redis is unreachable")
	}
}

// TestRedisRateLimiter_GinMiddleware_BucketPreference confirms the
// per-user > per-APIKey > per-IP key preference. Without this, a shared
// NAT'd corporate IP would throttle every legitimate user behind it.
func TestRedisRateLimiter_GinMiddleware_BucketPreference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rdb, _ := newTestRedis(t)
	rl := NewRedisRateLimiter(rdb, "test", 1, time.Minute, zap.NewNop())

	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Inject synthetic auth claims only for the /auth path.
		if c.Request.URL.Path == "/auth" {
			c.Set(contextKey, &Claims{UserID: "alice"})
		}
		c.Next()
	})
	r.Use(rl.GinMiddleware())
	r.GET("/auth", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/anon", func(c *gin.Context) { c.String(200, "ok") })

	// alice (authenticated) exhausts her per-user bucket.
	do := func(path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if do("/auth") != 200 {
		t.Fatal("alice first request should succeed")
	}
	if do("/auth") != http.StatusTooManyRequests {
		t.Fatal("alice second request should be rate-limited")
	}
	// An anonymous request from the same IP should NOT be affected by
	// alice's exhausted bucket — it uses a different key (ip:).
	if do("/anon") != 200 {
		t.Error("anonymous request must not share a bucket with authenticated user")
	}
}
