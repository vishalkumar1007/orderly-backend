-- Allocating an order number must work for a brand new tenant, which has no
-- counter row yet. The insert/ignore seeds it at zero so the first order a shop
-- ever takes is numbered 1 rather than failing.
-- name: EnsureOrderCounter :exec
INSERT INTO order_counters (tenant_id, last_number)
VALUES ($1, 0)
ON CONFLICT (tenant_id) DO NOTHING;

-- name: NextOrderNumber :one
INSERT INTO order_counters (tenant_id, last_number)
VALUES ($1, 1)
ON CONFLICT (tenant_id) DO UPDATE
SET last_number = order_counters.last_number + 1
RETURNING last_number;

-- name: CreateOrder :one
INSERT INTO orders (
    tenant_id, customer_id, order_number, status, order_type,
    subtotal, tax, discount, packaging_fee, total, customer_name, customer_phone,
    customer_email, notes, estimated_ready_at, client_token, source
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
)
RETURNING *;

-- Idempotent lookup for a retried checkout submit. A double tap, a refresh or
-- a flaky network must never produce a second order.
-- name: GetOrderByClientToken :one
SELECT * FROM orders
WHERE tenant_id = $1 AND client_token = $2
LIMIT 1;

-- Guest tracking: an order number alone must not be enough, so the phone used
-- at checkout is part of the lookup.
-- name: GetOrderByTenantNumberAndPhone :one
SELECT * FROM orders
WHERE tenant_id = $1 AND order_number = $2 AND customer_phone = $3
LIMIT 1;

-- Guest order lookup from the orders page: a phone number returns every order
-- placed with it, newest first.
-- name: ListOrdersByTenantAndPhone :many
SELECT * FROM orders
WHERE tenant_id = sqlc.arg(tenant_id) AND customer_phone = sqlc.arg(customer_phone)
ORDER BY created_at DESC
LIMIT sqlc.arg(limit_count);

-- name: GetOrderByTenantAndNumber :one
SELECT * FROM orders
WHERE tenant_id = $1 AND order_number = $2
LIMIT 1;

-- name: CreateOrderItem :one
INSERT INTO order_items (
    tenant_id, order_id, product_id, product_name_snapshot,
    unit_price, quantity, subtotal, addons, notes
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: ListOrderItemsByOrder :many
SELECT * FROM order_items
WHERE order_id = $1
ORDER BY created_at ASC;

-- name: UpdateOrderStatus :one
UPDATE orders
SET status = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- Lifecycle timestamps. Each transition stamps only the column it owns, so the
-- tracking timeline can show when the order actually reached each stage.
-- name: StampOrderAccepted :one
UPDATE orders
SET status = 'ACCEPTED', accepted_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: StampOrderPreparing :one
UPDATE orders
SET status = 'PREPARING', preparing_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: StampOrderReady :one
UPDATE orders
SET status = 'READY', ready_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: StampOrderCompleted :one
UPDATE orders
SET status = 'COMPLETED', completed_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: StampOrderCancelled :one
UPDATE orders
SET status = 'CANCELLED', cancelled_at = now(), cancel_reason = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: AppendOrderStatusHistory :one
INSERT INTO order_status_history (tenant_id, order_id, from_status, to_status, actor)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListOrderStatusHistory :many
SELECT * FROM order_status_history
WHERE order_id = $1 AND tenant_id = $2
ORDER BY created_at ASC;

-- name: ListOrdersByTenant :many
SELECT * FROM orders
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: GetOrderByID :one
SELECT * FROM orders
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: ListOrderIDsByTenant :many
SELECT id FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY created_at DESC
LIMIT sqlc.arg(limit_count);

-- name: CreatePayment :one
INSERT INTO payments (
    tenant_id, order_id, amount, method, status, provider, provider_reference
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: GetPaymentByOrderID :one
SELECT * FROM payments
WHERE order_id = $1 AND tenant_id = $2
LIMIT 1;

-- name: GetPaymentByID :one
SELECT * FROM payments
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: SetPaymentPending :one
UPDATE payments
SET status = 'PENDING',
    method = $3,
    provider = $4,
    provider_reference = $5,
    failure_reason = '',
    attempt_count = attempt_count + 1,
    paid_at = NULL,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'PAID'
RETURNING *;

-- Guarded so a late gateway callback can never mark a captured payment unpaid.
-- name: MarkPaymentPaid :one
UPDATE payments
SET status = 'PAID',
    failure_reason = '',
    paid_at = now(),
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'PAID'
RETURNING *;

-- name: MarkPaymentFailed :one
UPDATE payments
SET status = 'FAILED',
    failure_reason = $3,
    attempt_count = attempt_count + 1,
    updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'PAID'
RETURNING *;

-- name: ConfirmPayment :one
UPDATE payments
SET status = 'PAID', paid_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: TenantDashboardStats :one
SELECT
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.created_at >= date_trunc('day', now())) AS orders_today,
    COALESCE((SELECT SUM(o.total) FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.created_at >= date_trunc('day', now()) AND o.status = 'COMPLETED'), 0)::text AS revenue_today,
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'PENDING') AS pending_orders,
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'PREPARING') AS preparing_orders,
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'READY') AS ready_orders,
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'COMPLETED' AND o.created_at >= date_trunc('day', now())) AS completed_today;

