# ChainPulse Threat Model

**Status:** In Development
**Last Updated:** May 2026
**Author:** [@nickemma](https://github.com/nickemma)
**Framework:** STRIDE

---

## Purpose

This document identifies the attack surfaces in ChainPulse, the threats that apply to each, and the mitigations in place. It also identifies the residual risks — places where the mitigation is incomplete and the system relies on operational controls rather than technical ones.

A threat model that only lists mitigations without acknowledging residual risk is a false sense of security. Every section in this document ends with an honest statement of what the mitigation does not cover.

---

## System Context

ChainPulse is a modular monolith running as a single process with two external dependencies: Postgres and Redis. It is accessed by:

- **Dashboard clients** — internal operators (warehouse managers, analysts, admins)
- **Partner integrations** — external supplier systems authenticated by API key
- **IoT feeds** — automated sensor events (future, not in v1)

The trust boundary is the HTTP API layer. Everything inside the process (module-to-module calls) is trusted after the gateway validates the request. Everything outside (Postgres, Redis, client requests) is untrusted.

---

## Attack Surfaces

| Surface | Protocol | Authentication | Notes |
|---|---|---|---|
| REST API | HTTPS | JWT (users), API key (partners) | All external traffic enters here |
| Supplier event ingestion | HTTPS | API key | Dedicated endpoint, separate rate limit tier |
| Postgres | TCP (TLS) | Service account credentials | Application-only access; no direct client access |
| Redis | TCP | Password | Application-only access; no direct client access |
| ML subprocess | localhost HTTP | None (localhost only) | Not externally reachable |
| Admin CLI | Local binary | System user credentials | Not network-accessible |

---

## STRIDE Analysis

### S — Spoofing

**Threat 1: JWT forgery**

An attacker forges a JWT claiming to be a high-privilege user (role: admin, arbitrary tenant_id).

*Mitigation:* JWTs are signed with RS256. The private key is held only by the auth service and is never stored in the application binary or in Postgres. The public key is loaded from the filesystem at startup. A forged JWT that does not verify against the public key is rejected by the gateway before any request context is populated.

*Residual risk:* If the private key is compromised (key material exfiltrated from the auth service host), an attacker can issue valid JWTs for any identity. Mitigation requires key rotation — which invalidates all existing tokens and forces re-authentication. Key rotation is a documented operational procedure but has not been automated. Manual key rotation under incident conditions is error-prone.

**Threat 2: API key theft**

A supplier's API key is exfiltrated (intercepted in transit, stored in a log, or obtained from the supplier's systems).

*Mitigation:* API keys are stored hashed (bcrypt) in Postgres. The plaintext key is shown once at creation and never again. Keys are transmitted only over HTTPS. Keys are scoped to specific resource paths and rate limit tiers — a stolen key for supplier A cannot access supplier B's shipments (ABAC policy enforces supplier_id scoping).

*Residual risk:* A stolen key is valid until rotated. There is no automatic key rotation or anomaly detection on API key usage patterns in v1. An attacker with a stolen key can read supplier A's shipments at a rate limited by the tier, and this access will be logged (audit trail) but not automatically detected as anomalous.

**Threat 3: Internal module impersonation**

An attacker who has compromised the application process could issue requests between modules claiming to be a different module.

*Mitigation:* Module-to-module communication is in-process function calls, not network calls. There is no network surface for module impersonation. A compromised module has access to the request context of whatever request is currently executing — it cannot escalate to a different tenant or role than the one in the active context without forging a JWT (which requires the private key).

*Residual risk:* A compromised module (via a code injection or dependency vulnerability) that runs in the same process has access to all in-memory state, including the JWT signing public key cache and the Postgres connection pool. This is the fundamental limitation of a monolith versus process-isolated microservices.

---

### T — Tampering

**Threat 4: Order modification in transit**

An attacker intercepts an order creation request and modifies the payload (e.g., changes the quantity or destination).

*Mitigation:* All external communication is over HTTPS (TLS 1.3 minimum). Payload integrity is guaranteed by TLS. A man-in-the-middle attack requires compromising the TLS certificate chain, which is outside the application's threat model.

*Residual risk:* None within the application's control. Certificate pinning is not implemented.

**Threat 5: Concurrent order modification (race condition)**

Two authenticated clients simultaneously update the same order, resulting in a race condition that silently applies one update and discards the other.

*Mitigation:* Optimistic locking via a `version` column on the orders table. An update that does not match the current version returns `409 Conflict`. Both clients are informed of the conflict; neither update is silently lost. The audit log records both attempts.

