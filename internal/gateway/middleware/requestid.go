package middleware

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/logger"
)

// HeaderRequestID is the inbound/outbound correlation header. A client (or an
// upstream proxy) may supply one; otherwise the gateway mints it. This ID is
// the correlation_id that ties a request's log lines and audit records
// together across the whole pipeline.
const HeaderRequestID = "X-Request-ID"

// RequestID assigns a correlation ID to every request, stores it on the gin
// context and the response header, and attaches a request-scoped logger
// (already carrying request_id) to the request context so every downstream
// component logs with the same correlation field.
func RequestID(base *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(HeaderRequestID)
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set(httpx.RequestIDKey, rid)
		c.Header(HeaderRequestID, rid)

		reqLogger := base.With("request_id", rid)
		ctx := logger.WithContext(c.Request.Context(), reqLogger)
		c.Request = c.Request.WithContext(ctx)

		c.Next()
	}
}
