-- +goose Up
-- +goose StatementBegin

-- The actual, per-tenant, enabled/disabled set. Provisioning writes one row
-- per (tenant, capability) that business_type_capabilities allows; a row can
-- only exist here if business_type_capabilities has a matching pair, and
-- `enabled` can only be flipped when that pair says `configurable = true`.
-- That second rule is enforced in the `business` module in Go, not in SQL —
-- it needs to compare against the tenant's current business_type, which a
-- plain CHECK constraint cannot do across tables.
CREATE TABLE tenant_capabilities (
    tenant_id       UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    capability_code TEXT NOT NULL REFERENCES capabilities (code),
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    source          TEXT NOT NULL DEFAULT 'DEFAULT' CHECK (source IN ('DEFAULT', 'OVERRIDE')),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, capability_code)
);

CREATE INDEX tenant_capabilities_tenant_enabled_idx
    ON tenant_capabilities (tenant_id) WHERE enabled;

-- Backfill: every tenant that already exists gets its business type's default
-- capability set. This is what turns on Barber/Hotel modules for real once a
-- tenant of that type is provisioned, and is also what stops every existing
-- (currently all Food-shaped) tenant from losing access on deploy.
INSERT INTO tenant_capabilities (tenant_id, capability_code, enabled, source)
SELECT t.id, btc.capability_code, btc.default_enabled, 'DEFAULT'
FROM tenants t
JOIN business_type_capabilities btc ON btc.business_type_code = t.business_type
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_capabilities;
-- +goose StatementEnd
