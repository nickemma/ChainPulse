// Package logger provides a single, structured JSON logger for the whole
// application. Every log line is machine-parseable and carries the context
// fields that the audit trail and observability layers depend on:
// tenant_id, actor_id, trace_id.
//
// There is exactly one logger construction path. Components receive a
// *slog.Logger; they never build their own. This keeps log format consistent
// across the gateway, policy engine, and modules.
package logger

import (
	"context"
	"log/slog"
	"os"
)

// ctxKey is unexported so no other package can collide with our context keys.
type ctxKey int

const loggerKey ctxKey = iota

// New returns a JSON structured logger writing to stdout. The level is
// controlled by the caller (typically derived from config / env).
func New(level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler)
}

// WithContext stores a logger in the context so downstream handlers can
// retrieve a request-scoped logger that already carries tenant/trace fields.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the request-scoped logger, or the default logger if none
// was attached. It never returns nil — callers can always log safely.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// ParseLevel maps a human string ("debug", "info", "warn", "error") to a
// slog.Level, defaulting to Info for anything unrecognized.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "warn", "WARN", "warning":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
