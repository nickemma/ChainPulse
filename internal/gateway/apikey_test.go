package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nickemma/chainpulse/internal/audit"
	"github.com/nickemma/chainpulse/internal/auth"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/config"
	"github.com/nickemma/chainpulse/shared/identity"
	"github.com/nickemma/chainpulse/shared/logger"
	"github.com/nickemma/chainpulse/shared/storage"
)

// fakeResolver is an in-memory auth.APIKeyResolver so the gateway's API-key
// auth path can be tested without Postgres. It returns whatever the test maps a
// presented key to, or an error to exercise the failure branches.
type fakeResolver struct {
	keys map[string]identity.Identity
	err  map[string]error
}

func (f *fakeResolver) Resolve(_ context.Context, key string) (identity.Identity, error) {
	if err, ok := f.err[key]; ok {
		return identity.Identity{}, err
	}
	if id, ok := f.keys[key]; ok {
		return id, nil
	}
	return identity.Identity{}, auth.ErrKeyNotFound
}

// newAPIKeyHarness builds a router whose API-key resolution is backed by the
// given resolver. Management routes are not exercised here (List/Create need
// the full store) — this harness targets the authentication path.
func newAPIKeyHarness(t *testing.T, redisAddr string, resolver auth.APIKeyStore) (http.Handler, *StubModule) {
	t.Helper()
	rdb, err := storage.NewRedis(redisAddr, "")
	if err != nil {
		t.Fatalf("redis client: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	stub := NewStubModule()
	cfg := &config.Config{}
	cfg.Gateway.RateLimitRequests = 100
	cfg.Gateway.RateLimitWindow = time.Minute
	cfg.Gateway.IdempotencyTTL = time.Hour

	router := NewRouter(Deps{
		Config:  cfg,
		Logger:  logger.New(logger.ParseLevel("error")),
		Issuer:  auth.NewIssuer("test-secret", 15*time.Minute, 24*time.Hour),
		Engine:  policy.NewEngine(policy.DefaultRules(), 5*time.Millisecond),
		Auditor: audit.NewLogAuditor(logger.New(logger.ParseLevel("error"))),
		Redis:   rdb,
		APIKeys: resolver,
		Stub:    stub,
	})
	return router, stub
}

// storeFromResolver adapts a bare resolver into an APIKeyStore so it satisfies
// Deps.APIKeys; the management methods are unused in these auth-path tests.
type storeFromResolver struct{ auth.APIKeyResolver }

func (storeFromResolver) Create(context.Context, auth.CreateAPIKeySpec) (auth.APIKey, string, error) {
	return auth.APIKey{}, "", nil
}
func (storeFromResolver) Rotate(context.Context, string, string) (auth.APIKey, string, error) {
	return auth.APIKey{}, "", nil
}
func (storeFromResolver) Revoke(context.Context, string, string) error        { return nil }
func (storeFromResolver) List(context.Context, string) ([]auth.APIKey, error) { return nil, nil }

func TestAPIKeyAuthValidKeyAllows(t *testing.T) {
	resolver := &fakeResolver{keys: map[string]identity.Identity{
		"cp_validkey": {TenantID: "t", UserID: "key-1", Role: identity.RoleAdmin},
	}}
	router, stub := newAPIKeyHarness(t, mini(t), storeFromResolver{resolver})

	rec := do(router, http.MethodGet, "/v1/orders/abc", "", map[string]string{
		"X-API-Key": "cp_validkey",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid api key want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if stub.Calls.Load() != 1 {
		t.Fatalf("handler should run once for a valid key, ran %d", stub.Calls.Load())
	}
}

func TestAPIKeyAuthRevokedKeyReturns401(t *testing.T) {
	resolver := &fakeResolver{err: map[string]error{"cp_revoked": auth.ErrKeyRevoked}}
	router, _ := newAPIKeyHarness(t, mini(t), storeFromResolver{resolver})

	rec := do(router, http.MethodGet, "/v1/orders/abc", "", map[string]string{
		"X-API-Key": "cp_revoked",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key want 401, got %d", rec.Code)
	}
	// The response must not disclose that the key was specifically revoked.
	assertErrorCode(t, rec, "unauthorized")
}

func TestAPIKeyAuthExpiredKeyReturns401(t *testing.T) {
	resolver := &fakeResolver{err: map[string]error{"cp_expired": auth.ErrKeyExpired}}
	router, _ := newAPIKeyHarness(t, mini(t), storeFromResolver{resolver})

	rec := do(router, http.MethodGet, "/v1/orders/abc", "", map[string]string{
		"X-API-Key": "cp_expired",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key want 401, got %d", rec.Code)
	}
}

func TestAPIKeyAuthUnknownKeyReturns401(t *testing.T) {
	router, stub := newAPIKeyHarness(t, mini(t), storeFromResolver{&fakeResolver{}})

	rec := do(router, http.MethodGet, "/v1/orders/abc", "", map[string]string{
		"X-API-Key": "cp_nope",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key want 401, got %d", rec.Code)
	}
	if stub.Calls.Load() != 0 {
		t.Fatal("handler must not run for an unknown key")
	}
}

// A request with no credential at all is rejected before the key path.
func TestNoCredentialReturns401(t *testing.T) {
	router, _ := newAPIKeyHarness(t, mini(t), storeFromResolver{&fakeResolver{}})
	rec := do(router, http.MethodGet, "/v1/orders/abc", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential want 401, got %d", rec.Code)
	}
}
