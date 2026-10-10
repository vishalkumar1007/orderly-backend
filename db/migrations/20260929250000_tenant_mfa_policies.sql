-- +goose Up
-- +goose StatementBegin

-- One row per tenant describing how MFA behaves for that business, distinct
-- from tenants.mfa_allowed (the Super Admin's permission gate — whether the
-- business may use MFA at all). No row for a tenant means DISABLED with the
-- defaults below, handled in application code rather than backfilled, so a
-- business that never touches this stays exactly as it is today.
--
-- Owned by the business's own Tenant Admin after creation (PUT
-- /api/v1/tenant/mfa-policy) — a Super Admin only seeds the initial row at
-- onboarding and reads it back read-only afterward, the same split already
-- used for the storefront template.
CREATE TABLE tenant_mfa_policies (
    tenant_id         UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    mode              TEXT NOT NULL DEFAULT 'DISABLED',
    allowed_methods   TEXT[] NOT NULL DEFAULT ARRAY['TOTP'],
    enforce_scope     TEXT NOT NULL DEFAULT 'ALL_ADMINS',
    enforce_roles     TEXT[],
    grace_period_days INTEGER NOT NULL DEFAULT 7,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT tenant_mfa_policies_mode_check
        CHECK (mode IN ('DISABLED', 'OPTIONAL', 'REQUIRED')),
    CONSTRAINT tenant_mfa_policies_scope_check
        CHECK (enforce_scope IN ('ALL_ADMINS', 'SELECTED_ROLES')),
    CONSTRAINT tenant_mfa_policies_grace_check
        CHECK (grace_period_days >= 0)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_mfa_policies;
-- +goose StatementEnd
