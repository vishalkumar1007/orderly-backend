-- +goose Up
-- +goose StatementBegin

-- Per-business, not platform-wide: a super admin decides which specific
-- business may use two-factor authentication, set at onboarding and editable
-- afterward from that business's own Configuration tab — the same lifecycle
-- as every other per-tenant grant on this platform, and the one place to
-- look rather than a second, global switch under platform Settings.
ALTER TABLE tenants
    ADD COLUMN mfa_allowed BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE tenants DROP COLUMN IF EXISTS mfa_allowed;
-- +goose StatementEnd
