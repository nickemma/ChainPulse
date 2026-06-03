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

// Authenticate validates the Bearer JWT and attaches the trusted Identity to
// the request. This is the system's single trust boundary: every downstream
// component reads identity.FromContext / identity.FromGin and never re-parses
// a token. An expired token returns 401; a malformed or wrongly-signed token
// returns 401.
func Authenticate(issuer *auth.Issuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := bearerToken(header)
		if !ok {
			httpx.Unauthorized(c, "missing or malformed Authorization header")
			return
		}

		id, err := issuer.Validate(token)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrExpired):
				httpx.Unauthorized(c, "token expired")
			default:
				httpx.Unauthorized(c, "invalid token")
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
