-- +goose Up
-- +goose StatementBegin

-- Email OTP codes for staff/admin MFA — same shape as the existing customer
-- phone-OTP table (customer_otp_codes), scoped by user_id instead of
-- tenant_id+phone. Rate limiting (resend cooldown, attempt cap) is plain
-- application-code checks against this table's rows, mirroring
-- internal/customers/otp.go rather than introducing a new framework.
CREATE TABLE mfa_email_otp_codes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash    TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX mfa_email_otp_codes_user_idx ON mfa_email_otp_codes (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS mfa_email_otp_codes;
-- +goose StatementEnd
