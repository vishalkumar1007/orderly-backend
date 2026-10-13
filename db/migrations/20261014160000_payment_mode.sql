-- +goose Up
-- +goose StatementBegin

-- Payments as a configurable service (PAYMENTS-PLAN.md P-B). Same pattern as
-- the SMS migration: no new config tables, just widen the service check on the
-- four tables configsvc already uses.
ALTER TABLE platform_configurations
    DROP CONSTRAINT platform_configurations_service_check,
    ADD CONSTRAINT platform_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));

ALTER TABLE tenant_configurations
    DROP CONSTRAINT tenant_configurations_service_check,
    ADD CONSTRAINT tenant_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));

ALTER TABLE tenant_service_preferences
    DROP CONSTRAINT tenant_service_preferences_service_check,
    ADD CONSTRAINT tenant_service_preferences_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));

ALTER TABLE tenant_service_access
    DROP CONSTRAINT tenant_service_access_service_check,
    ADD CONSTRAINT tenant_service_access_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));

-- Dummy vs real. One platform default row (tenant_id NULL) and an optional
-- per-business override. No row means SANDBOX, so nothing goes live by
-- upgrade. Settings, not financial history, so they cascade with the tenant.
CREATE TABLE payment_settings (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID REFERENCES tenants (id) ON DELETE CASCADE,
    mode       TEXT        NOT NULL,
    updated_by UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT payment_settings_mode_check CHECK (mode IN ('SANDBOX', 'LIVE'))
);
CREATE UNIQUE INDEX payment_settings_tenant_unique
    ON payment_settings (tenant_id) WHERE tenant_id IS NOT NULL;
CREATE UNIQUE INDEX payment_settings_platform_unique
    ON payment_settings ((tenant_id IS NULL)) WHERE tenant_id IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS payment_settings;

-- A PAYMENTS row cannot satisfy the narrower check.
DELETE FROM tenant_service_access WHERE service_type = 'PAYMENTS';
DELETE FROM tenant_service_preferences WHERE service_type = 'PAYMENTS';
DELETE FROM tenant_configurations WHERE service_type = 'PAYMENTS';
DELETE FROM platform_configurations WHERE service_type = 'PAYMENTS';

ALTER TABLE platform_configurations
    DROP CONSTRAINT platform_configurations_service_check,
    ADD CONSTRAINT platform_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS'));
ALTER TABLE tenant_configurations
    DROP CONSTRAINT tenant_configurations_service_check,
    ADD CONSTRAINT tenant_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS'));
ALTER TABLE tenant_service_preferences
    DROP CONSTRAINT tenant_service_preferences_service_check,
    ADD CONSTRAINT tenant_service_preferences_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS'));
ALTER TABLE tenant_service_access
    DROP CONSTRAINT tenant_service_access_service_check,
    ADD CONSTRAINT tenant_service_access_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS'));

-- +goose StatementEnd
