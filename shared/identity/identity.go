// Package identity defines the authenticated caller identity that flows from
// the gateway (where the JWT is validated) through the policy engine and into
// the modules. The JWT is validated exactly once, at the edge; everything
// downstream reads a trusted Identity from the request context and never
// re-parses a token.
package identity

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
)

// Role is the RBAC role carried in the JWT. The set is closed — the policy
// engine's rules are written against exactly these four roles.
type Role string

const (
	RoleAdmin            Role = "admin"
	RoleSupplier         Role = "supplier"
	RoleWarehouseManager Role = "warehouse_manager"
	RoleAnalyst          Role = "analyst"
)

// Valid reports whether r is one of the known roles. Unknown roles are
// rejected at token issuance so they can never reach the policy engine.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleSupplier, RoleWarehouseManager, RoleAnalyst:
		return true
	default:
		return false
	}
}

// Identity is the trusted caller, derived from a validated JWT. Every field
// that downstream authorization depends on lives here. SupplierID and Region
// are optional and empty for roles that don't carry them.
type Identity struct {
	TenantID   string
	UserID     string
	Role       Role
	Region     string
	SupplierID string
}

// ctxKey is unexported to prevent collisions with other packages' context keys.
type ctxKey int

const identityKey ctxKey = iota

// ErrNoIdentity is returned when a handler expects an authenticated identity
// but none is present — this indicates a routing/middleware bug, not a client
// error, because unauthenticated requests should be rejected before any
// handler runs.
var ErrNoIdentity = errors.New("no identity in context")

// WithContext stores the identity in a context.
func WithContext(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// FromContext extracts the identity from a context.
func FromContext(ctx context.Context) (Identity, error) {
	id, ok := ctx.Value(identityKey).(Identity)
	if !ok {
		return Identity{}, ErrNoIdentity
	}
	return id, nil
}

// GinKey is the gin context key under which the identity is stored by the auth
// middleware, for handlers that work with *gin.Context directly.
const GinKey = "identity"

// FromGin extracts the identity from a gin context.
func FromGin(c *gin.Context) (Identity, bool) {
	v, ok := c.Get(GinKey)
	if !ok {
		return Identity{}, false
	}
	id, ok := v.(Identity)
	return id, ok
}
