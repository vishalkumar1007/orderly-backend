-- +goose Up
-- +goose StatementBegin

-- Adds SMS as a third configurable service, alongside SMTP/Storage/AI, in the
-- same platform-vs-tenant configuration tables (internal/configsvc). No new
-- tables: SMS reuses platform_configurations/tenant_configurations/
-- tenant_service_preferences/tenant_service_access exactly as they already
-- work for the other three services.
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

-- +goose Down
-- +goose StatementBegin

ALTER TABLE platform_configurations
    DROP CONSTRAINT platform_configurations_service_check,
    ADD CONSTRAINT platform_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI'));

ALTER TABLE tenant_configurations
    DROP CONSTRAINT tenant_configurations_service_check,
    ADD CONSTRAINT tenant_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI'));

ALTER TABLE tenant_service_preferences
    DROP CONSTRAINT tenant_service_preferences_service_check,
    ADD CONSTRAINT tenant_service_preferences_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI'));

ALTER TABLE tenant_service_access
    DROP CONSTRAINT tenant_service_access_service_check,
    ADD CONSTRAINT tenant_service_access_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI'));

-- +goose StatementEnd
