-- name: ListPlans :many
SELECT * FROM plans
WHERE is_active = TRUE
ORDER BY price ASC;

-- name: GetPlanByID :one
SELECT * FROM plans
WHERE id = $1
LIMIT 1;

-- name: GetPlanByName :one
SELECT * FROM plans
WHERE name = $1
LIMIT 1;

-- name: CreateSubscription :one
INSERT INTO subscriptions (tenant_id, plan_id, status, start_at, end_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSubscriptionByTenant :one
SELECT * FROM subscriptions
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdateSubscriptionPlan :one
UPDATE subscriptions
SET plan_id = $2, status = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetPlanByNameAny :one
SELECT * FROM plans
WHERE name = sqlc.arg(name)
LIMIT 1;

-- name: CreatePlan :one
INSERT INTO plans (name, description, price, max_staff, max_products, features, is_active)
VALUES (sqlc.arg(name), sqlc.arg(description), sqlc.arg(price), sqlc.arg(max_staff), sqlc.arg(max_products), sqlc.arg(features), sqlc.arg(is_active))
RETURNING *;

-- name: UpdatePlan :one
UPDATE plans
SET
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    price = COALESCE(sqlc.narg(price), price),
    max_staff = COALESCE(sqlc.narg(max_staff), max_staff),
    max_products = COALESCE(sqlc.narg(max_products), max_products),
    -- features carries the commercial terms the plans table has no column for:
    -- billing period, trial length, the feature list and which business types
    -- the plan is offered to. NULL leaves the stored document untouched.
    features = COALESCE(sqlc.narg(features)::jsonb, features),
    is_active = COALESCE(sqlc.narg(is_active), is_active),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: CountTenantsOnPlan :one
SELECT COUNT(*)::bigint
FROM tenants
WHERE plan_id = sqlc.arg(plan_id);

-- name: ListAllPlans :many
SELECT * FROM plans
ORDER BY price ASC, name ASC;

-- Every plan with the number of tenants provisioned on it, so the console can
-- warn before deactivating one that businesses are still using.
-- name: ListPlansWithUsage :many
SELECT
    p.*,
    (SELECT COUNT(*) FROM tenants t WHERE t.plan_id = p.id)::bigint AS tenant_count,
    (SELECT COUNT(*) FROM subscriptions s WHERE s.plan_id = p.id AND s.status IN ('TRIAL', 'ACTIVE'))::bigint AS active_subscriptions
FROM plans p
ORDER BY p.price ASC, p.name ASC;
