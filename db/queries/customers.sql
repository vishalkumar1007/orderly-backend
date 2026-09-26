-- Phone identity for storefront customers. The (tenant_id, phone) unique index
-- already exists in the schema, so this upsert cannot create a duplicate.
-- name: UpsertCustomerByPhone :one
INSERT INTO customers (tenant_id, name, phone, last_login_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), sqlc.arg(phone), now())
ON CONFLICT (tenant_id, phone) WHERE phone IS NOT NULL
DO UPDATE SET
    name = CASE WHEN customers.name = '' THEN EXCLUDED.name ELSE customers.name END,
    last_login_at = now()
RETURNING *;

-- name: GetCustomerByPhone :one
SELECT * FROM customers
WHERE tenant_id = $1 AND phone = $2
LIMIT 1;

-- name: GetCustomerByID :one
SELECT * FROM customers
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: UpdateCustomerProfile :one
UPDATE customers
SET
    name = COALESCE(sqlc.narg(name), name),
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: ListCustomerOrders :many
SELECT * FROM orders
WHERE tenant_id = sqlc.arg(tenant_id) AND customer_id = sqlc.arg(customer_id)
ORDER BY created_at DESC
LIMIT sqlc.arg(limit_count);
