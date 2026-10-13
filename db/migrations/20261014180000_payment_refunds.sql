-- +goose Up
-- +goose StatementBegin

-- Refunds (PAYMENTS-PLAN.md P-D). payment_refunds already exists from the
-- framework migration; this makes it safe to drive.

-- Which gateway payment a refund returns. A payment normally has one, but a
-- customer who got charged twice has two, and the duplicate is refunded on its
-- own without touching the order's payment status.
ALTER TABLE payment_refunds
    ADD COLUMN provider_payment_id TEXT NOT NULL DEFAULT '';

-- One live (pending or done) refund per gateway payment. A second request for
-- the same money cannot slip in, however many times a button is pressed or a
-- job is retried.
CREATE UNIQUE INDEX payment_refunds_active_unique
    ON payment_refunds (payment_id, provider_payment_id)
    WHERE status IN ('PENDING', 'COMPLETED');
CREATE INDEX payment_refunds_gateway_idx
    ON payment_refunds (gateway_refund_id) WHERE gateway_refund_id <> '';

INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('PAYMENT_REFUND_FAILED', 'PAYMENTS', 'Refund failed',
        'A refund could not be completed and needs attention.',
        ARRAY['IN_APP'], 'PAYMENTS', '["order_id","order_number","amount","failure_reason"]');
INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('PAYMENT_REFUND_FAILED', 'IN_APP', TRUE, 'ROLE:organization', 'HIGH', FALSE);

-- Whether an order's money should be left out of the shop's revenue figures:
-- paid through a test (dummy) gateway, or refunded. Defined once here so every
-- revenue query agrees. A "test" payment is an online one that went through
-- the sandbox gateway (it has a gateway order id); payments recorded before
-- the gateway framework existed are left as they were.
CREATE FUNCTION order_excluded_from_revenue(o_id UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM payments p
        WHERE p.order_id = o_id
          AND ((p.method = 'ONLINE' AND p.mode = 'SANDBOX' AND p.provider_order_id <> '')
               OR p.status IN ('REFUNDED', 'REFUNDING'))
    )
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS order_excluded_from_revenue(UUID);
DELETE FROM notification_rules WHERE event_code = 'PAYMENT_REFUND_FAILED';
DELETE FROM notification_events WHERE code = 'PAYMENT_REFUND_FAILED';
DROP INDEX IF EXISTS payment_refunds_gateway_idx;
DROP INDEX IF EXISTS payment_refunds_active_unique;
ALTER TABLE payment_refunds DROP COLUMN IF EXISTS provider_payment_id;
-- +goose StatementEnd
