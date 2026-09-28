-- Analytics for the tenant-facing dashboard. Every query is scoped to a single
-- tenant so the handler can never accidentally return another shop's numbers.

-- name: TenantOrdersByDayRange :many
-- Orders and revenue per day over a rolling window. Cancelled orders are
-- excluded from revenue but still counted as orders, which is what an operator
-- wants to see when judging how busy they were.
SELECT
    date_trunc('day', created_at)::date AS day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(CASE WHEN status <> 'CANCELLED' THEN total ELSE 0 END), 0)::text AS revenue
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND created_at >= now() - sqlc.arg(days)::interval
GROUP BY 1
ORDER BY 1 ASC;

-- name: TenantTopProducts :many
-- Best sellers by units, with the revenue they produced.
SELECT
    p.name AS name,
    COALESCE(SUM(oi.quantity), 0)::bigint AS units,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::text AS revenue
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
JOIN products p ON p.id = oi.product_id
WHERE o.tenant_id = sqlc.arg(tenant_id)
    AND o.status <> 'CANCELLED'
    AND o.created_at >= now() - sqlc.arg(days)::interval
GROUP BY p.id, p.name
ORDER BY units DESC, revenue DESC
LIMIT sqlc.arg(row_limit);

-- name: TenantOrdersByHour :many
-- Orders by hour of day, averaged over the window, so a single quiet Tuesday
-- does not read as a permanently quiet hour.
SELECT
    EXTRACT(HOUR FROM created_at)::int AS hour_of_day,
    COUNT(*)::bigint AS order_count
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND created_at >= now() - sqlc.arg(days)::interval
GROUP BY 1
ORDER BY 1 ASC;

-- name: TenantPeriodSummary :one
-- Headline figures plus the previous equivalent period, so the UI can show a
-- real trend rather than a bare number.
SELECT
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.created_at >= date_trunc('day', now())) AS orders_today,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status = 'COMPLETED'
        AND o.created_at >= date_trunc('day', now()))::text AS revenue_today,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '7 days') AS orders_7d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '7 days')::text AS revenue_7d,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '30 days') AS orders_30d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '30 days')::text AS revenue_30d,
    -- The 30 days before that, for the trend arrow.
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '60 days'
        AND o.created_at < now() - interval '30 days') AS orders_prev_30d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '60 days'
        AND o.created_at < now() - interval '30 days')::text AS revenue_prev_30d,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status = 'PENDING') AS pending_orders,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status = 'PREPARING') AS preparing_orders,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status = 'READY') AS ready_orders,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status = 'COMPLETED'
        AND o.created_at >= date_trunc('day', now())) AS completed_today,
    (SELECT COALESCE(AVG(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '30 days')::text AS avg_order_value_30d,
    (SELECT COUNT(*)::bigint FROM products p
      WHERE p.tenant_id = sqlc.arg(tenant_id) AND p.is_available) AS products_available,
    (SELECT COUNT(*)::bigint FROM products p
      WHERE p.tenant_id = sqlc.arg(tenant_id) AND NOT p.is_available) AS products_unavailable;

-- Operational quality over the window: how long the kitchen takes, and how
-- often an order does not make it. These are the numbers an owner manages
-- against, and none of them can be derived from order counts alone.
-- name: TenantOperationsSummary :one
SELECT
    -- Median rather than mean: one forgotten ticket left open for an hour
    -- would otherwise move the average enough to hide a real change.
    COALESCE(
        percentile_cont(0.5) WITHIN GROUP (
            ORDER BY EXTRACT(EPOCH FROM (ready_at - accepted_at)) / 60.0
        ) FILTER (WHERE accepted_at IS NOT NULL AND ready_at IS NOT NULL),
        0
    )::float AS median_prep_minutes,
    COALESCE(
        AVG(EXTRACT(EPOCH FROM (ready_at - accepted_at)) / 60.0)
            FILTER (WHERE accepted_at IS NOT NULL AND ready_at IS NOT NULL),
        0
    )::float AS avg_prep_minutes,
    COALESCE(
        AVG(EXTRACT(EPOCH FROM (accepted_at - created_at)) / 60.0)
            FILTER (WHERE accepted_at IS NOT NULL),
        0
    )::float AS avg_accept_minutes,
    COUNT(*) FILTER (WHERE status = 'COMPLETED')::bigint AS completed,
    COUNT(*) FILTER (WHERE status = 'CANCELLED')::bigint AS cancelled,
    COUNT(*)::bigint AS total,
    COUNT(*) FILTER (WHERE status = 'CANCELLED' AND created_at >= date_trunc('day', now()))::bigint AS cancelled_today
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
  AND created_at >= now() - sqlc.arg(days)::interval;

-- How customers actually pay, so an owner can see whether a method is worth
-- keeping. Orders with no payment row are grouped as unrecorded rather than
-- dropped, because a missing payment is itself worth seeing.
-- name: TenantPaymentBreakdown :many
SELECT
    COALESCE(NULLIF(p.method, ''), 'UNRECORDED')::text AS method,
    COALESCE(NULLIF(p.status, ''), 'NONE')::text AS status,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(o.total), 0)::text AS revenue
FROM orders o
LEFT JOIN LATERAL (
    SELECT method, status FROM payments
    WHERE payments.order_id = o.id
    ORDER BY created_at DESC LIMIT 1
) p ON TRUE
WHERE o.tenant_id = sqlc.arg(tenant_id)
  AND o.status <> 'CANCELLED'
  AND o.created_at >= now() - sqlc.arg(days)::interval
GROUP BY 1, 2
ORDER BY order_count DESC;

-- Best-selling categories. The menu is organised by category, so this is the
-- view that tells an owner which part of it is carrying the shop.
-- name: TenantTopCategories :many
SELECT
    c.name AS name,
    COALESCE(SUM(oi.quantity), 0)::bigint AS units,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::text AS revenue
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
JOIN products p ON p.id = oi.product_id
JOIN categories c ON c.id = p.category_id
WHERE o.tenant_id = sqlc.arg(tenant_id)
  AND o.status <> 'CANCELLED'
  AND o.created_at >= now() - sqlc.arg(days)::interval
GROUP BY c.id, c.name
ORDER BY units DESC, revenue DESC
LIMIT sqlc.arg(row_limit);
