-- +goose Up
-- +goose StatementBegin

-- Dine-in table tracking for Cafe/Restaurant. Purely additive: stays empty
-- until a tenant with the TABLES capability creates rows.
CREATE TABLE tables (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    label      TEXT NOT NULL,
    seats      INT NOT NULL DEFAULT 2,
    status     TEXT NOT NULL DEFAULT 'AVAILABLE'
               CHECK (status IN ('AVAILABLE', 'OCCUPIED', 'RESERVED', 'CLEANING')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tables_seats_positive CHECK (seats > 0),
    CONSTRAINT tables_tenant_label_unique UNIQUE (tenant_id, label)
);

CREATE INDEX tables_tenant_id_idx ON tables (tenant_id);

-- A dine-in order is attached to a table; a pickup/delivery order (Food Shop,
-- Cafe takeaway) leaves this NULL. Nullable, no behavior change for existing
-- orders.
ALTER TABLE orders ADD COLUMN table_id UUID REFERENCES tables (id) ON DELETE SET NULL;
CREATE INDEX orders_table_id_idx ON orders (table_id) WHERE table_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS orders_table_id_idx;
ALTER TABLE orders DROP COLUMN IF EXISTS table_id;
DROP TABLE IF EXISTS tables;
-- +goose StatementEnd
