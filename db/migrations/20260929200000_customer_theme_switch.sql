-- +goose Up
ALTER TABLE tenant_storefront_settings
    ADD COLUMN IF NOT EXISTS customer_theme_switch_enabled BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE tenant_storefront_settings
    DROP COLUMN IF EXISTS customer_theme_switch_enabled;
