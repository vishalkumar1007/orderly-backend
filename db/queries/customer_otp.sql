-- name: CreateOtpCode :one
INSERT INTO customer_otp_codes (tenant_id, phone, code_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- Latest unconsumed code for a phone number. One live code at a time: sending
-- a new one supersedes the old.
-- name: GetActiveOtpCode :one
SELECT * FROM customer_otp_codes
WHERE tenant_id = $1
  AND phone = $2
  AND consumed_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: IncrementOtpAttempts :one
UPDATE customer_otp_codes
SET attempts = attempts + 1
WHERE id = $1
RETURNING *;

-- name: ConsumeOtpCode :one
UPDATE customer_otp_codes
SET consumed_at = now()
WHERE id = $1 AND consumed_at IS NULL
RETURNING *;

-- name: InvalidateOtpCodes :exec
UPDATE customer_otp_codes
SET consumed_at = now()
WHERE tenant_id = $1 AND phone = $2 AND consumed_at IS NULL;
