# ChainPulse Runbook

**Purpose:** Operational procedures for diagnosing and recovering from failures in ChainPulse.
**Last Updated:** May 2026

---

## Guiding Principle

Two rules before any recovery action:

**Read the audit log first.** The audit log tells you what happened, who authorized it, and in what order. Recovery without reading the audit log is recovery without context — you may fix the symptom and leave the cause.

**Prefer explicit errors over silent degradation.** ChainPulse is designed to surface failures clearly — stale data is flagged as `stale: true`, ML fallbacks are logged as `ml_fallback=true`, Redis degradation is alerted. If a component is failing, the correct response is usually to let it fail visibly while the root cause is addressed, not to mask the failure. A system that hides its problems is harder to recover than one that announces them.

---

## Diagnostics

### Service health

```bash
# Overall health (all dependencies)
curl http://localhost:8080/health
# {"status":"ok","dependencies":{"postgres":"up","redis":"up","ml":"up"}}

# Readiness (includes event bus relay and stream processor)
curl http://localhost:8080/ready
# {"status":"ready","checks":{"outbox_relay":"running","stream_processor":"running","ml_subprocess":"running"}}
```

### Event bus state

```bash
# How many outbox rows are unprocessed?
chainpulse-cli eventbus status
# Unprocessed outbox rows: 0
# Oldest unprocessed row: (none)
# Relay workers: 4 running
# Dead-letter rows: 0

# Check lag (time between oldest unprocessed row and now)
chainpulse-cli eventbus lag
# Current lag: 340ms (threshold: warn=5m, critical=15m)

# List dead-letter events
chainpulse-cli eventbus dlq list --tenant acme-corp
# EVENT_ID       EVENT_TYPE         RETRY_COUNT  LAST_ERROR              CREATED_AT
# evt_01HABC     orders.created     5            consumer panic: nil ptr  2026-05-06T11:00:00Z
```

### Order pipeline

```bash
chainpulse-cli order status --id ord_01HXYZ --tenant acme-corp
# Status:         confirmed
# Version:        2
# Outbox row:     published (2026-05-06T12:04:23Z)
# Audit records:  3 (create, policy-eval, confirm)

# Outbox rows for a specific order
chainpulse-cli order outbox --id ord_01HXYZ --tenant acme-corp
# OUTBOX_ID  EVENT_TYPE      PUBLISHED  PUBLISHED_AT
# out_001    orders.created  true       2026-05-06T12:04:23Z
# out_002    orders.updated  true       2026-05-06T12:05:10Z
```

### Inventory state

```bash
chainpulse-cli inventory status --sku WIDGET-XL --region ontario --tenant acme-corp
# Physical count:      1,200
# Soft reserved:       340
# Available:           860
# Low-stock threshold: 100
# Cache status:        hit (TTL: 4m 12s remaining)
# Last updated:        2026-05-06T12:01:00Z

# Check active reservations
chainpulse-cli inventory reservations --sku WIDGET-XL --region ontario --tenant acme-corp
# ord_01HXYZ: 200 units, expires 2026-05-06T13:04:30Z, order status: confirmed
# ord_01HABC: 100 units, expires 2026-05-06T12:55:10Z, order status: confirmed
```

### ML state

```bash
chainpulse-cli ml status
# DELAY PREDICTION MODEL
#   Active version:     delay-v7
#   Trained at:         2026-05-04T03:00:00Z
#   Holdout accuracy:   0.84
#   Rolling accuracy:   0.81 (7d window, threshold: warn=0.72, critical=0.65)
#   Inference p99:      43ms (timeout: 200ms)
#   Fallback rate:      0.3% (threshold: alert=10%)
#   Status:             active
#
# DEMAND FORECAST MODEL
#   Active version:     forecast-v12
#   Trained at:         2026-05-03T03:00:00Z
#   Holdout MAPE:       11.2%
#   Status:             active

# Check ML subprocess
chainpulse-cli ml subprocess status
# PID: 14823, uptime: 4h 12m, memory: 412MB, last inference: 1.2s ago
```

### Policy state

