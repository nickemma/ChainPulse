package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/logger"
)

// RequestLog emits one structured access-log line per request after it
// completes. This is the outer edge of the audit trail: if a request reaches
// the gateway, it is logged, regardless of what happens inside. The line
// carries the correlation ID, identity, route, status, latency, and the policy
// decision (if a policy middleware ran on the route).
func RequestLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		latency := time.Since(start)
		log := logger.FromContext(c.Request.Context())

		decision, _ := c.Get(policy.CtxDecisionKey)
		rule, _ := c.Get(policy.CtxRuleKey)
		fallback, _ := c.Get("ratelimit_fallback")

		log.Info("request",
			"method", c.Request.Method,
			"route", c.FullPath(),
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", latency.Milliseconds(),
			"policy_decision", decision,
			"policy_rule", rule,
			"ratelimit_fallback", fallback,
		)
	}
}
