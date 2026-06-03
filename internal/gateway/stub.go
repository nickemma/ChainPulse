package gateway

import (
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/nickemma/chainpulse/internal/policy"
	"github.com/nickemma/chainpulse/shared/identity"
)

// StubModule stands in for the order/inventory/shipment modules that arrive in
// Phase 3. Phase 2's goal is to prove the gateway + policy pipeline in
// isolation, so these handlers do nothing but echo the authenticated identity
// and count how many times they actually executed. The Calls counter is what
// the idempotency test asserts on: a replayed request must NOT increment it.
type StubModule struct {
	Calls atomic.Int64
}

func NewStubModule() *StubModule { return &StubModule{} }

func (s *StubModule) handle(c *gin.Context) {
	n := s.Calls.Add(1)
	id, _ := identity.FromGin(c)
	status := http.StatusOK
	if c.Request.Method == http.MethodPost {
		status = http.StatusCreated
	}
	c.JSON(status, gin.H{
		"ok":        true,
		"handler":   c.FullPath(),
		"tenant_id": id.TenantID,
		"actor_id":  id.UserID,
		"role":      string(id.Role),
		"call_seq":  n,
	})
}

// queryResourceExtractor builds a policy.Resource from query params so tenant,
// region, supplier ownership, and value can be exercised from the command line
// during manual testing. For example:
//
//	GET /v1/inventory?tenant_id=other   -> models a cross-tenant read (denied)
//	PUT /v1/inventory?region=bc         -> models a write outside the WM region
func queryResourceExtractor(c *gin.Context, actor identity.Identity) policy.Resource {
	r := policy.Resource{
		ID:              c.Query("id"),
		TenantID:        c.Query("tenant_id"), // empty -> defaults to actor tenant
		Region:          c.Query("region"),
		OwnerSupplierID: c.Query("supplier_id"),
	}
	if v := c.Query("value_usd"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			r.ValueUSD = f
		}
	}
	return r
}
