-- +goose Up
-- +goose StatementBegin
CREATE TABLE platform_settings (
    id         SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    config     JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform_settings (id, config) VALUES (1, jsonb_build_object(
    'platform_name', 'Orderly',
    'support_email', 'support@orderly.local',
    'timezone', 'Asia/Kolkata',
    'default_locale', 'en-IN',
    'session_timeout_minutes', 15,
    'require_mfa_for_admins', false,
    'password_min_length', 8,
    'invite_expiry_hours', 168,
    'allow_self_serve', false,
    'default_plan', 'TRIAL',
    'maintenance_mode', false
));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS platform_settings;
-- +goose StatementEnd
