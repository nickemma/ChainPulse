package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for ChainPulse.
// Values are loaded once at startup from environment variables.
// No component reads os.Getenv directly — they receive a *Config.
type Config struct {
	Server   ServerConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Auth     AuthConfig
	Policy   PolicyConfig
	Gateway  GatewayConfig
	LogLevel string
}

type ServerConfig struct {
	Addr            string
	ShutdownTimeout time.Duration
}

type PostgresConfig struct {
	DSN string
}

type RedisConfig struct {
	Addr     string
	Password string
}

type AuthConfig struct {
	// JWTSecret is the HMAC-SHA256 signing key for tokens.
	// In production this comes from a secrets manager, never hardcoded.
	JWTSecret       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

type PolicyConfig struct {
	// CacheRefreshInterval is how often the policy engine reloads
	// active policies from Postgres into memory.
	CacheRefreshInterval time.Duration

	// EvalTimeoutMs is the maximum time a policy evaluation can take
	// before it is treated as a deny.
	EvalTimeoutMs int
}

// GatewayConfig tunes the edge controls: rate limiting and idempotency.
type GatewayConfig struct {
	// RateLimitRequests is the number of requests allowed per tenant+route
	// within RateLimitWindow.
	RateLimitRequests int
	RateLimitWindow   time.Duration

	// IdempotencyTTL is how long a stored idempotent response is replayed
	// for duplicate requests carrying the same key.
	IdempotencyTTL time.Duration
}

// Load reads configuration from environment variables and returns
// a validated Config. Returns an error if any required value is missing
// or malformed — the caller should treat this as fatal.
func Load() (*Config, error) {
	// Load a local .env file if one exists. This is a convenience for local
	// development only — in production, env vars come from the orchestrator or
	// a secrets manager and no .env file is present. Real environment
	// variables always win over .env values.
	loadDotEnv(".env")

	cfg := &Config{}

	// --- Logging ---
	cfg.LogLevel = getEnvOrDefault("LOG_LEVEL", "info")

	// --- Server ---
	cfg.Server.Addr = getEnvOrDefault("SERVER_ADDR", ":8080")
	cfg.Server.ShutdownTimeout = getDurationOrDefault("SERVER_SHUTDOWN_TIMEOUT", 10*time.Second)

	// --- Postgres ---
	dsn, err := requireEnv("POSTGRES_DSN")
	if err != nil {
		return nil, err
	}
	cfg.Postgres.DSN = dsn

	// --- Redis ---
	cfg.Redis.Addr = getEnvOrDefault("REDIS_ADDR", "localhost:6379")
	cfg.Redis.Password = os.Getenv("REDIS_PASSWORD") // empty password is valid

	// --- Auth ---
	jwtSecret, err := requireEnv("JWT_SECRET")
	if err != nil {
		return nil, err
	}
	cfg.Auth.JWTSecret = jwtSecret
	cfg.Auth.AccessTokenTTL = getDurationOrDefault("ACCESS_TOKEN_TTL", 15*time.Minute)
	cfg.Auth.RefreshTokenTTL = getDurationOrDefault("REFRESH_TOKEN_TTL", 7*24*time.Hour)

	// --- Policy ---
	cfg.Policy.CacheRefreshInterval = getDurationOrDefault("POLICY_CACHE_REFRESH", 60*time.Second)
	cfg.Policy.EvalTimeoutMs = getIntOrDefault("POLICY_EVAL_TIMEOUT_MS", 5)

	// --- Gateway ---
	cfg.Gateway.RateLimitRequests = getIntOrDefault("RATE_LIMIT_REQUESTS", 100)
	cfg.Gateway.RateLimitWindow = getDurationOrDefault("RATE_LIMIT_WINDOW", time.Minute)
	cfg.Gateway.IdempotencyTTL = getDurationOrDefault("IDEMPOTENCY_TTL", 24*time.Hour)

	return cfg, nil
}

// loadDotEnv reads KEY=VALUE lines from path into the process environment,
// skipping blank lines and comments. Existing environment variables are never
// overwritten — a real env var always takes precedence over the file. Missing
// file is not an error. This intentionally avoids an external dependency.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env file is the normal case in production
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Strip an inline comment ("VALUE   # note") on unquoted values, while
		// preserving a '#' that is part of a quoted value (e.g. a password).
		if !strings.HasPrefix(value, `"`) && !strings.HasPrefix(value, `'`) {
			if i := strings.IndexAny(value, " \t"); i >= 0 {
				if j := strings.Index(value[i:], "#"); j >= 0 {
					value = value[:i]
				}
			}
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

// requireEnv returns the value of an environment variable or an error
// if it is not set or empty. Use for values with no safe default.
func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("required environment variable %q is not set", key)
	}
	return v, nil
}

// getEnvOrDefault returns the environment variable value or a fallback.
func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getDurationOrDefault parses a duration string from env or returns fallback.
// Valid duration strings: "10s", "5m", "1h30m"
func getDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Malformed duration — use fallback and don't silently corrupt config.
		// In production you'd log a warning here. For now, fallback is safe.
		return fallback
	}
	return d
}

// getIntOrDefault parses an integer from env or returns fallback.
func getIntOrDefault(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}
