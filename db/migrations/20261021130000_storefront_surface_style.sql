-- +goose Up
-- +goose StatementBegin
-- Storefront surface template: the background and neutral palette the shop
-- sits on (light and dark variants each), independent of the brand colours.
ALTER TABLE tenant_storefront_settings
    ADD COLUMN surface_style TEXT NOT NULL DEFAULT 'clean',
    ADD CONSTRAINT tenant_storefront_surface_style_check CHECK (
        surface_style IN ('clean', 'paper', 'mist', 'stone', 'tinted', 'contrast')
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenant_storefront_settings
    DROP CONSTRAINT IF EXISTS tenant_storefront_surface_style_check,
    DROP COLUMN IF EXISTS surface_style;
-- +goose StatementEnd
