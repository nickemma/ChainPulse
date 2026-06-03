package policy

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/internal/audit"
	"github.com/nickemma/chainpulse/shared/httpx"
	"github.com/nickemma/chainpulse/shared/identity"
)

// gin context keys for the decision, so the request-logging middleware can
// record the policy outcome on the access log line.
const (
	CtxDecisionKey = "policy_decision"
	CtxRuleKey     = "policy_rule"
)

// ResourceExtractor builds the policy Resource for a request from the gin
// context and the authenticated actor. It lets each route describe the
// attributes (region, owner, value, owning tenant) relevant to its resource.
// If it returns a zero TenantID, the middleware defaults to the actor's tenant
// (a same-tenant access). Returning a different TenantID models a cross-tenant
// access, which the engine denies.
type ResourceExtractor func(c *gin.Context, actor identity.Identity) Resource

// Enforce returns gin middleware that authorizes the request against the
// policy engine and writes an audit record for the decision — allow or deny —
// before the handler runs (or before the 403 is returned). This is the
// "every policy decision is audited" guarantee from Phase 2.
func Enforce(engine *Engine, auditor audit.Auditor, action Action, resourceType string, extract ResourceExtractor) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := identity.FromGin(c)
		if !ok {
			// Auth middleware must run before policy; a missing identity here
			// is a wiring bug, not a client error.
			httpx.Unauthorized(c, "authentication required")
			return
		}

		resource := Resource{Type: resourceType, TenantID: actor.TenantID}
		if extract != nil {
			resource = extract(c, actor)
			resource.Type = resourceType
			if resource.TenantID == "" {
				resource.TenantID = actor.TenantID
			}
		}

		decision := engine.Evaluate(c.Request.Context(), Input{
			Actor:    actor,
			Action:   action,
			Resource: resource,
		})

		// Audit every decision before responding.
		reqID, _ := c.Get(httpx.RequestIDKey)
		rid, _ := reqID.(string)
		decStr := audit.DecisionAllow
		if !decision.Allow {
			decStr = audit.DecisionDeny
		}
		_ = auditor.Write(c.Request.Context(), audit.Record{
			TenantID:          actor.TenantID,
			ActorID:           actor.UserID,
			ActorRole:         string(actor.Role),
			Action:            string(action),
			ResourceType:      resourceType,
			ResourceID:        resource.ID,
			PolicyDecision:    decStr,
			PolicyRuleMatched: decision.RuleMatched,
			RequestID:         rid,
		})

		// Expose the decision to the request logger.
		c.Set(CtxDecisionKey, decStr)
		c.Set(CtxRuleKey, decision.RuleMatched)

		if !decision.Allow {
			httpx.Abort(c, http.StatusForbidden, httpx.CodeForbidden, "denied by policy: "+decision.Reason)
			return
		}
		c.Next()
	}
}