```bash
# Evaluate a policy for a specific context
chainpulse-cli policy eval \
  --tenant acme-corp \
  --actor-role warehouse_manager \
  --actor-region ontario \
  --resource-type order \
  --resource-region ontario \
  --action order.create
# Decision:  ALLOW
# Rule:      warehouse_manager_create_order_v3
# Eval time: 1.1ms
# Cache age: 34s (next refresh in 26s)

# List active policies
chainpulse-cli policy list --tenant acme-corp
# NAME                              VERSION  UPDATED_AT
# warehouse_manager_create_order    v3       2026-04-15T10:00:00Z
# supplier_read_own_shipments       v2       2026-04-10T09:00:00Z

# Force cache flush across all instances
chainpulse-cli policy reload --tenant acme-corp
# Cache flushed. All instances will reload within 5 seconds.
```

### Audit log

```bash
# View recent audit records for a resource
chainpulse-cli audit log \
  --resource-type order \
  --resource-id ord_01HXYZ \
  --tenant acme-corp \
  --last 10
# INDEX  TIMESTAMP             ACTION        ACTOR     DECISION  RULE
# 10432  2026-05-06T12:04:22Z  order.create  user-123  ALLOW     warehouse_manager_create_order_v3
# 10433  2026-05-06T12:10:11Z  order.update  user-456  DENY      (no matching rule)

# Verify audit chain integrity
chainpulse-cli audit verify --tenant acme-corp --from 0
# Verifying 10,432 records...
# Chain valid. No tampering detected.
```

### Prometheus key metrics

```bash
curl http://localhost:9090/metrics | grep chainpulse_

# chainpulse_order_creation_p99_ms                87.4
# chainpulse_eventbus_lag_seconds                 0.34
# chainpulse_eventbus_dlq_count                   0
# chainpulse_policy_eval_p99_ms                   1.1
# chainpulse_policy_cache_age_seconds             34
# chainpulse_ml_inference_p99_ms                  43.2
# chainpulse_ml_fallback_rate                     0.003
# chainpulse_ml_rolling_accuracy{model="delay"}   0.81
# chainpulse_redis_available                      1
# chainpulse_stream_processor_lag_seconds         0.34
# chainpulse_audit_write_errors_total             0
# chainpulse_inventory_low_stock_events_total     7
```

---

## Failure Triage

### Redis Unavailable

Symptoms: `chainpulse_redis_available = 0`. Rate limiting degrades to per-instance. Idempotency checks fall back to Postgres. P99 on write endpoints increases significantly.

1. Confirm Redis is unreachable from the application:
```bash
chainpulse-cli redis ping
# FAILED: dial tcp redis:6379: connection refused
```

2. Check Redis logs and restart if down:
```bash
# Docker
docker compose restart redis

# Verify recovery
chainpulse-cli redis ping
# OK (1.2ms)
```

3. After Redis recovery, check for idempotency key gaps:
```bash
# Keys in Postgres but not in Redis (will be served correctly from Postgres fallback)
# No action needed — Postgres fallback is authoritative

# Keys in Redis but not in Postgres (async write may have failed during outage)
chainpulse-cli idempotency reconcile --dry-run
# Found 3 keys in Redis not present in Postgres. Would backfill.
chainpulse-cli idempotency reconcile --confirm
# Backfilled 3 keys to Postgres.
```

4. Check if rate limit counters in Redis are stale (after Redis restart, all counters reset):
```bash
# After Redis restart, in-memory per-instance limits are gone.
# Tenants start with a clean rate limit slate — this is acceptable.
# Log the Redis restart time for audit purposes.
chainpulse-cli audit note \
  --message "Redis restarted at $(date -u +%Y-%m-%dT%H:%M:%SZ). Rate limit counters reset." \
  --tenant system
```

**Extended Redis outage (>15 minutes):**

Polling load falls back to Postgres. Under mass client reconnect, this risks connection pool saturation. Temporarily increase the recommended poll interval:

```bash
# Broadcast a configuration hint to clients via the /v1/config endpoint
chainpulse-cli config set RECOMMENDED_POLL_INTERVAL_SECONDS=120
# Clients that respect the config hint will back off
# Clients that do not are rate-limited at the gateway (per-instance limit)
```

---

### Event Bus Lag: Stream Processor Falling Behind

Symptoms: `chainpulse_eventbus_lag_seconds` exceeds 300 (5-minute warning threshold). Analytics tables are stale. Poll responses return `stale: true`.

