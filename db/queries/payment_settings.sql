-- name: GetPlatformPaymentMode :one
SELECT * FROM payment_settings WHERE tenant_id IS NULL;

-- name: UpsertPlatformPaymentMode :one
INSERT INTO payment_settings (tenant_id, mode, updated_by)
VALUES (NULL, $1, $2)
ON CONFLICT ((tenant_id IS NULL)) WHERE tenant_id IS NULL
DO UPDATE SET mode = EXCLUDED.mode, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: GetTenantPaymentMode :one
SELECT * FROM payment_settings WHERE tenant_id = $1;

-- name: UpsertTenantPaymentMode :one
INSERT INTO payment_settings (tenant_id, mode, updated_by)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id) WHERE tenant_id IS NOT NULL
DO UPDATE SET mode = EXCLUDED.mode, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: DeleteTenantPaymentMode :exec
DELETE FROM payment_settings WHERE tenant_id = $1;
