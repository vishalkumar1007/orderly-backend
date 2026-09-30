-- +goose Up
-- +goose StatementBegin

-- License is deliberately separate from Subscription (HLD §16): Subscription
-- is the billing relationship (plans.max_staff/max_products already carry the
-- plan's concrete limits — not duplicated here). License is the enforcement
-- gate with its own lifecycle, so a tenant can be billing-ACTIVE and
-- license-SUSPENDED at the same time (e.g. a compliance hold that has nothing
-- to do with whether an invoice was paid).
CREATE TABLE license_templates (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_id            UUID NOT NULL REFERENCES plans (id) ON DELETE RESTRICT,
    name               TEXT NOT NULL,
    validity_days      INT,  -- NULL = no expiry (renews with the subscription)
    grace_period_days  INT NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT license_templates_plan_unique UNIQUE (plan_id),
    CONSTRAINT license_templates_validity_check CHECK (validity_days IS NULL OR validity_days > 0),
    CONSTRAINT license_templates_grace_check CHECK (grace_period_days >= 0)
);

INSERT INTO license_templates (plan_id, name, validity_days, grace_period_days)
SELECT id, name || ' license', CASE WHEN name = 'TRIAL' THEN 14 ELSE NULL END, 3
FROM plans;

CREATE TABLE licenses (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL UNIQUE REFERENCES tenants (id) ON DELETE RESTRICT,
    template_id    UUID NOT NULL REFERENCES license_templates (id) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'DRAFT'
                   CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'EXPIRED', 'REVOKED')),
    issued_at      TIMESTAMPTZ,
    expires_at     TIMESTAMPTZ,
    revoked_reason TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX licenses_status_idx ON licenses (status);

-- Backfill: an ACTIVE license per existing tenant, on the template matching
-- its current plan (falling back to TRIAL's template if plan_id was never
-- set).
INSERT INTO licenses (tenant_id, template_id, status, issued_at)
SELECT
    t.id,
    COALESCE(
        (SELECT lt.id FROM license_templates lt WHERE lt.plan_id = t.plan_id),
        (SELECT lt.id FROM license_templates lt JOIN plans p ON p.id = lt.plan_id WHERE p.name = 'TRIAL' LIMIT 1)
    ),
    'ACTIVE',
    now()
FROM tenants t;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS licenses;
DROP TABLE IF EXISTS license_templates;
-- +goose StatementEnd
