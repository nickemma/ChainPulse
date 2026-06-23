-- +goose Up
-- +goose StatementBegin
-- API keys authenticate external partners at the gateway, alongside JWTs. The
-- plaintext key is shown to the caller exactly once at creation; only its
-- SHA-256 hash is stored. Keys are high-entropy random tokens, so a fast hash
-- (SHA-256) is the correct choice — bcrypt/argon2 defend low-entropy passwords
-- and would force an O(n) scan on every request. The hash is UNIQUE so lookup
-- is a single indexed point read.
CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL, -- non-secret display prefix, e.g. "cp_a1b2c3d4"
    role         TEXT NOT NULL,
    region       TEXT,
    supplier_id  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ, -- NULL = never expires
    revoked_at   TIMESTAMPTZ  -- NULL = active; set on rotate/revoke
);

-- Tenant-scoped listing of a partner's keys.
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys (tenant_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS api_keys;
-- +goose StatementEnd