1. Check current lag and which consumer is behind:
```bash
chainpulse-cli eventbus lag --verbose
# Stream processor: 8m 24s lag (CRITICAL threshold: 15m)
# Oldest unprocessed event: evt_01HXYZ (orders.created, 2026-05-06T11:56:00Z)
```

2. Check stream processor worker count and errors:
```bash
chainpulse-cli stream-processor status
# Workers: 8 (configured: 8)
# Processing rate: 1,240 events/sec
# Error rate: 0.1%
# Last error: "pq: too many connections" at 2026-05-06T12:01:00Z  ← root cause
```

3. If the lag is caused by Postgres connection errors, check the connection pool:
```bash
chainpulse-cli postgres connections
# Pool size: 50
# Active connections: 49  ← near saturation
# Waiting requests: 12
```

4. If the connection pool is saturated, reduce stream processor concurrency temporarily:
```bash
# In environment config (requires restart)
PROCESSOR_WORKER_COUNT=4  # reduce from 8 to relieve Postgres pressure

# Alternatively, increase pool size if headroom exists
POSTGRES_MAX_CONNS=75
```

5. If the lag is caused by high event volume (processor cannot keep up with throughput):
```bash
# Check event production rate
chainpulse-cli eventbus rate
# Production rate: 2,400 events/sec (processor capacity: ~1,600/sec)
# At current rate, critical threshold reached in: 18 minutes
```

   - Increase `PROCESSOR_WORKER_COUNT` (each additional worker adds ~200 events/sec capacity)
   - If you are above 5,000 events/sec sustained, this is the Kafka migration trigger. See [TRADEOFFS.md](TRADEOFFS.md).

6. Monitor recovery:
```bash
watch -n 10 'chainpulse-cli eventbus lag'
# Lag should decrease as processor catches up
# At 8 workers processing 1,600/sec against 1,200/sec production: catches up at 400/sec
# At 8m24s lag (~500K events): clears in approximately 20 minutes
```

---

### Dead-Letter Events

Symptoms: `chainpulse_eventbus_dlq_count > 0`. An alert fires. Events are accumulating in `event_dead_letters`.

1. List dead-letter events:
```bash
chainpulse-cli eventbus dlq list --tenant acme-corp
# EVENT_ID    EVENT_TYPE      RETRY_COUNT  LAST_ERROR                    CREATED_AT
# evt_01HABC  orders.created  5            consumer panic: nil ptr deref  2026-05-06T11:00:00Z
```

2. Inspect the event payload:
```bash
chainpulse-cli eventbus dlq inspect --event-id evt_01HABC
# {
#   "event_id": "evt_01HABC",
#   "event_type": "orders.created",
#   "payload": { "order_id": "ord_01HXYZ", "sku": null, ... }  ← sku is null
#   "error_history": [
#     {"attempt": 1, "error": "nil pointer dereference at processor.go:142"},
#     ...
#   ]
# }
```

3. If the event has a bad payload (data error, not a code bug):
```bash
# Discard the event (it will never process successfully)
chainpulse-cli eventbus dlq discard --event-id evt_01HABC --reason "null SKU in payload, data error"
# Event discarded. Audit record created.
```

4. If the event has a good payload and the consumer had a bug (now fixed):
```bash
# Replay the event after deploying the fix
chainpulse-cli eventbus dlq replay --event-id evt_01HABC
# Event re-queued for processing.
```

5. If multiple events have the same error pattern (a consumer bug affecting all events of a type):
```bash
# Do NOT replay until the bug is fixed — you will re-poison the same events
# Deploy the fix first, then bulk replay
chainpulse-cli eventbus dlq replay --event-type orders.created --tenant acme-corp
# 12 events re-queued for processing.
```

---

### ML Service: Fallback Rate High

Symptoms: `chainpulse_ml_fallback_rate > 0.10` (10% of routing decisions are using historical rates instead of ML inference). Alert fires.

1. Check ML subprocess status:
```bash
chainpulse-cli ml subprocess status
# Status: unresponsive (last heartbeat: 4m 32s ago)
```

2. Restart the ML subprocess:
```bash
chainpulse-cli ml subprocess restart
# ML subprocess restarted (PID: 15001). Warming up...
# Health check in 30s...
chainpulse-cli ml subprocess status
# Status: healthy (PID: 15001, uptime: 45s)
```

