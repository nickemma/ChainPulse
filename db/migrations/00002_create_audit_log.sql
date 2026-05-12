-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS audit (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id    UUID NOT NULL,
    actor_role  TEXT NOT NULL,
    action      TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id  UUID NOT NULL,
    policy_decision TEXT NOT NULL,
    policy_rule_matched TEXT,
    prev_hash   TEXT,
    hash        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit;
-- +goose StatementEnd
