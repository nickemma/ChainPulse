package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
)

// APIKeyHandler exposes partner-key management. Every endpoint is mounted under
// /v1 behind the gateway's auth + policy pipeline, so the caller is already an
// authenticated admin of the tenant by the time these run. Keys are always
// scoped to the caller's tenant — a request can never mint a key for another
// tenant, even with a forged body.
type APIKeyHandler struct {
	store APIKeyStore
}

func NewAPIKeyHandler(store APIKeyStore) *APIKeyHandler {
	return &APIKeyHandler{store: store}
}

type createKeyRequest struct {
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Region     string     `json:"region"`
	SupplierID string     `json:"supplier_id"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// keyResponse is the metadata view. Plaintext is set only on create/rotate and
// is the single moment the secret is ever returned.
type keyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"key_prefix"`
	Role       string     `json:"role"`
	Region     string     `json:"region,omitempty"`
	SupplierID string     `json:"supplier_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Plaintext  string     `json:"api_key,omitempty"` // shown once, on create/rotate
}

func toKeyResponse(k APIKey, plaintext string) keyResponse {
	return keyResponse{
		ID:         k.ID,
		Name:       k.Name,
		KeyPrefix:  k.KeyPrefix,
		Role:       string(k.Role),
		Region:     k.Region,
		SupplierID: k.SupplierID,
		CreatedAt:  k.CreatedAt,
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		RevokedAt:  k.RevokedAt,
		Plaintext:  plaintext,
	}
}

func (h *APIKeyHandler) Create(c *gin.Context) {
	actor, ok := identity.FromGin(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	var req createKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "invalid JSON body")
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpx.BadRequest(c, "role must be one of: admin, supplier, warehouse_manager, analyst")
		return
	}

	key, plaintext, err := h.store.Create(c.Request.Context(), CreateAPIKeySpec{
		TenantID:   actor.TenantID, // always the caller's tenant
		Name:       req.Name,
		Role:       role,
		Region:     req.Region,
		SupplierID: req.SupplierID,
		ExpiresAt:  req.ExpiresAt,
	})
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusCreated, toKeyResponse(key, plaintext))
}

func (h *APIKeyHandler) List(c *gin.Context) {
	actor, ok := identity.FromGin(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	keys, err := h.store.List(c.Request.Context(), actor.TenantID)
	if err != nil {
		httpx.Internal(c, "failed to list api keys")
		return
	}
	out := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyResponse(k, ""))
	}
	c.JSON(http.StatusOK, gin.H{"keys": out})
}

func (h *APIKeyHandler) Rotate(c *gin.Context) {
	actor, ok := identity.FromGin(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	key, plaintext, err := h.store.Rotate(c.Request.Context(), actor.TenantID, c.Param("id"))
	if err != nil {
		h.writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toKeyResponse(key, plaintext))
}

func (h *APIKeyHandler) Revoke(c *gin.Context) {
	actor, ok := identity.FromGin(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	if err := h.store.Revoke(c.Request.Context(), actor.TenantID, c.Param("id")); err != nil {
		h.writeStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// writeStoreError maps store sentinels to HTTP. A key the tenant doesn't own
// resolves to ErrKeyNotFound (scoped query), so this is also the cross-tenant
// "not found" response — a tenant can never learn another tenant's key IDs.
func (h *APIKeyHandler) writeStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrKeyNotFound):
		httpx.Abort(c, http.StatusNotFound, "not_found", "api key not found")
	case errors.Is(err, ErrKeyRevoked):
		httpx.Abort(c, http.StatusConflict, httpx.CodeConflict, "api key already revoked")
	default:
		httpx.Internal(c, "api key operation failed")
	}
}