3. Verify fallback rate drops:
```bash
watch -n 30 'curl -s http://localhost:9090/metrics | grep chainpulse_ml_fallback_rate'
# Should drop from >0.10 to <0.01 within 2 minutes of subprocess recovery
```

4. If the subprocess keeps crashing, check Python dependency issues:
```bash
# Check ML subprocess logs
tail -100 /var/log/chainpulse/ml-subprocess.log
# Look for: import errors, model loading failures, OOM signals
```

5. If the model artifact is corrupted (model loading failure):
```bash
chainpulse-cli ml rollback --model delay --to-version delay-v6
# Rolled back to delay-v6. Restart ML subprocess to load.
chainpulse-cli ml subprocess restart
```

**While ML is in fallback mode:** routing decisions use historical Postgres delay rates. Orders are not blocked. The fallback is acceptable for hours. Extended ML unavailability (>1 hour) should trigger investigation — historical rates do not reflect current supplier conditions.

---

### ML Model Drift: Rolling Accuracy Below Threshold

Symptoms: `chainpulse_ml_rolling_accuracy{model="delay"} < 0.72`. Warning alert fires.

1. Check current drift metrics:
```bash
chainpulse-cli ml drift status --model delay
# Rolling accuracy (7d): 0.68  ← below warn threshold 0.72
# Rolling accuracy (1d): 0.61  ← accelerating drift
# Last training: 2026-04-28T03:00:00Z  (9 days ago)
# Next scheduled training: 2026-05-05T03:00:00Z  (overdue — training job failed?)
```

2. Check if the scheduled training job ran:
```bash
chainpulse-cli ml training history --model delay --last 5
# 2026-04-28T03:00:00Z  COMPLETED  accuracy=0.84
# 2026-05-05T03:00:00Z  FAILED     "insufficient training data: only 847 shipments in window"
```

3. If the training job failed due to insufficient data, trigger it with an extended window:
```bash
chainpulse-cli ml train \
  --model delay \
  --window-days 60 \  # extend from default 30 to 60 days
  --reason "extended window due to low shipment volume this month"
# Training job started. ETA: 8 minutes.
```

4. Monitor training completion and eval gate:
```bash
chainpulse-cli ml training status --model delay
# Status:  training (67% complete)
# ...
# Status:  eval_gate (running holdout validation)
# ...
# Status:  candidate (accuracy=0.79, above gate floor 0.75)
# Promote to active? [y/N]:
chainpulse-cli ml promote --model delay --version delay-v8
# delay-v8 promoted to active. delay-v7 deprecated.
```

5. Monitor rolling accuracy after promotion:
```bash
watch -n 300 'chainpulse-cli ml drift status --model delay'
# Expect rolling accuracy to recover within 24-48 hours as new predictions
# are made against the better-fit model
```

---

### Policy Evaluation: Unexpected DENY

Symptoms: a user or API key is receiving `403 Forbidden` on a request that should be allowed.

1. Check the audit log for the denied request:
```bash
chainpulse-cli audit log \
  --tenant acme-corp \
  --actor-id user-456 \
  --last 5
# 10433  2026-05-06T12:10:11Z  order.update  user-456  DENY  (no matching rule)
```

2. Run a policy eval trace with the exact context:
```bash
chainpulse-cli policy eval \
  --tenant acme-corp \
  --actor-id user-456 \
  --actor-role warehouse_manager \
  --actor-region ontario \
  --resource-type order \
  --resource-id ord_01HXYZ \
  --resource-region bc \           ← different region from actor
  --action order.update
# Decision: DENY
# Reason:   No matching rule.
#           Nearest match: warehouse_manager_update_order_v3
#           Rule condition failed: input.actor.region == input.resource.region
#           Actor region: ontario
#           Resource region: bc
```

