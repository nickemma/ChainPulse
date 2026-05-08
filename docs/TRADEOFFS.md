# ChainPulse Tradeoffs

**Purpose:** Every major design decision in ChainPulse — what was considered, what was rejected, and the honest cost of what was chosen.
**Last Updated:** May 2026

---

## Why Document Tradeoffs?

A system without documented tradeoffs is a system where the next engineer assumes the current design is the only possible design. Every decision in ChainPulse was made under constraints — team size, operational budget, target throughput, time horizon. Documenting those constraints alongside the decisions means the next engineer knows not just what was chosen but when to revisit it.

One rule for this document: if a tradeoff does not hurt a little, it was not a real tradeoff. Comfortable decisions do not belong here.

---

## Modular Monolith vs. Microservices

**Chosen:** Modular monolith — single deployable binary, strict module boundaries enforced by package structure
**Rejected:** Microservices — separate deployable units per domain

The case for microservices is real: independent deployability, fault isolation at the process level, independent scaling per service, freedom to choose different languages per service. These are genuine advantages.

The case against microservices at ChainPulse's current scale is equally real: distributed tracing becomes mandatory instead of optional, every cross-module call becomes a network call with its own failure mode, database transactions that currently span two tables require distributed transactions or saga patterns, local development requires orchestrating 6+ running services instead of one binary and two Docker containers.

The honest cost of the modular monolith choice: fault isolation stops at the process boundary. A panic in the inventory module that is not caught can bring down the order module in the same binary. Mitigation is recovery middleware at the module handler level — every handler runs in a deferred-recover wrapper and returns 500 rather than crashing the process. This is not as strong as process-level isolation. It is an accepted limitation.

The second honest cost: the modular monolith requires discipline to maintain. Without enforcement, module boundaries erode — a developer under deadline imports an internal package across module boundaries and the boundary disappears. ChainPulse enforces boundaries via Go package structure (cross-module imports are a compile error) and architecture linting in CI. Without that enforcement, this is a monolith that calls itself modular.

**When to revisit:** When any single module's resource requirements (CPU, memory, or I/O) differ from the others by more than 3x under production load, or when team size exceeds 8 engineers working on the same codebase simultaneously.

---

## Internal Event Bus vs. Kafka

**Chosen:** Postgres-backed outbox with background relay workers
**Rejected:** Apache Kafka, RabbitMQ, Redis Streams

This is the decision that gets challenged most in reviews, so it gets the most thorough treatment.

**What the internal event bus actually provides:**
- At-least-once delivery with consumer-side deduplication
- Per-resource ordering (events for the same order ID are processed in sequence)
- 30-day replay window (bounded by outbox table retention)
- Dead-letter mechanism for poison events
- ~500ms consumer lag under normal load
- Zero additional infrastructure — Postgres is already required

**What Kafka provides that the internal bus does not:**
- Multi-process consumers (the internal bus is in-process only)
- Sub-100ms consumer lag at high throughput
- Log compaction and infinite retention (configurable)
- Horizontal consumer scaling independent of the producer
- Native partition-based parallelism without application-level consistent hashing

**The throughput math:** At 2,000 events/sec with 8 stream processor workers processing at ~5ms each, the processor sustains 1,600 events/sec. At sustained peak (2,000 events/sec), lag grows at 400 events/sec. At this rate, the 5-minute warning threshold is hit in 45 minutes. This is monitorable and manageable. At 5,000 events/sec, the math breaks — the processor cannot keep up and the outbox table itself becomes a write bottleneck.

**The ugly truth about this choice:** At 5,000 events/sec, we would need Kafka. We do not have 5,000 events/sec today. The decision to build without Kafka is a bet on current scale holding for long enough to validate the product before paying the operational cost of a Kafka cluster. If that bet is wrong — if the system needs to scale before the architecture is revisited — the migration path is well-defined (outbox relay becomes a Kafka producer, consumer interfaces stay identical) but it is still a migration, and migrations under load are risky.

This is the tradeoff that regrets itself slightly. Kafka from day one would have been operationally heavier but architecturally cleaner as the system grows. The outbox relay is clever in the way that solutions which avoid the right tool are clever — it works until it doesn't, and then you wish you had done it the harder way earlier.

**When to migrate to Kafka:** Sustained event throughput exceeds 3,000 events/sec, OR a consumer needs to run in a separate process, OR replay requirements exceed 30 days.

---

## Single Postgres vs. Postgres + ClickHouse

**Chosen:** Single Postgres instance for both OLTP and analytics
**Rejected:** Postgres (OLTP) + ClickHouse (OLAP), Postgres + TimescaleDB, Postgres + read replica

