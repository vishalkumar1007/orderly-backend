-- +goose Up
-- +goose StatementBegin

-- Payments framework foundation (PAYMENTS-PLAN.md P-A). Additive only: no
-- column is dropped or renamed, every new column has a default, and existing
-- rows keep working untouched as SANDBOX payments.

ALTER TABLE payments
    ADD COLUMN provider_order_id   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN provider_payment_id TEXT        NOT NULL DEFAULT '',
    -- Frozen when the intent is created, so flipping the platform mode later
    -- never reinterprets a payment already in flight.
    ADD COLUMN mode                TEXT        NOT NULL DEFAULT 'SANDBOX',
    ADD COLUMN currency            TEXT        NOT NULL DEFAULT 'INR',
    ADD COLUMN expires_at          TIMESTAMPTZ;

ALTER TABLE payments
    ADD CONSTRAINT payments_mode_check CHECK (mode IN ('SANDBOX', 'LIVE'));

-- Widen the status set (Ch.21). PAID stays the name for "captured".
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments
    ADD CONSTRAINT payments_status_check CHECK (
        status IN ('PENDING', 'PAID', 'FAILED', 'CANCELLED', 'EXPIRED', 'REFUNDING', 'REFUNDED')
    );

CREATE UNIQUE INDEX payments_provider_payment_unique
    ON payments (provider, provider_payment_id) WHERE provider_payment_id <> '';
CREATE INDEX payments_provider_order_idx
    ON payments (provider, provider_order_id) WHERE provider_order_id <> '';

-- Every webhook delivery, kept raw. (provider, event_id) is the replay guard.
-- tenant/payment are nullable: an event for a payment we cannot find is still
-- recorded.
CREATE TABLE payment_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID REFERENCES tenants (id) ON DELETE RESTRICT,
    payment_id   UUID REFERENCES payments (id) ON DELETE RESTRICT,
    provider     TEXT        NOT NULL,
    event_id     TEXT        NOT NULL,
    type         TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    error        TEXT        NOT NULL DEFAULT '',
    CONSTRAINT payment_events_provider_event_unique UNIQUE (provider, event_id)
);
CREATE INDEX payment_events_payment_idx ON payment_events (payment_id);

-- Refund history (Ch.21). V1 issues full refunds only; the table already
-- allows partial ones.
CREATE TABLE payment_refunds (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    payment_id        UUID NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    amount            NUMERIC(12, 2) NOT NULL,
    reason            TEXT        NOT NULL DEFAULT '',
    status            TEXT        NOT NULL DEFAULT 'PENDING',
    gateway_refund_id TEXT        NOT NULL DEFAULT '',
    error             TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_refunds_amount_positive CHECK (amount > 0),
    CONSTRAINT payment_refunds_status_check CHECK (status IN ('PENDING', 'COMPLETED', 'FAILED'))
);
CREATE INDEX payment_refunds_payment_idx ON payment_refunds (payment_id);

-- One row per payment state change: who, from what, to what, why.
CREATE TABLE payment_audit_log (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    payment_id  UUID NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    event_type  TEXT        NOT NULL,
    old_status  TEXT        NOT NULL DEFAULT '',
    new_status  TEXT        NOT NULL DEFAULT '',
    actor_type  TEXT        NOT NULL DEFAULT 'system',
    actor_id    TEXT        NOT NULL DEFAULT '',
    reason      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payment_audit_log_payment_idx ON payment_audit_log (payment_id, created_at);

-- New notification events. notify.Dispatch no-ops for a code with no catalog
-- row, so these are required. Owner is told in-app; the customer receipt email
-- ships disabled so nothing new is sent until a shop opts in.
INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('PAYMENT_RECEIVED', 'PAYMENTS', 'Payment received', 'An online payment is captured.',
        ARRAY['IN_APP'], 'PAYMENTS', '["order_id","order_number","amount"]'),
    ('PAYMENT_REFUNDED', 'PAYMENTS', 'Payment refunded', 'A payment is refunded.',
        ARRAY['IN_APP'], 'PAYMENTS', '["order_id","order_number","amount"]');

INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('PAYMENT_RECEIVED', 'IN_APP', TRUE, 'ROLE:organization', 'NORMAL', FALSE),
    ('PAYMENT_RECEIVED', 'EMAIL', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('PAYMENT_REFUNDED', 'IN_APP', TRUE, 'ROLE:organization', 'HIGH', FALSE),
    ('PAYMENT_REFUNDED', 'EMAIL', FALSE, 'CUSTOMER', 'NORMAL', FALSE);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM notification_rules WHERE event_code IN ('PAYMENT_RECEIVED', 'PAYMENT_REFUNDED');
DELETE FROM notification_events WHERE code IN ('PAYMENT_RECEIVED', 'PAYMENT_REFUNDED');

DROP TABLE IF EXISTS payment_audit_log;
DROP TABLE IF EXISTS payment_refunds;
DROP TABLE IF EXISTS payment_events;

DROP INDEX IF EXISTS payments_provider_order_idx;
DROP INDEX IF EXISTS payments_provider_payment_unique;

-- Rows moved into the new states cannot satisfy the old check.
UPDATE payments SET status = 'FAILED' WHERE status IN ('CANCELLED', 'EXPIRED');
UPDATE payments SET status = 'PAID' WHERE status = 'REFUNDING';
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments
    ADD CONSTRAINT payments_status_check CHECK (status IN ('PENDING', 'PAID', 'FAILED', 'REFUNDED'));

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_mode_check;
ALTER TABLE payments
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS mode,
    DROP COLUMN IF EXISTS provider_payment_id,
    DROP COLUMN IF EXISTS provider_order_id;
-- +goose StatementEnd
