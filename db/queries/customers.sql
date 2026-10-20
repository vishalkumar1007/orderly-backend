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

-- name: UpsertCustomerByEmail :one
-- Email sign-in. The unique index is on lower(email), so the conflict target
-- names the same expression.
INSERT INTO customers (tenant_id, name, email, last_login_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), lower(sqlc.arg(email)::text), now())
ON CONFLICT (tenant_id, lower(email)) WHERE email IS NOT NULL
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

-- name: SetCustomerBlocked :one
UPDATE customers
SET is_blocked = sqlc.arg(is_blocked), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: CountCustomersByTenant :one
SELECT COUNT(*)::bigint FROM customers WHERE tenant_id = sqlc.arg(tenant_id);

-- name: SetCustomerEmailIfEmpty :exec
-- Checkout opt-in may bring an email a phone-only customer never gave us.
UPDATE customers c SET email = sqlc.arg(email), updated_at = now()
WHERE c.id = sqlc.arg(id) AND c.tenant_id = sqlc.arg(tenant_id) AND c.email IS NULL
  AND NOT EXISTS (SELECT 1 FROM customers o WHERE o.tenant_id = c.tenant_id AND lower(o.email) = lower(sqlc.arg(email)));

-- Shop CRM list: registered customers and phone-only guests in one page.
-- Each order is attributed once: to its customer_id, else to the registered
-- customer with that phone, else to a guest keyed by phone.
-- ponytail: aggregates the tenant's whole order history per call; move to a
-- per-customer stats table if a shop passes ~100k orders.
-- name: ListShopCustomers :many
WITH ord AS (
    SELECT COALESCE(o.customer_id, cp.id) AS cid, o.customer_phone, o.customer_name, o.total, o.created_at
    FROM orders o
    LEFT JOIN customers cp
        ON o.customer_id IS NULL AND cp.tenant_id = o.tenant_id AND cp.phone = o.customer_phone
    WHERE o.tenant_id = sqlc.arg(tenant_id)
),
reg AS (
    SELECT cid, COUNT(*)::bigint AS n, SUM(total)::float8 AS spend, MAX(created_at)::timestamptz AS last_at
    FROM ord WHERE cid IS NOT NULL GROUP BY cid
),
guest AS (
    SELECT customer_phone, MAX(customer_name)::text AS name, COUNT(*)::bigint AS n,
           SUM(total)::float8 AS spend, MAX(created_at)::timestamptz AS last_at, MIN(created_at)::timestamptz AS first_at
    FROM ord WHERE cid IS NULL AND TRIM(customer_phone) <> '' GROUP BY customer_phone
),
all_rows AS (
    SELECT c.id::uuid AS id, 'registered'::text AS kind, c.name::text AS name,
           COALESCE(c.phone, '')::text AS phone, COALESCE(c.email, '')::text AS email,
           c.is_blocked::bool AS is_blocked, c.last_login_at::timestamptz AS last_login_at,
           c.created_at::timestamptz AS created_at, COALESCE(r.n, 0)::bigint AS order_count,
           COALESCE(r.spend, 0)::float8 AS total_spend, r.last_at::timestamptz AS last_order_at
    FROM customers c LEFT JOIN reg r ON r.cid = c.id
    WHERE c.tenant_id = sqlc.arg(tenant_id)
    UNION ALL
    SELECT NULL::uuid, 'guest', g.name, g.customer_phone, '', FALSE, NULL::timestamptz,
           g.first_at, g.n, g.spend, g.last_at
    FROM guest g
)
SELECT a.*,
       COUNT(*) OVER ()::bigint AS total_count,
       (COUNT(*) FILTER (WHERE a.kind = 'registered') OVER ())::bigint AS registered_count
FROM all_rows a
WHERE sqlc.arg(q)::text = ''
   OR a.name ILIKE '%' || sqlc.arg(q)::text || '%'
   OR a.phone LIKE '%' || sqlc.arg(q)::text || '%'
   OR a.email ILIKE '%' || sqlc.arg(q)::text || '%'
ORDER BY COALESCE(a.last_order_at, a.created_at) DESC, a.phone
LIMIT sqlc.arg(limit_count) OFFSET sqlc.arg(offset_count);

-- name: CustomerOrderStats :one
SELECT COUNT(*)::bigint AS order_count,
       COALESCE(SUM(total), 0)::float8 AS total_spend,
       MIN(created_at)::timestamptz AS first_order_at,
       MAX(created_at)::timestamptz AS last_order_at
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (customer_id = sqlc.narg(customer_id)::uuid OR (sqlc.arg(phone)::text <> '' AND customer_phone = sqlc.arg(phone)::text));

-- name: InsertCustomerLoginEvent :exec
INSERT INTO customer_login_events (tenant_id, customer_id, identifier, method, result, ip, user_agent)
VALUES (sqlc.arg(tenant_id), sqlc.narg(customer_id), sqlc.arg(identifier), sqlc.arg(method), sqlc.arg(result), sqlc.arg(ip), sqlc.arg(user_agent));

-- Customer profile timeline: sign-ins, orders, bookings, stays and shop
-- actions, newest first. Empty kinds cost nothing for shops without them.
-- name: CustomerActivity :many
SELECT * FROM (
    SELECT 'login'::text AS kind, e.result::text AS status, e.method::text AS ref,
           0::float8 AS amount, (e.ip || ' ' || e.user_agent)::text AS detail, e.created_at::timestamptz AS at
    FROM customer_login_events e
    WHERE e.tenant_id = sqlc.arg(tenant_id) AND e.customer_id = sqlc.arg(customer_id)::uuid
    UNION ALL
    SELECT 'order', o.status, o.order_number::text, o.total::float8, o.order_type, o.created_at
    FROM orders o
    WHERE o.tenant_id = sqlc.arg(tenant_id)
      AND (o.customer_id = sqlc.arg(customer_id)::uuid OR (sqlc.arg(phone)::text <> '' AND o.customer_phone = sqlc.arg(phone)::text))
    UNION ALL
    SELECT 'appointment', a.status, COALESCE(s.name, ''), 0, a.scheduled_at::text, a.created_at
    FROM appointments a LEFT JOIN services s ON s.id = a.service_id
    WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.customer_id = sqlc.arg(customer_id)::uuid
    UNION ALL
    SELECT 'stay', rv.status, rv.check_in_date::text, 0, rv.check_out_date::text, rv.created_at
    FROM reservations rv
    WHERE rv.tenant_id = sqlc.arg(tenant_id) AND rv.customer_id = sqlc.arg(customer_id)::uuid
    UNION ALL
    SELECT 'admin', l.action, COALESCE(u.name, ''), 0, '', l.created_at
    FROM audit_logs l LEFT JOIN users u ON u.id = l.user_id
    WHERE l.tenant_id = sqlc.arg(tenant_id) AND l.entity_type = 'customer' AND l.entity_id = sqlc.arg(customer_id)::uuid
) t
WHERE t.at < sqlc.arg(before)::timestamptz
ORDER BY t.at DESC
LIMIT sqlc.arg(limit_count);
