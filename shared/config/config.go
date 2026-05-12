package config

import (
	"fmt"
	"os"
	"strconv"
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

// Load reads configuration from environment variables and returns
// a validated Config. Returns an error if any required value is missing
// or malformed — the caller should treat this as fatal.
func Load() (*Config, error) {
	cfg := &Config{}

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

	return cfg, nil
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
