-- name: ListTenantTypes :many
SELECT code, label, active, sort_order, created_at
FROM tenant_types
ORDER BY sort_order ASC, label ASC;

-- name: GetTenantType :one
SELECT code, label, active, sort_order, created_at
FROM tenant_types
WHERE code = $1
LIMIT 1;

-- name: CreateTenantType :one
INSERT INTO tenant_types (code, label, active, sort_order)
VALUES ($1, $2, $3, $4)
RETURNING code, label, active, sort_order, created_at;

-- name: UpdateTenantType :one
UPDATE tenant_types
SET label = $2, active = $3, sort_order = $4
WHERE code = $1
RETURNING code, label, active, sort_order, created_at;
