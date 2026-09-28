-- name: AdminOrdersByDay :many
SELECT
    date_trunc('day', created_at)::date AS day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(total), 0)::text AS revenue
FROM orders
WHERE created_at >= now() - interval '30 days'
GROUP BY 1
ORDER BY 1 ASC;

-- name: AdminTenantsByWeek :many
SELECT
    date_trunc('week', created_at)::date AS week_start,
    COUNT(*)::bigint AS tenant_count
FROM tenants
WHERE created_at >= now() - interval '84 days'
GROUP BY 1
ORDER BY 1 ASC;

-- Every identity the platform knows about, tenant-scoped or not. The platform
-- owner (SUPER_ADMIN, tenant_id NULL) is included on purpose: IAM has to show
-- who can reach the console, and leaving the one account that can out of the
-- list made the page lie. last_activity is the most recent session issued to
-- the user, which is the closest thing to a sign-in time the schema records.
-- name: ListPlatformUsers :many
SELECT
    u.id,
    u.name,
    u.email,
    u.phone,
    u.role,
    u.status,
    u.must_set_password,
    u.created_at,
    u.tenant_id,
    t.name AS tenant_name,
    t.slug AS tenant_slug,
    t.status AS tenant_status,
    (SELECT MAX(rt.created_at) FROM refresh_tokens rt WHERE rt.user_id = u.id) AS last_activity
FROM users u
LEFT JOIN tenants t ON t.id = u.tenant_id
ORDER BY (u.tenant_id IS NOT NULL), u.created_at DESC;

-- name: ListSubscriptionsAdmin :many
SELECT
    s.id,
    s.tenant_id,
    s.plan_id,
    s.status,
    s.start_at,
    s.end_at,
    s.created_at,
    t.name AS tenant_name,
    p.name AS plan_name,
    p.price AS plan_price
FROM subscriptions s
JOIN tenants t ON t.id = s.tenant_id
JOIN plans p ON p.id = s.plan_id
ORDER BY s.created_at DESC;

-- name: ListAuditLogsEnriched :many
SELECT
    a.id,
    a.action,
    a.entity_type,
    a.entity_id,
    a.result,
    a.metadata,
    a.created_at,
    a.tenant_id,
    u.name AS actor_name,
    u.email AS actor_email,
    t.name AS tenant_name,
    t.slug AS tenant_slug
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
LEFT JOIN tenants t ON t.id = a.tenant_id
ORDER BY a.created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: ListAuditLogsEnrichedByTenant :many
SELECT
    a.id,
    a.action,
    a.entity_type,
    a.entity_id,
    a.result,
    a.metadata,
    a.created_at,
    a.tenant_id,
    u.name AS actor_name,
    u.email AS actor_email,
    t.name AS tenant_name,
    t.slug AS tenant_slug
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
LEFT JOIN tenants t ON t.id = a.tenant_id
WHERE a.tenant_id = sqlc.arg(tenant_id)
ORDER BY a.created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: TenantAdminMetrics :one
SELECT
    (SELECT COUNT(*)::bigint FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id)) AS total_orders,
    COALESCE((
        SELECT SUM(o.total) FROM orders o
        WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'COMPLETED'
    ), 0)::text AS total_revenue,
    (SELECT COUNT(*)::bigint FROM users u WHERE u.tenant_id = sqlc.arg(tenant_id) AND u.status = 'ACTIVE') AS active_users,
    (SELECT COUNT(*)::bigint FROM orders o
     WHERE o.tenant_id = sqlc.arg(tenant_id)
       AND o.created_at >= date_trunc('day', now())) AS orders_today,
    COALESCE((
        SELECT SUM(o.total) FROM orders o
        WHERE o.tenant_id = sqlc.arg(tenant_id)
          AND o.status <> 'CANCELLED'
          AND o.created_at >= date_trunc('day', now())
    ), 0)::text AS revenue_today,
    (SELECT COUNT(*)::bigint FROM orders o
     WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'CANCELLED') AS cancelled_orders,
    COALESCE((
        SELECT AVG(o.total) FROM orders o
        WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status = 'COMPLETED'
    ), 0)::text AS avg_order_value,
    (SELECT MIN(o.created_at) FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id)) AS first_order_at,
    (SELECT MAX(o.created_at) FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id)) AS last_order_at;

-- name: TenantOrderStatusBreakdown :many
SELECT
    o.status AS status,
    COUNT(*)::bigint AS count
FROM orders o
WHERE o.tenant_id = sqlc.arg(tenant_id)
GROUP BY o.status
ORDER BY 2 DESC;

