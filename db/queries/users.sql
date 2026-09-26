-- name: GetUserByEmail :one
SELECT * FROM users
WHERE email = $1
LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1
LIMIT 1;

-- name: GetUserByInviteTokenHash :one
SELECT * FROM users
WHERE invite_token_hash = $1
LIMIT 1;

-- name: CreateUser :one
INSERT INTO users (
    tenant_id, name, email, phone, password_hash, role, status, must_set_password, invite_token_hash
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: SetUserPassword :one
UPDATE users
SET
    password_hash = $2,
    must_set_password = FALSE,
    invite_token_hash = NULL,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListTenantAdmins :many
SELECT * FROM users
WHERE tenant_id = $1 AND role = 'TENANT_ADMIN'
ORDER BY created_at ASC;

-- name: ListTenantUsers :many
SELECT
    u.*,
    (SELECT MAX(rt.created_at) FROM refresh_tokens rt WHERE rt.user_id = u.id) AS last_activity
FROM users u
WHERE u.tenant_id = sqlc.arg(tenant_id)
ORDER BY u.created_at ASC;

-- name: UpdateUserProfile :one
UPDATE users
SET
    name = COALESCE(sqlc.narg(name), name),
    phone = COALESCE(sqlc.narg(phone), phone),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetUserStatus :one
UPDATE users
SET status = sqlc.arg(status), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetUserRole :one
UPDATE users
SET role = sqlc.arg(role), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ResetUserAccess :one
-- Clears the usable-password state and installs a fresh invite token so the
-- user must complete setup again. Callers revoke refresh tokens separately.
UPDATE users
SET
    must_set_password = TRUE,
    invite_token_hash = sqlc.arg(invite_token_hash),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: CountTenantUsersByRole :one
SELECT COUNT(*)::bigint
FROM users
WHERE tenant_id = sqlc.arg(tenant_id)
  AND role = sqlc.arg(role)
  AND status = 'ACTIVE';

-- name: CountTenantUsers :one
SELECT COUNT(*)::bigint
FROM users
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: InsertRefreshToken :one
INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens
WHERE token_hash = $1
LIMIT 1;

-- name: DeleteRefreshTokenByHash :exec
DELETE FROM refresh_tokens
WHERE token_hash = $1;

-- name: DeleteRefreshTokensByUser :exec
DELETE FROM refresh_tokens
WHERE user_id = $1;

-- name: HasSuperAdmin :one
SELECT EXISTS(
    SELECT 1 FROM users
    WHERE role = 'SUPER_ADMIN' AND tenant_id IS NULL
) AS has_super_admin;

-- name: GetTenantAdminForTenant :one
SELECT * FROM users
WHERE tenant_id = $1 AND role = 'TENANT_ADMIN'
ORDER BY created_at ASC
LIMIT 1;

-- name: UpdateUserInviteToken :one
UPDATE users
SET
    invite_token_hash = $2,
    must_set_password = TRUE,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetUserPasswordWithCurrent :one
UPDATE users
SET
    password_hash = $2,
    must_set_password = FALSE,
    invite_token_hash = NULL,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CreateSuperAdmin :one
INSERT INTO users (
    tenant_id, name, email, phone, password_hash, role, status, must_set_password, invite_token_hash
) VALUES (
    NULL, $1, $2, '', $3, 'SUPER_ADMIN', 'ACTIVE', FALSE, NULL
)
RETURNING *;