ClickHouse is genuinely faster for analytical queries — columnar storage, vectorized execution, and compression ratios that make a week of order volume data read 10x faster than row-oriented Postgres. For a system that needs sub-second aggregations over months of data, ClickHouse is the correct answer.

ChainPulse's analytics queries are pre-aggregated by the stream processor into dedicated Postgres tables: `order_volume_hourly`, `delay_rates_daily`, `inventory_turnover_weekly`. These tables are narrow (3–5 columns), indexed on `(tenant_id, sku, region, period)`, and queried by time range. The query patterns are known and bounded — the stream processor writes exactly the aggregations that the dashboard and ML module need. This is not ad-hoc analytics over raw events.

For these pre-aggregated tables, Postgres performs adequately: a time-range query over `order_volume_hourly` for one SKU and region over 90 days reads approximately 2,160 rows. At ~1ms per 1,000 rows for an indexed scan, this is a ~2ms query. ClickHouse would be faster, but 2ms is already fast enough.

**The honest cost:** The pre-aggregation approach means the analytics layer is not flexible. A new dashboard metric requires a new stream processor computation and a new Postgres table. In ClickHouse, a new metric is a new SQL query over the raw event log. ChainPulse trades analytics flexibility for operational simplicity.

**The second honest cost:** OLTP and OLAP workloads compete on the same Postgres instance. Under high write load (order creation surge), analytical reads slow down. The stream processor's analytics writes slow down, which increases consumer lag. The workloads interfere with each other in ways that a separate ClickHouse instance would avoid.

**When to revisit:** When analytical query latency exceeds 500ms for pre-aggregated tables, OR when OLAP write load (stream processor) measurably degrades OLTP write latency (order creation).

---

## Polling vs. WebSockets for Client Alerts

**Chosen:** Client-initiated polling with Redis-cached responses
**Rejected:** WebSocket push, Server-Sent Events (SSE), long-polling

This is the tradeoff that hurts the most in production.

Polling introduces a minimum alert latency equal to the poll interval. At the recommended 30-second interval, a `CRITICAL inventory.low_stock` alert — one that might trigger an emergency procurement decision — reaches the client up to 30 seconds after the event fires. In a warehouse where a stockout costs $10,000 per hour, 30 seconds is not a rounding error.

The thundering herd problem is real and not fully solved. When 50 dashboard clients reconnect simultaneously after a network blip, all 50 poll at the same time. The Redis cache serves 49 of them from the 10-second TTL cache. The 50th — the one that misses the cache because its last-seen ID is slightly different — hits Postgres. Under a true mass reconnect (200+ clients, all with different last-seen IDs), the cache miss rate could saturate the Postgres connection pool.

Consistency guarantees shift to the client. If a client misses a poll window (browser tab in background, mobile app suspended), it must handle the gap correctly — fetch all events since the last-seen ID, process them in order, handle duplicates. This is state management complexity that lives in the client and is easy to implement incorrectly.

**Why polling anyway:** WebSocket connection management in a horizontally-scaled monolith requires a shared connection registry (Redis pub/sub or a sticky load balancer). Adding sticky sessions or Redis pub/sub fan-out to the alert delivery path adds infrastructure complexity that is not justified by the current client count. At 50–200 concurrent dashboard clients, polling with a 30-second interval generates 100–400 requests/minute — comfortably within the rate limit budget and the Postgres connection pool.

**The migration path is clean:** The internal event bus already produces the events. The polling layer reads from the same event tables. Adding WebSocket push means adding a WebSocket handler that subscribes to the same event tables via Postgres LISTEN/NOTIFY (or Redis pub/sub) and pushes to connected clients. The event infrastructure does not change. The delivery mechanism does.

**When to migrate to WebSockets:** When alert latency requirements drop below 10 seconds, OR when connected client count exceeds 500 (at which point thundering herd risk becomes unacceptable), OR when client-side gap handling complexity produces visible bugs.

---

## Embedded ML vs. External ML Service

**Chosen:** ML module embedded in the monolith (Python subprocess called via internal HTTP)
**Rejected:** Separate ML service (dedicated process/container), managed ML platform (SageMaker, Vertex AI)

The ML module runs as a Python subprocess started by the Go binary. The Go routing logic calls it via a localhost HTTP endpoint with a 200ms timeout. The Python process manages model loading and inference. This is not elegant, but it is simple — one additional process, no network hops beyond localhost, no service discovery.

