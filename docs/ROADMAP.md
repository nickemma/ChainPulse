# ChainPulse Roadmap

**Current Phase:** Phase 1 — Foundation
**Last Updated:** May 2026

---

## Design Philosophy

Complexity is earned, not assumed. Each phase adds one capability that is justified by the previous phase being complete and correct. A phase is not complete when the happy path works — it is complete when the failure paths are understood, tested, and documented.

The rule: **every phase exit requires a failure test, not just a feature test.** Phase 2 is not done when orders can be created. It is done when a duplicate order (same idempotency key) returns the correct cached response, an order that fails policy evaluation is audited, and a Postgres failure mid-transaction rolls back cleanly without an orphaned outbox row.

This discipline is why the system has documented failure modes at all. Building with exit criteria forces you to think about what the system does when things go wrong before you move on to the next thing.

---

## Status

| Component | Status | Phase |
|---|---|---|
| Project structure + CI | ✅ Complete | 1 |
| Docker compose (Postgres + Redis) | ✅ Complete | 1 |
| Hexagonal module skeleton (order, inventory, shipment) | ✅ Complete | 1 |
| Gateway: JWT validation + routing | 🔄 In Progress | 2 |
| Gateway: Redis rate limiting | ⬜ Not started | 2 |
| Gateway: Idempotency key enforcement | ⬜ Not started | 2 |
| Auth + Policy engine (RBAC + ABAC) | ⬜ Not started | 2 |
| Order module: state machine + Postgres persistence | ⬜ Not started | 3 |
| Order module: transactional outbox | ⬜ Not started | 3 |
| Order module: optimistic locking | ⬜ Not started | 3 |
| Order module: high-value order gating | ⬜ Not started | 3 |
| Inventory module: stock management + reservations | ⬜ Not started | 3 |
| Inventory module: Redis cache + low-stock detection | ⬜ Not started | 3 |
| Shipment module: state machine + delay detection | ⬜ Not started | 3 |
| Internal event bus: outbox relay + consumer registration | ⬜ Not started | 4 |
| Internal event bus: dead-letter mechanism | ⬜ Not started | 4 |
| Internal event bus: replay from watermark | ⬜ Not started | 4 |
| Stream processor: rolling metrics + anomaly signals | ⬜ Not started | 5 |
| Polling layer: alert endpoint + burst handling | ⬜ Not started | 5 |
| Tamper-evident audit log (hash chain) | ⬜ Not started | 5 |
| ML module: demand forecasting (training + serving) | ⬜ Not started | 6 |
| ML module: delay prediction (training + serving) | ⬜ Not started | 6 |
| ML module: model lifecycle (versioning + eval gate) | ⬜ Not started | 6 |
| Prometheus metrics + Grafana dashboards | ⬜ Not started | 7 |
| Multi-tenant isolation tests | ⬜ Not started | 7 |
| Disruption simulation suite | ⬜ Not started | 7 |

---

## Phase 1 — Foundation: Project Structure and Module Skeleton

**Goal:** A compilable, runnable binary with the hexagonal module structure in place and no business logic yet. The scaffolding that all future phases build on.

**What gets built:**
- Repository structure: `cmd/`, `internal/`, `ml/`, `shared/`, `docs/`
- Hexagonal skeleton for order, inventory, and shipment modules: domain package, ports (interfaces), adapters (stubs), app service (empty)
- Architecture linting in CI: cross-module package imports fail the build
- Docker compose: Postgres and Redis running locally with health checks
- `make run` starts the binary and returns `200 OK` on `/health`
- `make test` runs (empty test suite passes)

**What is explicitly not built:** Any business logic, any database writes, any authentication. The skeleton compiles. The health check passes. Nothing else.

**Exit criteria:**
- `make run` starts cleanly
- `make test` passes
- A deliberate cross-module import (e.g., order importing inventory internals) fails the CI architecture lint
- `docker compose up` brings up Postgres and Redis with healthy status

---

## Phase 2 — Gateway, Auth, and Policy Engine

**Goal:** Every request is authenticated, authorized, rate-limited, and idempotency-checked before it reaches a module. No module logic exists yet — the gateway and policy engine are tested against stub handlers.

**What gets built:**

*Gateway:*
- JWT validation middleware (RS256, public key from env)
- API key resolution (Postgres lookup, hashed storage)
- Redis sliding-window rate limiter (per-tenant, per-route)
- In-memory fallback rate limiter (activated when Redis is unavailable)
- Idempotency key enforcement for POST endpoints (Redis primary, Postgres fallback)
- Structured request logging (tenant, actor, route, latency, policy decision)