-- name: TenantSecuritySummary :one
SELECT
    (SELECT COUNT(*)::bigint FROM users u WHERE u.tenant_id = sqlc.arg(tenant_id)) AS users_total,
    (SELECT COUNT(*)::bigint FROM users u
     WHERE u.tenant_id = sqlc.arg(tenant_id) AND u.status = 'ACTIVE') AS users_active,
    (SELECT COUNT(*)::bigint FROM users u
     WHERE u.tenant_id = sqlc.arg(tenant_id) AND u.must_set_password) AS users_pending_password,
    (SELECT COUNT(*)::bigint
     FROM refresh_tokens rt
     JOIN users u ON u.id = rt.user_id
     WHERE u.tenant_id = sqlc.arg(tenant_id) AND rt.expires_at > now()) AS active_sessions,
    (SELECT COUNT(*)::bigint FROM audit_logs a WHERE a.tenant_id = sqlc.arg(tenant_id)) AS audit_events,
    (SELECT COUNT(*)::bigint FROM audit_logs a
     WHERE a.tenant_id = sqlc.arg(tenant_id)
       AND a.created_at >= now() - interval '7 days') AS audit_events_7d;

-- name: TenantOrdersByDay :many
SELECT
    date_trunc('day', created_at)::date AS day,
    COUNT(*)::bigint AS order_count,
    COALESCE(SUM(CASE WHEN status <> 'CANCELLED' THEN total ELSE 0 END), 0)::text AS revenue
FROM orders
WHERE tenant_id = sqlc.arg(tenant_id)
    AND created_at >= now() - interval '30 days'
GROUP BY 1
ORDER BY 1 ASC;

-- name: ListAuditLogsByResult :many
SELECT
    a.id, a.action, a.entity_type, a.entity_id, a.result, a.metadata,
    a.created_at, a.tenant_id,
    u.name AS actor_name, u.email AS actor_email,
    t.name AS tenant_name, t.slug AS tenant_slug
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
LEFT JOIN tenants t ON t.id = a.tenant_id
WHERE a.result = sqlc.arg(result)
ORDER BY a.created_at DESC
LIMIT sqlc.arg(row_limit);

-- name: ListAuditLogsByTenantAndResult :many
SELECT
    a.id, a.action, a.entity_type, a.entity_id, a.result, a.metadata,
    a.created_at, a.tenant_id,
    u.name AS actor_name, u.email AS actor_email,
    t.name AS tenant_name, t.slug AS tenant_slug
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
LEFT JOIN tenants t ON t.id = a.tenant_id
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.result = sqlc.arg(result)
ORDER BY a.created_at DESC
LIMIT sqlc.arg(row_limit);

-- Subscriptions whose trial or term ends inside the window, newest deadline
-- first. The dashboard's "needs attention" list is built from this rather than
-- from a client-side scan of every subscription.
-- name: ListExpiringSubscriptions :many
SELECT
    s.id,
    s.tenant_id,
    t.name AS tenant_name,
    t.slug AS tenant_slug,
    p.name AS plan_name,
    s.status,
    s.end_at
FROM subscriptions s
JOIN tenants t ON t.id = s.tenant_id
JOIN plans p ON p.id = s.plan_id
WHERE s.end_at IS NOT NULL
  AND s.status IN ('TRIAL', 'ACTIVE')
  AND s.end_at <= now() + make_interval(days => sqlc.arg(within_days)::int)
ORDER BY s.end_at ASC
LIMIT 20;

-- Businesses whose administrator has never set a password. They are onboarded
-- but nobody can sign in, which is the single most common thing a Super Admin
-- needs to chase.
-- name: ListTenantsAwaitingSetup :many
SELECT
    t.id,
    t.name,
    t.slug,
    t.status,
    t.created_at
FROM tenants t
WHERE t.setup_status <> 'COMPLETED'
ORDER BY t.created_at DESC
LIMIT 20;

-- Everyone who can reach the platform console. Business users are excluded by
-- construction: a console identity has no tenant.
-- name: ListConsoleUsers :many
SELECT
    u.id,
    u.name,
    u.email,
    u.phone,
    u.role,
    u.status,
    u.must_set_password,
    u.created_at,
    (SELECT MAX(rt.created_at) FROM refresh_tokens rt WHERE rt.user_id = u.id) AS last_activity,
    (SELECT COUNT(*)::bigint FROM refresh_tokens rt
      WHERE rt.user_id = u.id AND rt.expires_at > now()) AS active_sessions
FROM users u
WHERE u.tenant_id IS NULL
ORDER BY
    CASE u.role WHEN 'SUPER_ADMIN' THEN 0 WHEN 'PLATFORM_ADMIN' THEN 1 ELSE 2 END,
    u.created_at ASC;

-- name: CreateConsoleUser :one
INSERT INTO users (tenant_id, name, email, phone, password_hash, role, status, must_set_password, invite_token_hash)
VALUES (NULL, sqlc.arg(name), sqlc.arg(email), sqlc.arg(phone), sqlc.arg(password_hash),
        sqlc.arg(role), 'ACTIVE', TRUE, sqlc.arg(invite_token_hash))
RETURNING *;

-- name: CountActivePlatformAdmins :one
SELECT COUNT(*)::bigint
FROM users
WHERE tenant_id IS NULL
  AND role IN ('SUPER_ADMIN', 'PLATFORM_ADMIN')
  AND status = 'ACTIVE';
