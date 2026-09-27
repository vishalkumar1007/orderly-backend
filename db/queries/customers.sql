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

-- Shop CRM: signed-in / known customers with order stats (matched by id or phone).
-- name: ListCustomersByTenant :many
SELECT
    c.id,
    c.name,
    c.phone,
    c.is_blocked,
    c.last_login_at,
    c.created_at,
    c.updated_at,
    (
        SELECT COUNT(*)::bigint
        FROM orders o
        WHERE o.tenant_id = c.tenant_id
          AND (
              o.customer_id = c.id
              OR (c.phone IS NOT NULL AND o.customer_phone = c.phone)
          )
    ) AS order_count,
    (
        SELECT COALESCE(SUM(o.total), 0)::float8
        FROM orders o
        WHERE o.tenant_id = c.tenant_id
          AND (
              o.customer_id = c.id
              OR (c.phone IS NOT NULL AND o.customer_phone = c.phone)
          )
    ) AS total_spend,
    (
        SELECT MAX(o.created_at)
        FROM orders o
        WHERE o.tenant_id = c.tenant_id
          AND (
              o.customer_id = c.id
              OR (c.phone IS NOT NULL AND o.customer_phone = c.phone)
          )
    ) AS last_order_at
FROM customers c
WHERE c.tenant_id = sqlc.arg(tenant_id)
ORDER BY COALESCE(
    (
        SELECT MAX(o.created_at)
        FROM orders o
        WHERE o.tenant_id = c.tenant_id
          AND (
              o.customer_id = c.id
              OR (c.phone IS NOT NULL AND o.customer_phone = c.phone)
          )
    ),
    c.created_at
) DESC
LIMIT sqlc.arg(limit_count)
OFFSET sqlc.arg(offset_count);

-- Guests who placed orders but never got a customers row (no phone login).
-- name: ListGuestCustomersFromOrders :many
SELECT
    MAX(o.customer_name) AS name,
    o.customer_phone AS phone,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(o.total), 0)::float8 AS total_spend,
    MAX(o.created_at) AS last_order_at,
    MIN(o.created_at) AS first_seen_at
FROM orders o
WHERE o.tenant_id = sqlc.arg(tenant_id)
  AND NULLIF(TRIM(o.customer_phone), '') IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM customers c
      WHERE c.tenant_id = o.tenant_id AND c.phone = o.customer_phone
  )
GROUP BY o.customer_phone
ORDER BY MAX(o.created_at) DESC
LIMIT sqlc.arg(limit_count);

-- name: SetCustomerBlocked :one
UPDATE customers
SET is_blocked = sqlc.arg(is_blocked), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: CountCustomersByTenant :one
SELECT COUNT(*)::bigint FROM customers WHERE tenant_id = sqlc.arg(tenant_id);