3. Determine: is the DENY correct (user should not update cross-region orders) or a misconfiguration (user's region is wrong)?

4. If the user's assigned region is wrong, update it:
```bash
chainpulse-cli user update --id user-456 --region bc --tenant acme-corp
# User updated. JWT re-issued on next login.
# Note: existing JWT tokens carry the old region claim until they expire (15 minutes).
```

5. If the policy rule is wrong, update it:
```bash
chainpulse-cli policy put \
  --tenant acme-corp \
  --name warehouse_manager_update_order \
  --file updated-policy.rego
# Policy v4 uploaded. Active within 60 seconds.

# For immediate effect:
chainpulse-cli policy reload --tenant acme-corp
```

---

### Audit Chain Verification Failure

Symptoms: `chainpulse-cli audit verify` reports a break.

```bash
chainpulse-cli audit verify --tenant acme-corp --from 0
# Chain break at record 8,421.
# Expected prev_hash: abc123...
# Found prev_hash:    def456...
```

**This is a potential security incident. Do not dismiss it. Do not modify records.**

1. Immediately preserve a full audit dump:
```bash
chainpulse-cli audit dump --tenant acme-corp --all \
  > audit_dump_acme_$(date +%Y%m%dT%H%M%S).json
```

2. Identify the records on either side of the break:
```bash
chainpulse-cli audit log --from 8418 --to 8425 --tenant acme-corp
```

3. Check if the break coincides with a schema migration or a service restart:
```bash
chainpulse-cli audit log --from 8418 --to 8425 --filter system
# If a schema migration record exists near index 8421, the canonical JSON
# format may have changed — this is a bug in the migration, not tampering.
```

4. File a security incident report with:
   - The full audit dump
   - The verification output
   - The deployment history around the break timestamp
   - Whether the break is consistent with a software bug or unexplained

5. Do not restore from backup without security team approval. A backup restore that eliminates the break may also eliminate evidence of tampering.

---

## Disaster Scenario Walkthrough

### Supplier Outage + Demand Spike + Event Processor Lag

This is the scenario that tests everything simultaneously.

**T+0:** Supplier `sup-acme-primary` stops responding. Active shipments begin aging past their expected arrival windows.

**T+5 minutes:** The shipment delay detector (runs every 5 minutes) queries active shipments. 12 shipments from `sup-acme-primary` are overdue by > 6 hours. The detector publishes 12 `shipments.delayed` events to the event bus outbox.

**T+5 minutes (simultaneous):** A flash sale begins for `WIDGET-XL`. Order volume spikes 4x. The outbox relay is publishing `orders.created` events at 4x normal rate. Event production rate is now 1,800 events/sec.

**T+8 minutes:** The stream processor is processing at 1,600 events/sec. It is falling behind at 200 events/sec. Lag is 2.4 minutes and growing. The Prometheus warning threshold (5 minutes) has not fired yet.

**T+12 minutes:** Lag reaches 5 minutes. Warning alert fires. Analytics tables are stale. The stream processor has not yet processed the `shipments.delayed` events — the delay rate for `sup-acme-primary` in the analytics table has not been updated.

**What the system gets right:** Every order is being created correctly. The outbox relay is keeping up — all `orders.created` events are published within 500ms of commit. Inventory reservations are being created. The policy engine is evaluating every request correctly. Nothing is lost.

**What the system gets wrong:** The ML module's delay prediction is reading from a pre-aggregated Postgres table that the stream processor has not updated yet. The delay probability for `sup-acme-primary` routes is still 0.22 (historical) instead of the updated 0.89 (based on today's events). New orders during this window are routing to `sup-acme-primary` with a falsely low delay prediction.

**T+15 minutes:** Lag reaches 15 minutes. Critical alert fires. The operator reads this runbook. First action: check what the stream processor is actually behind on.

```bash
chainpulse-cli eventbus lag --verbose
# Lag: 15m 12s (CRITICAL)
# Stream processor processing rate: 1,580 events/sec
# Event production rate: 1,820 events/sec
# Deficit: 240 events/sec

# Increase worker count
PROCESSOR_WORKER_COUNT=12  # add 4 workers (+~800 events/sec capacity)
# Restart the stream processor goroutines (no full restart needed)
chainpulse-cli stream-processor scale --workers 12
```

**T+17 minutes:** Stream processor is now processing 2,400 events/sec against 1,820 events/sec production. Lag begins to decrease at 580 events/sec. At 15 minutes of accumulated lag (~500K events), the processor clears in approximately 14 minutes.

