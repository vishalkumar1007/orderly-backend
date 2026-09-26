-- +goose Up
-- +goose StatementBegin
-- Customer storefront configuration.
--
-- One row per tenant, created lazily. Everything the customer storefront needs
-- to render lives here, split into scalar columns (identity, theme choice) and
-- four JSONB documents (homepage layout, payment methods, order workflow,
-- opening hours) so the shape can evolve without a migration per field.
CREATE TABLE tenant_storefront_settings (
    tenant_id                 UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,

    -- Brand identity
    logo_url                  TEXT    NOT NULL DEFAULT '',
    favicon_url               TEXT    NOT NULL DEFAULT '',
    business_name             TEXT    NOT NULL DEFAULT '',
    tagline                   TEXT    NOT NULL DEFAULT '',
    description               TEXT    NOT NULL DEFAULT '',
    phone                     TEXT    NOT NULL DEFAULT '',
    address                   TEXT    NOT NULL DEFAULT '',

    -- Controlled design tokens. Never free-form CSS.
    theme_preset              TEXT    NOT NULL DEFAULT 'modern',
    primary_color             TEXT    NOT NULL DEFAULT '',
    secondary_color           TEXT    NOT NULL DEFAULT '',
    accent_color              TEXT    NOT NULL DEFAULT '',
    theme_mode                TEXT    NOT NULL DEFAULT 'system',
    font_family               TEXT    NOT NULL DEFAULT 'inter',
    radius                    TEXT    NOT NULL DEFAULT 'md',
    button_style              TEXT    NOT NULL DEFAULT 'rounded',
    card_style                TEXT    NOT NULL DEFAULT 'elevated',
    header_style              TEXT    NOT NULL DEFAULT 'sticky',
    hero_style                TEXT    NOT NULL DEFAULT 'image',
    hero_image_url            TEXT    NOT NULL DEFAULT '',

    -- Store behaviour
    ordering_enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    closed_message            TEXT    NOT NULL DEFAULT '',
    customer_login_enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    prep_time_minutes         INT     NOT NULL DEFAULT 20,
    tax_percent               NUMERIC(5, 2) NOT NULL DEFAULT 0,
    packaging_fee             NUMERIC(12, 2) NOT NULL DEFAULT 0,

    -- Flexible documents
    opening_hours             JSONB   NOT NULL DEFAULT '{}'::jsonb,
    homepage                  JSONB   NOT NULL DEFAULT '{}'::jsonb,
    payments                  JSONB   NOT NULL DEFAULT '{}'::jsonb,
    workflow                  JSONB   NOT NULL DEFAULT '{}'::jsonb,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_storefront_theme_preset_check CHECK (
        theme_preset IN ('classic', 'modern', 'street-food', 'minimal', 'fresh', 'dark')
    ),
    CONSTRAINT tenant_storefront_theme_mode_check CHECK (
        theme_mode IN ('light', 'dark', 'system')
    ),
    CONSTRAINT tenant_storefront_font_check CHECK (
        font_family IN ('inter', 'sora', 'poppins', 'system')
    ),
    CONSTRAINT tenant_storefront_radius_check CHECK (
        radius IN ('none', 'sm', 'md', 'lg', 'pill')
    ),
    CONSTRAINT tenant_storefront_button_style_check CHECK (
        button_style IN ('rounded', 'pill', 'square', 'soft')
    ),
    CONSTRAINT tenant_storefront_card_style_check CHECK (
        card_style IN ('elevated', 'outlined', 'filled', 'minimal')
    ),
    CONSTRAINT tenant_storefront_header_style_check CHECK (
        header_style IN ('sticky', 'solid', 'transparent')
    ),
    CONSTRAINT tenant_storefront_hero_style_check CHECK (
        hero_style IN ('image', 'gradient', 'compact', 'none')
    ),
    CONSTRAINT tenant_storefront_prep_time_check CHECK (prep_time_minutes BETWEEN 0 AND 240),
    CONSTRAINT tenant_storefront_tax_check CHECK (tax_percent >= 0 AND tax_percent <= 100),
    CONSTRAINT tenant_storefront_packaging_check CHECK (packaging_fee >= 0)
);

-- Product merchandising: dietary flag, merchandising flags, add-on groups.
ALTER TABLE products
    ADD COLUMN is_vegetarian             BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN is_featured               BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN is_popular                BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN allow_special_instructions BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN addons                    JSONB   NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT products_addons_array_check CHECK (jsonb_typeof(addons) = 'array');

CREATE INDEX products_tenant_featured_idx ON products (tenant_id, is_featured) WHERE is_featured;
CREATE INDEX products_tenant_popular_idx  ON products (tenant_id, is_popular)  WHERE is_popular;

-- Order line customisations. Price is snapshotted from the product's add-on
-- catalogue at order time; the client never supplies an amount.
ALTER TABLE order_items
    ADD COLUMN addons JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN notes   TEXT   NOT NULL DEFAULT '',
    ADD CONSTRAINT order_items_addons_array_check CHECK (jsonb_typeof(addons) = 'array');

