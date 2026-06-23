package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nickemma/chainpulse/shared/identity"
)

// PostgresAPIKeyStore is the durable APIKeyStore. It is the system of record for
// partner credentials; the gateway resolves against it on every API-key
// request. Safe for concurrent use (pgxpool is).
type PostgresAPIKeyStore struct {
	pool *pgxpool.Pool
	now  func() time.Time // injectable for deterministic tests
}

// NewPostgresAPIKeyStore wires a store onto an existing pool.
func NewPostgresAPIKeyStore(pool *pgxpool.Pool) *PostgresAPIKeyStore {
	return &PostgresAPIKeyStore{pool: pool, now: time.Now}
}

var _ APIKeyStore = (*PostgresAPIKeyStore)(nil)

// Resolve hashes the presented key, looks it up by its unique hash, and returns
// the scoped Identity if the key is active and unexpired. It fails closed: any
// lookup error, a revoked key, or an expired key yields a sentinel error that
// the middleware maps to 401 without disclosing which condition matched.
func (s *PostgresAPIKeyStore) Resolve(ctx context.Context, presentedKey string) (identity.Identity, error) {
	hash := hashAPIKey(presentedKey)

	var (
		id, tenantID, role string
		region, supplier   *string
		storedHash         string
		expiresAt          *time.Time
		revokedAt          *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, role, region, supplier_id, key_hash, expires_at, revoked_at
		FROM api_keys
		WHERE key_hash = $1`, hash,
	).Scan(&id, &tenantID, &role, &region, &supplier, &storedHash, &expiresAt, &revokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return identity.Identity{}, ErrKeyNotFound
		}
		return identity.Identity{}, fmt.Errorf("resolve api key: %w", err)
	}

	// Defense-in-depth: the DB matched on hash equality, but confirm in
	// constant time before trusting the row.
	if !constantTimeEqual(hash, storedHash) {
		return identity.Identity{}, ErrKeyNotFound
	}
	if revokedAt != nil {
		return identity.Identity{}, ErrKeyRevoked
	}
	if expiresAt != nil && !expiresAt.After(s.now()) {
		return identity.Identity{}, ErrKeyExpired
	}

	r := identity.Role(role)
	if !r.Valid() {
		// A stored key with an unknown role can never authorize anything.
		return identity.Identity{}, ErrKeyNotFound
	}

	// Best-effort last-used stamp; never fail the request if it doesn't land.
	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)

	return identity.Identity{
		TenantID:   tenantID,
		UserID:     id, // the key's own id is the actor for audit purposes
		Role:       r,
		Region:     deref(region),
		SupplierID: deref(supplier),
	}, nil
}

// Create mints and persists a key, returning metadata and the one-time secret.
func (s *PostgresAPIKeyStore) Create(ctx context.Context, spec CreateAPIKeySpec) (APIKey, string, error) {
	if spec.TenantID == "" || !spec.Role.Valid() {
		return APIKey{}, "", fmt.Errorf("%w: tenant_id and a valid role are required", ErrInvalidToken)
	}

	plaintext, hash, prefix, err := generateAPIKey()
	if err != nil {
		return APIKey{}, "", err
	}

	var (
		id        string
		createdAt time.Time
	)
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (tenant_id, name, key_hash, key_prefix, role, region, supplier_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`,
		spec.TenantID, spec.Name, hash, prefix, string(spec.Role),
		nullify(spec.Region), nullify(spec.SupplierID), spec.ExpiresAt,
	).Scan(&id, &createdAt)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("insert api key: %w", err)
	}

	return APIKey{
		ID:         id,
		TenantID:   spec.TenantID,
		Name:       spec.Name,
		KeyPrefix:  prefix,
		Role:       spec.Role,
		Region:     spec.Region,
		SupplierID: spec.SupplierID,
		CreatedAt:  createdAt,
		ExpiresAt:  spec.ExpiresAt,
	}, plaintext, nil
}

// Rotate revokes the named key and issues a replacement with the same scope, in
// a single transaction so there is never a window with zero or two live keys.
func (s *PostgresAPIKeyStore) Rotate(ctx context.Context, tenantID, id string) (APIKey, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		name, role       string
		region, supplier *string
		expiresAt        *time.Time
		revokedAt        *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT name, role, region, supplier_id, expires_at, revoked_at
		FROM api_keys WHERE id = $1 AND tenant_id = $2
		FOR UPDATE`, id, tenantID,
	).Scan(&name, &role, &region, &supplier, &expiresAt, &revokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIKey{}, "", ErrKeyNotFound
		}
		return APIKey{}, "", fmt.Errorf("load key for rotate: %w", err)
	}
	if revokedAt != nil {
		return APIKey{}, "", ErrKeyRevoked
	}

	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1`, id); err != nil {
		return APIKey{}, "", fmt.Errorf("revoke old key: %w", err)
	}

	plaintext, hash, prefix, err := generateAPIKey()
	if err != nil {
		return APIKey{}, "", err
	}
	var (
		newID     string
		createdAt time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO api_keys (tenant_id, name, key_hash, key_prefix, role, region, supplier_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`,
		tenantID, name, hash, prefix, role, region, supplier, expiresAt,
	).Scan(&newID, &createdAt)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("insert rotated key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, "", fmt.Errorf("commit rotate: %w", err)
	}

	return APIKey{
		ID:         newID,
		TenantID:   tenantID,
		Name:       name,
		KeyPrefix:  prefix,
		Role:       identity.Role(role),
		Region:     deref(region),
		SupplierID: deref(supplier),
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}, plaintext, nil
}

// Revoke marks a key revoked. Revoking an already-revoked or unknown key is a
// no-op error so callers can be idempotent.
func (s *PostgresAPIKeyStore) Revoke(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now()
		 WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL`, id, tenantID)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// List returns a tenant's keys, newest first. Secrets are never returned.
func (s *PostgresAPIKeyStore) List(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, key_prefix, role, region, supplier_id,
		       created_at, last_used_at, expires_at, revoked_at
		FROM api_keys WHERE tenant_id = $1
		ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var (
			k                APIKey
			role             string
			region, supplier *string
		)
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &role, &region, &supplier,
			&k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		k.TenantID = tenantID
		k.Role = identity.Role(role)
		k.Region = deref(region)
		k.SupplierID = deref(supplier)
		out = append(out, k)
	}
	return out, rows.Err()
}

// nullify maps an empty string to nil so optional columns store SQL NULL rather
// than an empty string (keeps region/supplier semantics clean).
func nullify(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
