-- +goose Up
-- +goose StatementBegin

-- Every gateway order ever opened for a payment. A payment row is reused across
-- retries, so its own provider_order_id only names the latest attempt; this
-- table lets a late callback for an earlier attempt still find its payment
-- instead of being dropped while the customer's money has moved.
CREATE TABLE payment_attempts (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    payment_id        UUID NOT NULL REFERENCES payments (id) ON DELETE RESTRICT,
    provider          TEXT NOT NULL,
    provider_order_id TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_attempts_order_unique UNIQUE (provider, provider_order_id)
);
CREATE INDEX payment_attempts_payment_idx ON payment_attempts (payment_id);

INSERT INTO payment_attempts (tenant_id, payment_id, provider, provider_order_id)
SELECT tenant_id, id, provider, provider_order_id FROM payments WHERE provider_order_id <> ''
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS payment_attempts;
-- +goose StatementEnd
