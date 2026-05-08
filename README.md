# ChainPulse — Intelligent Supply Chain Control Tower

<div align="center">

![Status](https://img.shields.io/badge/status-In%20development-orange)
![Go Version](https://img.shields.io/badge/go-1.25-blue)
![Python Version](https://img.shields.io/badge/python-3.12-blue)
![License](https://img.shields.io/badge/license-MIT-green)
[![CI](https://github.com/nickemma/chainpulse/workflows/CI/badge.svg)](https://github.com/nickemma/chainpulse/actions)

**A real-time, multi-tenant supply chain control tower — built from first principles.**

_Zero-trust event-driven core. Clean hexagonal architecture. RBAC + ABAC policy enforcement & authorization. Redis-backed idempotency and rate limiting. Every component production-hardened, without unnecessary complexity._

[Architecture](docs/ARCHITECTURE.md) • [Design Doc](docs/DESIGN_DOC.md) • [Runbook](docs/RUNBOOK.md) • [Roadmap](docs/ROADMAP.md) • [Security](docs/security/THREAT_MODEL.md) • [Tradeoffs](docs/TRADEOFFS.md)

</div>

---

## What is ChainPulse?

Modern supply chain systems fail silently and are reactive. A shipment gets delayed - nobody notices until inventory drops. Demand spikes - the system only reflects it after the demage is done. Decisions depend on Operational teams constantly watching dashboards.

ChainPulse is built differently.

At its foundation: a multi-tenant, event-driven
modular monolith that ingests real-time signals — orders, inventory updates, shipment status, supplier events — and propagates them through an internal event pipeline designed like a streaming system. Every event is durable, replayable, and carries a full lineage.

Events flow through the system, update state, trigger decisions, and produce derived insights — all inside a single deployable binary.

On top of that foundation: 
- A **forecasting layer** predicts demand trends using historical event data and runs live inference on every routing and procurement decision. 
- A **decision layer** detects disruptions like delays and anomalies.
- A **policy engine** enforces strict access control (RBAC + ABAC). 
- A **reliable API layer** ensures idempotency, rate limiting, and safe retries.

Above that: a security model that treats internal network trust as a liability. Every call is authenticated. Every API request is validated against a policy engine that evaluates role-based and attribute-based rules at runtime — a warehouse manager cannot see another region's inventory, a supplier cannot see another supplier's shipment, a high-value order requires elevated authorization before it can be modified. Policies are versioned, audited, and enforced at the edge before any service logic runs.

Beneath all of it: an audit log that records every sensitive action — every order created, every shipment modified, every policy decision — with enough context to reconstruct exactly what happened, who authorized it, and why.

**The question ChainPulse is built to answer:** When demand spikes and a key supplier is delayed, what does the system do immediately — automatically, correctly, consistently, and with a full audit trail of every decision? Most supply chain tools require a human to notice, escalate, and act. ChainPulse has a specific, tested, verifiable answer.

---

## What Each Layer Proves

| Layer | What It Demonstrates |
|---|---|
| API Gateway (inside monolith) | Production API design — Rate limiting, idempotency, and request safety |
| ABAC + RBAC policy engine | Fine-grained authorization — attribute-aware, runtime-access control |
| Internal event bus (Kafka-like) | Event-driven thinking without external complexity |
| ML Demand forecasting pipeline | Practical ML integration (not just theory) |
| ML disruption detection | Detect delays and anomalies from event flow |
| Structured logging + metrics | Observability from day one |
| Hexagonal design (ports & adapters) | Clean separation of domain, application, and infrastructure |
| Redis integration | Fast state for idempotency + rate limiting |
| Multi-tenancy enforced end-to-end | Tenant isolation enforced across the system |
| Unit + integration testing | Reliability without chaos engineering overhead |

---

## Architecture

```
Clients (Dashboard / Partner Integrations / IoT)
                      ↓
         ┌────────────────────────┐
         │       Gateway          │
         │  REST API · (Go)       │
         │  JWT · Rate Limiting   │
         │  Idempotency (Redis)   │
         └────────────────────────┘
                      ↓
         ┌────────────────────────┐
         │  Auth + Policy Engine  │
         │  RBAC · ABAC           │
         └────────────────────────┘
                      ↓
    ┌─────────────────┬──────────────────┐
    ↓                 ↓                  ↓
Inventory Module     Order Module    Shipment Module 
    (Go)              (Go)               (Go)
    ↓                 ↓                  ↓
    └──────── Internal Event Bus ────────┘
                (in-memory / DB)
                      ↓
    ┌─────────────────┬──────────────────┐
    ↓                 ↓                  ↓
Stream Processor    ML Module      Polling Layer
(Python/Go)         (Python)               (Go)         
    ↓                 ↓
    └──────→ PostgreSQL ←────────────────┘
            (OLTP + analytics store)

Shared: PostgreSQL · Redis (Cache) · Prometheus + Grafana
```

---

## System Design

### Gateway — `internal/gateway/`

The entry point into the system. Even in a monolith, this behaves like a real API gateway.

- **JWT validation** — short-lived access tokens with tenant and role claims extracted at the edge; no downstream service re-validates tokens
- **API key support** — external partners authenticate via rotating API keys; keys are scoped to specific resources and rate limits
- **Redis-backed rate limiting** — per-tenant, per-route limits enforced before any logic runs; burst allowances tracked in a sliding window
- **Idempotency key handling** — `POST /orders` and `POST /shipments` require an `Idempotency-Key` header; duplicate requests within a 24-hour window return the original response without re-executing; keys stored in Redis with a Postgres fallback for durability
- **Routing** — forwards requests into application modules

Every request produces a structured log entry with tenant, identity, route, latency, and policy decision. This is the outer edge of the audit trail.

### Auth & Policy Engine

Authorization is a runtime decision, not an application concern. The policy engine evaluates every access request against a set of versioned, structured policies before the target service sees the request.

**Authentication:**
- JWT access tokens (15-minute TTL) paired with refresh tokens (7-day TTL)
- Token claims carry: `tenant_id`, `user_id`, `role`, `region`, `supplier_id` (where applicable)

**Authorization model:**

Role | Default Permissions
-----|--------------------
`admin` | Full read/write across all resources within tenant
`supplier` | Read/write own shipments; read own inventory allocations only
`warehouse_manager` | Read/write inventory in assigned region; read cross-region (no write)
`analyst` | Read-only access to all data within tenant

ABAC policies layer over RBAC to enforce attribute-level constraints at runtime. Policy evaluation receives the full request context — identity, resource path, action, source region, order value, time of day — and produces an allow/deny with a reason code.

```rego
# A supplier can only read shipments they originated

default allow = false

allow {
  input.role == "supplier"
  input.action == "read"
  shipment := data.shipments[input.resource_id]
  shipment.supplier_id == input.supplier_id
}

# High-value orders require manager-level role or above
allow {
  input.action in ["update", "cancel"]
  order := data.orders[input.resource_id]
  order.value_usd < 50000
  input.role in ["admin", "warehouse_manager"]
}
```

Policy versions are stored in Postgres. Every policy change is a new version with the author, timestamp, and diff recorded. Rollback is a single write. Every policy decision — allow or deny — is appended to the audit log with the matched rule and input context.

### Event Streaming — `Internal Event Bus`

The event bus connects modules without tight coupling. Every significant state change is emitted as an event and processed asynchronously inside the same application.

This is **Kafka-inspired**, but intentionally simplified:
- No external broker
- No partitions or distributed coordination
- Events persisted in Postgres (outbox table)
- Consumers run as background workers

**Core topics:**  

```
orders.created         
orders.updated       
inventory.updated     
inventory.low_stock    
shipments.created       
shipments.status       
shipments.delayed       
supplier.event
```

Every event includes: `tenant_id`, `event_id`, `event_type`, `produced_at`, and a `correlation_id`

Events can be replayed for:
- rebuilding derived state
- ML training
- debugging & audit

This keeps the **event-driven model**, without introducing distributed system overhead.

### Order Module — `order-module/`

The authoritative source for order state. Every order write is committed to Postgres before the internal event is published. The internal event bus publish is transactional — if the DB write succeeds but the event bus publish fails, a background reconciliation job replays the event from the outbox table.

- **Transactional outbox pattern** — no order event is lost between DB commit and event publish
- **Optimistic locking** — concurrent modifications to the same order fail fast with a conflict response, not silent data corruption
- **Order lifecycle** — `draft → confirmed → allocated → shipped → delivered → closed`; every state transition is an event on `orders.updated`
- **High-value order gating** — orders above a configurable threshold require an additional authorization check against the policy engine before confirmation

### Inventory Module — `inventory-Module/`

Real-time inventory state across all warehouses and regions, scoped per tenant. Stock levels are updated in Postgres and cached in Redis for low-latency reads. Cache invalidation is event-driven — an `inventory.updated` event from internal event bus triggers a cache flush for the affected SKU and region.

- **Low-stock event generation** — when any SKU falls below its configured threshold, the service publishes `inventory.low_stock`; the threshold is configurable per SKU, per region, per tenant
- **Region-scoped access** — warehouse managers can only modify inventory for their assigned region; cross-region reads are allowed; enforced at the policy engine before the service sees the request
- **Reservation model** — inventory is soft-reserved when an order is confirmed; hard-decremented when shipped; released if an order is cancelled within the grace period

### Shipment Module — `shipment-Module/`

Tracks shipments from creation to delivery. Integrates with supplier events and the routing engine to reflect real-world status.

- **Delay detection** — shipments that exceed their expected arrival window by a configurable margin automatically publish a `shipments.delayed` event; the ML uses this signal as a historical data label
- **Supplier event ingestion** — external partner systems POST updates to a dedicated ingestion endpoint; the gateway validates and forwards to internal event bus; the shipment module consumes and reconciles
- **Status history** — every status transition is recorded with timestamp, source, and operator identity; the full history is queryable for any shipment

### ML Module

The intelligence layer. Two models run continuously: a demand forecasting model and a disruption/delay prediction model. Both are fully versioned, deployed without downtime, and integrated into live service decisions.

**Demand Forecasting:**

A time-series model pulled from historical order volume, inventory levels, and seasonal signals per SKU, per region, per tenant. Retrained on a configurable schedule (default: weekly) using data pulled from PostgreSQL.

```
Training pipeline:
  PostgreSQL → feature extraction (Python) → model training (scikit-learn / lightweight TS model)
  → model artifact + metadata → version store (Postgres) → deployment gate
```

The deployed model is exposed via an endpoint. The inventory module calls it during restocking decisions. The routing calls it when evaluating supplier options for large orders.

**Disruption & Delay Prediction:**

A classification model that predicts the probability of a shipment delay given: supplier identity, origin/destination pair, current lead time, historical delay rate for this route, and recent `shipments.delayed` event frequency on the route.

The routing calls this model synchronously before confirming a supplier selection. If the predicted delay probability exceeds a configurable threshold, the routing re-evaluates alternatives. The decision — including the model's score and the alternative selected — is recorded in the audit log.

**Model lifecycle:**

Stage | Detail
------|-------
Training | Weekly batch job; Python; reads from PostgreSQL
Versioning | Model file (local/S3), metadata in Postgres: version, trained_at, dataset_range, eval_metrics
Deployment | New version flagged as `candidate`; passes automated eval gate; promoted to `active`
Monitoring | Inference latency, prediction distribution, and drift signals exposed via Prometheus

### Stream Processor — `stream-processor/`

A background worker that consumes internal events and builds derived state.

**What it computes:**

- Rolling 1h / 24h / 7d order volume per region, per SKU, per tenant
- Shipment delay rate per supplier, per route, per time window
- Inventory turnover rate per warehouse
- Anomaly signals: demand spikes, unusual order patterns, supplier event gaps

Outputs:
- PostgreSQL → historical analytics
- Redis → fast dashboard reads

The processor is **replayable** — it can rebuild state from stored events when needed.

### Polling Layer

Instead of WebSockets, the system uses **polling-based updates** for simplicity.

Clients periodically request:
- shipment updates
- low stock alerts
- anomaly signals

This avoids:
- connection management complexity
- real-time infra overhead

A future upgrade can introduce WebSockets without changing core logic, since events already exist internally.

### Analytics Layer — `postgresql/`

PostgreSQL serves both:
- transactional data (orders, inventory, shipments)
- derived analytics (aggregations, metrics)

There is **no separate OLAP database**.

All analytics data is produced by the stream processor and stored in dedicated tables.

This keeps the system:
- simple to operate
- easy to reason about
- consistent with a monolith architecture

---

## Tech Stack

| Layer | Technology | Why |
|---|---|---|
| **Gateway + Core** | Go | Simple deployment, strong concurrency, single binary |
| **Processing + ML** | Python | Forecasting + anomaly detection |
| **Event Bus** | Internal (Postgres-backed) | Event-driven design without Kafka complexity |
| **Database** | PostgreSQL | Single source of truth (OLTP + analytics)|
| **Cache** | Redis | Rate limiting, idempotency, fast reads |
| **Observability** | Structured logs + Prometheus | metrics, and visibility|
| **Container** | Docker | Local reproducibility |

---

## Security Architecture

ChainPulse treats the internal network as untrusted. Every hop is authenticated. Every decision is policy-evaluated and logged.

### Zero-Trust Principles (Adapted for Monolith)
- **No implicit trust in requests** — every request is authenticated and authorized
- **JWT-based identity** — all access scoped by tenant and role
- **Policy enforcement at the gateway layer**
- **Strict multi-tenant isolation at the data level**

Note: mTLS between services is intentionally not implemented, since this is a single deployable system.

### Defense in Depth

Layer | Control
------|--------
Network | TLS for all client-facing endpoints
Authentication | JWT for users; API keys for partners
Authorization | RBAC baseline + ABAC runtime evaluation on every request
Data at rest | PostgreSQL and event bus encrypted at rest (AES-256)
Data in transit | TLS on all external connections;
Secrets | Service credentials managed externally; no hardcoded secrets in source
Audit | Tamper-evident log of every sensitive action committed before response is returned

### Threat Model (STRIDE)

The system has a documented threat model covering:

- **Spoofing** — mitigated by jwt service identity and JWT user identity; no service accepts requests without verified caller identity
- **Tampering** — mitigated by optimistic locking on order writes, hash-chained audit log, and transactional outbox for event consistency
- **Repudiation** — mitigated by tamper-evident audit log recording every action with identity, timestamp, and policy decision
- **Information Disclosure** — mitigated by ABAC policies enforcing attribute-level data isolation; suppliers cannot read other suppliers' data at the query level
- **Denial of Service** — mitigated by Redis-backed rate limiting at the gateway; circuit breakers prevent cascade failures between services
- **Elevation of Privilege** — mitigated by deny-by-default policy engine; no implicit grants; every access is an explicit allow from a matching policy rule

Full threat model: [`docs/security/THREAT_MODEL.md`](docs/security/THREAT_MODEL.md)

### Audit Log

Every sensitive action in the system is recorded in a tamper-evident audit log before the response is returned to the caller:

- Order created, modified, or cancelled
- Inventory threshold crossed
- Shipment status updated
- Policy decision (allow or deny) with matched rule and full input context
- Policy version change
- ML model promoted to active
- Alert triggered

Each audit record includes: `tenant_id`, `actor_id`, `actor_role`, `action`, `resource_type`, `resource_id`, `policy_decision`, `policy_rule_matched`, `timestamp`, and a `prev_hash` linking it to the previous record. The hash chain makes silent modification of historical records detectable.

---

## Quick Start

### Prerequisites

- Go 1.25
- Python 3.12
- Docker + docker-compose
- `make`

```bash
# Clone
git clone https://github.com/nickemma/chainpulse.git
cd chainpulse

# Start dependencies (Postgres + Redis)
make cluster-up

# Run app
make run

# Health check
curl http://localhost:8080/health
# {"status":"ok"}

# Create a tenant and get a JWT
./bin/chainpulse-cli auth login --tenant acme-corp --role admin
# eyJhbGci...

export TOKEN="eyJhbGci..."

# Create an order (idempotency key required)
curl -X POST http://localhost:8080/v1/orders \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: order-$(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{
    "sku": "WIDGET-XL",
    "quantity": 500,
    "destination_region": "ontario",
    "supplier_id": "sup-acme-primary"
  }'
# {"order_id":"ord_01HXYZ...","status":"confirmed","routing_decision":{"supplier":"sup-acme-primary","delay_probability":0.12}}

# Check inventory for a region (warehouse manager scope)
curl http://localhost:8080/v1/inventory?region=ontario \
  -H "Authorization: Bearer $TOKEN"

# Get demand forecast for a SKU
curl http://localhost:8080/v1/ml/forecast?sku=WIDGET-XL&region=ontario&horizon_days=14 \
  -H "Authorization: Bearer $TOKEN"
# {"sku":"WIDGET-XL","region":"ontario","forecast":[{"date":"2026-05-07","predicted_units":842},...]}

# Simulate a shipment delay (triggers anomaly detection + alert)
./bin/chainpulse-cli simulate shipment-delay --shipment-id shp_01HABC... --delay-days 5

# View audit log for an order
./bin/chainpulse-cli audit log --resource-type order --resource-id ord_01HXYZ... --last 10

# Connect a WebSocket client for real-time alerts
./bin/chainpulse-cli alerts subscribe --tenant acme-corp --token $TOKEN
# Listening for alerts...
# [CRITICAL] inventory.low_stock: WIDGET-XL in ontario — 47 units remaining (threshold: 100)

# Run the disruption simulation suite
make simulate-disruption
# Simulating supplier blackout: sup-acme-primary...
# Verifying routing fallback to: sup-backup-west...
# Checking audit trail completeness...
# All assertions passed.
```

### Policy Example

```rego
package chainpulse.inventory

default allow = false

# Warehouse managers can write inventory for their assigned region only
allow {
  input.role == "warehouse_manager"
  input.action in ["create", "update"]
  input.resource.region == input.actor.assigned_region
}

# Analysts get read-only access across all regions within their tenant
allow {
  input.role == "analyst"
  input.action == "read"
  input.resource.tenant_id == input.actor.tenant_id
}

# Deny cross-tenant access regardless of role
deny {
  input.resource.tenant_id != input.actor.tenant_id
}
```

### Environment Variables

```bash
# Databases
POSTGRES_DSN=postgres://chainpulse:password@postgres:5432/nexus
REDIS_ADDR=redis:6379
```

## Engineering Deep Dive

This project intentionally avoids unnecessary complexity while still demonstrating:

- Event-driven architecture without Kafka
- Modular monolith with clean boundaries (hexagonal design)
- Real-world API concerns (rate limiting, idempotency, retries)
- Fine-grained authorization (RBAC + ABAC)
- Multi-tenant system design
- Background processing & derived state
- Practical ML integration (forecasting + anomaly detection)
- Observability (structured logs + basic metrics)
- Unit and integration testing over chaos engineering

The goal is not to simulate distributed systems — but to show the same **thinking**, applied in a simpler, production-realistic way.

---

## Roadmap

- [ ] Linear programming optimization engine for multi-supplier allocation under capacity constraints
- [ ] What-if simulation mode — project the impact of a supplier outage or demand shift before it happens
- [ ] Pricing signal integration — factor in dynamic procurement costs in routing decisions
- [ ] Advanced forecasting models
- [ ] Compliance export — SOC2-style audit report generation from the audit log
- [ ] WebSocket real-time updates
- [ ] Optimization engine (routing decisions)
- [ ] Policy versioning + audit trail
- [ ] Dashboard UI & Event replay support
---

## Author

**[@nickemma](https://github.com/nickemma)** — Building production-grade backend systems with a focus on: distributed systems, platform engineering, and secure infrastructure from first principles.

_Designed and implemented a secure, distributed supply chain control tower applying principles from graduate-level work in MSE-SSC - Master of Science in Engineering in Software Systems and Cybersecurity — including zero-trust architecture, fine-grained authorization (RBAC/ABAC), real-time anomaly detection over streaming data pipelines, and ML integrated into live operational decisions._

💼 Open to distributed systems, infrastructure, platform, and backend engineering.

<div align="center">
<a href="https://www.linkedin.com/in/techieemma/"><img src="https://img.shields.io/badge/linkedin-%23f78a38.svg?style=for-the-badge&logo=linkedin&logoColor=white" alt="Linkedin"></a>
<a href="https://twitter.com/techieemma"><img src="https://img.shields.io/badge/Twitter-%23f78a38.svg?style=for-the-badge&logo=Twitter&logoColor=white" alt="Twitter"></a>
<a href="https://github.com/nickemma/"><img src="https://img.shields.io/badge/github-%23f78a38.svg?style=for-the-badge&logo=github&logoColor=white" alt="Github"></a>
<a href="https://techieemma.medium.com/"><img src="https://img.shields.io/badge/Medium-%23f78a38.svg?style=for-the-badge&logo=Medium&logoColor=white" alt="Medium"></a>
<a href="mailto:nicholasemmanuel321@gmail.com"><img src="https://img.shields.io/badge/Gmail-f78a38?style=for-the-badge&logo=gmail&logoColor=white" alt="Gmail"></a>
</div>

---

<div align="center">

**Building Systems, Building Faith — One Commit at a Time**

[⬆ Back to Top](#chainpulse--intelligent-supply-chain-control-tower)

</div>
