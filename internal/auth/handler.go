package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
)

// Handler exposes the auth HTTP endpoints. In production, token issuance is
// fronted by a real identity provider; the /token endpoint here is the
// development issuance path the roadmap calls for, so the system is testable
// end-to-end without external auth infrastructure.
type Handler struct {
	issuer *Issuer
}

func NewHandler(issuer *Issuer) *Handler {
	return &Handler{issuer: issuer}
}

// Register wires the auth routes onto the given router group.
func (h *Handler) Register(r gin.IRouter) {
	r.POST("/v1/auth/token", h.issueToken)
	r.POST("/v1/auth/refresh", h.refresh)
}

// tokenRequest is the dev issuance payload. tenant_id and role are required;
// user_id is generated if omitted so quick manual testing needs less typing.
type tokenRequest struct {
	TenantID   string `json:"tenant_id"`
	UserID     string `json:"user_id"`
	Role       string `json:"role"`
	Region     string `json:"region"`
	SupplierID string `json:"supplier_id"`
}

func (h *Handler) issueToken(c *gin.Context) {
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "invalid JSON body")
		return
	}

	if req.TenantID == "" {
		httpx.BadRequest(c, "tenant_id is required")
		return
	}
	role := identity.Role(req.Role)
	if !role.Valid() {
		httpx.BadRequest(c, "role must be one of: admin, supplier, warehouse_manager, analyst")
		return
	}
	userID := req.UserID
	if userID == "" {
		userID = uuid.NewString()
	}

	pair, err := h.issuer.Issue(identity.Identity{
		TenantID:   req.TenantID,
		UserID:     userID,
		Role:       role,
		Region:     req.Region,
		SupplierID: req.SupplierID,
	})
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, pair)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.RefreshToken == "" {
		httpx.BadRequest(c, "refresh_token is required")
		return
	}

	pair, err := h.issuer.Refresh(req.RefreshToken)
	if err != nil {
		switch {
		case errors.Is(err, ErrExpired):
			httpx.Unauthorized(c, "refresh token expired")
		case errors.Is(err, ErrWrongTokenUse):
			httpx.Unauthorized(c, "not a refresh token")
		default:
			httpx.Unauthorized(c, "invalid refresh token")
		}
		return
	}
	c.JSON(http.StatusOK, pair)
}
