-- +goose Up
-- +goose StatementBegin
-- Storefront layout controls and three-mode customer login.
--
-- product_layout / filter_style are closed enums the customer storefront
-- reads as CSS tokens. customer_login_mode replaces the boolean toggle with
-- off | optional | required while keeping customer_login_enabled in sync for
-- older clients (enabled = mode != 'off').

ALTER TABLE tenant_storefront_settings
    ADD COLUMN product_layout TEXT NOT NULL DEFAULT 'list',
    ADD COLUMN filter_style TEXT NOT NULL DEFAULT 'chips',
    ADD COLUMN customer_login_mode TEXT NOT NULL DEFAULT 'optional';

UPDATE tenant_storefront_settings
SET customer_login_mode = CASE
    WHEN customer_login_enabled THEN 'optional'
    ELSE 'off'
END;

ALTER TABLE tenant_storefront_settings
    ADD CONSTRAINT tenant_storefront_product_layout_check CHECK (
        product_layout IN ('list', 'grid', 'compact')
    ),
    ADD CONSTRAINT tenant_storefront_filter_style_check CHECK (
        filter_style IN ('chips', 'pills', 'rail')
    ),
    ADD CONSTRAINT tenant_storefront_login_mode_check CHECK (
        customer_login_mode IN ('off', 'optional', 'required')
    );

-- Keep the legacy boolean aligned whenever mode is written outside the app.
CREATE OR REPLACE FUNCTION tenant_storefront_sync_login_enabled()
RETURNS trigger AS $$
BEGIN
    NEW.customer_login_enabled := (NEW.customer_login_mode <> 'off');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenant_storefront_login_mode_sync
    BEFORE INSERT OR UPDATE OF customer_login_mode
    ON tenant_storefront_settings
    FOR EACH ROW
    EXECUTE FUNCTION tenant_storefront_sync_login_enabled();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS tenant_storefront_login_mode_sync ON tenant_storefront_settings;
DROP FUNCTION IF EXISTS tenant_storefront_sync_login_enabled();

ALTER TABLE tenant_storefront_settings
    DROP CONSTRAINT IF EXISTS tenant_storefront_product_layout_check,
    DROP CONSTRAINT IF EXISTS tenant_storefront_filter_style_check,
    DROP CONSTRAINT IF EXISTS tenant_storefront_login_mode_check;

ALTER TABLE tenant_storefront_settings
    DROP COLUMN IF EXISTS product_layout,
    DROP COLUMN IF EXISTS filter_style,
    DROP COLUMN IF EXISTS customer_login_mode;
-- +goose StatementEnd
