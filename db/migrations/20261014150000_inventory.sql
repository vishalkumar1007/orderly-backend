-- +goose Up
-- +goose StatementBegin

-- Inventory, Food first (Release V1 §3.6, Ch.19).
--
-- A product is tracked when it has a row here; a product with no row behaves
-- exactly as before. quantity is whole units (pcs, plates, bottles): fractional
-- stock and ingredient-level recipes are V2.
CREATE TABLE inventory_items (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    product_id        UUID NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    quantity          INT  NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    unit              TEXT NOT NULL DEFAULT 'pcs',
    reorder_threshold INT  NOT NULL DEFAULT 0 CHECK (reorder_threshold >= 0),
    -- track = FALSE pauses tracking without losing the ledger.
    track             BOOLEAN NOT NULL DEFAULT TRUE,
    -- Set when stock hit zero and WE hid the product, so restocking only
    -- re-shows products the owner did not hide by hand.
    auto_disabled     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT inventory_items_product_unique UNIQUE (tenant_id, product_id)
);

-- Append-only ledger: every change to quantity, who made it and why.
CREATE TABLE inventory_movements (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    item_id        UUID NOT NULL REFERENCES inventory_items (id) ON DELETE CASCADE,
    delta          INT  NOT NULL CHECK (delta <> 0),
    quantity_after INT  NOT NULL CHECK (quantity_after >= 0),
    reason         TEXT NOT NULL
                   CHECK (reason IN ('INITIAL', 'ADD', 'WASTE', 'CORRECTION', 'ORDER', 'CANCEL')),
    order_id       UUID REFERENCES orders (id) ON DELETE SET NULL,
    note           TEXT NOT NULL DEFAULT '',
    actor          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX inventory_movements_item_idx ON inventory_movements (item_id, created_at DESC);

-- An order takes stock once and gives it back once, however often the
-- transition is retried (double tap, payment landing mid-accept).
CREATE UNIQUE INDEX inventory_movements_order_once
    ON inventory_movements (order_id, item_id, reason)
    WHERE order_id IS NOT NULL AND reason IN ('ORDER', 'CANCEL');

-- Low-stock alert: a new notification category and one event, following the
-- pattern of every other event (notify.Dispatch no-ops without a catalog row).
ALTER TABLE notification_events DROP CONSTRAINT notification_events_category_check;
ALTER TABLE notification_events ADD CONSTRAINT notification_events_category_check
    CHECK (category IN ('ORDERS', 'PAYMENTS', 'BOOKINGS', 'STAFF', 'SECURITY', 'PLATFORM', 'INVENTORY'));

INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('INVENTORY_LOW', 'INVENTORY', 'Low stock',
     'A tracked product reaches its reorder level or runs out.',
     ARRAY['IN_APP'], 'CATALOG',
     '["product_id","product_name","quantity","threshold","unit"]');

INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('INVENTORY_LOW', 'IN_APP', TRUE, 'ROLE:menu', 'NORMAL', FALSE);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM notification_rules WHERE event_code = 'INVENTORY_LOW';
DELETE FROM notification_events WHERE code = 'INVENTORY_LOW';
ALTER TABLE notification_events DROP CONSTRAINT notification_events_category_check;
ALTER TABLE notification_events ADD CONSTRAINT notification_events_category_check
    CHECK (category IN ('ORDERS', 'PAYMENTS', 'BOOKINGS', 'STAFF', 'SECURITY', 'PLATFORM'));
DROP TABLE IF EXISTS inventory_movements;
DROP TABLE IF EXISTS inventory_items;
-- +goose StatementEnd
