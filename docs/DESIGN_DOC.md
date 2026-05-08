# ChainPulse Design Document

**Status:** In Development
**Last Updated:** May 2026
**Author:** [@nickemma](https://github.com/nickemma)

---

## Purpose

This document explains the hard problems in ChainPulse — not the patterns, but the specific implementation decisions and the failure modes those decisions create. It covers the internal event bus and exactly where its guarantees end, the ABAC evaluation model and the failure modes naive implementations miss, the ML pipeline and its cold-start, drift, and timeout behaviors, and the audit hash chain and what it survives and does not survive.

Every section answers a question a senior engineer would ask in a review. Where the answer is "we accepted a limitation," that limitation is named explicitly.

---

## The Internal Event Bus: Design, Guarantees, and Where It Breaks

### Why not Kafka

The decision to build an internal event bus over Kafka is the most scrutinized choice in this system. Here is the honest answer.

At ChainPulse's target throughput — approximately 500–2,000 events per second under peak load, with 4–6 consumer types — a Postgres-backed outbox with background relay workers is within comfortable operating range. A single Postgres instance handles 5,000–10,000 row inserts per second under normal conditions. The relay workers consume published rows at a rate limited by the consumers, not the database. Consumer lag under normal operation is under 500ms. This is measurable and it is within the requirements.

Kafka becomes justified when one or more of the following is true:

1. **Throughput exceeds ~5,000 events/sec sustained** — at this point the outbox table becomes a write bottleneck regardless of indexing. The relay workers spend more time waiting for Postgres than publishing.
2. **Consumer count exceeds ~10 independent groups** — each consumer group polls the outbox independently. At 10+ groups, polling pressure on the outbox table creates read contention that degrades write throughput.
3. **Retention requirements exceed 30 days** — the outbox table is retained for `EVENT_RETENTION_DAYS` (default: 30). Longer retention requires either a larger table (with growing scan costs) or a separate event archive — at which point Kafka's log-based retention is the simpler model.
4. **Cross-service or cross-process consumers are required** — the internal event bus is in-process. The moment a consumer needs to run in a separate deployable unit, the internal bus cannot serve it. This is the decomposition trigger.

The migration path is explicit and non-destructive: the outbox table stays. The relay workers are replaced by a Kafka producer that reads from the outbox and publishes to Kafka topics. Consumer interfaces are unchanged — they consume from Kafka instead of the internal bus, but the event schema and correlation ID structure are identical. The migration does not require touching domain logic.

### Ordering guarantees

Events within a single tenant and resource are ordered by `created_at` timestamp and a monotonic sequence number. The relay publishes in creation order. Consumers receive events in publication order. This is a **per-resource ordering guarantee**, not a global ordering guarantee.

Two events for different resources (e.g., `orders.created` for order A and `orders.created` for order B) may be consumed in any order relative to each other. Consumers that require cross-resource ordering must implement their own coordination — for example, the stream processor tracks per-resource watermarks to ensure it processes events for a given order in sequence.

**What breaks ordering:** If the relay has multiple workers processing the same resource type concurrently, two events for the same resource can be published out of order if the second event's outbox row is processed by a faster worker before the first event's row. Mitigation: the relay uses a consistent hash of `(tenant_id, resource_id)` to route outbox rows to workers — all events for a given resource are processed by the same worker, preserving order.

### At-least-once delivery and deduplication

The outbox relay guarantees at-least-once delivery. A relay worker that publishes an event and then crashes before marking the row as `published=true` will re-publish the event on restart. Consumers receive duplicate events.

Consumer deduplication is enforced by a `processed_events` table: before processing any event, the consumer checks whether the `event_id` is already in the table. If it is, the event is acknowledged and discarded without reprocessing. The `processed_events` table is per-consumer-group and is pruned of entries older than `EVENT_DEDUP_WINDOW` (default: 48 hours).

The deduplication window is 48 hours. An event that is delayed more than 48 hours between publication and consumption (which would require the relay to be down for 48 hours) may be processed twice. This scenario is considered outside the acceptable operating window — a relay outage of 48 hours is a catastrophic failure requiring manual intervention.

### Dead-letter mechanism

An event that fails consumer processing more than `EVENT_MAX_RETRY` times (default: 5) is moved to the `event_dead_letters` table with the error, the retry count, the last failure timestamp, and the full event payload. The dead-letter table is never automatically retried — it requires operator inspection.

Prometheus alert fires when `event_dead_letters` count exceeds `DLQ_ALERT_THRESHOLD` (default: 10 rows). The runbook procedure for dead-letter events is in [RUNBOOK.md](RUNBOOK.md).

### Consumer lag under load

At peak event throughput (2,000 events/sec), the stream processor is the highest-volume consumer. The stream processor runs 8 goroutines by default (`PROCESSOR_WORKER_COUNT`). Each goroutine processes one event at a time, including a Postgres write for derived state. At ~5ms per event (Postgres write + analytics table update), 8 workers process ~1,600 events/sec — below peak throughput. At sustained peak, the stream processor falls behind at approximately 400 events/sec.

At this rate, the processor reaches a 5-minute lag (the warning threshold) in approximately 45 minutes of sustained peak load. This is not a silent failure — the Prometheus alert fires at 5 minutes of lag, giving operators 40 minutes to scale the worker count before the critical threshold is reached.

Increasing `PROCESSOR_WORKER_COUNT` to 16 handles the current peak headroom. At 5,000+ events/sec, the worker model is no longer sufficient and the Kafka migration becomes the correct answer.

---

## ABAC Policy Evaluation: Failure Modes

### The evaluation path

Every request passes through the policy evaluator before reaching a module handler. The evaluator loads the active policy set from its in-memory cache (refreshed every 60 seconds from Postgres) and evaluates each rule against the request context.

Evaluation time target: p99 < 5ms. Measured at: ~1.2ms for simple rules, ~3.8ms for rules that involve resource metadata lookups.

### Failure mode 1: Policy cache staleness

The evaluator caches the active policy set for 60 seconds. A policy change (new version promoted, old version deprecated) is visible to all evaluators within one refresh window. During the window, some requests are evaluated against the old policy and some against the new one.

For policy tightenings (a new rule that denies access previously allowed): requests that hit an evaluator with the old cache can succeed during the window. This is a known and documented behavior. Operators deploying a security-critical policy tightening should force a cache flush:

```bash
chainpulse-cli policy reload --tenant acme-corp
# Flushes in-memory cache on all running instances within 5 seconds
```

For policy loosenings (a new rule that allows access previously denied): requests that hit an evaluator with the old cache fail during the window. This produces user-visible errors for up to 60 seconds. The tradeoff: a Postgres read on every request eliminates the stale window but adds ~15ms P99 latency to every policy evaluation — on an endpoint receiving 500 requests/sec, this is 7.5 additional Postgres seconds per second of load. The cache is the correct tradeoff.

### Failure mode 2: Policy evaluator panic

The evaluator runs untrusted policy DSL compiled to an evaluation AST. A malformed policy can produce a panic in the evaluator if the DSL compiler does not catch all invalid cases.

Mitigation: the evaluator runs each policy rule in a deferred-recover wrapper. A panic in a single rule evaluation produces a `DENY` for that rule (fails closed) and logs the panic with the policy version and the request context. The overall evaluation continues with the remaining rules. A policy that consistently panics fires a Prometheus counter (`policy_eval_panics_total`) and is automatically demoted from `active` to `panicking` status — subsequent evaluations skip it and fall back to deny-by-default.

### Failure mode 3: The missing cross-tenant check

The most dangerous policy bug is an allow rule that does not check tenant scope. Example:

```rego
# DANGEROUS: missing tenant check
allow {
  input.role == "analyst"
  input.action == "read"
}
```

This allows any analyst in any tenant to read any resource. The hardcoded evaluator-level rule — `input.resource.tenant_id != input.actor.tenant_id → DENY` — fires before this allow rule and blocks the cross-tenant access. But this protection only applies if the resource metadata in the input context is correctly populated. If the gateway forwards a request context without the resource's `tenant_id` (a gateway bug), the hardcoded rule cannot fire.

Defense: the resource metadata population function is centralized — all module handlers call the same `enrichRequestContext` function before passing context to the evaluator. Integration tests verify that the `tenant_id` is present in the evaluator context for every request path.

---

## ML Pipeline: Cold Start, Drift, and Timeout

### Cold start: new SKUs and sparse regions

A new SKU has no order history. A new region has no shipment history. The forecasting model trained on historical data has no basis for prediction.

**Demand forecasting cold start:** For a SKU with fewer than 30 days of order history (or fewer than 100 total orders), the model uses a regional average as a prior. The `ForecastResponse` includes:
- `data_confidence: "low"` — the calling module must handle this
- `prior_type: "regional_average"` — identifies what prior was used
- `prior_basis_days: N` — how many days of regional data the prior is based on

The inventory module's restocking job, on receiving `data_confidence: "low"`, applies a configurable safety buffer (`FORECAST_LOW_CONFIDENCE_BUFFER`, default: 1.5x) to the predicted quantity. It does not ignore the forecast — it acts more conservatively on it.

After 30 days of order history, the model re-trains on the SKU's own data during the next weekly training job. The `data_confidence` field transitions to `"medium"` (30–90 days) and then `"high"` (90+ days).

**Delay prediction cold start:** A new supplier or route has no delay history. The model uses the global delay rate across all suppliers as a prior (`delay_probability` defaults to 0.25). New suppliers start with a conservative prior — they are not treated as reliable until they have accumulated history. After 10 completed shipments, the supplier-specific history is used.

### Model drift detection

The stream processor tracks rolling accuracy for the delay prediction model: for every shipment where the model predicted `delay_probability`, it compares the prediction against the actual outcome (delayed: yes/no) using the `shipments.delayed` event as the label. Rolling accuracy is computed over a 7-day window and exposed as a Prometheus metric (`ml_delay_model_rolling_accuracy`).

When rolling accuracy drops below `ML_DRIFT_ALERT_THRESHOLD` (default: 0.72), an alert fires and the next training job is triggered immediately (bypassing the weekly cadence). When rolling accuracy drops below `ML_DRIFT_CRITICAL_THRESHOLD` (default: 0.65), the active model is demoted to `degraded` status and routing falls back to historical delay rates from Postgres. The degraded status fires a critical alert.

### ML inference timeout

The routing logic calls the ML module with a timeout of `ML_INFERENCE_TIMEOUT_MS` (default: 200ms). If the ML module does not respond within 200ms — due to model loading, a slow prediction, or the ML service being unavailable — the routing logic falls back to the 30-day historical delay rate for the route read from a pre-aggregated Postgres table.

The fallback is not a silent degradation. Every routing decision logs `ml_fallback=true` and `ml_fallback_reason` (timeout, unavailable, or error). A Prometheus counter tracks fallback frequency. If fallbacks exceed `ML_FALLBACK_ALERT_RATE` (default: 10% of routing decisions over 5 minutes), an alert fires.

The 200ms timeout was chosen by measuring the p99 inference time at expected model complexity: ~45ms for the delay prediction model. The timeout provides 4x headroom. If model complexity increases to the point where p99 exceeds 50ms, the timeout should be re-evaluated — a timeout that is too tight produces unnecessary fallbacks; a timeout that is too loose delays order creation.

### Model rollback triggers

A model is rolled back (active version demoted, previous version promoted) when:

1. Rolling accuracy drops below `ML_DRIFT_CRITICAL_THRESHOLD` (automatic demotion to `degraded`, operator promotes previous version)
2. Inference errors exceed `ML_ERROR_RATE_THRESHOLD` (default: 5% of requests) — the model is producing invalid output (NaN, out-of-range probabilities)
3. Operator manual rollback via CLI: `chainpulse-cli ml rollback --model delay --to-version delay-v6`

Rollback is a Postgres write: `UPDATE model_versions SET status='active' WHERE version='delay-v6'` and `UPDATE model_versions SET status='deprecated' WHERE version='delay-v7'`. The ML module's 5-minute version refresh picks up the change. No restart required.

---

## Audit Log Hash Chain

### Structure

Every sensitive action produces an audit record written to the `audit_log` table before the response is returned:

```
Record N:
  id:               uuid
  tenant_id:        acme-corp
  actor_id:         user-123
  actor_role:       warehouse_manager
  action:           order.create
  resource_type:    order
  resource_id:      ord_01HXYZ
  policy_decision:  ALLOW
  policy_rule:      warehouse_manager_create_order_v3
  metadata:         jsonb (action-specific context)
  timestamp:        2026-05-06T12:04:22.847291Z  (nanosecond precision)
  prev_hash:        sha256(Record N-1 canonical JSON)
  hash:             sha256(this record canonical JSON || prev_hash)
```

Canonical JSON: keys sorted lexicographically, no extra whitespace, timestamps in RFC3339 nanosecond format. The canonical form is deterministic across Go's `encoding/json` and Python's `json` module — both are used in the verification tooling.

### What the chain survives

- **Service restarts:** The last record's hash is read from Postgres on startup. The next record's `prev_hash` is populated from the database read, not from in-memory state.
- **Schema migrations:** New columns added to the audit record are appended to the canonical JSON in sorted key order and default to `null` for pre-migration records. The hash of a pre-migration record is stable — the pre-migration canonical JSON did not include the new column. Post-migration records include the new column.
- **Clock skew:** Nanosecond timestamps from Go's `time.Now()` are monotonic within a process. Cross-record ordering is guaranteed by the hash chain, not by the timestamp — the chain defines ordering even if timestamps are non-monotonic (e.g., after an NTP adjustment).

### What the chain does not survive

- **Postgres backup restore to an earlier point in time:** A point-in-time restore loses all records written after the restore point. The chain is intact up to the restore point but the records after it are gone. This is not a hash chain failure — it is a data loss event that must be detected by comparing the restored chain length against the expected length.
- **Truncation of the `audit_log` table:** A `TRUNCATE audit_log` drops all records. An attacker with database write access can eliminate the audit trail. Defense: the application service account does not have `TRUNCATE` privilege on `audit_log`. Only the DBA account (not used by the application) has this privilege. The DBA account requires two-person authorization for production access.

### Verification

```bash
chainpulse-cli audit verify --tenant acme-corp --from 0
# Verifying 10,432 records...
# Chain valid. No tampering detected.

# On failure:
# Chain break at record 8,421.
# Expected prev_hash: abc123...
# Found prev_hash: def456...
# Preserve audit dump immediately. File security incident.
```

---

## Failure Modes & Degradation Strategy

This section is the honest answer to: "what happens when things go wrong?"

### Redis unavailable

Redis serves: rate limiting, idempotency key storage, inventory cache, polling result cache.

| Component | Redis down behavior | Impact |
|---|---|---|
| Rate limiting | Falls back to per-instance in-memory token bucket | Tenants with N instances get N× configured limit. Not a security hole (limits still apply per instance) but a fairness issue. |
| Idempotency | Falls back to Postgres read/write | P99 latency for write endpoints increases from ~20ms to ~120ms. Throughput on `POST /orders` drops ~40% under load. |
| Inventory cache | Falls back to Postgres read on every inventory request | P99 for inventory reads increases from ~5ms to ~35ms. Acceptable for short outages. |
| Polling cache | Falls back to Postgres read on every poll | P99 for `/v1/alerts/pending` increases from ~8ms to ~45ms. Under thundering herd (mass reconnect), this risks Postgres saturation. |

Redis unavailability fires a critical alert within 30 seconds (health check interval). The system continues operating in degraded mode. Extended Redis outages (>15 minutes) risk Postgres saturation from polling load — the runbook procedure is to temporarily increase the recommended poll interval to 120 seconds via a configuration flag broadcast to clients.

### Event processor lag

If the stream processor falls behind:

- **Lag < 5 minutes:** Normal operating variance. No alert.
- **Lag 5–15 minutes:** Warning alert. Analytics tables are stale. Polling responses return `stale: true` for derived metrics. Clients see the flag and know the data is behind.
- **Lag > 15 minutes:** Critical alert. The ML module's next training job is blocked (it reads from analytics tables). Routing falls back to the last computed delay rates. Operator intervention required — scale worker count or investigate consumer errors.

**What does not degrade:** Order creation, inventory updates, and shipment tracking are unaffected by stream processor lag. These write to Postgres directly. The processor is downstream of the writes — processor lag means analytics are stale, not that operations are blocked.

### ML inference unavailable

Covered in the ML section above. Short version: routing falls back to historical Postgres rates within 200ms. Orders are never blocked by ML unavailability.

### Policy engine failure (panic or timeout)

A rule panic produces `DENY` for that rule (fails closed) and continues evaluation. A complete evaluator failure (panic in the evaluator itself, not a rule) produces `DENY` for the entire request — the gateway returns `503 Service Unavailable` with a `Retry-After` header. This is intentional: a broken policy engine is safer closed than open.

### Postgres primary unavailable

This is the catastrophic failure. Postgres is the single source of truth. If the primary is unavailable:

- All write endpoints return `503`
- Read endpoints that are Redis-cached continue serving from cache (with `stale: true`)
- The audit log cannot be written — requests are rejected rather than processed without audit records
- The event bus relay stops publishing (it cannot mark outbox rows as published)

Postgres high availability (streaming replication with automatic failover) is a deployment concern outside the application. The application detects primary unavailability within one health check interval (30 seconds) and begins returning `503`. On primary recovery or failover completion, the application reconnects automatically via the connection pool's retry logic.

---

## Load & Scale Thought Experiment

Not benchmarks — reasoning about where the system breaks under load.

**At 1,000 events/sec:** Comfortable. The outbox relay handles this with 4 workers. Stream processor keeps up with 8 workers. Postgres handles the write load without noticeable latency increase. Redis handles the read load trivially.

**At 5,000 events/sec:** The outbox table starts showing write contention. The relay workers spend measurable time waiting for row locks on the outbox table. The stream processor falls behind by the warning threshold within 10 minutes of sustained load. The polling layer's Postgres fallback (if Redis is degraded) risks connection pool saturation.

**What breaks first at 5,000 events/sec:** The stream processor, followed by the outbox relay. The domain modules (order, inventory, shipment) continue operating correctly — they write to Postgres and the outbox, which is not yet saturated. The degradation is in the analytics layer, not the operational layer.

**What to externalize first:** The stream processor. Moving it to a Kafka consumer that reads from Kafka topics (populated by a Kafka producer replacing the outbox relay) eliminates the Postgres read pressure on the analytics path. The domain modules and the outbox pattern stay unchanged. The stream processor becomes a separate process consuming Kafka topics at its own pace.

**What to externalize second:** The ML module. At the point where model training requires GPU resources or the inference latency budget requires dedicated compute, the ML module becomes a separate service with a gRPC interface. The routing logic's `MLClient` interface already abstracts the call — swapping from in-process to gRPC requires changing the adapter, not the domain logic.

**What never needs externalizing (at realistic scale):** The policy engine. Policy evaluation is CPU-bound and in-memory. Scaling the application horizontally scales policy evaluation capacity linearly. There is no shared state in the evaluator — each instance has its own cache.

---

## References

- [Architecture](ARCHITECTURE.md) — system map, module structure, hexagonal design
- [Tradeoffs](TRADEOFFS.md) — every major design decision with alternatives
- [Runbook](RUNBOOK.md) — operational procedures for the failure modes described here
- [Threat Model](security/THREAT_MODEL.md) — STRIDE analysis, attack surfaces, tenant isolation
