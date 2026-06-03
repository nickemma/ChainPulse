// Package policy is the authorization engine. Every request that passes JWT
// validation at the gateway is evaluated here before any module handler runs.
//
// Three invariants define the engine:
//
//  1. Deny by default. If no allow rule matches, the decision is deny.
//  2. Cross-tenant access is denied before any allow rule is consulted. This
//     rule is hardcoded — it cannot be overridden by a tenant-configured
//     policy, because it is the foundation of multi-tenant isolation.
//  3. Evaluation is bounded. If rule evaluation exceeds the configured
//     timeout, the decision is deny (fail closed), never allow.
package policy

import (
	"context"
	"time"

	"github.com/nickemma/chainpulse/shared/identity"
)

// Action is the operation the caller wants to perform.
type Action string

const (
	ActionCreate Action = "create"
	ActionRead   Action = "read"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionCancel Action = "cancel"
)

// Resource describes the thing being acted upon. TenantID is the tenant that
// owns the resource — the cross-tenant deny compares it against the actor's
// tenant. Fields that don't apply to a given resource type are left zero.
type Resource struct {
	Type            string // "order", "inventory", "shipment"
	ID              string
	TenantID        string
	Region          string
	OwnerSupplierID string
	ValueUSD        float64
}

// Input is the full context handed to the evaluator.
type Input struct {
	Actor    identity.Identity
	Action   Action
	Resource Resource
}

// Decision is the result of evaluation, including which rule matched so it can
// be written to the audit trail.
type Decision struct {
	Allow       bool
	RuleMatched string
	Reason      string
}

// Reserved rule identifiers for the engine's built-in decisions.
const (
	RuleCrossTenantDeny = "cross_tenant_deny"
	RuleDefaultDeny     = "default_deny"
	RuleEvalTimeout     = "eval_timeout"
)

// Engine evaluates inputs against a rule set. It is safe for concurrent use as
// long as the rule set is not mutated after construction (the static provider
// guarantees this).
type Engine struct {
	rules       []Rule
	evalTimeout time.Duration
}

// NewEngine builds an engine from a rule set and an evaluation timeout. A
// non-positive timeout disables the timeout guard.
func NewEngine(rules []Rule, evalTimeout time.Duration) *Engine {
	return &Engine{rules: rules, evalTimeout: evalTimeout}
}

// Evaluate returns an authorization decision for the given input.
func (e *Engine) Evaluate(ctx context.Context, in Input) Decision {
	// Invariant 2: cross-tenant deny, evaluated first and unconditionally. This
	// is a trivial comparison, deliberately outside the timeout guard so it can
	// never be skipped by a slow rule set.
	if in.Resource.TenantID != "" && in.Resource.TenantID != in.Actor.TenantID {
		return Decision{
			Allow:       false,
			RuleMatched: RuleCrossTenantDeny,
			Reason:      "resource belongs to a different tenant",
		}
	}

	// Invariant 3: bounded evaluation. Rule matchers are simple predicates, but
	// the timeout is a hard guarantee that a pathological rule cannot hang the
	// request path — it fails closed to deny.
	if e.evalTimeout <= 0 {
		return e.evaluateRules(in)
	}

	ctx, cancel := context.WithTimeout(ctx, e.evalTimeout)
	defer cancel()

	resultCh := make(chan Decision, 1)
	go func() { resultCh <- e.evaluateRules(in) }()

	select {
	case d := <-resultCh:
		return d
	case <-ctx.Done():
		return Decision{
			Allow:       false,
			RuleMatched: RuleEvalTimeout,
			Reason:      "policy evaluation exceeded timeout",
		}
	}
}

// evaluateRules applies explicit denies first (an explicit deny always wins),
// then allows, then falls back to deny-by-default.
func (e *Engine) evaluateRules(in Input) Decision {
	for _, r := range e.rules {
		if r.Effect == EffectDeny && r.Match(in) {
			return Decision{Allow: false, RuleMatched: r.ID, Reason: r.Reason}
		}
	}
	for _, r := range e.rules {
		if r.Effect == EffectAllow && r.Match(in) {
			return Decision{Allow: true, RuleMatched: r.ID}
		}
	}
	return Decision{Allow: false, RuleMatched: RuleDefaultDeny, Reason: "no matching allow rule"}
}
