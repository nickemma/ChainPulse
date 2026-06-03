// Package auth issues and validates JWTs. The gateway is the single trust
// boundary: tokens are minted here (or by an external IdP in production) and
// validated by the gateway's auth middleware. No module ever parses a token.
//
// Tokens are signed with HMAC-SHA256 using the secret from config. The README
// describes RS256 as the production target; HMAC is used here because the
// system is a single binary with a shared secret and no need to distribute a
// public key. Swapping to RS256 later is a change isolated to this package —
// the Identity contract the rest of the system depends on does not change.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nickemma/chainpulse/shared/identity"
)

// Token type discriminator carried in the claims. An access token cannot be
// used where a refresh token is expected, and vice versa.
const (
	tokenTypeAccess  = "access"
	tokenTypeRefresh = "refresh"
)

// Claims is the JWT payload. Standard registered claims (exp, iat, sub) are
// embedded; the custom claims carry the tenant scoping the policy engine needs.
type Claims struct {
	TenantID   string `json:"tenant_id"`
	Role       string `json:"role"`
	Region     string `json:"region,omitempty"`
	SupplierID string `json:"supplier_id,omitempty"`
	TokenType  string `json:"token_type"`
	jwt.RegisteredClaims
}

// TokenPair is what the issuance and refresh endpoints return.
type TokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"` // OAuth-style: always "Bearer"
	ExpiresAt    time.Time `json:"expires_at"`
}

// Sentinel errors so the middleware can map causes to the right HTTP status
// and message without string matching.
var (
	ErrExpired       = errors.New("token expired")
	ErrInvalidToken  = errors.New("invalid token")
	ErrWrongTokenUse = errors.New("token used for wrong purpose")
)

// Issuer mints and validates tokens. It is safe for concurrent use.
type Issuer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time // injectable for deterministic tests
}

// NewIssuer constructs an Issuer. An empty secret is a programming error and
// panics — config.Load already requires JWT_SECRET, so this never fires in a
// correctly-configured process.
func NewIssuer(secret string, accessTTL, refreshTTL time.Duration) *Issuer {
	if secret == "" {
		panic("auth: empty JWT secret")
	}
	return &Issuer{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}
}

// Issue mints an access+refresh pair for the given identity. The role is
// validated so an unknown role can never be embedded in a token and reach the
// policy engine.
func (i *Issuer) Issue(id identity.Identity) (TokenPair, error) {
	if !id.Role.Valid() {
		return TokenPair{}, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, id.Role)
	}
	if id.TenantID == "" || id.UserID == "" {
		return TokenPair{}, fmt.Errorf("%w: tenant_id and user_id are required", ErrInvalidToken)
	}

	now := i.now()
	accessExp := now.Add(i.accessTTL)

	access, err := i.sign(id, tokenTypeAccess, now, accessExp)
	if err != nil {
		return TokenPair{}, err
	}
	refresh, err := i.sign(id, tokenTypeRefresh, now, now.Add(i.refreshTTL))
	if err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresAt:    accessExp,
	}, nil
}

func (i *Issuer) sign(id identity.Identity, tokenType string, iat, exp time.Time) (string, error) {
	claims := Claims{
		TenantID:   id.TenantID,
		Role:       string(id.Role),
		Region:     id.Region,
		SupplierID: id.SupplierID,
		TokenType:  tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.UserID,
			Issuer:    "chainpulse",
			IssuedAt:  jwt.NewNumericDate(iat),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(i.secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// Validate parses and verifies an access token, returning the trusted
// Identity. It rejects expired tokens, tokens signed with the wrong algorithm
// (alg-confusion defense), and refresh tokens presented as access tokens.
func (i *Issuer) Validate(tokenString string) (identity.Identity, error) {
	claims, err := i.parse(tokenString)
	if err != nil {
		return identity.Identity{}, err
	}
	if claims.TokenType != tokenTypeAccess {
		return identity.Identity{}, ErrWrongTokenUse
	}
	return i.toIdentity(claims)
}

// Refresh validates a refresh token and issues a fresh token pair. An access
// token presented here is rejected.
func (i *Issuer) Refresh(refreshToken string) (TokenPair, error) {
	claims, err := i.parse(refreshToken)
	if err != nil {
		return TokenPair{}, err
	}
	if claims.TokenType != tokenTypeRefresh {
		return TokenPair{}, ErrWrongTokenUse
	}
	id, err := i.toIdentity(claims)
	if err != nil {
		return TokenPair{}, err
	}
	return i.Issue(id)
}

// parse verifies the signature and standard claims, mapping library errors to
// our sentinels.
func (i *Issuer) parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		// Reject any algorithm other than HMAC — defends against the classic
		// "alg: none" and RS256/HS256 confusion attacks.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, t.Header["alg"])
		}
		return i.secret, nil
	}, jwt.WithTimeFunc(i.now))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return claims, nil
}

func (i *Issuer) toIdentity(c *Claims) (identity.Identity, error) {
	role := identity.Role(c.Role)
	if !role.Valid() {
		return identity.Identity{}, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, c.Role)
	}
	return identity.Identity{
		TenantID:   c.TenantID,
		UserID:     c.Subject,
		Role:       role,
		Region:     c.Region,
		SupplierID: c.SupplierID,
	}, nil
}
