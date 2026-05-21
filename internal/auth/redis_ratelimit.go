package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisRateLimiter is a fixed-window rate limiter backed by Redis INCR, so
// limits are shared across every pod/replica pointing at the same Redis.
//
// Why fixed-window instead of leaky-bucket: the algorithm uses exactly one
// round-trip per request (a single Lua EVAL that INCRs and sets TTL on the
// first increment). Sliding-log or leaky-bucket implementations need either
// Redis sorted sets (more CPU) or two round-trips. Fixed-window is the
// established industry compromise for HTTP-level rate limiting — a burst at
// the second-to-last second of one window + another burst at the first
// second of the next window is technically 2x the configured rate, but
// that's acceptable for DoS/abuse protection.
//
// Key format: "<prefix>:<bucket>:<windowStart>"
//   - prefix: caller-provided (e.g. "ratelimit:api")
//   - bucket: per-identity string (userID, IP, API-key hash)
//   - windowStart: unix seconds, floored to the window size
type RedisRateLimiter struct {
	rdb    *redis.Client
	prefix string
	limit  int           // max requests per window
	window time.Duration // window size (e.g. 1 minute)
	logger *zap.Logger
}

// NewRedisRateLimiter creates a distributed rate limiter.
// prefix is prepended to every key so different services sharing Redis
// don't collide (e.g. "ratelimit:chat" vs "ratelimit:ingest").
func NewRedisRateLimiter(rdb *redis.Client, prefix string, limit int, window time.Duration, logger *zap.Logger) *RedisRateLimiter {
	if prefix == "" {
		prefix = "ratelimit"
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RedisRateLimiter{
		rdb:    rdb,
		prefix: prefix,
		limit:  limit,
		window: window,
		logger: logger.With(zap.String("component", "redis_ratelimit")),
	}
}

// rateLimitScript atomically increments the per-window counter and sets
// its TTL the first time it's created. Returns 1 if allowed, 0 if over
// the limit. ARGV: [windowSeconds, limit]. KEYS: [bucketKey].
//
// Atomicity matters: without Lua, two pods racing the INCR+EXPIRE pair can
// miss the EXPIRE and leave the counter alive forever under the same
// bucket, eventually blocking all traffic for that user.
var rateLimitScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
if count > tonumber(ARGV[2]) then
  return 0
end
return 1
`)

// Allow reports whether a request from the given bucket key is within the
// configured rate. On any Redis error the function FAILS OPEN — the agent's
// availability is more valuable than strict rate enforcement when Redis is
// down, and outages during infra issues are a worse user experience than
// occasional enforcement gaps.
func (rl *RedisRateLimiter) Allow(ctx context.Context, bucket string) bool {
	windowStart := time.Now().Unix() / int64(rl.window.Seconds())
	key := rl.prefix + ":" + bucket + ":" + itoa(windowStart)

	res, err := rateLimitScript.Run(ctx, rl.rdb,
		[]string{key},
		int(rl.window.Seconds()),
		rl.limit,
	).Int()
	if err != nil {
		// Fail open on Redis error, but log so operators notice.
		rl.logger.Warn("redis rate limit check failed, allowing request",
			zap.String("bucket", bucket), zap.Error(err))
		return true
	}
	return res == 1
}

// GinMiddleware returns a Gin handler that enforces the rate limit on
// every request. Key preference: authenticated user ID > API-key > client
// IP. Anonymous IP-bucketed traffic shares a bucket per IP across all
// replicas, which is the point — a coordinated DoS no longer amplifies
// with replica count.
func (rl *RedisRateLimiter) GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		bucket := "ip:" + c.ClientIP()
		if claims := GetClaims(c); claims != nil && claims.UserID != "" {
			bucket = "user:" + claims.UserID
		} else if apiKey := c.GetHeader("X-API-Key"); apiKey != "" {
			// Use the API key hash rather than the plaintext — don't ever
			// log the raw key via the rate limit keyspace.
			bucket = "apikey:" + hashAPIKeyHex(apiKey)
		}

		if !rl.Allow(c.Request.Context(), bucket) {
			c.Header("Retry-After", itoa(int64(rl.window.Seconds())))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
			})
			return
		}
		c.Next()
	}
}

// hashAPIKeyHex returns a short hex digest of the API key so bucket keys
// never carry the plaintext. Uses the same hash as the APIKeyStore for
// consistency — ratelimit and auth share the same notion of identity.
func hashAPIKeyHex(key string) string {
	h := hashAPIKey(key)
	// First 8 bytes of the SHA-256 (16 hex chars) is plenty to avoid
	// collisions across a realistic number of active API keys.
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 0; i < 8; i++ {
		out[i*2] = hexDigits[h[i]>>4]
		out[i*2+1] = hexDigits[h[i]&0x0f]
	}
	return string(out)
}

// itoa is a tiny local int→string to avoid strconv just for this file.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
