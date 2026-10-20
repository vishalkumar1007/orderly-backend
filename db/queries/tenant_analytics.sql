-- Analytics for the tenant-facing dashboard. Every query is scoped to a single
-- tenant so the handler can never accidentally return another shop's numbers.

-- name: TenantOrdersByDayRange :many
-- Orders and revenue per day over a rolling window. Cancelled orders are
-- excluded from revenue but still counted as orders, which is what an operator
-- wants to see when judging how busy they were.
SELECT
    (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(CASE WHEN status <> 'CANCELLED' AND NOT order_excluded_from_revenue(id) THEN total ELSE 0 END), 0)::text AS revenue
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

-- name: TenantTopProductsByRevenue :many
-- The same best-seller list ranked by the money it made rather than by how many
-- were sold: a ₹400 dish sold twice outranks a ₹10 snack sold ten times.
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
ORDER BY SUM(oi.quantity * oi.unit_price) DESC, SUM(oi.quantity) DESC, p.name ASC
LIMIT sqlc.arg(row_limit);

-- name: TenantOrdersByHour :many
-- Orders by hour of day on the shop's own clock, averaged over the window in
-- Go, so a single quiet Tuesday does not read as a permanently quiet hour.
-- Revenue travels with the count so the rhythm chart can show what an hour is
-- worth, not just how busy it was.
SELECT
    EXTRACT(HOUR FROM created_at AT TIME ZONE sqlc.arg(tz)::text)::int AS hour_of_day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(CASE WHEN status <> 'CANCELLED' AND NOT order_excluded_from_revenue(id) THEN total ELSE 0 END), 0)::text AS revenue
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND created_at >= now() - sqlc.arg(days)::interval
GROUP BY 1
ORDER BY 1 ASC;

-- name: TenantOrdersByDayPrevious :many
-- The window immediately before the selected one, bucketed by the same shop
-- clock. The dashboard pairs these points with the current window by index so
-- "up on last period" is drawn from real data rather than inferred.
SELECT
    (created_at AT TIME ZONE sqlc.arg(tz)::text)::date AS day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(CASE WHEN status <> 'CANCELLED' AND NOT order_excluded_from_revenue(id) THEN total ELSE 0 END), 0)::text AS revenue
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND created_at >= now() - sqlc.arg(days)::interval - sqlc.arg(back)::interval
    AND created_at < now() - sqlc.arg(back)::interval
GROUP BY 1
ORDER BY 1 ASC;

-- name: TenantCustomerSummary :one
-- Who the window's orders belonged to. Only identified customers count: a
-- takeaway order with no customer attached says nothing about loyalty.
WITH active AS (
    SELECT o.customer_id, COUNT(*)::bigint AS order_count
    FROM orders o
    WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.customer_id IS NOT NULL
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - sqlc.arg(days)::interval
    GROUP BY o.customer_id
)
SELECT
    (SELECT COUNT(*)::bigint FROM customers c
      WHERE c.tenant_id = sqlc.arg(tenant_id)) AS total,
    -- New and returning split the customers who ordered in the window, so the
    -- two always add up to ActiveCustomers.
    (SELECT COUNT(*)::bigint FROM active a
      JOIN customers c ON c.id = a.customer_id
      WHERE c.created_at >= now() - sqlc.arg(days)::interval) AS new_customers,
    (SELECT COUNT(*)::bigint FROM active a
      JOIN customers c ON c.id = a.customer_id
      WHERE c.created_at < now() - sqlc.arg(days)::interval) AS returning_customers,
    (SELECT COUNT(*)::bigint FROM active) AS active_customers,
    (SELECT COUNT(*)::bigint FROM active WHERE order_count > 1) AS repeat_customers;

-- name: TenantPeriodSummary :one
-- Headline figures plus the previous equivalent period, so the UI can show a
-- real trend rather than a bare number.
SELECT
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.created_at >= date_trunc('day', now())) AS orders_today,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND NOT order_excluded_from_revenue(o.id)
        AND o.status = 'COMPLETED'
        AND o.created_at >= date_trunc('day', now()))::text AS revenue_today,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '7 days') AS orders_7d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND NOT order_excluded_from_revenue(o.id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '7 days')::text AS revenue_7d,
    (SELECT COUNT(*)::bigint FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND o.status <> 'CANCELLED'
        AND o.created_at >= now() - interval '30 days') AS orders_30d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o
      WHERE o.tenant_id = sqlc.arg(tenant_id)
        AND NOT order_excluded_from_revenue(o.id)
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
        AND NOT order_excluded_from_revenue(o.id)
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
    CASE WHEN p.is_test THEN 'ONLINE_TEST'
         ELSE COALESCE(NULLIF(p.method, ''), 'UNRECORDED') END::text AS method,
    COALESCE(NULLIF(p.status, ''), 'NONE')::text AS status,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(o.total), 0)::text AS revenue
FROM orders o
LEFT JOIN LATERAL (
    SELECT method, status,
           (method = 'ONLINE' AND mode = 'SANDBOX' AND provider_order_id <> '') AS is_test
    FROM payments
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

-- name: TenantOrdersByWeekdayHour :many
-- Orders by weekday (0 = Monday) and hour on the shop's own clock: the grid
-- behind "when is the rush". Totals, not averages, so one order reads as one.
SELECT
    (EXTRACT(ISODOW FROM created_at AT TIME ZONE sqlc.arg(tz)::text)::int - 1) AS weekday,
    EXTRACT(HOUR FROM created_at AT TIME ZONE sqlc.arg(tz)::text)::int AS hour_of_day,
    COUNT(*)::bigint AS order_count
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND status <> 'CANCELLED'
    AND created_at >= now() - sqlc.arg(days)::interval
GROUP BY 1, 2
ORDER BY 1, 2;
