// Package storage owns the construction of the shared infrastructure clients:
// the Postgres connection pool and the tenant-scoped Redis wrapper. Modules
// receive these already-constructed; they never dial a database themselves.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPostgres opens a pgx connection pool against the given DSN and verifies
// connectivity with a ping. A failure here is fatal at startup — the system
// cannot serve authenticated traffic without its system of record.
func NewPostgres(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}

	// Conservative pool defaults; tuned later under load testing (Phase 2 exit
	// criteria includes a 200-concurrent-request load test).
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return pool, nil
}