**T+31 minutes:** Lag returns to 0. The stream processor processes the `shipments.delayed` events. The analytics table updates. The ML module's next inference call reads the updated delay rate. `sup-acme-primary` delay probability jumps to 0.89. Routing begins selecting `sup-backup-west` for new orders.

**T+31 minutes:** Polling clients receive `supplier_anomaly` alert on their next poll (within 30 seconds). Warehouse managers see the affected shipments and escalate to the supplier.

**What the audit log shows:** Every order created during the 15-minute window is audited. Every routing decision is logged with `ml_fallback=false` (ML inference was running) but with the stale delay probability (0.22). The audit record includes the ML model version and the prediction — an auditor can reconstruct exactly which orders were routed sub-optimally and why.

**What needs human review after recovery:**
- The 12 delayed shipments from `sup-acme-primary` — status, customer impact, expedite options
- The orders routed to `sup-acme-primary` during the 15-minute stale window — consider proactive re-routing to `sup-backup-west`
- The stream processor scaling configuration — if 4x spikes are expected to recur, the default worker count should be raised

---

## Environment Variables Reference

```bash
# Core
CHAINPULSE_ENV=production
LOG_LEVEL=info

# Postgres
POSTGRES_DSN=postgres://chainpulse:password@postgres:5432/chainpulse
POSTGRES_MAX_CONNS=50
POSTGRES_CONN_TIMEOUT_MS=5000

# Redis
REDIS_ADDR=redis:6379
REDIS_POOL_SIZE=20
REDIS_CONNECT_TIMEOUT_MS=500

# Event bus
EVENT_RETENTION_DAYS=30
EVENT_MAX_RETRY=5
EVENT_DEDUP_WINDOW_HOURS=48
DLQ_ALERT_THRESHOLD=10
OUTBOX_RELAY_BATCH_SIZE=100
OUTBOX_RELAY_INTERVAL_MS=500
OUTBOX_RELAY_WORKERS=4

# Stream processor
PROCESSOR_WORKER_COUNT=8
PROCESSOR_LAG_WARN_SECONDS=300
PROCESSOR_LAG_CRITICAL_SECONDS=900

# ML
ML_INFERENCE_TIMEOUT_MS=200
ML_FALLBACK_ALERT_RATE=0.10
ML_DRIFT_ALERT_THRESHOLD=0.72
ML_DRIFT_CRITICAL_THRESHOLD=0.65
ML_MODEL_REFRESH_INTERVAL_SECONDS=300

# Orders
ORDER_HIGH_VALUE_THRESHOLD_USD=50000

# Inventory
INVENTORY_CACHE_TTL_SECONDS=300
INVENTORY_LOW_STOCK_CHECK_INTERVAL_SECONDS=60
INVENTORY_RESERVATION_DEFAULT_TTL_HOURS=1

# Shipment
SHIPMENT_DELAY_MARGIN_HOURS=6
SHIPMENT_DELAY_CHECK_INTERVAL_MINUTES=5

# Polling
POLLING_CACHE_TTL_SECONDS=10
RECOMMENDED_POLL_INTERVAL_SECONDS=30

# Policy
POLICY_REFRESH_INTERVAL_SECONDS=60
POLICY_EVAL_TIMEOUT_MS=10

# Auth
JWT_PUBLIC_KEY_PATH=/certs/chainpulse-jwt.pub
JWT_ACCESS_TOKEN_TTL_MINUTES=15
JWT_REFRESH_TOKEN_TTL_HOURS=168

# Rate limiting
RATE_LIMIT_DEFAULT_RPM=1000

# Idempotency
IDEMPOTENCY_KEY_TTL_HOURS=24
IDEMPOTENCY_RECONCILE_INTERVAL_MINUTES=5

# Audit
AUDIT_HASH_ALGORITHM=sha256
AUDIT_WRITE_TIMEOUT_MS=500

# Observability
PROMETHEUS_PORT=9090
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

---

## See Also

- [Architecture](ARCHITECTURE.md) — system map, module responsibilities, request lifecycle
- [Design Doc](DESIGN_DOC.md) — event bus guarantees, ML failure modes, policy evaluation, audit chain
- [Tradeoffs](TRADEOFFS.md) — why each component was built the way it was
- [Threat Model](security/THREAT_MODEL.md) — attack surfaces and mitigations
