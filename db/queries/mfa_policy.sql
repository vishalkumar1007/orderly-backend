-- name: GetTenantMfaPolicy :one
SELECT * FROM tenant_mfa_policies WHERE tenant_id = $1;

-- name: UpsertTenantMfaPolicy :one
INSERT INTO tenant_mfa_policies (tenant_id, mode, allowed_methods, enforce_scope, enforce_roles, grace_period_days)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id) DO UPDATE SET
    mode = EXCLUDED.mode,
    allowed_methods = EXCLUDED.allowed_methods,
    enforce_scope = EXCLUDED.enforce_scope,
    enforce_roles = EXCLUDED.enforce_roles,
    grace_period_days = EXCLUDED.grace_period_days,
    updated_at = now()
RETURNING *;
