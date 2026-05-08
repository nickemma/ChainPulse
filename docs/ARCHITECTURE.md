# ChainPulse Architecture

**Status:** In Development
**Last Updated:** May 2026
**Author:** [@nickemma](https://github.com/nickemma)

---

## Overview

ChainPulse is a modular monolith. That is not a compromise — it is the explicit architectural choice that makes every other decision in this system coherent.

A microservices architecture for a supply chain platform at this scale introduces network partitions, distributed transaction complexity, and operational overhead that the system does not need and cannot yet justify. A modular monolith achieves the same logical separation — order domain, inventory domain, shipment domain, ML domain — with in-process communication, a single deployment artifact, and a fraction of the operational surface area.

The central architectural constraint: **module boundaries must be as strict as if the modules were separate services.** No module imports another module's internal packages. All cross-module communication goes through the internal event bus or explicit service interfaces. The boundary is enforced by Go package structure and validated by architecture tests — a compile-time guarantee, not a convention.

This constraint means the system can be decomposed into true microservices later — not by refactoring, but by extracting modules that already have clean interfaces — at the point where the throughput or team-size justification actually exists.

---

## System Map

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        External Clients                                  │
│         Dashboard  ·  Partner Integrations  ·  IoT Feeds                │
└─────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌─────────────────────────────────────────────────────────────────────────┐
│                   Gateway Layer  (internal/gateway/)                     │
│   REST routing  ·  JWT validation  ·  API key auth                      │
│   Redis-backed rate limiting  ·  Idempotency key enforcement            │
│   Request logging (outer edge of audit trail)                           │
└─────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌─────────────────────────────────────────────────────────────────────────┐
│                  Auth + Policy Engine  (internal/policy/)                │
│   RBAC baseline  ·  ABAC runtime evaluation  ·  Deny-by-default         │
│   Policy versioning  ·  Every decision logged to audit trail            │
└─────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌──────────────────┬─────────────────────┬───────────────────────────────┐
│  Order Module    │  Inventory Module   │  Shipment Module              │
│  (internal/order)│  (internal/inv)     │  (internal/shipment)          │
│                  │                     │                               │
│  State machine   │  Stock levels       │  Status tracking              │
│  Outbox pattern  │  Reservations       │  Delay detection              │
│  High-value gate │  Low-stock events   │  Supplier ingestion           │
└──────────────────┴─────────────────────┴───────────────────────────────┘
                                   ↓
┌─────────────────────────────────────────────────────────────────────────┐
│              Internal Event Bus  (internal/eventbus/)                    │
│   Postgres-backed outbox  ·  Background relay workers                   │
│   Correlation ID propagation  ·  Replay from any offset                 │
│   Dead-letter table for poison events                                   │
└─────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌──────────────────────────────────────────────────────────────────────────┐
│              Async Processing Layer                                       │
│  ┌────────────────────────┐  ┌──────────────────┐  ┌─────────────────┐ │
│  │   Stream Processor     │  │    ML Module     │  │  Polling Layer  │ │
│  │  (internal/processor)  │  │   (ml/)          │  │  (internal/poll)│ │
│  │                        │  │                  │  │                 │ │
│  │  Rolling metrics       │  │  Demand forecast │  │  Client-facing  │ │
│  │  Anomaly baselines     │  │  Delay predict   │  │  alert polling  │ │
│  │  Derived state         │  │  Model lifecycle │  │  Burst handling │ │
│  └────────────────────────┘  └──────────────────┘  └─────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌─────────────────────────────────────────────────────────────────────────┐
│                        Storage Layer                                     │
│  ┌──────────────────────────────────────┐  ┌──────────────────────────┐│
│  │           PostgreSQL                 │  │         Redis            ││
│  │  Orders · Inventory · Shipments      │  │  Rate limit counters     ││
│  │  Event outbox · Audit log            │  │  Idempotency keys        ││
│  │  ML models · Policies                │  │  Inventory cache         ││
│  │  Analytics tables (stream processor) │  │  Polling result cache    ││
│  └──────────────────────────────────────┘  └──────────────────────────┘│
└─────────────────────────────────────────────────────────────────────────┘
                                   ↓
┌─────────────────────────────────────────────────────────────────────────┐
│              Observability                                               │
│   Structured JSON logs  ·  Prometheus metrics  ·  Grafana dashboards    │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## Hexagonal Architecture (Ports & Adapters)

Each module is structured as a hexagon. The domain logic sits in the center and has no knowledge of HTTP, Postgres, or Redis. Adapters on the outside translate between the domain and the infrastructure.

```
         ┌──────────────────────────────────────────────┐
         │              Order Module                    │
         │                                              │
         │  ┌────────────────────────────────────────┐  │
         │  │           Domain Core                  │  │
         │  │  OrderService, OrderState, rules       │  │
         │  │  No HTTP. No SQL. No Redis.            │  │
         │  └────────────────────────────────────────┘  │
         │          ↑                    ↑              │
         │   Input Port            Output Port          │
         │  (AppService)          (Repository)          │
         │          ↑                    ↑              │
         │   HTTP Adapter          Postgres Adapter     │
         │   Event Adapter         Event Bus Adapter    │
         └──────────────────────────────────────────────┘
```

**What this means in practice:**

The `OrderService` domain struct does not import `database/sql`. It depends on an `OrderRepository` interface. The Postgres adapter implements that interface. In tests, a fake in-memory adapter implements the same interface. Swapping Postgres for a different store requires changing one adapter, not touching domain logic.

This is not architecture for architecture's sake. It is the reason module boundaries hold under pressure — when a developer under deadline reaches for a quick fix, the package structure makes the shortcut a compile error.

---

## Module Responsibilities

### Gateway — `internal/gateway/`

The single entry point for all external traffic. In a microservices system this would be a separate process; in ChainPulse it is a layer inside the binary that all requests pass through before reaching any module.

**JWT validation:** Access tokens are validated here. Claims (`tenant_id`, `user_id`, `role`, `region`, `supplier_id`) are extracted and attached to the request context. No downstream module re-validates the JWT — the gateway is the trust boundary.

**API key support:** External partners authenticate with rotating API keys. Keys are stored hashed in Postgres. The gateway resolves the key to a synthetic identity with a tenant scope and rate limit tier before the request proceeds.

**Redis-backed rate limiting:** Sliding-window counters in Redis enforce per-tenant, per-route limits. If Redis is unavailable, rate limiting degrades to a per-instance in-memory token bucket — limits are no longer shared across instances, so a tenant with 3 running instances effectively gets 3x the configured limit during the Redis outage. This is documented, alerted on, and acceptable for short outages; extended Redis unavailability requires operator intervention. P99 latency for the rate limit check is ~2ms at steady state.

**Idempotency key enforcement:** `POST /orders` and `POST /shipments` require an `Idempotency-Key` header. The gateway stores the key and response hash in Redis on first execution (TTL: 24 hours). A duplicate request returns the stored response without forwarding to the module. Redis is the primary store; Postgres is the durable fallback. If Redis is unavailable, the check falls back to Postgres — P99 latency for the idempotency check increases from ~8ms to ~95ms, and throughput on write endpoints drops by approximately 35% under load.

**Request logging:** Every request produces a structured JSON log entry: `tenant_id`, `actor_id`, `route`, `method`, `status`, `latency_ms`, `policy_decision`, `idempotency_key`. This is the outer edge of the audit trail — if a request reaches the gateway, it is logged, regardless of what happens inside.

### Auth + Policy Engine — `internal/policy/`

Policy evaluation is synchronous and on the critical path. Every request that passes JWT validation is evaluated against the active policy set before any module handler runs.

**RBAC baseline:** Four roles with default permission sets encoded in policy rules. Role is extracted from the JWT claim and passed to the evaluator as `input.actor.role`.

**ABAC layering:** Attribute policies evaluate constraints that role alone cannot express. The evaluator receives the full request context — actor claims, resource metadata, action, source IP, order value, time of day. It returns allow/deny with the matched rule identifier. Deny rules are evaluated before allow rules. If no rule matches: deny.

**Policy versioning:** Policies are stored in Postgres as versioned rows. The evaluator caches the active policy set in memory with a 60-second refresh. A policy change is visible to all requests within one refresh window. This is an intentional tradeoff — up to 60 seconds of stale policy evaluation is acceptable; the alternative (database read on every request) adds ~15ms P99 latency to every policy evaluation.

**Audit:** Every policy decision — allow or deny — is written to the audit log with the matched rule, full input context, and evaluation latency before the response is returned to the caller. A request that is denied is still audited.

### Order Module — `internal/order/`

Authoritative state for orders. The order lifecycle is a state machine: `draft → confirmed → allocated → shipped → delivered → closed`. Every transition is validated by the domain core before it is persisted.

**Transactional outbox:** The order write and the event publication are a single Postgres transaction. The order row and an outbox row are committed together or not at all. A background relay reads unpublished outbox rows, publishes them to the internal event bus, and marks them published. If the relay crashes between publication and marking, the event is published twice — consumers deduplicate by `event_id`. This is intentional: at-least-once delivery with consumer-side deduplication is simpler and safer than exactly-once semantics at the producer.

**Optimistic locking:** A `version` column on every order row prevents silent concurrent modification. An update that does not match the current version returns `409 Conflict`. The caller retries with the current state. No locks are held between the read and the write.

**High-value order gating:** Orders above `ORDER_HIGH_VALUE_THRESHOLD_USD` (default: $50,000) trigger an additional policy evaluation before transitioning from `draft` to `confirmed`. The gating policy can require a specific role or a specific attribute. The gate decision is logged to the audit trail with the order ID, the threshold, the evaluating policy version, and the actor.

### Inventory Module — `internal/inventory/`

Stock state for all SKUs across all warehouses and regions, scoped per tenant. Postgres is the authoritative store. Redis is the read cache.

**Cache strategy:** On write, the Postgres row is updated and the Redis key is deleted (cache-aside). On read, the Redis key is checked first. A miss reads from Postgres and repopulates Redis with a TTL of 5 minutes. A Redis key for inventory follows the structure `tenant:{tenant_id}:inv:{sku}:{region}` — tenant scoping is structural, not conditional. A bug in the application that omits the tenant prefix produces a key that does not collide with another tenant's data; it produces a malformed key that returns a cache miss.

**Low-stock detection:** A background worker runs on a configurable interval (default: 60 seconds) and checks all SKUs for the tenant against their configured thresholds. When a SKU falls below threshold, it publishes `inventory.low_stock` to the event bus. The event includes the SKU, region, current count, threshold, and tenant ID. The low-stock check is a read-only Postgres query — it does not modify inventory state.

**Reservation model:** A confirmed order creates a soft reservation — a row in the `inventory_reservations` table that reduces available quantity without touching the physical count. Reservations have a TTL. A background job releases expired reservations and publishes `inventory.reservation_expired`. If an order is cancelled within the cancellation window, the reservation is released synchronously and the event is published via the outbox.

### Shipment Module — `internal/shipment/`

Tracks shipments from creation to delivery. Integrates with external supplier event feeds via the ingestion endpoint.

**Delay detection:** A background worker (default interval: 5 minutes) queries all active shipments against their expected arrival timestamps. A shipment that has exceeded its window by more than `SHIPMENT_DELAY_MARGIN_HOURS` (default: 6) triggers a `shipments.delayed` event. The event carries the shipment ID, supplier ID, route, and estimated delay in hours. This event is the primary delay signal for the ML module's training pipeline.

**Supplier event ingestion:** External partners POST to `/v1/ingest/shipment-events` authenticated by API key. The gateway validates the key and the payload schema. The gateway writes the raw event to the event bus as a `supplier.event`. The shipment module consumes `supplier.event` and reconciles against its own state machine — illegal state transitions are rejected and logged. The original event is preserved in the audit trail regardless of whether the transition was accepted.

**Status history:** Every state transition appends a row to the `shipment_history` table with the previous state, new state, timestamp, source (operator ID or supplier event ID), and the identity of the actor. The full history is queryable and included in the audit log for any status update.

### ML Module — `ml/`

Two models run continuously: demand forecasting and delay prediction. Both integrate into live decisions — the ML module is not a reporting tool, it is in the operational loop.

**Demand forecasting:** A time-series model trained on historical order volume, inventory levels, and seasonal signals from Postgres. Retrained weekly. Exposed as an internal HTTP endpoint called by the inventory module's restocking job and the routing logic. Cold-start behavior for new SKUs (fewer than 30 days of history) uses a regional average as a prior — the response includes a `data_confidence` field that is `low` for cold-start predictions. The calling module is responsible for deciding whether to act on a low-confidence forecast.

**Delay prediction:** A binary classification model predicting delay probability for a supplier-route pair. Called synchronously by the routing logic before supplier selection. If the ML module is unavailable or times out (default timeout: 200ms), the routing logic falls back to the 30-day historical delay rate for the route read from a pre-aggregated Postgres table. The fallback is logged with `ml_fallback=true`. An ML outage does not block order creation.

**Model lifecycle:** Training → candidate → active → deprecated. Promotion from candidate to active requires passing the automated eval gate (holdout accuracy above floor). Only one active version per model type per tenant. The active model version is stored in Postgres and loaded by the ML module at startup and on a 5-minute refresh.

### Stream Processor — `internal/processor/`

A background worker that consumes events from the internal event bus and builds derived state in Postgres analytics tables and Redis.

**What it computes:** Rolling 1h/24h/7d order volume per SKU and region; shipment delay rate per supplier and route; inventory turnover per warehouse; demand spike signals (>2x rolling average triggers `demand_spike` event); supplier anomaly signals (>3x historical delay rate triggers `supplier_anomaly` event).

**Lag behavior:** The stream processor runs as a goroutine pool. If the processor falls behind (outbox rows older than `PROCESSOR_LAG_WARN_THRESHOLD`, default: 5 minutes), a Prometheus alert fires. If lag exceeds `PROCESSOR_LAG_CRITICAL_THRESHOLD` (default: 15 minutes), the analytics tables are stale and the polling layer returns cached results with a `stale: true` flag in the response. Clients are not silently served stale data — staleness is explicit in the API response.

**Replay:** Because events are stored in the Postgres outbox (retained for `EVENT_RETENTION_DAYS`, default: 30 days), the stream processor can rebuild all derived state from scratch by resetting its watermark and replaying from the beginning of the retention window. This is the recovery path after a processor bug — fix the bug, reset the watermark, replay. No data is lost.

### Polling Layer — `internal/polling/`

Clients poll for updates — shipment status changes, low-stock alerts, anomaly signals — rather than receiving pushed notifications.

**What clients poll:** `/v1/alerts/pending` returns all unacknowledged alerts for the authenticated tenant and role since the client's last-seen event ID. The client sends its last-seen ID with each request; the server returns events newer than that ID.

**Burst handling:** On reconnect (or first poll after a long absence), all clients with the same tenant poll simultaneously. This is the thundering herd problem inherent to polling. Mitigation: poll responses are cached in Redis keyed by `tenant:{id}:alerts:since:{event_id}` with a 10-second TTL. Within a 10-second window, all clients with the same tenant and the same last-seen ID receive the cached response. The first request populates the cache; subsequent requests within the TTL are served from Redis at ~2ms latency instead of the ~40ms Postgres query.

**Latency floor:** The minimum time between an event firing and a client receiving it equals one poll interval. The default poll interval recommendation is 30 seconds. A critical `inventory.low_stock` alert can reach a client up to 30 seconds after it fires. This is a known, accepted limitation. At the point where alert latency requirements drop below 10 seconds, the polling layer is replaced with WebSocket push — the event infrastructure does not change, only the delivery mechanism.

---

## Tenant Isolation Guarantees

Multi-tenancy is the kind of claim that sounds easy and fails quietly. This section is explicit about what ChainPulse guarantees, how those guarantees are enforced, and what the worst-case failure looks like.

### What is guaranteed

**Postgres:** Every table that contains tenant-scoped data has a `tenant_id` column with a `NOT NULL` constraint and a foreign key to the `tenants` table. Every query in the application includes `WHERE tenant_id = $1` with the tenant ID extracted from the validated JWT. Postgres row-level security (RLS) is enabled as a defense-in-depth backstop — even if the application omits the `tenant_id` clause (a bug), the database policy rejects the query for the service account. RLS is not the primary enforcement mechanism; it is the last line of defense.

**Redis:** All Redis keys are prefixed with `tenant:{tenant_id}:`. The key structure is enforced in a central Redis client wrapper — callers pass a key suffix, the wrapper prepends the tenant prefix from the request context. A caller cannot construct an un-prefixed key without bypassing the wrapper. The wrapper is the only path to Redis writes in the application.

**Event bus:** Every event payload includes `tenant_id` as a required field. The event bus relay validates the field before publishing. A consumer that processes an event always has access to the `tenant_id` and is required to scope any state changes accordingly.

**Policy engine:** The deny-by-default rule `input.resource.tenant_id != input.actor.tenant_id → DENY` is evaluated before any allow rule. It cannot be overridden by a tenant-specific policy — it is hardcoded in the evaluator, not stored as a configurable policy row.

### Worst-case failure scenario

The most plausible tenant data leak in ChainPulse is a missing `tenant_id` in a Postgres query that hits a code path not covered by integration tests. Example: a new analytics query added under deadline that reads `SELECT * FROM orders WHERE sku = $1` without the tenant scope. The query returns rows from all tenants. The result is cached in Redis under the querying tenant's key. Other tenants do not see the data (their key is different), but tenant A sees tenant B's orders.

**Detection:** Integration tests run two concurrent tenants for every read endpoint and assert that tenant A's response contains no resources belonging to tenant B. This is a required test for every new query path — it is enforced by a PR checklist item, not by tooling. The RLS policy is the runtime backstop that catches what the tests miss.

**What RLS does not protect:** Redis. A cached response that contains cross-tenant data is served from Redis without hitting the RLS policy. The Redis key structure is the only protection at the cache layer.

### Testing strategy

1. Every module has a `tenant_isolation_test.go` file that creates two tenants, populates data under each, and asserts that all read endpoints return only the requesting tenant's data.
2. The integration test suite runs a full request lifecycle (create order → confirm → ship → deliver) for two tenants simultaneously and asserts no cross-contamination in any intermediate state.
3. The Postgres RLS policy is tested in isolation: a test connects to the database as the application service account and attempts a query without a `tenant_id` clause — the query must be rejected by RLS.

---

## Request Lifecycle

### Order Creation (Happy Path)

```
POST /API/v1/orders
  Authorization: Bearer <JWT>
  Idempotency-Key: idem-abc-123
  │
  ↓ Gateway
  ├─ Validate JWT → {tenant_id: "acme", role: "warehouse_manager", region: "ontario"}
  ├─ Check idempotency key in Redis → miss (first request) ~8ms
  ├─ Check rate limit in Redis → within limit ~2ms
  │
  ↓ Policy Engine
  ├─ Evaluate: role=warehouse_manager, action=order.create, resource.region=ontario
  ├─ Actor.region == resource.region → ALLOW ~1ms
  ├─ Write audit record to Postgres
  │
  ↓ Order Module
  ├─ Validate payload
  ├─ Call routing logic → call ML module (delay prediction) ~40ms
  │   └─ If ML times out after 200ms → fallback to historical rate
  ├─ BEGIN TRANSACTION
  │   ├─ INSERT orders row (status=confirmed, version=1)
  │   └─ INSERT outbox row (event_type=orders.created, published=false)
  ├─ COMMIT
  ├─ Store response in Redis under idempotency key
  └─ Return 201 Created ~total P99: 120ms

  ↓ Background (async, does not block response)
  Outbox relay reads unpublished row
  Publishes orders.created to internal event bus
  Marks outbox row published=true

  ↓ Stream processor consumes orders.created
  Updates rolling order volume in Postgres analytics tables
  Updates Redis dashboard cache
```

### Supplier Blackout (Failure Path)

```
Supplier sup-acme-primary stops sending events
  │
  ↓ Shipment module delay detector (runs every 5 minutes)
  ├─ Queries active shipments for sup-acme-primary
  ├─ shp-001: expected 2026-05-06T18:00Z, now 2026-05-07T01:00Z → 7h overdue
  ├─ Publishes shipments.delayed to event bus
  │
  ↓ Stream processor consumes shipments.delayed
  ├─ Updates delay rate for sup-acme-primary / ontario→bc route
  ├─ delay_rate now 0.89 (threshold: 0.60)
  ├─ Publishes supplier_anomaly event
  │
  ↓ ML module next inference call (next order for this route)
  ├─ delay_probability = 0.91 (above threshold 0.60)
  ├─ Routing selects sup-backup-west (delay_probability = 0.18)
  ├─ Decision logged: {supplier_selected, ml_score, ml_version, fallback=false}
  │
  ↓ Polling layer (next client poll)
  ├─ Returns supplier_anomaly alert to admins
  ├─ Alert includes: supplier, affected shipments, current delay, routing impact
  └─ Audit record written for alert delivery
```

---

## Module Structure

```
chainpulse/
├── cmd/
│   ├── server/               ← Main binary entrypoint
│   └── chainpulse-cli/       ← Operator CLI
├── internal/
│   ├── gateway/              ← JWT, rate limiting, idempotency, routing
│   ├── policy/               ← RBAC/ABAC evaluator, policy versioning, audit
│   ├── order/
│   │   ├── domain/           ← OrderService, state machine, rules (no infra imports)
│   │   ├── app/              ← Application service (orchestrates domain + ports)
│   │   ├── ports/            ← Repository interface, event publisher interface
│   │   └── adapters/         ← Postgres adapter, event bus adapter, HTTP handler
│   ├── inventory/            ← Same hexagonal structure as order/
│   ├── shipment/             ← Same hexagonal structure as order/
│   ├── eventbus/             ← Outbox relay, event publishing, consumer registration
│   ├── processor/            ← Stream processor workers, derived state, anomaly signals
│   ├── polling/              ← Alert polling endpoint, Redis cache, burst handling
│   └── audit/                ← Hash-chained audit log writer
├── ml/
│   ├── forecast/             ← Demand forecasting model + training job
│   ├── delay/                ← Delay prediction model + training job
│   └── lifecycle/            ← Model versioning, eval gate, promotion
├── shared/
│   ├── events/               ← Event type definitions, envelope schema
│   └── testutil/             ← Shared test helpers, fake adapters
├── docs/
│   ├── ARCHITECTURE.md       ← This file
│   ├── DESIGN_DOC.md
│   ├── TRADEOFFS.md
│   ├── RUNBOOK.md
│   ├── ROADMAP.md
│   └── security/
│       └── THREAT_MODEL.md
├── docker-compose.yml        ← Postgres + Redis for local development
└── Makefile
```

---

## References

- [Design Doc](DESIGN_DOC.md) — transactional outbox internals, event bus guarantees, ML failure modes, audit chain design
- [Tradeoffs](TRADEOFFS.md) — modular monolith vs microservices, internal event bus vs Kafka, polling vs WebSockets
- [Runbook](RUNBOOK.md) — operational procedures for every failure mode
- [Roadmap](ROADMAP.md) — phase-by-phase build plan with exit criteria
- [Threat Model](security/THREAT_MODEL.md) — STRIDE analysis, tenant isolation guarantees, attack surfaces