*Policy engine:*
- Policy DSL: Rego-inspired, subset of features needed for ChainPulse rules
- Policy evaluator: deny-by-default, cross-tenant deny hardcoded before allow rules
- Policy versioning in Postgres: draft → active → deprecated lifecycle
- In-memory policy cache with 60-second refresh
- `chainpulse-cli policy reload` for forced cache flush
- Audit record written for every policy decision

*Auth:*
- JWT issuance endpoint (`POST /v1/auth/token`) for development use
- Refresh token flow
- API key rotation endpoint

**Failure tests (required for exit):**
- A request with an expired JWT returns `401`
- A request with a valid JWT for tenant A attempting to access a tenant B resource returns `403` (cross-tenant deny rule fires)
- A role that has no matching allow rule returns `403` (deny-by-default)
- A duplicate `POST /orders` with the same idempotency key returns the cached response without hitting the stub handler a second time — verified by a request counter on the stub
- When Redis is killed mid-test, rate limiting falls back to per-instance and the test verifies the fallback behavior explicitly (not just that it "works")
- A policy evaluation that exceeds `POLICY_EVAL_TIMEOUT_MS` returns `DENY` and is logged

**Exit criteria:**
- All gateway failure tests pass
- Policy eval p99 < 5ms under 200 concurrent requests (load test, not benchmark)
- A cross-module package import still fails CI lint (regression check)
- Audit log contains a record for every request in the test suite, with no gaps

---

## Phase 3 — Core Domain Modules

**Goal:** Orders can be created, inventory can be tracked, shipments can be managed. Every write is durable and every state transition is audited.

**What gets built:**

*Order module:*
- Order state machine: `draft → confirmed → allocated → shipped → delivered → closed`
- Postgres persistence: `orders` table, `version` column for optimistic locking
- Transactional outbox: `order_events_outbox` table, committed in same transaction as order write
- High-value order gating: additional policy evaluation for orders above threshold
- Order history: every state transition logged

*Inventory module:*
- Stock management: `inventory` table with physical count and soft reservation columns
- Reservation model: `inventory_reservations` table, TTL-bound, background expiry job
- Redis cache: inventory reads cached with 5-minute TTL, invalidated on write
- Low-stock detection: background worker, `inventory.low_stock` event published when threshold crossed

*Shipment module:*
- Shipment state machine: `created → in_transit → out_for_delivery → delivered`
- Delay detection: background worker, `shipments.delayed` event published when window exceeded
- Supplier event ingestion: `/v1/ingest/shipment-events` endpoint, validated and forwarded to event bus
- Status history: `shipment_history` table, every transition recorded

**Failure tests (required for exit):**
- Concurrent modification of the same order by two goroutines — exactly one succeeds, one receives `409 Conflict`, no silent data corruption
- A Postgres failure (connection killed) mid-order-write leaves no orphaned outbox row — transaction rolled back atomically
- A soft reservation expires after TTL — background job releases it and publishes `inventory.reservation_expired`
- A shipment exceeding its delay window triggers `shipments.delayed` event in the next detector run
- A supplier event with an illegal state transition is rejected and logged; the outbox event is preserved

**Exit criteria:**
- Full order lifecycle (create → confirm → allocate → ship → deliver) end-to-end test passes
- Concurrent order modification test passes with 0 silent corruptions across 100 iterations
- Tenant isolation test: two tenants creating orders simultaneously, no cross-contamination in any read endpoint
- All module handlers are wrapped in policy evaluation — a request with no matching policy rule returns `403`, not a module error

---

## Phase 4 — Internal Event Bus

**Goal:** Modules communicate via events. No module calls another module's handler directly. The event bus is durable, ordered per-resource, and recoverable after failure.

**What gets built:**

*Outbox relay:*
- Background relay workers (configurable count)
- Consistent hashing of `(tenant_id, resource_id)` to workers (per-resource ordering)
- At-least-once delivery: relay re-publishes on restart if crash before `published=true` mark
- Consumer deduplication: `processed_events` table, 48-hour window

*Consumer registration:*
- Typed consumer interface: `type EventConsumer interface { EventType() string; Handle(ctx, Event) error }`
- Consumer registry: modules register consumers at startup
- Consumer goroutine pool per consumer type

*Dead-letter mechanism:*
- `event_dead_letters` table for events exceeding `EVENT_MAX_RETRY`
- `chainpulse-cli eventbus dlq list/inspect/replay/discard` commands
- Prometheus alert when DLQ count exceeds threshold

