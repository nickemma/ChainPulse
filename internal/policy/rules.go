package policy

import "github.com/nickemma/chainpulse/shared/identity"

// Effect is whether a matching rule allows or denies.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Rule is a single policy rule: an effect plus a predicate over the input.
// Rules are evaluated by the engine — denies before allows. In Phase 2 the
// rule set is the static DefaultRules below. Phase 2's roadmap also calls for
// Postgres-backed policy versioning with a 60s in-memory cache; the engine is
// structured to accept any []Rule, so a Postgres-loading provider can replace
// DefaultRules without touching the evaluator.
type Rule struct {
	ID     string
	Effect Effect
	Reason string
	Match  func(Input) bool
}

// HighValueThresholdUSD is the order value above which an order mutation
// requires elevated authorization (admin). Mirrors ORDER_HIGH_VALUE_THRESHOLD_USD.
const HighValueThresholdUSD = 50000

// DefaultRules encodes the RBAC baseline from the README, layered with the
// ABAC constraints (region scoping, supplier ownership, high-value gating).
// Deny rules are listed first for readability; the engine evaluates all denies
// before any allow regardless of slice order.
func DefaultRules() []Rule {
	return []Rule{
		// --- ABAC deny overlays (evaluated before allows) ---

		// High-value order mutations require admin, even for roles that could
		// otherwise mutate orders. This is an attribute-level constraint that
		// role alone cannot express.
		{
			ID:     "deny_high_value_order_mutation_non_admin",
			Effect: EffectDeny,
			Reason: "high-value order mutation requires admin",
			Match: func(in Input) bool {
				return in.Resource.Type == "order" &&
					(in.Action == ActionUpdate || in.Action == ActionCancel) &&
					in.Resource.ValueUSD >= HighValueThresholdUSD &&
					in.Actor.Role != identity.RoleAdmin
			},
		},

		// --- RBAC + ABAC allow rules ---

		// admin: full read/write within the tenant. Cross-tenant access is
		// already denied by the engine's hardcoded rule before we get here.
		{
			ID:     "allow_admin_within_tenant",
			Effect: EffectAllow,
			Match:  func(in Input) bool { return in.Actor.Role == identity.RoleAdmin },
		},

		// analyst: read-only across all resources within the tenant.
		{
			ID:     "allow_analyst_read",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleAnalyst && in.Action == ActionRead
			},
		},

		// warehouse_manager: read any inventory in the tenant (cross-region
		// reads allowed), but write only within the manager's assigned region.
		{
			ID:     "allow_wm_read_inventory",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleWarehouseManager &&
					in.Resource.Type == "inventory" &&
					in.Action == ActionRead
			},
		},
		{
			ID:     "allow_wm_write_own_region_inventory",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleWarehouseManager &&
					in.Resource.Type == "inventory" &&
					(in.Action == ActionCreate || in.Action == ActionUpdate) &&
					in.Resource.Region != "" &&
					in.Resource.Region == in.Actor.Region
			},
		},
		// warehouse_manager: create/read orders within tenant (region-scoped
		// writes mirror inventory; reads are tenant-wide).
		{
			ID:     "allow_wm_orders",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleWarehouseManager &&
					in.Resource.Type == "order" &&
					(in.Action == ActionRead || in.Action == ActionCreate)
			},
		},

		// supplier: read/write only their own shipments; read only their own
		// inventory allocations. Ownership is matched on SupplierID.
		{
			ID:     "allow_supplier_own_shipments",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleSupplier &&
					in.Resource.Type == "shipment" &&
					in.Resource.OwnerSupplierID != "" &&
					in.Resource.OwnerSupplierID == in.Actor.SupplierID
			},
		},
		{
			ID:     "allow_supplier_read_own_inventory",
			Effect: EffectAllow,
			Match: func(in Input) bool {
				return in.Actor.Role == identity.RoleSupplier &&
					in.Resource.Type == "inventory" &&
					in.Action == ActionRead &&
					in.Resource.OwnerSupplierID != "" &&
					in.Resource.OwnerSupplierID == in.Actor.SupplierID
			},
		},
	}
}
