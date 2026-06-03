package policy

import (
	"context"
	"testing"
	"time"

	"github.com/nickemma/chainpulse/shared/identity"
)

func eng() *Engine {
	return NewEngine(DefaultRules(), 5*time.Millisecond)
}

func actor(role identity.Role, tenant, region, supplier string) identity.Identity {
	return identity.Identity{
		TenantID: tenant, UserID: "u1", Role: role, Region: region, SupplierID: supplier,
	}
}

// Roadmap failure test: cross-tenant access is denied before any allow rule —
// even for an admin, who is otherwise allowed everything in their tenant.
func TestCrossTenantDenyBeatsAdminAllow(t *testing.T) {
	d := eng().Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleAdmin, "tenant-A", "", ""),
		Action:   ActionRead,
		Resource: Resource{Type: "order", TenantID: "tenant-B"},
	})
	if d.Allow {
		t.Fatal("expected cross-tenant deny, got allow")
	}
	if d.RuleMatched != RuleCrossTenantDeny {
		t.Fatalf("expected %s, got %s", RuleCrossTenantDeny, d.RuleMatched)
	}
}

// Roadmap failure test: a role with no matching allow rule is denied by default.
func TestDefaultDeny(t *testing.T) {
	// A supplier trying to read an order — no rule grants that.
	d := eng().Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleSupplier, "t", "", "sup-1"),
		Action:   ActionRead,
		Resource: Resource{Type: "order", TenantID: "t"},
	})
	if d.Allow {
		t.Fatal("expected default deny, got allow")
	}
	if d.RuleMatched != RuleDefaultDeny {
		t.Fatalf("expected %s, got %s", RuleDefaultDeny, d.RuleMatched)
	}
}

func TestAdminAllowedWithinTenant(t *testing.T) {
	d := eng().Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleAdmin, "t", "", ""),
		Action:   ActionCreate,
		Resource: Resource{Type: "order", TenantID: "t"},
	})
	if !d.Allow {
		t.Fatalf("expected allow, got deny (%s)", d.Reason)
	}
}

func TestAnalystReadOnly(t *testing.T) {
	e := eng()
	read := e.Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleAnalyst, "t", "", ""),
		Action:   ActionRead,
		Resource: Resource{Type: "inventory", TenantID: "t"},
	})
	if !read.Allow {
		t.Fatal("analyst should be allowed to read")
	}
	write := e.Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleAnalyst, "t", "", ""),
		Action:   ActionUpdate,
		Resource: Resource{Type: "inventory", TenantID: "t"},
	})
	if write.Allow {
		t.Fatal("analyst must not be allowed to write")
	}
}

// ABAC: warehouse manager can write inventory only in their assigned region,
// but can read across regions.
func TestWarehouseManagerRegionScoping(t *testing.T) {
	e := eng()
	wm := actor(identity.RoleWarehouseManager, "t", "ontario", "")

	ownRegionWrite := e.Evaluate(context.Background(), Input{
		Actor: wm, Action: ActionUpdate,
		Resource: Resource{Type: "inventory", TenantID: "t", Region: "ontario"},
	})
	if !ownRegionWrite.Allow {
		t.Fatal("WM should write own-region inventory")
	}

	otherRegionWrite := e.Evaluate(context.Background(), Input{
		Actor: wm, Action: ActionUpdate,
		Resource: Resource{Type: "inventory", TenantID: "t", Region: "bc"},
	})
	if otherRegionWrite.Allow {
		t.Fatal("WM must not write other-region inventory")
	}

	otherRegionRead := e.Evaluate(context.Background(), Input{
		Actor: wm, Action: ActionRead,
		Resource: Resource{Type: "inventory", TenantID: "t", Region: "bc"},
	})
	if !otherRegionRead.Allow {
		t.Fatal("WM should read cross-region inventory")
	}
}

// ABAC high-value gating: a non-admin cannot mutate an order above the
// high-value threshold even though warehouse managers can mutate normal orders.
func TestHighValueOrderGating(t *testing.T) {
	e := eng()
	wm := actor(identity.RoleWarehouseManager, "t", "ontario", "")
	d := e.Evaluate(context.Background(), Input{
		Actor: wm, Action: ActionUpdate,
		Resource: Resource{Type: "order", TenantID: "t", ValueUSD: 75000},
	})
	if d.Allow {
		t.Fatal("high-value order mutation by non-admin must be denied")
	}
	if d.RuleMatched != "deny_high_value_order_mutation_non_admin" {
		t.Fatalf("unexpected rule: %s", d.RuleMatched)
	}
}

// Roadmap failure test: evaluation exceeding the timeout fails closed to deny.
func TestEvalTimeoutFailsClosed(t *testing.T) {
	slow := []Rule{{
		ID:     "slow",
		Effect: EffectAllow,
		Match: func(Input) bool {
			time.Sleep(50 * time.Millisecond)
			return true
		},
	}}
	e := NewEngine(slow, 5*time.Millisecond)
	d := e.Evaluate(context.Background(), Input{
		Actor:    actor(identity.RoleAdmin, "t", "", ""),
		Action:   ActionRead,
		Resource: Resource{Type: "order", TenantID: "t"},
	})
	if d.Allow {
		t.Fatal("timed-out evaluation must deny, not allow")
	}
	if d.RuleMatched != RuleEvalTimeout {
		t.Fatalf("expected %s, got %s", RuleEvalTimeout, d.RuleMatched)
	}
}