*Replay:*
- Watermark tracking per consumer group in Postgres
- `chainpulse-cli eventbus replay --from-watermark 0` rebuilds all derived state from the beginning of the retention window

**Failure tests (required for exit):**
- Relay worker killed between publish and `published=true` mark — event is re-published on restart, consumer deduplication discards the duplicate without reprocessing
- Two goroutines processing the same outbox row simultaneously (simulated race) — consistent hashing prevents this; test verifies only one worker processes each row
- Consumer handler panic — event moves to DLQ after `EVENT_MAX_RETRY`, not processed again automatically
- Full replay from watermark 0 — derived state matches direct Postgres reads for all events in the retention window
- Event bus lag alert fires at exactly `PROCESSOR_LAG_WARN_SECONDS` (not before, not after)

**Exit criteria:**
- No module handler calls another module's adapter directly — all cross-module communication via event bus (verified by architecture lint rule)
- Relay crash-and-restart test: 1,000 events published, relay killed randomly 10 times, all 1,000 events processed exactly once (deduplication log shows duplicate counts)
- DLQ test: 5 consumer panics on the same event, event in DLQ, replay after fix processes successfully

---

## Phase 5 — Stream Processor, Polling Layer, and Audit Log

**Goal:** Derived state is maintained from events. Clients can poll for alerts. Every sensitive action has a tamper-evident audit record.

**What gets built:**

*Stream processor:*
- Rolling metrics: 1h/24h/7d order volume, delay rates, inventory turnover — written to Postgres analytics tables
- Anomaly signals: demand spike (>2x rolling average), supplier anomaly (>3x historical delay rate)
- Redis dashboard cache: fast reads for poll responses
- Lag monitoring: Prometheus metrics, warn/critical thresholds

*Polling layer:*
- `/v1/alerts/pending?since={event_id}` endpoint
- Redis cache for poll responses: 10-second TTL, keyed by `tenant:{id}:alerts:since:{event_id}`
- `stale: true` flag in response when stream processor lag exceeds critical threshold
- Burst handling: cache serves repeated polls within TTL without hitting Postgres

*Audit log:*
- `audit_log` table with hash chain: `prev_hash`, `hash`, canonical JSON serialization
- `chainpulse-cli audit verify` — recomputes chain from genesis
- Audit records written for: all policy decisions, order state transitions, inventory threshold crossings, shipment status updates, policy version changes, ML model promotions

**Failure tests (required for exit):**
- Stream processor killed and restarted — replay from watermark rebuilds analytics tables to match Postgres state
- Poll response under mass reconnect (50 clients with same last-seen ID simultaneously) — Redis cache absorbs 49 requests, only 1 hits Postgres
- Audit chain tamper test: manually update a record's content in Postgres, `audit verify` detects the break at the correct index
- `stale: true` appears in poll responses when stream processor lag exceeds critical threshold — tested by artificially pausing the processor
- Two concurrent poll requests with identical parameters — exactly one Postgres query fired, both receive identical response (cache hit verification)

**Exit criteria:**
- Audit chain verification passes after a full order lifecycle (create through deliver) with 0 breaks
- Poll response time p99 < 15ms under 100 concurrent clients (Redis-cached path)
- Stream processor rebuilds 10,000 events of derived state in < 60 seconds from replay
- Tenant isolation: two tenants' alert streams are strictly separated — verified by asserting tenant A's poll never returns tenant B's events across 1,000 iterations

---

## Phase 6 — ML Module

**Goal:** Demand forecasting and delay prediction are integrated into live decisions. Model lifecycle is managed explicitly with an eval gate and rollback capability.

**What gets built:**

*Demand forecasting:*
- Training job: reads from Postgres analytics tables, trains time-series model (scikit-learn), stores artifact
- Cold-start handling: regional average prior for SKUs with < 30 days history, `data_confidence` field in response
- Serving: internal HTTP endpoint called by inventory restocking job

*Delay prediction:*
- Training job: binary classification on `shipments.delayed` labels, reads from Postgres
- Cold-start handling: global delay rate prior for new suppliers (after < 10 shipments)
- Serving: internal HTTP endpoint called by routing logic, 200ms timeout, fallback to Postgres historical rates
- Drift monitoring: rolling accuracy tracked by stream processor against actual shipment outcomes

*Model lifecycle:*
- Training → candidate → active → deprecated state machine in Postgres
- Eval gate: holdout accuracy floor before promotion
- `chainpulse-cli ml promote/rollback/train` commands
- Prometheus metrics: inference latency, fallback rate, rolling accuracy, drift score

