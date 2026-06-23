package auth

import (
	"strings"
	"testing"

	"github.com/nickemma/chainpulse/shared/identity"
)

func TestGenerateAPIKeyShape(t *testing.T) {
	plaintext, hash, prefix, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(plaintext, APIKeyDisplayPrefix) {
		t.Fatalf("plaintext missing %q prefix: %q", APIKeyDisplayPrefix, plaintext)
	}
	// "cp_" + 64 hex chars of 32 random bytes.
	if got := len(plaintext); got != len(APIKeyDisplayPrefix)+64 {
		t.Fatalf("unexpected key length %d", got)
	}
	if hashAPIKey(plaintext) != hash {
		t.Fatal("returned hash does not match hash of plaintext")
	}
	if !strings.HasPrefix(plaintext, prefix) {
		t.Fatalf("display prefix %q is not a prefix of the key", prefix)
	}
	if len(prefix) >= len(plaintext) {
		t.Fatal("display prefix must be far shorter than the key")
	}
}

func TestGenerateAPIKeyIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		pt, _, _, err := generateAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[pt] {
			t.Fatal("duplicate key generated")
		}
		seen[pt] = true
	}
}

func TestHashAPIKeyDeterministicAndDistinct(t *testing.T) {
	if hashAPIKey("cp_abc") != hashAPIKey("cp_abc") {
		t.Fatal("hash is not deterministic")
	}
	if hashAPIKey("cp_abc") == hashAPIKey("cp_abd") {
		t.Fatal("distinct keys produced the same hash")
	}
	if !constantTimeEqual(hashAPIKey("cp_abc"), hashAPIKey("cp_abc")) {
		t.Fatal("constantTimeEqual rejected equal hashes")
	}
}

// Guard: the role check in Resolve relies on identity.Role.Valid, so a key
// minted with a bad role is impossible through Create.
func TestCreateRejectsInvalidRole(t *testing.T) {
	s := &PostgresAPIKeyStore{} // no pool needed; validation happens first
	_, _, err := s.Create(t.Context(), CreateAPIKeySpec{TenantID: "t", Role: identity.Role("root")})
	if err == nil {
		t.Fatal("expected error for invalid role")
	}
}
