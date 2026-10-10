-- name: CreateMfaEmailOtpCode :one
INSERT INTO mfa_email_otp_codes (user_id, code_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- Latest unconsumed code for a user. One live code at a time: sending a new
-- one supersedes the old.
-- name: GetActiveMfaEmailOtpCode :one
SELECT * FROM mfa_email_otp_codes
WHERE user_id = $1 AND used_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: IncrementMfaEmailOtpAttempts :one
UPDATE mfa_email_otp_codes
SET attempts = attempts + 1
WHERE id = $1
RETURNING *;

-- name: ConsumeMfaEmailOtpCode :one
UPDATE mfa_email_otp_codes
SET used_at = now()
WHERE id = $1 AND used_at IS NULL
RETURNING *;

-- name: InvalidateMfaEmailOtpCodes :exec
UPDATE mfa_email_otp_codes
SET used_at = now()
WHERE user_id = $1 AND used_at IS NULL;
