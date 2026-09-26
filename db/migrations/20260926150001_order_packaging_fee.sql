-- +goose Up
-- +goose StatementBegin
-- An order must snapshot its own costing. Without this, changing the tenant's
-- packaging fee later would silently rewrite what past orders say they cost.
ALTER TABLE orders
    ADD COLUMN packaging_fee NUMERIC(12, 2) NOT NULL DEFAULT 0,
    ADD CONSTRAINT orders_packaging_fee_non_negative CHECK (packaging_fee >= 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE orders
    DROP CONSTRAINT IF EXISTS orders_packaging_fee_non_negative,
    DROP COLUMN IF EXISTS packaging_fee;
-- +goose StatementEnd
