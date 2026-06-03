// Package httpx defines the canonical error shape returned by every HTTP
// endpoint in ChainPulse. A consistent error envelope means clients (and the
// audit trail) can rely on a stable structure regardless of which layer
// rejected the request — the gateway, the policy engine, or a module.
package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ErrorBody is the JSON envelope returned for every non-2xx response.
//
//	{ "error": { "code": "forbidden", "message": "...", "request_id": "..." } }
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a stable machine code, a human message, and the request
// ID so a client report can be correlated with a server-side log line.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// Stable error codes. These are part of the API contract — do not rename them
// casually; clients and tests match on them.
const (
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeRateLimited  = "rate_limited"
	CodeConflict     = "conflict"
	CodeBadRequest   = "bad_request"
	CodeInternal     = "internal_error"
	CodeUnavailable  = "service_unavailable"
	CodeIdempotency  = "idempotency_key_required"
)

// Abort writes the canonical error envelope and stops the gin handler chain.
// It pulls the request ID from the context if one is set.
func Abort(c *gin.Context, status int, code, message string) {
	reqID, _ := c.Get(RequestIDKey)
	rid, _ := reqID.(string)
	c.AbortWithStatusJSON(status, ErrorBody{
		Error: ErrorDetail{
			Code:      code,
			Message:   message,
			RequestID: rid,
		},
	})
}

// RequestIDKey is the gin context key under which the per-request correlation
// ID is stored. Defined here so both httpx and the middleware agree on it.
const RequestIDKey = "request_id"

// Common shorthands for the cases the gateway hits most.

func Unauthorized(c *gin.Context, msg string) {
	Abort(c, http.StatusUnauthorized, CodeUnauthorized, msg)
}
func Forbidden(c *gin.Context, msg string)  { Abort(c, http.StatusForbidden, CodeForbidden, msg) }
func BadRequest(c *gin.Context, msg string) { Abort(c, http.StatusBadRequest, CodeBadRequest, msg) }
func Internal(c *gin.Context, msg string) {
	Abort(c, http.StatusInternalServerError, CodeInternal, msg)
}
