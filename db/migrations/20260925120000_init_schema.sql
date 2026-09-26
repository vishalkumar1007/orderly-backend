-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE tenants (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    slug          TEXT NOT NULL,
    business_type TEXT NOT NULL,
    owner_name    TEXT NOT NULL,
    phone         TEXT NOT NULL DEFAULT '',
    email         TEXT NOT NULL DEFAULT '',
    address       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tenants_slug_unique UNIQUE (slug),
    CONSTRAINT tenants_business_type_check CHECK (
        business_type IN ('MOMO', 'MANCHURIAN', 'FAST_FOOD', 'ROLLS', 'TEA', 'OTHER')
    ),
    CONSTRAINT tenants_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED'))
);

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID REFERENCES tenants (id) ON DELETE RESTRICT,
    name          TEXT NOT NULL,
    email         TEXT NOT NULL,
    phone         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'ACTIVE',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_email_unique UNIQUE (email),
    CONSTRAINT users_role_check CHECK (role IN ('SUPER_ADMIN', 'TENANT_ADMIN', 'STAFF')),
    CONSTRAINT users_status_check CHECK (status IN ('ACTIVE', 'DISABLED')),
    CONSTRAINT users_tenant_role_check CHECK (
        (role = 'SUPER_ADMIN' AND tenant_id IS NULL)
        OR (role IN ('TENANT_ADMIN', 'STAFF') AND tenant_id IS NOT NULL)
    )
);

CREATE INDEX users_tenant_id_idx ON users (tenant_id);

CREATE TABLE categories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    sort_order  INT NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT categories_tenant_name_unique UNIQUE (tenant_id, name)
);

CREATE INDEX categories_tenant_id_idx ON categories (tenant_id);

CREATE TABLE products (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    category_id  UUID NOT NULL REFERENCES categories (id) ON DELETE RESTRICT,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    price        NUMERIC(12, 2) NOT NULL,
    image_url    TEXT,
    sort_order   INT NOT NULL DEFAULT 0,
    is_available BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT products_price_non_negative CHECK (price >= 0)
);

CREATE INDEX products_tenant_id_idx ON products (tenant_id);
CREATE INDEX products_tenant_category_idx ON products (tenant_id, category_id);

CREATE TABLE customers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    name       TEXT NOT NULL DEFAULT '',
    phone      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX customers_tenant_id_idx ON customers (tenant_id);
CREATE UNIQUE INDEX customers_tenant_phone_unique
    ON customers (tenant_id, phone)
    WHERE phone IS NOT NULL;

CREATE TABLE order_counters (
    tenant_id   UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE RESTRICT,
    last_number BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT order_counters_last_number_non_negative CHECK (last_number >= 0)
);

CREATE TABLE orders (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    customer_id    UUID REFERENCES customers (id) ON DELETE RESTRICT,
    order_number   INT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'PENDING',
    order_type     TEXT NOT NULL DEFAULT 'PICKUP',
    subtotal       NUMERIC(12, 2) NOT NULL DEFAULT 0,
    tax            NUMERIC(12, 2) NOT NULL DEFAULT 0,
    discount       NUMERIC(12, 2) NOT NULL DEFAULT 0,
    total          NUMERIC(12, 2) NOT NULL DEFAULT 0,
    customer_name  TEXT NOT NULL DEFAULT '',
    customer_phone TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT orders_tenant_number_unique UNIQUE (tenant_id, order_number),
    CONSTRAINT orders_status_check CHECK (
        status IN ('PENDING', 'ACCEPTED', 'PREPARING', 'READY', 'COMPLETED', 'CANCELLED')
    ),
    CONSTRAINT orders_order_type_check CHECK (order_type IN ('PICKUP')),
    CONSTRAINT orders_subtotal_non_negative CHECK (subtotal >= 0),
    CONSTRAINT orders_tax_non_negative CHECK (tax >= 0),
    CONSTRAINT orders_discount_non_negative CHECK (discount >= 0),
    CONSTRAINT orders_total_non_negative CHECK (total >= 0),
    CONSTRAINT orders_order_number_positive CHECK (order_number > 0)
);

CREATE INDEX orders_tenant_id_idx ON orders (tenant_id);
CREATE INDEX orders_tenant_status_created_idx ON orders (tenant_id, status, created_at DESC);

CREATE TABLE order_items (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    order_id              UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    product_id            UUID NOT NULL REFERENCES products (id) ON DELETE RESTRICT,
    product_name_snapshot TEXT NOT NULL,
    unit_price            NUMERIC(12, 2) NOT NULL,
    quantity              INT NOT NULL,
    subtotal              NUMERIC(12, 2) NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT order_items_quantity_positive CHECK (quantity > 0),
    CONSTRAINT order_items_unit_price_non_negative CHECK (unit_price >= 0),
    CONSTRAINT order_items_subtotal_non_negative CHECK (subtotal >= 0)
);

CREATE INDEX order_items_tenant_id_idx ON order_items (tenant_id);
CREATE INDEX order_items_order_id_idx ON order_items (order_id);

CREATE TABLE payments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    order_id           UUID NOT NULL REFERENCES orders (id) ON DELETE RESTRICT,
    amount             NUMERIC(12, 2) NOT NULL,
    method             TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'PENDING',
    provider           TEXT NOT NULL DEFAULT '',
    provider_reference TEXT NOT NULL DEFAULT '',
    paid_at            TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payments_order_id_unique UNIQUE (order_id),
    CONSTRAINT payments_amount_non_negative CHECK (amount >= 0),
    CONSTRAINT payments_method_check CHECK (method IN ('UPI', 'CASH', 'CARD')),
    CONSTRAINT payments_status_check CHECK (
        status IN ('PENDING', 'PAID', 'FAILED', 'REFUNDED')
    )
);

CREATE INDEX payments_tenant_id_idx ON payments (tenant_id);

CREATE TABLE audit_logs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID REFERENCES tenants (id) ON DELETE RESTRICT,
    user_id     UUID REFERENCES users (id) ON DELETE RESTRICT,
    action      TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id   UUID,
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_tenant_id_idx ON audit_logs (tenant_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS order_counters;
DROP TABLE IF EXISTS customers;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
-- +goose StatementEnd
