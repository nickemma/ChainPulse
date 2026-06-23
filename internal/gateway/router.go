// Package gateway assembles the HTTP edge: the middleware pipeline every
// request passes through (correlation ID, structured logging, JWT auth, rate
// limiting, idempotency) and the policy enforcement that guards each route.
// In Phase 2 the protected routes are stub handlers — the real modules arrive
// in Phase 3 and slot in behind the same gateway unchanged.
package gateway

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/internal/audit"
	"github.com/nickemma/chainpulse/internal/auth"
	"github.com/nickemma/chainpulse/internal/gateway/middleware"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/config"
	"github.com/nickemma/chainpulse/shared/storage"
)

// Deps are the constructed collaborators the gateway needs. They are built
// once at startup and injected — the gateway dials nothing itself.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Issuer  *auth.Issuer
	Engine  *policy.Engine
	Auditor audit.Auditor
	Redis   *storage.Redis   // always non-nil; calls may fail and degrade
	APIKeys auth.APIKeyStore // may be nil when no Postgres store is configured
	Stub    *StubModule
}

// NewRouter builds the gin engine with the full middleware pipeline wired.
func NewRouter(d Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Recovery())
	// Global pipeline: correlation ID first, then access logging wraps the rest.
	router.Use(middleware.RequestID(d.Logger))
	router.Use(middleware.RequestLog())

	// Health is public — no auth, no policy. Used by docker/compose and probes.
	router.GET("/health", handleHealth(d.Redis))

	// Auth endpoints are public (they mint tokens).
	auth.NewHandler(d.Issuer).Register(router)

	// Rate limiters: Redis is authoritative, memory is the per-instance fallback.
	redisLimiter := middleware.NewRedisLimiter(d.Redis)
	memLimiter := middleware.NewMemoryLimiter()

	// Everything under /v1 (except auth) requires a valid credential (JWT or API
	// key) and is rate-limited.
	v1 := router.Group("/v1")
	v1.Use(middleware.Authenticate(d.Issuer, d.APIKeys))
	v1.Use(middleware.RateLimit(redisLimiter, memLimiter,
		d.Config.Gateway.RateLimitRequests, d.Config.Gateway.RateLimitWindow))

	registerStubRoutes(v1, d)
	registerAPIKeyRoutes(v1, d)

	return router
}

// registerAPIKeyRoutes wires partner-key management (create/rotate/revoke/list),
// each guarded by a policy.Enforce on the "api_key" resource — only an admin
// rule allows it. They are no-ops when no key store is configured.
func registerAPIKeyRoutes(v1 *gin.RouterGroup, d Deps) {
	if d.APIKeys == nil {
		return
	}
	enforce := func(action policy.Action) gin.HandlerFunc {
		return policy.Enforce(d.Engine, d.Auditor, action, "api_key", queryResourceExtractor)
	}
	h := auth.NewAPIKeyHandler(d.APIKeys)
	v1.POST("/api-keys", enforce(policy.ActionCreate), h.Create)
	v1.GET("/api-keys", enforce(policy.ActionRead), h.List)
	v1.POST("/api-keys/:id/rotate", enforce(policy.ActionUpdate), h.Rotate)
	v1.DELETE("/api-keys/:id", enforce(policy.ActionDelete), h.Revoke)
}

// registerStubRoutes wires the placeholder module endpoints, each guarded by a
// policy.Enforce middleware describing its action + resource type. Write
// endpoints additionally require an idempotency key.
func registerStubRoutes(v1 *gin.RouterGroup, d Deps) {
	enforce := func(action policy.Action, resourceType string) gin.HandlerFunc {
		return policy.Enforce(d.Engine, d.Auditor, action, resourceType, queryResourceExtractor)
	}
	idem := middleware.Idempotency(d.Redis, d.Config.Gateway.IdempotencyTTL)

	// Orders
	v1.POST("/orders", idem, enforce(policy.ActionCreate, "order"), d.Stub.handle)
	v1.GET("/orders/:id", enforce(policy.ActionRead, "order"), d.Stub.handle)
	v1.PUT("/orders/:id", enforce(policy.ActionUpdate, "order"), d.Stub.handle)

	// Inventory
	v1.GET("/inventory", enforce(policy.ActionRead, "inventory"), d.Stub.handle)
	v1.PUT("/inventory", enforce(policy.ActionUpdate, "inventory"), d.Stub.handle)

	// Shipments
	v1.GET("/shipments/:id", enforce(policy.ActionRead, "shipment"), d.Stub.handle)
}

// handleHealth reports liveness and Redis reachability. It never fails the
// request on a Redis outage — it reports the degraded state in the body.
func handleHealth(rdb *storage.Redis) gin.HandlerFunc {
	return func(c *gin.Context) {
		redisOK := rdb.Ping(c.Request.Context()) == nil
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"redis":  redisOK,
		})
	}
}