-- Order lifecycle bookkeeping.
ALTER TABLE orders
    ADD COLUMN customer_email  TEXT NOT NULL DEFAULT '',
    ADD COLUMN notes           TEXT NOT NULL DEFAULT '',
    ADD COLUMN estimated_ready_at TIMESTAMPTZ,
    ADD COLUMN accepted_at     TIMESTAMPTZ,
    ADD COLUMN preparing_at    TIMESTAMPTZ,
    ADD COLUMN ready_at        TIMESTAMPTZ,
    ADD COLUMN completed_at    TIMESTAMPTZ,
    ADD COLUMN cancelled_at    TIMESTAMPTZ,
    ADD COLUMN cancel_reason   TEXT NOT NULL DEFAULT '',
    ADD COLUMN source          TEXT NOT NULL DEFAULT 'GUEST';

-- Idempotency key from the client. The unique index is what makes a retried
-- checkout (double tap, flaky network, refresh) return the original order
-- instead of creating a second one.
ALTER TABLE orders ADD COLUMN client_token TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX orders_tenant_client_token_unique
    ON orders (tenant_id, client_token)
    WHERE client_token <> '';

ALTER TABLE payments
    ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN attempt_count   INT    NOT NULL DEFAULT 0;

-- A payment row is created with the order and then only ever updated in place,
-- so `payments_order_id_unique` (already present) is the duplicate-payment
-- guarantee. No new index needed.
-- Online payment is now one method ("ONLINE") rather than per-rail
-- "UPI"/"CARD", so the old constraint comes off, existing online rows collapse
-- onto the new value, and the constraint is replaced.
ALTER TABLE payments DROP CONSTRAINT payments_method_check;
UPDATE payments SET method = 'ONLINE' WHERE method IN ('UPI', 'CARD');
ALTER TABLE payments
    ADD CONSTRAINT payments_method_check CHECK (method IN ('ONLINE', 'CASH'));

-- Customer phone identity. `customers_tenant_phone_unique` already exists.
ALTER TABLE customers
    ADD COLUMN last_login_at TIMESTAMPTZ,
    ADD COLUMN is_blocked     BOOLEAN NOT NULL DEFAULT FALSE;

-- OTP lifecycle for phone login. Codes are stored hashed; a row is consumed on
-- first successful verification and attempt-counted to stop brute force.
CREATE TABLE customer_otp_codes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    phone       TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    attempts    INT  NOT NULL DEFAULT 0,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT customer_otp_attempts_check CHECK (attempts >= 0)
);

CREATE INDEX customer_otp_tenant_phone_idx
    ON customer_otp_codes (tenant_id, phone, created_at DESC);

-- Immutable order state log. Drives the customer tracking timeline and gives
-- the tenant a free audit of who moved an order where.
CREATE TABLE order_status_history (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    order_id    UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status TEXT NOT NULL DEFAULT '',
    to_status   TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT 'system',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX order_status_history_order_idx ON order_status_history (order_id, created_at);
CREATE INDEX orders_tenant_customer_idx ON orders (tenant_id, customer_id, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS orders_tenant_customer_idx;
DROP INDEX IF EXISTS order_status_history_order_idx;
DROP TABLE IF EXISTS order_status_history;
DROP INDEX IF EXISTS customer_otp_tenant_phone_idx;
DROP TABLE IF EXISTS customer_otp_codes;

ALTER TABLE customers
    DROP COLUMN IF EXISTS is_blocked,
    DROP COLUMN IF EXISTS last_login_at;

ALTER TABLE payments
    DROP CONSTRAINT IF EXISTS payments_method_check;
ALTER TABLE payments ADD CONSTRAINT payments_method_check CHECK (method IN ('UPI', 'CASH', 'CARD'));
ALTER TABLE payments
    DROP COLUMN IF EXISTS attempt_count,
    DROP COLUMN IF EXISTS failure_reason;

DROP INDEX IF EXISTS orders_tenant_client_token_unique;
ALTER TABLE orders
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS completed_at,
    DROP COLUMN IF EXISTS ready_at,
    DROP COLUMN IF EXISTS preparing_at,
    DROP COLUMN IF EXISTS accepted_at,
    DROP COLUMN IF EXISTS estimated_ready_at,
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS customer_email,
    DROP COLUMN IF EXISTS client_token;

ALTER TABLE order_items
    DROP CONSTRAINT IF EXISTS order_items_addons_array_check,
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS addons;

DROP INDEX IF EXISTS products_tenant_popular_idx;
DROP INDEX IF EXISTS products_tenant_featured_idx;
ALTER TABLE products
    DROP CONSTRAINT IF EXISTS products_addons_array_check,
    DROP COLUMN IF EXISTS addons,
    DROP COLUMN IF EXISTS allow_special_instructions,
    DROP COLUMN IF EXISTS is_popular,
    DROP COLUMN IF EXISTS is_featured,
    DROP COLUMN IF EXISTS is_vegetarian;

DROP TABLE IF EXISTS tenant_storefront_settings;
-- +goose StatementEnd
