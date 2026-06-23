package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickemma/chainpulse/shared/identity"
)

// API keys are the second authentication path at the gateway (JWTs are the
// first). External partners present a long-lived, rotatable key instead of a
// short-lived token. The gateway resolves the key to the same Identity the JWT
// path produces, so every downstream component — policy engine, audit, modules
// — is unaware of which credential was used.

// APIKeyDisplayPrefix is the human-visible, non-secret prefix every key carries
// so it can be identified in logs and dashboards without revealing the secret.
const APIKeyDisplayPrefix = "cp_"

// apiKeyRandomBytes is the entropy of the secret portion. 32 bytes (256 bits)
// is well beyond brute-force reach and matches the SHA-256 digest width.
const apiKeyRandomBytes = 32

// Sentinel errors for the API key resolution path. All map to 401 at the edge;
// the gateway deliberately does not tell the caller which one occurred, so a
// probe cannot distinguish "unknown key" from "revoked key".
var (
	ErrKeyNotFound = errors.New("api key not found")
	ErrKeyRevoked  = errors.New("api key revoked")
	ErrKeyExpired  = errors.New("api key expired")
)

// APIKey is the non-secret metadata of a stored key. The plaintext secret is
// never held here — it exists only at creation time, returned once to the
// caller and then discarded.
type APIKey struct {
	ID         string
	TenantID   string
	Name       string
	KeyPrefix  string // e.g. "cp_a1b2c3d4"
	Role       identity.Role
	Region     string
	SupplierID string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// CreateAPIKeySpec describes a key to mint. TenantID and Role are required;
// Region/SupplierID scope the resulting Identity exactly as JWT claims do.
type CreateAPIKeySpec struct {
	TenantID   string
	Name       string
	Role       identity.Role
	Region     string
	SupplierID string
	ExpiresAt  *time.Time // nil = never expires
}

// APIKeyResolver turns a presented key string into a trusted Identity. The
// gateway's auth middleware depends only on this narrow interface.
type APIKeyResolver interface {
	Resolve(ctx context.Context, presentedKey string) (identity.Identity, error)
}

// APIKeyStore is the full lifecycle: resolution plus management (create,
// rotate, revoke, list). The management handler depends on this; the auth
// middleware depends only on the embedded APIKeyResolver.
type APIKeyStore interface {
	APIKeyResolver
	// Create mints a new key, returning its metadata and the one-time plaintext
	// secret. The plaintext is never persisted and never recoverable after this.
	Create(ctx context.Context, spec CreateAPIKeySpec) (APIKey, string, error)
	// Rotate revokes an existing key and issues a replacement carrying the same
	// scope (role/region/supplier). Returns the new key and its plaintext.
	Rotate(ctx context.Context, tenantID, id string) (APIKey, string, error)
	// Revoke marks a key revoked. A revoked key never resolves again.
	Revoke(ctx context.Context, tenantID, id string) error
	// List returns a tenant's keys (metadata only, no secrets).
	List(ctx context.Context, tenantID string) ([]APIKey, error)
}

// generateAPIKey mints a fresh plaintext key and its derived hash + display
// prefix. The plaintext is "cp_" followed by 64 hex chars of randomness.
func generateAPIKey() (plaintext, hash, prefix string, err error) {
	buf := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("generate api key entropy: %w", err)
	}
	plaintext = APIKeyDisplayPrefix + hex.EncodeToString(buf)
	hash = hashAPIKey(plaintext)
	prefix = displayPrefix(plaintext)
	return plaintext, hash, prefix, nil
}

// hashAPIKey returns the hex SHA-256 of a key. Lookups hash the presented key
// and match against the stored hash, so the plaintext never has to be stored.
func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqual compares two hex hashes without leaking timing. Lookups are
// by indexed hash equality, but defense-in-depth: any in-memory comparison of a
// secret-derived value uses a constant-time compare.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// displayPrefix is the non-secret leading segment used for identification:
// "cp_" plus the first 8 hex chars. Enough to recognise a key, far too little
// to reconstruct it.
func displayPrefix(plaintext string) string {
	body := strings.TrimPrefix(plaintext, APIKeyDisplayPrefix)
	n := 8
	if len(body) < n {
		n = len(body)
	}
	return APIKeyDisplayPrefix + body[:n]
}
