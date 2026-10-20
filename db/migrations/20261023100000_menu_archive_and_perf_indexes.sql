-- +goose Up
-- +goose StatementBegin

-- A product that was ever ordered cannot be deleted (order_items keeps it), so
-- the console archives it instead: hidden everywhere, history intact.
ALTER TABLE products ADD COLUMN archived_at TIMESTAMPTZ;

-- payments_order_id_unique was dropped with the polymorphic payments change and
-- nothing replaced it, so order lookups scanned every payment of the tenant.
CREATE INDEX IF NOT EXISTS payments_order_id_idx ON payments (order_id) WHERE order_id IS NOT NULL;

-- Guests are matched to orders by phone.
CREATE INDEX IF NOT EXISTS orders_tenant_customer_phone_idx
    ON orders (tenant_id, customer_phone) WHERE customer_phone <> '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS orders_tenant_customer_phone_idx;
DROP INDEX IF EXISTS payments_order_id_idx;
ALTER TABLE products DROP COLUMN IF EXISTS archived_at;
-- +goose StatementEnd
