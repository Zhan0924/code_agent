// ratelimit.go — per-user / per-IP token-bucket 限流（进程内版）。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【token bucket vs leaky bucket】
//
//	本实现用 token bucket：每个 bucket 有一个 tokens 计数，每 window 刷新
//	一次补回 rate 个 token，但不超过 burst。收请求时扣 1，0 就拒绝。
//	好处：允许短期突发（burst）；滑动平均等于 rate。
//
// 【key 选择的 fallback 链】
//
//	用户 ID > API-Key hash > Client IP。理由：
//	  · 用户 ID：最精确，不会被 NAT 合并；
//	  · API-Key：服务账号场景，用户 ID 为空；
//	  · IP：最后兜底，匿名请求。
//	注意：公司内网 NAT 出口下所有用户共用一个 IP，若用 IP 限流会互相影响；
//	所以**登录后务必切到 user_id**。
//
// 【进程内的局限】
//
//	N 个副本各自独立计数，实际限流是 N × rate。分布式限流见
//	internal/auth/redis_ratelimit.go（P0 #22）。这里的实现仍然保留是因为：
//	  · Redis 故障时的 fallback（fail-open 后退回本地限流）；
//	  · 本地测试 / 单副本部署不需要 Redis 就能限流。
//
// 【cleanup goroutine】
//
//	每 5min 扫一次 buckets map，删除 10×window 内没请求过的条目。
//	如果不清理，一个对所有 IP 做 DDoS 的对手能让 map 长到 OOM。
//
// ============================================================================
package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// PerUserRateLimiter provides per-user/API-key rate limiting using a token bucket.
type PerUserRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    int           // tokens per interval
	burst   int           // max burst
	window  time.Duration // refill interval
}

type bucket struct {
	tokens    int
	lastReset time.Time
}

// NewPerUserRateLimiter creates a per-user rate limiter.
// rate: max requests per window, burst: max burst above rate.
func NewPerUserRateLimiter(rate, burst int, window time.Duration) *PerUserRateLimiter {
	rl := &PerUserRateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		window:  window,
	}
	// Background cleanup of stale buckets every 5 minutes
	go rl.cleanup()
	return rl
}

func (rl *PerUserRateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * rl.window)
		for key, b := range rl.buckets {
			if b.lastReset.Before(cutoff) {
				delete(rl.buckets, key)
			}
		}
		rl.mu.Unlock()
	}
}

// Allow checks if a request from the given key is allowed.
func (rl *PerUserRateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists {
		rl.buckets[key] = &bucket{tokens: rl.burst - 1, lastReset: now}
		return true
	}

	// Refill tokens based on elapsed time
	elapsed := now.Sub(b.lastReset)
	refill := int(elapsed/rl.window) * rl.rate
	if refill > 0 {
		b.tokens += refill
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.lastReset = now
	}

	if b.tokens > 0 {
		b.tokens--
		return true
	}
	return false
}

// GinMiddleware returns Gin middleware that rate-limits per authenticated user.
// Falls back to IP-based limiting for unauthenticated requests.
func (rl *PerUserRateLimiter) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Determine rate limit key: user_id > API-key > IP
		key := c.ClientIP()
		if claims := GetClaims(c); claims != nil {
			key = "user:" + claims.UserID
		} else if apiKey := c.GetHeader("X-API-Key"); apiKey != "" {
			key = "apikey:" + apiKey
		}

		if !rl.Allow(key) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
				"key":   key,
			})
			return
		}
		c.Next()
	}
}
