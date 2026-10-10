-- name: CreateTenant :one
INSERT INTO tenants (
    name, slug, business_type, owner_name, phone, email, address, status, is_published, plan_id, setup_status,
    theme_preset_id, theme_color_mode, theme_overrides,
    logo_url, favicon_url, short_description, currency, timezone, language, store_status, status_message,
    mfa_allowed
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, FALSE, $9, $10, $11, $12, $13,
    $14, $15, $16, $17, $18, $19, $20, $21, $22
)
RETURNING *;

-- name: GetTenantByID :one
SELECT * FROM tenants
WHERE id = $1
LIMIT 1;

-- name: GetTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1
LIMIT 1;

-- name: ListTenants :many
SELECT * FROM tenants
ORDER BY created_at DESC;

-- name: UpdateTenant :one
UPDATE tenants
SET
    name = COALESCE(sqlc.narg(name), name),
    business_type = COALESCE(sqlc.narg(business_type), business_type),
    owner_name = COALESCE(sqlc.narg(owner_name), owner_name),
    phone = COALESCE(sqlc.narg(phone), phone),
    email = COALESCE(sqlc.narg(email), email),
    address = COALESCE(sqlc.narg(address), address),
    logo_url = COALESCE(sqlc.narg(logo_url), logo_url),
    favicon_url = COALESCE(sqlc.narg(favicon_url), favicon_url),
    short_description = COALESCE(sqlc.narg(short_description), short_description),
    currency = COALESCE(sqlc.narg(currency), currency),
    timezone = COALESCE(sqlc.narg(timezone), timezone),
    language = COALESCE(sqlc.narg(language), language),
    store_status = COALESCE(sqlc.narg(store_status), store_status),
    status_message = COALESCE(sqlc.narg(status_message), status_message),
    mfa_allowed = COALESCE(sqlc.narg(mfa_allowed), mfa_allowed),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetTenantStatus :one
UPDATE tenants
SET status = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetTenantPublished :one
UPDATE tenants
SET is_published = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetTenantSetupStatus :one
UPDATE tenants
SET setup_status = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: AdminDashboardStats :one
SELECT
    (SELECT COUNT(*)::bigint FROM tenants) AS total_tenants,
    (SELECT COUNT(*)::bigint FROM tenants WHERE status = 'ACTIVE') AS active_tenants,
    (SELECT COUNT(*)::bigint FROM tenants WHERE status = 'SUSPENDED') AS suspended_tenants,
    (SELECT COUNT(*)::bigint FROM subscriptions WHERE status = 'TRIAL') AS trial_tenants,
    (SELECT COUNT(*)::bigint FROM tenants WHERE setup_status <> 'COMPLETED') AS pending_setup,
    (SELECT COUNT(*)::bigint FROM orders) AS total_orders,
    COALESCE((SELECT SUM(total) FROM orders WHERE status = 'COMPLETED'), 0)::text AS total_revenue,
    (SELECT COUNT(*)::bigint FROM orders WHERE created_at >= date_trunc('day', now())) AS orders_today,
    COALESCE((SELECT SUM(total) FROM orders WHERE created_at >= date_trunc('day', now())), 0)::text AS order_value_today,
    (SELECT COUNT(*)::bigint FROM users WHERE tenant_id IS NOT NULL AND status = 'ACTIVE') AS active_users;

-- name: ListTenantsWithPlans :many
SELECT
    sqlc.embed(t),
    p.name AS plan_name,
    p.price AS plan_price,
    tp.name AS theme_name,
    tp.tokens AS theme_tokens
FROM tenants t
LEFT JOIN plans p ON p.id = t.plan_id
LEFT JOIN theme_presets tp ON tp.id = t.theme_preset_id
ORDER BY t.created_at DESC;

-- name: GetTenantWithPlanByID :one
SELECT
    sqlc.embed(t),
    p.name AS plan_name,
    p.price AS plan_price,
    tp.name AS theme_name,
    tp.tokens AS theme_tokens
FROM tenants t
LEFT JOIN plans p ON p.id = t.plan_id
LEFT JOIN theme_presets tp ON tp.id = t.theme_preset_id
WHERE t.id = $1
LIMIT 1;

-- name: UpdateTenantPlanID :one
UPDATE tenants
SET plan_id = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetTenantTheme :one
UPDATE tenants
SET
    theme_preset_id = $2,
    theme_color_mode = $3,
    theme_overrides = $4,
    updated_at = now()
WHERE id = $1
RETURNING *;
