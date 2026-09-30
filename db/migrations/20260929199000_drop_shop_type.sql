-- +goose Up
-- +goose StatementBegin

-- tenants.shop_type (default 'restaurant') was a second, redundant business
-- discriminator sitting alongside tenants.business_type. Nothing keyed real
-- behavior off it — grep shows it was only ever echoed back in one admin
-- response payload (now removed). One column, one source of truth.
ALTER TABLE tenants DROP COLUMN shop_type;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants ADD COLUMN shop_type TEXT NOT NULL DEFAULT 'restaurant';
-- +goose StatementEnd
