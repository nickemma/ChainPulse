package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nickemma/chainpulse/internal/audit"
	"github.com/nickemma/chainpulse/internal/auth"
	"github.com/nickemma/chainpulse/internal/gateway"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/config"
	"github.com/nickemma/chainpulse/shared/logger"
	"github.com/nickemma/chainpulse/shared/storage"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config error: %w", err)
	}

	log := logger.New(logger.ParseLevel(cfg.LogLevel))
	log.Info("starting chainpulse", "addr", cfg.Server.Addr, "log_level", cfg.LogLevel)

	// --- Infrastructure clients ---
	// Redis: the client is constructed even if the server is currently
	// unreachable; the gateway degrades gracefully (in-memory rate limiting,
	// idempotency bypass) and reports the state on /health.
	rdb, err := storage.NewRedis(cfg.Redis.Addr, cfg.Redis.Password)
	if err != nil {
		return fmt.Errorf("redis init: %w", err)
	}
	defer rdb.Close()
	if pingErr := rdb.Ping(context.Background()); pingErr != nil {
		log.Warn("redis unreachable at startup — gateway will run degraded", "error", pingErr.Error())
	}

	// --- Application collaborators ---
	issuer := auth.NewIssuer(cfg.Auth.JWTSecret, cfg.Auth.AccessTokenTTL, cfg.Auth.RefreshTokenTTL)
	engine := policy.NewEngine(policy.DefaultRules(),
		time.Duration(cfg.Policy.EvalTimeoutMs)*time.Millisecond)
	auditor := audit.NewLogAuditor(log)

	router := gateway.NewRouter(gateway.Deps{
		Config:  cfg,
		Logger:  log,
		Issuer:  issuer,
		Engine:  engine,
		Auditor: auditor,
		Redis:   rdb,
		Stub:    gateway.NewStubModule(),
	})

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      router,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Channel to receive OS shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start server in a goroutine so it doesn't block
	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Block until signal or server error
	select {
	case err := <-serverErr:
		return fmt.Errorf("server failed to start: %w", err)
	case sig := <-quit:
		log.Info("received shutdown signal", "signal", sig.String())
	}

	// Graceful shutdown — give in-flight requests time to complete.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	log.Info("server stopped cleanly")
	return nil
}
