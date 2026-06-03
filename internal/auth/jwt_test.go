package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nickemma/chainpulse/shared/identity"
)

func testIdentity() identity.Identity {
	return identity.Identity{
		TenantID:   "acme",
		UserID:     "user-1",
		Role:       identity.RoleWarehouseManager,
		Region:     "ontario",
		SupplierID: "",
	}
}

func TestIssueAndValidateRoundTrip(t *testing.T) {
	iss := NewIssuer("secret", 15*time.Minute, 24*time.Hour)
	pair, err := iss.Issue(testIdentity())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := iss.Validate(pair.AccessToken)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got != testIdentity() {
		t.Fatalf("identity round-trip mismatch: got %+v", got)
	}
}

// Roadmap failure test: "A request with an expired JWT returns 401."
// This is the engine-level half — Validate must report ErrExpired.
func TestValidateRejectsExpiredToken(t *testing.T) {
	iss := NewIssuer("secret", 15*time.Minute, 24*time.Hour)
	// Pin issuance one hour in the past so the 15m access token is expired.
	iss.now = func() time.Time { return time.Now().Add(-time.Hour) }
	pair, err := iss.Issue(testIdentity())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Validate at the real present time.
	iss.now = time.Now
	if _, err := iss.Validate(pair.AccessToken); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestRefreshTokenCannotBeUsedAsAccess(t *testing.T) {
	iss := NewIssuer("secret", 15*time.Minute, 24*time.Hour)
	pair, _ := iss.Issue(testIdentity())

	if _, err := iss.Validate(pair.RefreshToken); !errors.Is(err, ErrWrongTokenUse) {
		t.Fatalf("expected ErrWrongTokenUse, got %v", err)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	iss := NewIssuer("secret", 15*time.Minute, 24*time.Hour)
	pair, _ := iss.Issue(testIdentity())

	// Mutate the first character of the signature segment. The first base64
	// char encodes a full byte, so this always changes the decoded signature
	// (unlike the last char, whose low bits are insignificant padding).
	parts := strings.Split(pair.AccessToken, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	tampered := strings.Join(parts, ".")

	if _, err := iss.Validate(tampered); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}

func TestTokenSignedWithDifferentSecretRejected(t *testing.T) {
	issuer := NewIssuer("secret-A", 15*time.Minute, 24*time.Hour)
	pair, _ := issuer.Issue(testIdentity())

	other := NewIssuer("secret-B", 15*time.Minute, 24*time.Hour)
	if _, err := other.Validate(pair.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for foreign secret, got %v", err)
	}
}

// Defense against the alg-confusion / "alg:none" family: a token whose header
// declares a non-HMAC algorithm must be rejected by the keyfunc.
func TestNonHMACAlgorithmRejected(t *testing.T) {
	iss := NewIssuer("secret", 15*time.Minute, 24*time.Hour)
	// Craft an unsigned ("none") token with otherwise-valid claims.
	claims := Claims{TenantID: "acme", Role: "admin", TokenType: tokenTypeAccess}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("craft none token: %v", err)
	}
	if _, err := iss.Validate(raw); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for alg:none, got %v", err)
	}
}
