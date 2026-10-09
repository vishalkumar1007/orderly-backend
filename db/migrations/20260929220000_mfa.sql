-- +goose Up
-- +goose StatementBegin

-- TOTP two-factor auth. mfa_secret is sealed with the same secretbox used for
-- provider credentials elsewhere — never stored in plaintext when
-- CONFIG_ENCRYPTION_KEY is set. NULL until the user confirms enrollment; the
-- secret generated during setup lives only in a short-lived signed token
-- until then, never written half-finished.
ALTER TABLE users
    ADD COLUMN mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN mfa_secret TEXT;

-- One row per recovery code so each can be marked used independently.
-- code_hash is sha256 (internal/auth.hashToken), the same scheme invite
-- tokens already use — these are high-entropy random codes, not passwords,
-- so bcrypt's slow-hash property buys nothing here.
CREATE TABLE mfa_recovery_codes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX mfa_recovery_codes_user_idx ON mfa_recovery_codes (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS mfa_recovery_codes;
ALTER TABLE users
    DROP COLUMN IF EXISTS mfa_secret,
    DROP COLUMN IF EXISTS mfa_enabled;
-- +goose StatementEnd
