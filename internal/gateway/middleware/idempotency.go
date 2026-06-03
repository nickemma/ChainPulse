package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
	"github.com/nickemma/chainpulse/shared/logger"
	"github.com/nickemma/chainpulse/shared/storage"
)

// HeaderIdempotencyKey is the required header for idempotent write endpoints.
const HeaderIdempotencyKey = "Idempotency-Key"

// storedResponse is the cached result replayed for duplicate requests.
type storedResponse struct {
	Status      int    `json:"status"`
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
}

// captureWriter tees the handler's response into a buffer so it can be stored
// for replay, while still writing through to the real client.
type captureWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// Idempotency enforces the Idempotency-Key contract on write endpoints. The
// first request with a given (tenant, route, key) executes the handler and the
// response is cached in Redis for the TTL. A duplicate request with the same
// key replays the cached response without re-executing the handler — so a
// retried POST never creates a second order.
//
// Phase 2 uses Redis as the store. ARCHITECTURE.md describes a Postgres
// durable fallback when Redis is down; that fallback is a remaining Phase 2
// item. Today, if Redis is unavailable, the gateway logs the degradation and
// proceeds without the dedup guarantee rather than failing the write.
func Idempotency(rdb *storage.Redis, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader(HeaderIdempotencyKey)
		if key == "" {
			httpx.Abort(c, http.StatusBadRequest, httpx.CodeIdempotency,
				"Idempotency-Key header is required for this endpoint")
			return
		}

		actor, ok := identity.FromGin(c)
		if !ok {
			httpx.Unauthorized(c, "authentication required")
			return
		}

		suffix := "idem:" + c.FullPath() + ":" + key
		ctx := c.Request.Context()

		// Replay path: a stored response means this key was already processed.
		if val, err := rdb.Get(ctx, actor.TenantID, suffix); err == nil {
			var sr storedResponse
			if json.Unmarshal([]byte(val), &sr) == nil {
				c.Header("Idempotent-Replay", "true")
				c.Data(sr.Status, sr.ContentType, []byte(sr.Body))
				c.Abort()
				return
			}
		} else if !errors.Is(err, storage.ErrCacheMiss) {
			// Redis is unavailable — degrade rather than block the write.
			logger.FromContext(ctx).Warn("idempotency store unavailable, proceeding without dedup",
				"error", err.Error(), "tenant_id", actor.TenantID)
			c.Next()
			return
		}

		// First execution: capture the response and store it for future replays.
		cw := &captureWriter{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
		c.Writer = cw
		c.Next()

		status := c.Writer.Status()
		// Cache 2xx/4xx outcomes; 5xx is transient and should be retryable.
		if status >= 200 && status < 500 {
			sr := storedResponse{
				Status:      status,
				Body:        cw.body.String(),
				ContentType: c.Writer.Header().Get("Content-Type"),
			}
			if b, err := json.Marshal(sr); err == nil {
				if err := rdb.Set(ctx, actor.TenantID, suffix, string(b), ttl); err != nil {
					logger.FromContext(ctx).Warn("failed to persist idempotent response",
						"error", err.Error())
				}
			}
		}
	}
}