-- Order history for the tenant console. Unlike the Selling board — which shows
-- the live queue — this searches the whole record, so every filter is optional
-- and applied in SQL rather than by shipping the table to the browser.
--
-- sqlc.narg values are NULL when the console did not filter on them, and the
-- COALESCE pairs below turn that into "match everything".
-- name: ListOrderHistory :many
SELECT
    o.*,
    (SELECT COUNT(*)::bigint FROM order_items oi WHERE oi.order_id = o.id) AS item_count,
    -- Coalesced because the join is optional: an order with no payment row
    -- yields NULL, which the generated scanner would refuse for a column the
    -- payments table declares NOT NULL.
    COALESCE(p.status, '')::text AS payment_status,
    COALESCE(p.method, '')::text AS payment_method
FROM orders o
LEFT JOIN LATERAL (
    SELECT status, method FROM payments
    WHERE payments.order_id = o.id
    ORDER BY created_at DESC LIMIT 1
) p ON TRUE
WHERE o.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(status)::text IS NULL OR o.status = sqlc.narg(status)::text)
  AND (sqlc.narg(source)::text IS NULL OR o.source = sqlc.narg(source)::text)
  AND (sqlc.narg(from_date)::timestamptz IS NULL OR o.created_at >= sqlc.narg(from_date)::timestamptz)
  AND (sqlc.narg(to_date)::timestamptz IS NULL OR o.created_at < sqlc.narg(to_date)::timestamptz)
  AND (
        sqlc.narg(search)::text IS NULL
        OR o.customer_name ILIKE '%' || sqlc.narg(search)::text || '%'
        OR o.customer_phone ILIKE '%' || sqlc.narg(search)::text || '%'
        OR o.order_number::text = sqlc.narg(search)::text
      )
ORDER BY o.created_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- The totals that go with a filtered history page: how many rows match, and
-- what they are worth. Counting in SQL keeps the pager honest when the page
-- itself is only twenty rows.
-- name: SummarizeOrderHistory :one
SELECT
    COUNT(*)::bigint AS total_orders,
    COALESCE(SUM(CASE WHEN o.status <> 'CANCELLED' THEN o.total ELSE 0 END), 0)::text AS total_revenue,
    COUNT(*) FILTER (WHERE o.status = 'COMPLETED')::bigint AS completed,
    COUNT(*) FILTER (WHERE o.status = 'CANCELLED')::bigint AS cancelled
FROM orders o
WHERE o.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(status)::text IS NULL OR o.status = sqlc.narg(status)::text)
  AND (sqlc.narg(source)::text IS NULL OR o.source = sqlc.narg(source)::text)
  AND (sqlc.narg(from_date)::timestamptz IS NULL OR o.created_at >= sqlc.narg(from_date)::timestamptz)
  AND (sqlc.narg(to_date)::timestamptz IS NULL OR o.created_at < sqlc.narg(to_date)::timestamptz)
  AND (
        sqlc.narg(search)::text IS NULL
        OR o.customer_name ILIKE '%' || sqlc.narg(search)::text || '%'
        OR o.customer_phone ILIKE '%' || sqlc.narg(search)::text || '%'
        OR o.order_number::text = sqlc.narg(search)::text
      );

-- The business activity feed: what happened in the shop, as opposed to who
-- changed its configuration (that is the audit log).
--
-- Every row is an order moving through the workflow, joined back to the order
-- so the feed can name it without a second query per row.
-- name: ListTenantActivity :many
SELECT
    h.id,
    h.order_id,
    h.from_status,
    h.to_status,
    h.actor,
    h.created_at,
    o.order_number,
    o.customer_name,
    o.customer_phone,
    o.total,
    o.source
FROM order_status_history h
JOIN orders o ON o.id = h.order_id
WHERE h.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(to_status)::text IS NULL OR h.to_status = sqlc.narg(to_status)::text)
ORDER BY h.created_at DESC
LIMIT sqlc.arg(row_limit);
