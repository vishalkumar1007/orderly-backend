-- Expand store_status from binary OPEN/CLOSED to multi-status and add
-- status_message for customer-facing communication.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_store_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_store_status_check
    CHECK (store_status IN ('OPEN', 'BUSY', 'AWAY', 'CLOSED'));
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS status_message TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS tenants_store_status_idx ON tenants (store_status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS tenants_store_status_idx;
ALTER TABLE tenants DROP COLUMN IF EXISTS status_message;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_store_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_store_status_check
    CHECK (store_status IN ('OPEN', 'CLOSED'));
-- +goose StatementEnd