*Residual risk:* None. This is fully mitigated at the database level.

**Threat 6: Event payload tampering**

An attacker with Postgres write access modifies an event payload in the outbox table before the relay publishes it.

*Mitigation:* The application service account has INSERT and SELECT privileges on the outbox table, not UPDATE. Modifying an outbox row requires direct database access with elevated credentials — outside the application's threat model.

*Residual risk:* A database administrator account could modify outbox rows. The audit log records what events were consumed and their content — a tampered event that produces an unexpected state change can be detected by comparing the audit log against the event payloads. However, the outbox table itself is not hash-chained — only the audit log is. A tampered event that produces a plausible state change may not be detected.

**Threat 7: Audit log tampering**

An attacker with Postgres write access modifies or deletes audit records to cover tracks after unauthorized access.

*Mitigation:* The audit log is hash-chained — each record includes a SHA-256 hash of the previous record. Modifying or deleting a record breaks the chain from that point forward, which is detectable by `chainpulse-cli audit verify`. The application service account does not have UPDATE or DELETE privileges on the `audit_log` table. It does not have TRUNCATE privilege — only the DBA account has this, and DBA access to production requires two-person authorization.

*Residual risk:* A DBA with production access and no second authorizer could truncate the audit log. The hash chain is broken by truncation, but if the chain is gone, the break is also gone. Mitigation requires shipping audit log hashes to an external, independently controlled system (e.g., a write-once S3 bucket or an external audit service). This is not implemented in v1.

---

### R — Repudiation

**Threat 8: Actor denies creating an order or modifying a shipment**

A user claims they did not perform an action that the system recorded.

*Mitigation:* Every sensitive action produces an audit record before the response is returned. The record includes: `actor_id`, `actor_role`, `action`, `resource_id`, `policy_decision`, `matched_rule`, `timestamp`, `prev_hash`, and `hash`. The actor_id and role are extracted from the validated JWT — the actor cannot claim a different identity without a different JWT. The audit record is written to Postgres within the same request context, before the 200 OK is returned to the client.

*Residual risk:* JWT token sharing. If a user shares their JWT with another person (or if a JWT is stolen within its 15-minute TTL), an action performed by the second person is attributed to the first. The short TTL (15 minutes) limits the exposure window. This is a social/operational risk, not a technical one.

**Threat 9: System denies an action occurred**

The system claims an action did not happen, but the client asserts it did.

*Mitigation:* Idempotency keys stored durably in Postgres. A client that received a 200 OK for an order creation with idempotency key K can prove the order exists by querying the order directly. The audit log independently records the creation.

*Residual risk:* If both Redis and Postgres are unavailable when the response is returned (extremely unlikely but possible), the order may be committed to Postgres without the idempotency key being stored. A retry with the same key would create a duplicate order. The probability requires a Postgres connectivity failure between the order commit and the idempotency key write (two separate Postgres operations in the same connection) — this requires the Postgres connection to fail between two writes, which is rare and produces a transaction rollback (atomic failure), not a partial commit.

---

### I — Information Disclosure

**Threat 10: Cross-tenant data leak via missing WHERE clause**

A developer adds a Postgres query that reads `SELECT * FROM orders WHERE sku = $1` without the `tenant_id = $2` clause. Tenant A's query returns tenant B's orders.

*Mitigation (layered):*

Layer 1 — Application: All queries include `tenant_id = $1` enforced by the repository layer. The repository interface requires `tenantID string` as a parameter; callers cannot omit it without changing the interface signature.

Layer 2 — Database: Postgres row-level security (RLS) is enabled on all tenant-scoped tables. The application service account's RLS policy permits reads only on rows where `tenant_id` matches the session variable set at connection time. A query without a `tenant_id` clause is filtered by RLS — not all rows are returned, only those matching the session variable.

Layer 3 — Testing: Every module has a `tenant_isolation_test.go` that creates two tenants and asserts all read endpoints return only the requesting tenant's data.

*Residual risk:* RLS protects Postgres reads. It does not protect Redis. A cached response stored in Redis under a malformed key (missing tenant prefix) could be served to a different tenant if they happen to construct the same key. The Redis key wrapper enforces the prefix structure, but a bug in the wrapper (or a direct Redis write that bypasses the wrapper) would not be caught by RLS. Redis cache poisoning requires an application bug — it cannot be exploited from outside without application-level write access.

**Threat 11: Sensitive data in logs**

Structured logs contain order payloads, inventory counts, or shipment details that should not be in log aggregation systems.