**The honest cost:** The Go binary now has a Python process dependency. If Python is not installed in the deployment environment, the ML module fails to start. The deployment artifact is more complex. A crash in the Python subprocess requires the Go process to detect it (via the health check endpoint) and restart it — this is a custom supervision mechanism that a proper process manager (systemd, Kubernetes) would handle more robustly.

The 200ms timeout was calibrated against current model complexity (~45ms p99 inference). If model complexity increases — more features, larger training sets, ensemble methods — inference latency increases and the timeout must be raised or the fallback rate increases. There is no elasticity here: the ML module gets exactly the CPU and memory allocated to the host process, shared with everything else.

**When to externalize:** When model training requires GPU resources, OR when inference p99 consistently exceeds 100ms with the current timeout, OR when the ML team and the backend team need independent deployment cycles.

---

## Idempotency: Redis Primary + Postgres Fallback vs. Postgres Only

**Chosen:** Redis as primary idempotency store, Postgres as durable fallback
**Rejected:** Postgres only, Redis only

Idempotency key lookups on `POST /orders` are on the hot path — every order creation checks the key before proceeding. Redis gives ~8ms P99 for this check. Postgres gives ~95ms P99. The latency difference is significant at scale: at 200 orders/sec, 87ms of additional latency per request means 17.4 additional seconds of request-processing time per second — a system that is always catching up.

**The honest cost of the dual-store design:** Two writes instead of one. On first execution, the response is stored in Redis (with 24-hour TTL) and in Postgres (as a durable backup). The Postgres write is asynchronous — it does not block the response. If the async Postgres write fails, the idempotency key exists only in Redis. If Redis then fails within 24 hours, a duplicate request is not detected and the operation executes twice.

This is a real failure window: Redis failure during the idempotency key's TTL period, after the async Postgres write failed. The probability is low (two sequential failures), but the consequence (duplicate order) is visible to the user. Mitigation: the async Postgres write is retried 3 times before the key is considered Postgres-unflushed. A background reconciliation job scans Redis for keys not present in Postgres and backfills them. The reconciliation runs every 5 minutes.

**Why not Postgres only:** 95ms P99 for the idempotency check is too slow when combined with the rest of the order creation path. The total P99 for order creation would exceed 200ms, which is above the target SLA.

---

## ABAC Policy Cache: 60-Second Refresh vs. Per-Request Database Read

**Chosen:** In-memory policy cache with 60-second refresh interval
**Rejected:** Per-request Postgres read, distributed cache (Redis)

The policy evaluator caches the active policy set in memory. A policy change is visible to all evaluators within 60 seconds. During the window, some requests are evaluated against the old policy.

**The honest cost:** For security-critical policy tightenings, 60 seconds of stale evaluation is a real window where access that should be denied is allowed. This is not theoretical — if an operator revokes a supplier's access, that supplier can still access their data for up to 60 seconds after the revocation.

**Why not per-request Postgres read:** At 500 requests/sec, a Postgres read on every policy evaluation is 500 additional queries/sec. At ~15ms per query, this is 7.5 seconds of Postgres query time per second — the database is spending more time answering policy questions than serving business logic. The cache is necessary.

**Why not Redis for the policy cache:** Redis adds a network hop (~2ms) and a failure dependency. If Redis is unavailable, policy evaluation falls back to... what? A per-request Postgres read (acceptable for short outages) or deny-all (unacceptable). An in-memory cache with a 60-second refresh has one failure mode: stale data. A Redis cache has two: stale data and Redis unavailability.

**The mitigation for the stale window:** The `chainpulse-cli policy reload` command issues an internal signal to all running instances that flushes the in-memory cache immediately. For security-critical policy changes, this is the standard operating procedure — documented in the runbook, not a workaround.

---

## Summary

| Decision | Chosen | Rejected | What It Costs |
|---|---|---|---|
| Deployment model | Modular monolith | Microservices | Process-level fault isolation; module discipline requires enforcement |
| Event bus | Postgres outbox | Kafka | Breaks at ~5,000 events/sec; no cross-process consumers |
| Analytics storage | Single Postgres | Postgres + ClickHouse | Analytics inflexibility; OLTP/OLAP contention under load |
| Alert delivery | Polling (30s interval) | WebSockets, SSE | 30s alert latency floor; thundering herd risk on reconnect |
| ML runtime | Embedded Python subprocess | Separate service | Python deployment dependency; shared CPU/memory with application |
| Idempotency store | Redis primary + Postgres fallback | Postgres only | Dual-write complexity; failure window if both stores fail sequentially |
| Policy cache | In-memory 60s refresh | Per-request DB read | Up to 60s stale policy after change; operator reload required for immediate effect |
