// Package llm / shared_breaker.go — cross-replica circuit breaker.
//
// gobreaker (used in client.go) tracks failures per process. When the
// service is horizontally scaled to N replicas and the upstream LLM
// provider is degraded, each replica sees ~1/N of the failures and none
// of them individually cross its local threshold — so N replicas cheerfully
// hammer a sick provider in parallel. SharedBreaker closes that gap by
// aggregating failures in Redis, so one pod observing a failure counts
// toward the trip decision of every other pod.
//
// Design: fixed-window counter keyed by (provider, window_epoch). On every
// LLM error we INCR the counter with a TTL; before every call we read the
// counter and block if it is at or above the shared threshold. This is
// coarser than gobreaker's closed/half-open/open state machine, but simpler
// and strictly additive on top of the local breaker — the local breaker
// still opens instantly when a single pod's failures concentrate, and the
// shared breaker catches distributed degradation the local one would miss.
//
// Failure modes: Redis unreachable → SharedBreaker permits everything
// (fails open). This is consistent with the rate limiter policy: Redis
// availability should not gate LLM availability.
package llm

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// SharedBreakerConfig configures the cross-replica breaker. Set Rdb to nil
// to disable entirely (Client falls back to the in-process gobreaker only).
type SharedBreakerConfig struct {
	Rdb       *redis.Client
	Prefix    string        // Redis key prefix, default "llm:breaker"
	Window    time.Duration // aggregation window; default 30s
	Threshold int           // max aggregate failures per window before trip
}

// SharedCircuitBreaker is the cross-replica failure accumulator.
type SharedCircuitBreaker struct {
	rdb       *redis.Client
	prefix    string
	window    time.Duration
	threshold int
	logger    *zap.Logger
}

// NewSharedCircuitBreaker constructs a SharedCircuitBreaker. Returns nil
// if cfg.Rdb is nil — callers should treat nil as "no shared breaker"
// and fall back to local behaviour only. This keeps the integration in
// Client optional without a boolean flag.
func NewSharedCircuitBreaker(cfg SharedBreakerConfig, logger *zap.Logger) *SharedCircuitBreaker {
	if cfg.Rdb == nil {
		return nil
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "llm:breaker"
	}
	if cfg.Window <= 0 {
		cfg.Window = 30 * time.Second
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 20
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SharedCircuitBreaker{
		rdb:       cfg.Rdb,
		prefix:    cfg.Prefix,
		window:    cfg.Window,
		threshold: cfg.Threshold,
		logger:    logger.With(zap.String("component", "shared_breaker")),
	}
}

// sharedBreakerCheckScript atomically reads the current-window failure
// count. Returning it from Lua rather than doing a plain GET avoids a
// round-trip race with TTL expiry.
var sharedBreakerCheckScript = redis.NewScript(`
local n = redis.call('GET', KEYS[1])
if n == false then return 0 end
return tonumber(n)
`)

// sharedBreakerRecordScript atomically increments + sets TTL on the
// current-window failure key. Mirrors the rate-limiter pattern.
var sharedBreakerRecordScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// keyFor returns the current-window Redis key for a provider name. The
// window epoch is folded into the key so older windows roll off via TTL
// rather than needing explicit reset logic.
func (s *SharedCircuitBreaker) keyFor(provider string) string {
	epoch := time.Now().Unix() / int64(s.window.Seconds())
	return s.prefix + ":" + provider + ":" + itoaI64(epoch)
}

// Allow reports whether the shared breaker would permit a call to this
// provider. False means "the distributed failure count is at/over the
// trip threshold; short-circuit to fallback without hitting the provider".
// Fails open on Redis errors.
func (s *SharedCircuitBreaker) Allow(ctx context.Context, provider string) bool {
	if s == nil {
		return true
	}
	n, err := sharedBreakerCheckScript.Run(ctx, s.rdb, []string{s.keyFor(provider)}).Int()
	if err != nil && err != redis.Nil {
		s.logger.Warn("shared breaker read failed, permitting",
			zap.String("provider", provider), zap.Error(err))
		return true
	}
	return n < s.threshold
}

// RecordFailure increments the shared failure counter for this provider.
// Callers should call this once per LLM error (not per retry attempt, to
// avoid double-counting). TTL on the counter is the aggregation window,
// so the count naturally rolls off without explicit reset.
func (s *SharedCircuitBreaker) RecordFailure(ctx context.Context, provider string) {
	if s == nil {
		return
	}
	_, err := sharedBreakerRecordScript.Run(ctx, s.rdb,
		[]string{s.keyFor(provider)},
		int(s.window.Seconds()),
	).Int()
	if err != nil {
		s.logger.Warn("shared breaker record failed",
			zap.String("provider", provider), zap.Error(err))
	}
}

// itoaI64 avoids pulling strconv in for this single conversion.
func itoaI64(n int64) string {
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
