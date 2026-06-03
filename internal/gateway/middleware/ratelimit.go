package middleware

import (
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
	"github.com/nickemma/chainpulse/shared/logger"
	"github.com/nickemma/chainpulse/shared/storage"
)

// Limiter is a per-tenant, per-suffix rate limiter. Implementations decide
// whether one more request is allowed within the given window.
type Limiter interface {
	Allow(ctx context.Context, tenantID, suffix string, limit int, window time.Duration) (bool, error)
}

// RateLimit returns middleware enforcing a sliding-window limit keyed by
// tenant + route. The Redis limiter is authoritative and shared across
// instances. If Redis is unavailable, the middleware falls back to a
// per-instance in-memory limiter: limits are no longer shared across
// instances (a tenant on N instances effectively gets N× the limit during the
// outage), which is documented in ARCHITECTURE.md as an accepted degradation.
// The fallback is logged so the degradation is visible, never silent.
func RateLimit(primary, fallback Limiter, limit int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := identity.FromGin(c)
		if !ok {
			httpx.Unauthorized(c, "authentication required")
			return
		}

		// Suffix is per-route so limits are independent across endpoints.
		suffix := "ratelimit:" + c.FullPath()

		allowed, err := primary.Allow(c.Request.Context(), actor.TenantID, suffix, limit, window)
		if err != nil {
			logger.FromContext(c.Request.Context()).Warn(
				"rate limiter falling back to in-memory",
				"error", err.Error(), "tenant_id", actor.TenantID, "route", c.FullPath(),
			)
			c.Set("ratelimit_fallback", true)
			allowed, _ = fallback.Allow(c.Request.Context(), actor.TenantID, suffix, limit, window)
		}

		if !allowed {
			httpx.Abort(c, 429, httpx.CodeRateLimited, "rate limit exceeded")
			return
		}
		c.Next()
	}
}

// --- Redis sliding-window limiter ---

// slidingWindowScript implements an atomic sliding-window-log limiter. It drops
// entries older than the window, counts what remains, and admits the request
// only if the count is below the limit. Atomicity matters: without it, two
// concurrent requests could both read a count below the limit and both be
// admitted, exceeding it.
const slidingWindowScript = `
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now - window)
local count = redis.call('ZCARD', KEYS[1])
if count < limit then
  redis.call('ZADD', KEYS[1], now, member)
  redis.call('PEXPIRE', KEYS[1], window)
  return 1
end
return 0
`

// RedisLimiter is the authoritative, cross-instance limiter.
type RedisLimiter struct {
	rdb *storage.Redis
}

func NewRedisLimiter(rdb *storage.Redis) *RedisLimiter { return &RedisLimiter{rdb: rdb} }

func (l *RedisLimiter) Allow(ctx context.Context, tenantID, suffix string, limit int, window time.Duration) (bool, error) {
	key := l.rdb.Key(tenantID, suffix)
	nowMs := time.Now().UnixMilli()
	member := uuid.NewString()
	res, err := l.rdb.Eval(ctx, slidingWindowScript,
		[]string{key}, nowMs, window.Milliseconds(), limit, member)
	if err != nil {
		return false, err
	}
	v, _ := res.(int64)
	return v == 1, nil
}

// --- In-memory fallback limiter ---

// MemoryLimiter is a per-instance sliding-window-log limiter used when Redis is
// unavailable. It is intentionally simple; it prunes lazily on each call.
type MemoryLimiter struct {
	mu      sync.Mutex
	windows map[string][]int64 // composite key -> sorted-ish timestamps (ms)
}

func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{windows: make(map[string][]int64)}
}

func (l *MemoryLimiter) Allow(_ context.Context, tenantID, suffix string, limit int, window time.Duration) (bool, error) {
	key := tenantID + "|" + suffix
	now := time.Now().UnixMilli()
	cutoff := now - window.Milliseconds()

	l.mu.Lock()
	defer l.mu.Unlock()

	stamps := l.windows[key]
	kept := stamps[:0]
	for _, ts := range stamps {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= limit {
		l.windows[key] = kept
		return false, nil
	}
	l.windows[key] = append(kept, now)
	return true, nil
}
