package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/nickemma/chainpulse/internal/audit"
	"github.com/nickemma/chainpulse/internal/auth"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/config"
	"github.com/nickemma/chainpulse/shared/identity"
	"github.com/nickemma/chainpulse/shared/logger"
	"github.com/nickemma/chainpulse/shared/storage"
)

// newHarness builds a real gateway router. redisAddr lets a test point at a
// live miniredis (idempotency path) or a dead address (fallback path).
func newHarness(t *testing.T, redisAddr string, rateLimit int) (http.Handler, *StubModule, *auth.Issuer) {
	t.Helper()
	rdb, err := storage.NewRedis(redisAddr, "")
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	issuer := auth.NewIssuer("test-secret", 15*time.Minute, 24*time.Hour)
	stub := NewStubModule()

	cfg := &config.Config{}
	cfg.Gateway.RateLimitRequests = rateLimit
	cfg.Gateway.RateLimitWindow = time.Minute
	cfg.Gateway.IdempotencyTTL = time.Hour

	router := NewRouter(Deps{
		Config:  cfg,
		Logger:  logger.New(logger.ParseLevel("error")),
		Issuer:  issuer,
		Engine:  policy.NewEngine(policy.DefaultRules(), 5*time.Millisecond),
		Auditor: audit.NewLogAuditor(logger.New(logger.ParseLevel("error"))),
		Redis:   rdb,
		Stub:    stub,
	})
	return router, stub, issuer
}

func tokenFor(t *testing.T, iss *auth.Issuer, id identity.Identity) string {
	t.Helper()
	pair, err := iss.Issue(id)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return pair.AccessToken
}

func do(router http.Handler, method, path, token string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func mini(t *testing.T) string {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return mr.Addr()
}

// --- Failure tests required by the Phase 2 roadmap ---

func TestMissingTokenReturns401(t *testing.T) {
	router, _, _ := newHarness(t, mini(t), 100)
	rec := do(router, http.MethodGet, "/v1/orders/abc", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestExpiredTokenReturns401(t *testing.T) {
	router, _, _ := newHarness(t, mini(t), 100)
	rec := do(router, http.MethodGet, "/v1/orders/abc", expiredToken(t, "test-secret"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for expired token, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestCrossTenantReturns403(t *testing.T) {
	router, _, iss := newHarness(t, mini(t), 100)
	tok := tokenFor(t, iss, identity.Identity{TenantID: "tenant-A", UserID: "u", Role: identity.RoleAdmin})
	// Admin of tenant-A reaches for a resource owned by tenant-B.
	rec := do(router, http.MethodGet, "/v1/inventory?tenant_id=tenant-B", tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 cross-tenant, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "forbidden")
}

func TestDefaultDenyReturns403(t *testing.T) {
	router, _, iss := newHarness(t, mini(t), 100)
	// A supplier has no rule allowing them to read orders.
	tok := tokenFor(t, iss, identity.Identity{
		TenantID: "t", UserID: "u", Role: identity.RoleSupplier, SupplierID: "sup-1",
	})
	rec := do(router, http.MethodGet, "/v1/orders/abc", tok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 default-deny, got %d", rec.Code)
	}
}

func TestHappyPathAllow(t *testing.T) {
	router, stub, iss := newHarness(t, mini(t), 100)
	tok := tokenFor(t, iss, identity.Identity{TenantID: "t", UserID: "u", Role: identity.RoleAdmin})
	rec := do(router, http.MethodGet, "/v1/orders/abc", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.Calls.Load() != 1 {
		t.Fatalf("want handler called once, got %d", stub.Calls.Load())
	}
}

// Roadmap failure test: a duplicate POST with the same idempotency key returns
// the cached response WITHOUT executing the handler a second time.
func TestIdempotentDuplicateNotReprocessed(t *testing.T) {
	router, stub, iss := newHarness(t, mini(t), 100)
	tok := tokenFor(t, iss, identity.Identity{TenantID: "t", UserID: "u", Role: identity.RoleAdmin})
	headers := map[string]string{"Idempotency-Key": "key-123"}

	first := do(router, http.MethodPost, "/v1/orders", tok, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST want 201, got %d (body=%s)", first.Code, first.Body.String())
	}
	second := do(router, http.MethodPost, "/v1/orders", tok, headers)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay want 201, got %d", second.Code)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("expected Idempotent-Replay header on duplicate")
	}
	if got := stub.Calls.Load(); got != 1 {
		t.Fatalf("handler must run exactly once across duplicates, ran %d times", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatal("replay body must match original")
	}
}

func TestIdempotencyKeyRequired(t *testing.T) {
	router, _, iss := newHarness(t, mini(t), 100)
	tok := tokenFor(t, iss, identity.Identity{TenantID: "t", UserID: "u", Role: identity.RoleAdmin})
	rec := do(router, http.MethodPost, "/v1/orders", tok, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 when idempotency key missing, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "idempotency_key_required")
}

// Roadmap failure test: when Redis is unavailable, rate limiting falls back to
// the per-instance in-memory limiter and still enforces the limit. Here Redis
// points at a dead address, so every Redis call errors and the memory fallback
// is exercised.
func TestRateLimitFallsBackToMemoryWhenRedisDown(t *testing.T) {
	const limit = 3
	router, _, iss := newHarness(t, "127.0.0.1:1", limit) // 127.0.0.1:1 refuses connections
	tok := tokenFor(t, iss, identity.Identity{TenantID: "t", UserID: "u", Role: identity.RoleAdmin})

	var got429 bool
	for i := 0; i < limit+2; i++ {
		rec := do(router, http.MethodGet, "/v1/orders/abc", tok, nil)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("expected in-memory fallback to enforce the rate limit (429) while Redis is down")
	}
}

// --- helpers ---

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (raw=%s)", err, rec.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("want error code %q, got %q", code, body.Error.Code)
	}
}

// expiredToken mints a valid-but-already-expired HS256 access token using the
// exported Claims shape, so the auth middleware exercises its real expiry path.
func expiredToken(t *testing.T, secret string) string {
	t.Helper()
	claims := auth.Claims{
		TenantID:  "t",
		Role:      string(identity.RoleAdmin),
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u",
			Issuer:    "chainpulse",
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("craft expired token: %v", err)
	}
	return signed
}
