-- +goose Up
-- +goose StatementBegin
--
-- Provider configuration for SMTP, object storage and AI, at two levels:
-- platform-wide (managed by a Super Admin) and per tenant (managed by that
-- tenant's admin). A tenant either uses its own configuration or the platform's,
-- chosen in tenant_service_preferences and gated by tenant_service_access.
--
-- Secrets never live in `config`: that column is served to the browser. They
-- live in `secret_config`, where every value is an AES-256-GCM envelope
-- produced by internal/secretbox and is only ever decrypted inside the backend.
--
-- This project uses TEXT plus CHECK rather than native enums, matching the rest
-- of the schema and keeping future values a non-breaking ALTER.
CREATE TABLE platform_configurations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_type   TEXT NOT NULL,
    provider       TEXT NOT NULL DEFAULT '',
    config         JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_config  JSONB NOT NULL DEFAULT '{}'::jsonb,
    status         TEXT NOT NULL DEFAULT 'UNCONFIGURED',
    enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    allow_tenants  BOOLEAN NOT NULL DEFAULT FALSE,
    last_error     TEXT NOT NULL DEFAULT '',
    last_tested_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT platform_configurations_service_unique UNIQUE (service_type),
    CONSTRAINT platform_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI')),
    CONSTRAINT platform_configurations_status_check
        CHECK (status IN ('UNCONFIGURED', 'CONFIGURED', 'ENABLED', 'DISABLED',
                          'CONNECTION_FAILED', 'TESTING')),
    -- An enabled row must actually have something to connect with.
    CONSTRAINT platform_configurations_enabled_needs_provider
        CHECK (NOT enabled OR provider <> '')
);

CREATE INDEX platform_configurations_enabled_idx
    ON platform_configurations (service_type) WHERE enabled;

CREATE TABLE tenant_configurations (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    service_type   TEXT NOT NULL,
    provider       TEXT NOT NULL DEFAULT '',
    config         JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_config  JSONB NOT NULL DEFAULT '{}'::jsonb,
    status         TEXT NOT NULL DEFAULT 'UNCONFIGURED',
    enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    last_error     TEXT NOT NULL DEFAULT '',
    last_tested_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_configurations_tenant_service_unique UNIQUE (tenant_id, service_type),
    CONSTRAINT tenant_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI')),
    CONSTRAINT tenant_configurations_status_check
        CHECK (status IN ('UNCONFIGURED', 'CONFIGURED', 'ENABLED', 'DISABLED',
                          'CONNECTION_FAILED', 'TESTING')),
    CONSTRAINT tenant_configurations_enabled_needs_provider
        CHECK (NOT enabled OR provider <> '')
);

CREATE INDEX tenant_configurations_tenant_idx ON tenant_configurations (tenant_id);

-- Which level a tenant has elected to use. The resolver reads this; it never
-- infers intent from the presence of a row.
CREATE TABLE tenant_service_preferences (
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    service_type TEXT NOT NULL,
    source       TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_service_preferences_pk PRIMARY KEY (tenant_id, service_type),
    CONSTRAINT tenant_service_preferences_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI')),
    CONSTRAINT tenant_service_preferences_source_check
        CHECK (source IN ('PLATFORM', 'ORGANIZATION'))
);

-- Per-tenant permission to borrow the platform configuration. A tenant may only
-- select PLATFORM when this row says so *and* the platform row allows tenants.
CREATE TABLE tenant_service_access (
    tenant_id     UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    service_type  TEXT NOT NULL,
    allow_platform BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_service_access_pk PRIMARY KEY (tenant_id, service_type),
    CONSTRAINT tenant_service_access_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_service_access;
DROP TABLE IF EXISTS tenant_service_preferences;
DROP TABLE IF EXISTS tenant_configurations;
DROP TABLE IF EXISTS platform_configurations;
-- +goose StatementEnd
