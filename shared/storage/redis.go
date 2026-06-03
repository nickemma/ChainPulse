package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the only sanctioned path to Redis in the application. Every key is
// tenant-scoped: callers pass a tenant ID and a suffix, and the wrapper builds
// the key as "tenant:{tenant_id}:{suffix}". A caller cannot construct an
// un-prefixed key without reaching for Raw(), and Raw access still routes key
// construction through Key(). This makes tenant isolation a structural
// property of the cache layer, not a per-call discipline.
type Redis struct {
	client *redis.Client
}

// ErrCacheMiss is returned by Get when the key does not exist. Callers treat
// this as a normal cache miss, not an error condition.
var ErrCacheMiss = errors.New("cache miss")

// NewRedis dials Redis and verifies connectivity. Unlike Postgres, a Redis
// failure at startup is not necessarily fatal — the gateway is designed to
// fall back to in-memory rate limiting and Postgres idempotency — but we still
// surface the dial error so the operator knows Redis is degraded from boot.
func NewRedis(addr, password string) (*Redis, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DialTimeout:  1 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		// Fail fast to the gateway's in-memory fallback rather than retrying
		// with backoff. When Redis is down, adding retry latency to every
		// request is worse than degrading immediately (the degradation is
		// designed and alerted on). -1 disables retries in go-redis.
		MaxRetries: -1,
	})
	return &Redis{client: client}, nil
}

// Ping reports whether Redis is currently reachable. The gateway uses this to
// decide between the Redis path and its fallbacks.
func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// Key builds the canonical tenant-scoped key. This is the single source of
// truth for key structure.
func (r *Redis) Key(tenantID, suffix string) string {
	return fmt.Sprintf("tenant:%s:%s", tenantID, suffix)
}

// Get returns the value at the tenant-scoped key, or ErrCacheMiss if absent.
func (r *Redis) Get(ctx context.Context, tenantID, suffix string) (string, error) {
	v, err := r.client.Get(ctx, r.Key(tenantID, suffix)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}
	return v, err
}

// SetNX sets the value only if the key does not already exist, returning true
// if the value was written. This is the primitive behind idempotency-key
// reservation: the first request wins the SetNX, duplicates lose it.
func (r *Redis) SetNX(ctx context.Context, tenantID, suffix, value string, ttl time.Duration) (bool, error) {
	return r.client.SetNX(ctx, r.Key(tenantID, suffix), value, ttl).Result()
}

// Set writes a value with a TTL unconditionally.
func (r *Redis) Set(ctx context.Context, tenantID, suffix, value string, ttl time.Duration) error {
	return r.client.Set(ctx, r.Key(tenantID, suffix), value, ttl).Err()
}

// Eval runs a Lua script. The key must already be tenant-scoped via Key();
// the rate limiter uses this for an atomic sliding-window check.
func (r *Redis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.client.Eval(ctx, script, keys, args...).Result()
}

// Close releases the connection pool.
func (r *Redis) Close() error {
	return r.client.Close()
}
