package middleware

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/internal/auth"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
	"github.com/nickemma/chainpulse/shared/logger"
)

// HeaderAPIKey is the header partners present their API key in. It is kept
// distinct from Authorization so the credential type is never ambiguous: a
// Bearer token is always a JWT, an X-API-Key is always a partner key.
const HeaderAPIKey = "X-API-Key"

// Authenticate establishes the request's trusted Identity, accepting either a
// Bearer JWT (users) or an X-API-Key (partners). This is the system's single
// trust boundary: every downstream component reads identity.FromContext /
// identity.FromGin and never re-parses a credential. Any failure — missing,
// expired, malformed, revoked — returns 401.
//
// keys may be nil (e.g. when no Postgres-backed store is configured), in which
// case only the JWT path is available and an X-API-Key request is rejected.
func Authenticate(issuer *auth.Issuer, keys auth.APIKeyResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := resolveIdentity(c, issuer, keys)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrExpired), errors.Is(err, auth.ErrKeyExpired):
				httpx.Unauthorized(c, "credential expired")
			case errors.Is(err, errNoCredential):
				httpx.Unauthorized(c, "missing Authorization or X-API-Key")
			default:
				// Do not disclose whether a key is unknown vs revoked.
				httpx.Unauthorized(c, "invalid credential")
			}
			return
		}

		// Make the identity available to handlers (gin) and to context-based
		// consumers (policy engine), and enrich the request logger.
		c.Set(identity.GinKey, id)
		ctx := identity.WithContext(c.Request.Context(), id)
		reqLogger := logger.FromContext(ctx).With(
			"tenant_id", id.TenantID, "actor_id", id.UserID, "role", string(id.Role),
		)
		ctx = logger.WithContext(ctx, reqLogger)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}

// errNoCredential signals that the request carried neither a Bearer token nor
// an API key — distinct from a credential that was present but invalid.
var errNoCredential = errors.New("no credential presented")

// resolveIdentity picks the credential path: a Bearer JWT takes precedence; if
// absent, an X-API-Key is resolved against the store (when one is configured).
func resolveIdentity(c *gin.Context, issuer *auth.Issuer, keys auth.APIKeyResolver) (identity.Identity, error) {
	if token, ok := bearerToken(c.GetHeader("Authorization")); ok {
		return issuer.Validate(token)
	}
	if key := c.GetHeader(HeaderAPIKey); key != "" {
		if keys == nil {
			return identity.Identity{}, auth.ErrKeyNotFound
		}
		return keys.Resolve(c.Request.Context(), key)
	}
	return identity.Identity{}, errNoCredential
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, tolerating case and surrounding whitespace.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
