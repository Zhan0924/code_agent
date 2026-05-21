// middleware.go — 所有 Gin 中间件实现集合。
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么每个中间件都是独立闭包工厂】
//
//	requestIDMiddleware() / rateLimiterMiddleware(rl) 等函数返回 HandlerFunc
//	而不是直接作为 HandlerFunc 暴露。原因：
//	  · 有状态中间件（限流 token bucket、熔断计数）需要在每次调用之间保持
//	    状态，必须闭包捕获；
//	  · 便于测试：可以构造带特定配置的中间件副本；
//	  · 便于组合：Server 的 setupMiddleware 方法按固定顺序 Use(...) 注入。
//
// 【Recovery 中间件】
//
//	这是最外层的"保险丝"。任何 handler panic 都被它 recover 并翻译成
//	HTTP 500，同时把 stack trace 打到日志。没有它，panic 会杀掉整个
//	Gin worker goroutine（虽然不至于崩服务，因为 Gin 每个连接都有独立
//	goroutine，但会丢掉当前请求的响应）。
//
// 【限流设计（当前 in-memory 版）】
//
//	这里的 rateLimiter 是进程内 token bucket，以 client IP 为 key。
//	局限：N 个副本彼此不同步，实际 RPS 上限是 N*limit。真正的分布式限流
//	见 internal/auth/redis_ratelimit.go（P0 #22）。要切换：在 setupMiddleware
//	里把 rateLimiterMiddleware(rl) 替换为 redisRL.GinMiddleware()。
//
// 【metrics 中间件注意事项】
//
//	必须在认证后（status code 已定型）才能打点，否则所有 401 都会被记成
//	"无鉴权"。metrics 用 c.FullPath()（Gin 的路由模板）而不是 c.Request.URL.Path
//	——后者含 :id 参数会导致 cardinality 爆炸。
//
// 【CORS / Origin】
//
//	允许的 origin 在 corsMiddleware 里硬编码，未来应从配置读取。
//	WS 升级路径的 CheckOrigin 逻辑独立，见 handlers.go 的 wsUpgrader。
//
// ============================================================================
// Package api middleware provides request ID tracking, rate limiting,
// and Prometheus metrics collection for the API layer.
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agent/code_agent/internal/metrics"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ─── Request ID Middleware ───────────────────────────────────────────────────
// Injects a unique X-Request-ID into every request for distributed tracing.

const RequestIDHeader = "X-Request-ID"

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(RequestIDHeader)
		if requestID == "" {
			requestID = uuid.New().String()
		}
		c.Set("request_id", requestID)
		c.Header(RequestIDHeader, requestID)
		c.Next()
	}
}

// ─── Prometheus Metrics Middleware ────────────────────────────────────────────

func metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}

		c.Next()

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		metrics.APIRequestTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		metrics.APIRequestDuration.WithLabelValues(c.Request.Method, path).Observe(duration)
	}
}

// ─── Rate Limiter Middleware ─────────────────────────────────────────────────
// Token-bucket rate limiter per client IP to prevent DoS attacks.

// RateLimiterConfig configures the rate limiter.
type RateLimiterConfig struct {
	RequestsPerSecond float64
	BurstSize         int
	CleanupInterval   time.Duration
}

// DefaultRateLimiterConfig returns sensible defaults.
func DefaultRateLimiterConfig() *RateLimiterConfig {
	return &RateLimiterConfig{
		RequestsPerSecond: 10,
		BurstSize:         20,
		CleanupInterval:   5 * time.Minute,
	}
}

type tokenBucket struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	cfg     *RateLimiterConfig
	logger  *zap.Logger
}

func newRateLimiter(cfg *RateLimiterConfig, logger *zap.Logger) *rateLimiter {
	rl := &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		cfg:     cfg,
		logger:  logger,
	}
	// Background cleanup of stale buckets
	go rl.cleanupLoop()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	bucket, exists := rl.buckets[key]
	if !exists {
		bucket = &tokenBucket{
			tokens:     float64(rl.cfg.BurstSize),
			maxTokens:  float64(rl.cfg.BurstSize),
			refillRate: rl.cfg.RequestsPerSecond,
			lastRefill: time.Now(),
		}
		rl.buckets[key] = bucket
	}
	return bucket.allow()
}

func (rl *rateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cfg.CleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.cfg.CleanupInterval)
		for key, bucket := range rl.buckets {
			if bucket.lastRefill.Before(cutoff) {
				delete(rl.buckets, key)
			}
		}
		rl.mu.Unlock()
	}
}

func rateLimiterMiddleware(rl *rateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		if !rl.allow(clientIP) {
			rl.logger.Warn("rate limit exceeded",
				zap.String("ip", clientIP),
				zap.String("path", c.Request.URL.Path),
			)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
				"code":  "RATE_LIMITED",
			})
			return
		}
		c.Next()
	}
}

// ─── Recovery with Request ID ────────────────────────────────────────────────

func recoveryMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				requestID, _ := c.Get("request_id")
				logger.Error("panic recovered",
					zap.Any("error", err),
					zap.Any("request_id", requestID),
					zap.String("path", c.Request.URL.Path),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error":      "internal server error",
					"code":       "INTERNAL",
					"request_id": fmt.Sprintf("%v", requestID),
				})
			}
		}()
		c.Next()
	}
}