*Mitigation:* Log fields are explicitly allowlisted. Request and response logging includes: `tenant_id`, `actor_id`, `route`, `method`, `status`, `latency_ms`, `policy_decision`. It does not include request or response bodies. The structured logger is configured at startup with an explicit field allowlist — new fields must be explicitly added, they are not logged by default.

*Residual risk:* Error logs. When a request handler returns an error, the error message may include data from the request context (e.g., "invalid order quantity for SKU WIDGET-XL: -50"). Error messages are logged at the ERROR level. If error messages contain business data, that data appears in logs. Mitigation: error messages in the domain layer use coded error types without embedded data; the HTTP adapter layer translates coded errors to HTTP status codes without including the raw error message in logs.

**Threat 12: ML model leaks training data**

The demand forecasting model's responses reveal information about other tenants' order volumes (model inversion or membership inference attacks).

*Mitigation:* Models are trained per-tenant. Tenant A's model is trained only on tenant A's data. Tenant B cannot query tenant A's model — the ML module validates the `tenant_id` in the request context before loading the model for inference. The model artifact filenames include the tenant ID.

*Residual risk:* At very low tenant data volumes (a tenant with 50 total orders), a model trained on that tenant's data may be vulnerable to membership inference (an attacker who can make many inference queries could infer whether specific orders exist in the training data). This risk is acknowledged and not mitigated in v1 — it requires differential privacy techniques that are outside the current scope.

---

### D — Denial of Service

**Threat 13: Rate limit exhaustion by a single tenant**

A tenant (or an attacker with a valid JWT) floods the API with requests, consuming rate limit capacity and degrading service for other tenants.

*Mitigation:* Rate limits are per-tenant. A single tenant exhausting their rate limit does not affect other tenants' limits (Redis counters are per-tenant-per-route). When a tenant's limit is exhausted, subsequent requests return `429 Too Many Requests` with a `Retry-After` header. The rate limit is enforced before any application logic runs — an exhausted tenant does not generate Postgres or ML load.

*Residual risk:* If Redis is unavailable, rate limiting falls back to per-instance in-memory counters. A tenant with N running instances effectively gets N times the configured limit during the Redis outage. In a single-instance deployment this is not a risk; in a scaled deployment with many instances it is a meaningful degradation.

**Threat 14: Event bus flooding via supplier ingestion endpoint**

An attacker with a valid API key floods the supplier event ingestion endpoint, filling the outbox table and degrading the relay workers.

*Mitigation:* The supplier ingestion endpoint has a separate, lower rate limit tier than the dashboard API (configurable via `RATE_LIMIT_SUPPLIER_RPM`, default: 100/minute per API key). The gateway enforces this before the event is written to the outbox. An attacker who exhausts the rate limit is throttled; the outbox accumulates at a bounded rate.

*Residual risk:* At 100 events/minute sustained over time, an attacker could generate 144,000 events per day. At a payload size of ~500 bytes per event, this is ~72MB per day of outbox table growth. Over 30 days (the retention period), this is ~2.1GB of attacker-generated events. This is bounded but non-zero. A more aggressive mitigation would require per-key circuit breakers that suspend a key after anomalous volume — not implemented in v1.

**Threat 15: Thundering herd on polling reconnect**

After a network blip, all dashboard clients reconnect simultaneously and poll at the same time, saturating the Postgres connection pool.

*Mitigation:* Poll responses are cached in Redis keyed by `tenant:{id}:alerts:since:{event_id}` with a 10-second TTL. Within a 10-second window, all clients with the same tenant and last-seen ID receive the cached response (one Postgres query, N Redis reads). Clients are also encouraged to add a random jitter (0–5 seconds) to their reconnect delay.

*Residual risk:* Clients with different last-seen IDs all miss the cache (each has a unique cache key). In practice, a mass reconnect after a 2-minute outage means all clients have a last-seen ID from 2 minutes ago — they likely have the same ID if the polling interval is 30 seconds. If clients have diverged last-seen IDs (some were connected at different times), cache miss rate increases. This is a worst-case that the system does not fully solve without WebSocket push.

---

### E — Elevation of Privilege

**Threat 16: Policy rule misconfiguration allows privilege escalation**

A misconfigured policy rule grants a `supplier` role access to admin-only endpoints.

*Mitigation:* Deny-by-default: a request with no matching allow rule receives `DENY`. A new endpoint with no policy rule is inaccessible to all non-admin roles by default. The hardcoded cross-tenant deny fires before any allow rule — no tenant-specific policy can grant cross-tenant access regardless of how it is written. Policy changes are versioned and audited — a misconfigured rule promotion is visible in the audit log with the author and timestamp.

