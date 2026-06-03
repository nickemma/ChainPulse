// Package audit records sensitive actions. In Phase 2 the requirement is that
// every policy decision produces an audit record; the tamper-evident,
// hash-chained, Postgres-backed audit log is a Phase 5 deliverable. This
// package therefore defines the Auditor contract that the policy engine writes
// to, plus a structured-log implementation that satisfies Phase 2. The Phase 5
// Postgres auditor will implement the same interface — no caller changes.
package audit

import (
	"context"
	"log/slog"
	"time"
)

// Decision values recorded in the audit trail.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// Record is a single audit entry. It mirrors the columns of the `audit` table
// so the Phase 5 Postgres auditor can persist it without a shape change.
type Record struct {
	TenantID          string
	ActorID           string
	ActorRole         string
	Action            string
	ResourceType      string
	ResourceID        string
	PolicyDecision    string // DecisionAllow | DecisionDeny
	PolicyRuleMatched string
	RequestID         string
	Timestamp         time.Time
}

// Auditor persists audit records. Implementations must be safe for concurrent
// use. Write returning an error signals the caller that the action could not
// be recorded — for sensitive actions the caller may choose to fail closed.
type Auditor interface {
	Write(ctx context.Context, r Record) error
}

// LogAuditor writes audit records as structured log lines at INFO. This is the
// Phase 2 implementation: it guarantees an audit record exists for every
// decision and is trivially testable, without depending on the UUID-typed
// `audit` table (which the Phase 5 hash-chain auditor will own).
type LogAuditor struct {
	log *slog.Logger
}

func NewLogAuditor(log *slog.Logger) *LogAuditor {
	return &LogAuditor{log: log}
}

func (a *LogAuditor) Write(_ context.Context, r Record) error {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	a.log.Info("audit",
		"audit", true,
		"tenant_id", r.TenantID,
		"actor_id", r.ActorID,
		"actor_role", r.ActorRole,
		"action", r.Action,
		"resource_type", r.ResourceType,
		"resource_id", r.ResourceID,
		"policy_decision", r.PolicyDecision,
		"policy_rule_matched", r.PolicyRuleMatched,
		"request_id", r.RequestID,
		"timestamp", r.Timestamp.UTC().Format(time.RFC3339Nano),
	)
	return nil
}
