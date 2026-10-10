-- +goose Up
-- +goose StatementBegin

-- One row per enrolled method per user, replacing the single
-- users.mfa_secret column now that TOTP has a second method (Email OTP)
-- alongside it. secret_enc is TOTP-only (sealed with secretbox, same context
-- string "user:"+userID already used for mfa_secret, so this is a pure
-- column move — the sealed value is byte-identical, not re-encrypted).
CREATE TABLE user_mfa_methods (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    method        TEXT NOT NULL,
    secret_enc    TEXT,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT user_mfa_methods_method_check
        CHECK (method IN ('TOTP', 'EMAIL_OTP')),
    CONSTRAINT user_mfa_methods_user_method_unique
        UNIQUE (user_id, method)
);

CREATE INDEX user_mfa_methods_user_idx ON user_mfa_methods (user_id);

INSERT INTO user_mfa_methods (user_id, method, secret_enc, enabled)
SELECT id, 'TOTP', mfa_secret, TRUE
FROM users
WHERE mfa_enabled AND mfa_secret IS NOT NULL;

ALTER TABLE users DROP COLUMN mfa_secret;

-- users.mfa_enabled stays, but only as a cosmetic status cache from here on
-- (kept in sync by auth.syncMfaEnabledCache after every enrollment change) —
-- login/refresh logic must query user_mfa_methods directly, never this flag.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users ADD COLUMN mfa_secret TEXT;
UPDATE users SET mfa_secret = m.secret_enc
FROM user_mfa_methods m
WHERE m.user_id = users.id AND m.method = 'TOTP';
DROP TABLE IF EXISTS user_mfa_methods;
-- +goose StatementEnd