**Failure tests (required for exit):**
- ML subprocess killed mid-request — routing falls back to historical Postgres rates within 200ms, `ml_fallback=true` logged
- Rolling accuracy drops below critical threshold — model auto-demoted to `degraded`, routing falls back, critical alert fires
- Cold-start: new SKU with 0 history — forecast returns regional average with `data_confidence: "low"`, inventory module applies safety buffer
- Model rollback: promote delay-v8, observe inference, rollback to delay-v7 — Prometheus shows version change, no requests blocked during rollback
- Concurrent routing calls during ML training job — inference continues on active model while training runs (no lock contention)

**Exit criteria:**
- Full model lifecycle (train → eval gate → candidate → active → deprecated) end-to-end test passes
- Fallback test: ML unavailable for 5 minutes, 100% of routing decisions use Postgres fallback, 0 orders blocked
- Disaster scenario walkthrough from RUNBOOK.md runs end-to-end with assertions on: correct routing re-evaluation after lag recovery, correct audit records for stale-window decisions

---

## Phase 7 — Observability, Isolation Testing, and Hardening

**Goal:** The system is fully observable, tenant isolation is proven at every layer, and the disruption simulation suite passes.

**What gets built:**

*Observability:*
- Prometheus metrics for every component (see RUNBOOK.md for full list)
- Grafana dashboards: event bus health, ML model status, order pipeline, tenant activity
- Structured logging audit: every log line has `tenant_id`, `trace_id`, `actor_id`
- OpenTelemetry trace context propagation through the event bus (correlation ID → trace ID)

*Isolation testing:*
- `tenant_isolation_test.go` for every module: two tenants, all read endpoints, no cross-contamination
- Postgres RLS policy test: direct DB connection as application service account, query without tenant clause → rejected
- Redis key structure test: application cannot construct an un-prefixed Redis key (wrapper enforced)
- Cache poisoning test: manually insert a cross-tenant payload in Redis, verify application detects and discards it

*Disruption simulation:*
- Supplier blackout simulation: stop supplier events, verify delay detection, verify routing re-evaluation
- Demand spike simulation: inject 4x order volume, verify stream processor lag response, verify stale flag
- Redis failure simulation: kill Redis, verify all three fallback paths activate correctly
- ML outage simulation: kill ML subprocess, verify fallback rate, verify recovery after restart
- Concurrent order modification simulation: 100 goroutines updating the same order, verify 0 silent corruptions

**Exit criteria:**
- All disruption simulations pass assertions (documented expected behavior matches observed behavior)
- Tenant isolation tests pass across all modules with 0 cross-contamination in 10,000 iterations
- Audit chain verification passes after every disruption simulation
- Prometheus alert coverage: every failure mode in RUNBOOK.md has a corresponding Prometheus alert that fires in the simulation

---

## Post-Roadmap

- [ ] WebSocket real-time push (replaces polling layer when latency requirements tighten)
- [ ] Kafka event bus (replaces internal bus when throughput exceeds 5,000 events/sec or cross-process consumers required)
- [ ] Linear programming optimization for multi-supplier allocation
- [ ] What-if simulation mode (project impact of supplier outage before it happens)
- [ ] SOC2-style compliance export from audit log
- [ ] Separate ML service (when training requires GPU or inference latency budget requires dedicated compute)
- [ ] ClickHouse analytics layer (when OLAP queries exceed 500ms on pre-aggregated Postgres tables)

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Outbox relay falling behind under peak load | Medium | Medium | Worker count tuning documented; Kafka migration path defined |
| Cross-tenant data leak via missing tenant clause | Low | Critical | RLS as database-level backstop; isolation tests for every query path |
| ML model drift causing systematic routing errors | Medium | Medium | Rolling accuracy monitoring; automatic demotion below threshold |
| Postgres connection pool saturation under thundering herd | Medium | High | Polling cache absorbs burst; poll interval tuning documented in runbook |
| Policy cache serving stale policy after security change | Low | High | `policy reload` command; 60-second window documented and accepted |
| Module boundary erosion under delivery pressure | Medium | Medium | Architecture lint in CI; cross-module imports fail the build |

**The non-negotiables:** No order is lost between Postgres commit and event publication (outbox guarantees this). No tenant sees another tenant's data (RLS + query scoping + isolation tests). Every sensitive action has an audit record before the response is returned. These three invariants are the product. The phase exit criteria are designed to prove them.
