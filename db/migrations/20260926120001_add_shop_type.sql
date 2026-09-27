-- +goose Up
-- +goose StatementBegin
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS shop_type TEXT NOT NULL DEFAULT 'restaurant';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants DROP COLUMN IF EXISTS shop_type;
-- +goose StatementEnd
