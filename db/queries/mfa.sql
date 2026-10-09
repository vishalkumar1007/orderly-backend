-- name: SyncUserMfaEnabledCache :exec
-- Cosmetic status flag only (HandleMFAStatus display) — login/refresh logic
-- must never read this as authority, see user_mfa_methods queries below.
UPDATE users
SET mfa_enabled = EXISTS (SELECT 1 FROM user_mfa_methods WHERE user_id = $1 AND enabled)
WHERE id = $1;

-- name: UpsertUserMfaMethod :one
INSERT INTO user_mfa_methods (user_id, method, secret_enc, enabled)
VALUES ($1, $2, $3, TRUE)
ON CONFLICT (user_id, method) DO UPDATE SET secret_enc = EXCLUDED.secret_enc, enabled = TRUE
RETURNING *;

-- name: ListUserMfaMethods :many
SELECT * FROM user_mfa_methods WHERE user_id = $1 AND enabled ORDER BY created_at;

-- name: CountUserMfaMethods :one
SELECT count(*) FROM user_mfa_methods WHERE user_id = $1 AND enabled;

-- name: GetUserMfaMethod :one
SELECT * FROM user_mfa_methods WHERE user_id = $1 AND method = $2 AND enabled;

-- name: DeleteUserMfaMethod :exec
DELETE FROM user_mfa_methods WHERE user_id = $1 AND method = $2;

-- name: DeleteAllUserMfaMethods :exec
DELETE FROM user_mfa_methods WHERE user_id = $1;

-- name: TouchUserMfaMethod :exec
UPDATE user_mfa_methods SET last_used_at = now() WHERE user_id = $1 AND method = $2;

-- name: CreateRecoveryCode :exec
INSERT INTO mfa_recovery_codes (user_id, code_hash)
VALUES ($1, $2);

-- name: ListUnusedRecoveryCodes :many
SELECT * FROM mfa_recovery_codes
WHERE user_id = $1 AND used_at IS NULL;

-- name: MarkRecoveryCodeUsed :exec
UPDATE mfa_recovery_codes
SET used_at = now()
WHERE id = $1;

-- name: DeleteRecoveryCodesForUser :exec
DELETE FROM mfa_recovery_codes WHERE user_id = $1;
