---
name: project-status
description: ChainPulse phased-roadmap position — Phase 2 nearly complete, remaining gaps
metadata:
  type: project
---

ChainPulse is a multi-tenant, event-driven supply-chain control tower (modular monolith, Go + planned Python ML). Built in 7 phases; every phase exit requires a failure test, not just a feature test (see docs/ROADMAP.md).

As of 2026-06-23 the work is in **Phase 2 (Gateway, Auth, Policy)**, nearly complete:
- **Done & tested:** JWT issue/refresh/validate with alg-confusion defense (`internal/auth`), gateway middleware pipeline (request ID, structured access log, authenticate, Redis sliding-window rate limit + in-memory fallback, idempotency via Redis), policy engine (deny-by-default, hardcoded cross-tenant deny, fail-closed eval timeout) with audit-per-decision (log-based auditor). **API key authentication** (added 2026-06-23): `api_keys` table (migration 00003), SHA-256-hashed keys (`cp_` + 64 hex), `PostgresAPIKeyStore` (Resolve/Create/Rotate/Revoke/List), `X-API-Key` accepted by `middleware.Authenticate` alongside Bearer JWT, admin-only management endpoints `/v1/api-keys` (create/list/rotate/revoke) guarded by policy on resource type `api_key`. All Phase 2 failure tests pass including under `-race`; DB-gated integration tests in `internal/auth/apikey_postgres_test.go` (skip if no Postgres).
- **main.go now requires Postgres at startup** (fatal if unreachable) — it's the system of record for API keys. Redis still degrades gracefully.
- **Remaining Phase 2 gaps:** (1) Policy versioning in Postgres (draft→active→deprecated) + 60s in-memory cache + `chainpulse-cli policy reload`; (2) idempotency durable Postgres fallback when Redis is down. There is **no CLI binary yet** (`chainpulse-cli` referenced in README/roadmap but not built).

DB migrations: `tenants`, `audit_log`, `api_keys` (goose, `db/migrations/`; run `make migrate-up`). Auditor is currently log-based; the hash-chained Postgres audit log is a Phase 5 deliverable.

On 2026-06-23 I also fixed a real bug in `shared/config` loadDotEnv (quoted values like `"p@ss#word" # comment` kept the closing quote + trailing comment) that was failing `TestLoadDotEnv`, and removed a stray editor temp file in `internal/gateway/middleware/`.
