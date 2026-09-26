-- +goose Up
-- +goose StatementBegin

CREATE TABLE plans (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    price       NUMERIC(12, 2) NOT NULL DEFAULT 0,
    max_staff   INT NOT NULL DEFAULT 5,
    max_products INT NOT NULL DEFAULT 100,
    features    JSONB NOT NULL DEFAULT '{}'::jsonb,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plans_name_unique UNIQUE (name),
    CONSTRAINT plans_price_non_negative CHECK (price >= 0)
);

INSERT INTO plans (name, description, price, max_staff, max_products, features) VALUES
    ('TRIAL', 'Trial plan for new shops', 0, 3, 50, '{"trial_days": 14}'::jsonb),
    ('STARTER', 'Starter plan for small shops', 499, 5, 100, '{}'::jsonb),
    ('BUSINESS', 'Business plan for growing shops', 1499, 20, 500, '{}'::jsonb);

CREATE TABLE subscriptions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    plan_id    UUID NOT NULL REFERENCES plans (id) ON DELETE RESTRICT,
    status     TEXT NOT NULL DEFAULT 'TRIAL',
    start_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    end_at     TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT subscriptions_status_check CHECK (
        status IN ('TRIAL', 'ACTIVE', 'EXPIRED', 'CANCELLED')
    )
);

CREATE INDEX subscriptions_tenant_id_idx ON subscriptions (tenant_id);

ALTER TABLE tenants
    ADD COLUMN plan_id UUID REFERENCES plans (id) ON DELETE RESTRICT,
    ADD COLUMN setup_status TEXT NOT NULL DEFAULT 'PENDING';

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_status_check;
ALTER TABLE tenants
    ADD CONSTRAINT tenants_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    ADD CONSTRAINT tenants_setup_status_check CHECK (
        setup_status IN ('PENDING', 'IN_PROGRESS', 'COMPLETED')
    );

UPDATE tenants SET plan_id = (SELECT id FROM plans WHERE name = 'TRIAL' LIMIT 1)
WHERE plan_id IS NULL;

ALTER TABLE users
    ADD COLUMN must_set_password BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN invite_token_hash TEXT;

CREATE UNIQUE INDEX users_invite_token_hash_unique
    ON users (invite_token_hash)
    WHERE invite_token_hash IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS users_invite_token_hash_unique;
ALTER TABLE users
    DROP COLUMN IF EXISTS must_set_password,
    DROP COLUMN IF EXISTS invite_token_hash;

ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_setup_status_check,
    DROP COLUMN IF EXISTS setup_status,
    DROP COLUMN IF EXISTS plan_id;

DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plans;
-- +goose StatementEnd