*Residual risk:* The 60-second policy cache means a misconfigured rule that grants elevated access is active for up to 60 seconds after detection and rollback is initiated. `chainpulse-cli policy reload` reduces this to ~5 seconds for an operator who is monitoring the system. An unmonitored misconfiguration persists until the next cache refresh.

**Threat 17: JWT claim manipulation**

An authenticated user modifies their JWT claims (role, tenant_id, region) to access resources they should not.

*Mitigation:* JWTs are signed. Modifying any claim invalidates the signature. The gateway rejects tokens that do not verify against the public key. A user cannot modify their own JWT claims without the private key.

*Residual risk:* None within the application's control. See Threat 1 (private key compromise).

---

## Tenant Isolation: Worst-Case Analysis

The most plausible path to a tenant data leak, ordered by likelihood:

**1. Missing tenant_id in a Postgres query (most likely)**

Path: developer adds a query without tenant scope → RLS catches it at the database layer → no data leak but a Postgres error is logged → developer investigates and finds the missing clause.

If RLS is misconfigured (wrong policy for the service account): the query returns all tenants' data. The application serves tenant B's data to tenant A. This is detectable by the isolation test suite — but only for endpoints that are tested. A new, untested endpoint is the gap.

**2. Redis cache key without tenant prefix (less likely)**

Path: developer bypasses the Redis wrapper (uses a raw Redis client imported directly) → stores a value without tenant prefix → a different tenant requests the same key (highly unlikely for most data) → receives the wrong tenant's cached data.

Detection: the isolation test suite does not test Redis key structure directly — it tests API responses. A Redis cache poisoning that produces correct-looking responses for the wrong tenant would not be caught. Mitigation: architecture lint rule preventing direct Redis client imports (all Redis access through the wrapper) — this is in the Phase 7 hardening work.

**3. Event consumer processing another tenant's event (least likely)**

Path: a bug in the event bus dispatcher routes an event to the wrong tenant's consumer → the consumer processes it and modifies that tenant's state.

Detection: every event payload includes `tenant_id`. Every consumer validates `event.TenantID == ctx.TenantID` before processing. A mismatch returns an error and moves the event to the DLQ. The DLQ alert fires. The cross-tenant event is visible in the DLQ with full payload.

---

## Security Controls Summary

| Control | Mechanism | Strength | Gap |
|---|---|---|---|
| Authentication | RS256 JWT, bcrypt API keys | Strong | Private key compromise invalidates entirely |
| Authorization | ABAC + RBAC, deny-by-default, cross-tenant deny hardcoded | Strong | 60-second stale window on policy change |
| Tenant isolation | RLS (DB layer) + query scoping (app layer) + Redis key prefix (wrapper) | Strong | Redis wrapper bypass; untested query paths |
| Audit integrity | SHA-256 hash chain | Strong | No external hash shipping in v1 |
| Data in transit | HTTPS (TLS 1.3) | Strong | No certificate pinning |
| Data at rest | Postgres encryption at rest (deployment-level) | Medium | Redis at-rest encryption depends on deployment config |
| Rate limiting | Redis sliding window, per-tenant | Strong | Degrades to per-instance on Redis failure |
| Idempotency | Redis primary + Postgres durable fallback | Strong | Dual-failure window (rare) |

---

## Out of Scope (v1)

The following threats are acknowledged but not mitigated in v1:

- **DDoS at the network layer** — requires infrastructure-level protection (CDN, WAF) outside the application
- **SQL injection** — parameterized queries throughout; not a realistic threat given the ORM usage pattern, but not explicitly tested with a fuzzer
- **Supply chain attacks on dependencies** — Go module checksums and a locked `go.sum` provide integrity, but malicious dependencies are not scanned automatically
- **Insider threat (DBA with production access)** — two-person authorization for DBA production access is a process control, not a technical one
- **ML model inversion / membership inference** — acknowledged in Threat 12; not mitigated in v1
- **External audit log shipping** — the hash chain is verifiable but only within Postgres; a DBA can truncate the table

---

## References

- [Architecture](../ARCHITECTURE.md) — system map, trust boundaries, module structure
- [Design Doc](../DESIGN_DOC.md) — audit hash chain design, policy evaluation, tenant isolation implementation
- [Tradeoffs](../TRADEOFFS.md) — security tradeoffs including policy cache window and Redis fallback behavior
- [Runbook](../RUNBOOK.md) — incident response procedures for audit chain breaks and suspected data leaks
