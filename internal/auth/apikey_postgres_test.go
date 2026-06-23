package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nickemma/chainpulse/shared/identity"
)

// newTestStore connects to the test Postgres (POSTGRES_DSN or the compose
// default) and returns a store plus a fresh tenant id to scope the test. If no
// database is reachable the test is skipped — these are integration tests, run
// when `make cluster-up` has brought Postgres up and migrations have run.
func newTestStore(t *testing.T) (*PostgresAPIKeyStore, string) {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://chainpulse:chainpulse@localhost:5432/chainpulse?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no postgres pool (%v) — skipping integration test", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (%v) — skipping integration test", err)
	}
	t.Cleanup(pool.Close)

	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`,
		"apikey-test-"+uuid.NewString(),
	).Scan(&tenantID); err != nil {
		t.Skipf("cannot create tenant (migrations applied?): %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	return NewPostgresAPIKeyStore(pool), tenantID
}

func TestPostgresCreateAndResolve(t *testing.T) {
	store, tenant := newTestStore(t)
	ctx := context.Background()

	key, plaintext, err := store.Create(ctx, CreateAPIKeySpec{
		TenantID: tenant, Name: "partner-a", Role: identity.RoleSupplier, SupplierID: "sup-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plaintext == "" || key.KeyPrefix == "" {
		t.Fatal("create must return a plaintext secret and a display prefix")
	}

	id, err := store.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatalf("resolve valid key: %v", err)
	}
	if id.TenantID != tenant || id.Role != identity.RoleSupplier || id.SupplierID != "sup-1" {
		t.Fatalf("resolved identity wrong: %+v", id)
	}
	// The wrong key must never resolve.
	if _, err := store.Resolve(ctx, plaintext+"x"); err != ErrKeyNotFound {
		t.Fatalf("tampered key: want ErrKeyNotFound, got %v", err)
	}
}

func TestPostgresRevokeStopsResolution(t *testing.T) {
	store, tenant := newTestStore(t)
	ctx := context.Background()

	key, plaintext, err := store.Create(ctx, CreateAPIKeySpec{
		TenantID: tenant, Name: "to-revoke", Role: identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Resolve(ctx, plaintext); err != nil {
		t.Fatalf("key should resolve before revoke: %v", err)
	}

	if err := store.Revoke(ctx, tenant, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.Resolve(ctx, plaintext); err != ErrKeyRevoked {
		t.Fatalf("after revoke: want ErrKeyRevoked, got %v", err)
	}
	// Revoking again is a not-found (already revoked / nothing to do).
	if err := store.Revoke(ctx, tenant, key.ID); err != ErrKeyNotFound {
		t.Fatalf("double revoke: want ErrKeyNotFound, got %v", err)
	}
}

func TestPostgresExpiredKeyDoesNotResolve(t *testing.T) {
	store, tenant := newTestStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	_, plaintext, err := store.Create(ctx, CreateAPIKeySpec{
		TenantID: tenant, Name: "expired", Role: identity.RoleAnalyst, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Resolve(ctx, plaintext); err != ErrKeyExpired {
		t.Fatalf("expired key: want ErrKeyExpired, got %v", err)
	}
}

func TestPostgresRotateRevokesOldIssuesNew(t *testing.T) {
	store, tenant := newTestStore(t)
	ctx := context.Background()

	orig, origPlain, err := store.Create(ctx, CreateAPIKeySpec{
		TenantID: tenant, Name: "rotating", Role: identity.RoleWarehouseManager, Region: "ontario",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	rotated, newPlain, err := store.Rotate(ctx, tenant, orig.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.ID == orig.ID || newPlain == origPlain {
		t.Fatal("rotate must produce a new key id and secret")
	}
	// Scope is carried across the rotation.
	if rotated.Region != "ontario" || rotated.Role != identity.RoleWarehouseManager {
		t.Fatalf("rotated key lost its scope: %+v", rotated)
	}

	// Old secret is dead; new secret resolves.
	if _, err := store.Resolve(ctx, origPlain); err != ErrKeyRevoked {
		t.Fatalf("old key after rotate: want ErrKeyRevoked, got %v", err)
	}
	id, err := store.Resolve(ctx, newPlain)
	if err != nil {
		t.Fatalf("new key after rotate: %v", err)
	}
	if id.Region != "ontario" {
		t.Fatalf("resolved rotated identity wrong: %+v", id)
	}
}

// Tenant isolation: a key belongs to its tenant, and another tenant cannot
// revoke or rotate it (the scoped query simply doesn't find it).
func TestPostgresKeysAreTenantScoped(t *testing.T) {
	store, tenantA := newTestStore(t)
	_, tenantB := newTestStore(t) // a second tenant on the same database
	ctx := context.Background()

	key, _, err := store.Create(ctx, CreateAPIKeySpec{
		TenantID: tenantA, Name: "owned-by-a", Role: identity.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Revoke(ctx, tenantB, key.ID); err != ErrKeyNotFound {
		t.Fatalf("cross-tenant revoke: want ErrKeyNotFound, got %v", err)
	}
	if _, _, err := store.Rotate(ctx, tenantB, key.ID); err != ErrKeyNotFound {
		t.Fatalf("cross-tenant rotate: want ErrKeyNotFound, got %v", err)
	}
}
