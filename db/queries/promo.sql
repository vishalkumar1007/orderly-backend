-- Offers (promotions) and loyalty. Every query is tenant-scoped. Redemption
-- and earning run inside the order transaction (callers pass a tx-bound
-- Queries), so money and points move together or not at all.

/* ---------- promotions ---------- */

-- name: ListPromotions :many
SELECT p.*,
       COALESCE(r.redemptions, 0)::bigint AS redemptions,
       COALESCE(r.discount_total, 0)::numeric AS discount_total
FROM promotions p
LEFT JOIN (
    SELECT promotion_id, COUNT(*) AS redemptions, SUM(discount) AS discount_total
    FROM promotion_redemptions GROUP BY promotion_id
) r ON r.promotion_id = p.id
WHERE p.tenant_id = sqlc.arg(tenant_id)
ORDER BY p.created_at DESC;

-- name: GetPromotion :one
SELECT * FROM promotions WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: CreatePromotion :one
INSERT INTO promotions (tenant_id, code, name, kind, value, min_subtotal, max_discount, starts_at, ends_at,
                        max_redemptions, per_customer_limit, first_order_only, active)
VALUES (sqlc.arg(tenant_id), sqlc.arg(code), sqlc.arg(name), sqlc.arg(kind), sqlc.arg(value), sqlc.arg(min_subtotal),
        sqlc.arg(max_discount), sqlc.arg(starts_at), sqlc.arg(ends_at), sqlc.arg(max_redemptions),
        sqlc.arg(per_customer_limit), sqlc.arg(first_order_only), sqlc.arg(active))
RETURNING *;

-- name: UpdatePromotion :one
UPDATE promotions
SET name = sqlc.arg(name), kind = sqlc.arg(kind), value = sqlc.arg(value), min_subtotal = sqlc.arg(min_subtotal),
    max_discount = sqlc.arg(max_discount), starts_at = sqlc.arg(starts_at), ends_at = sqlc.arg(ends_at),
    max_redemptions = sqlc.arg(max_redemptions), per_customer_limit = sqlc.arg(per_customer_limit),
    first_order_only = sqlc.arg(first_order_only), active = sqlc.arg(active), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DeletePromotion :execrows
DELETE FROM promotions WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: LockPromotionByCode :one
-- Row lock for a redemption: two checkouts racing for the last use queue up.
SELECT * FROM promotions
WHERE tenant_id = sqlc.arg(tenant_id) AND code = sqlc.arg(code)
FOR UPDATE;

-- name: GetPromotionByCode :one
SELECT * FROM promotions WHERE tenant_id = sqlc.arg(tenant_id) AND code = sqlc.arg(code);

-- name: CountPromotionRedemptions :one
SELECT COUNT(*)::bigint FROM promotion_redemptions WHERE promotion_id = sqlc.arg(promotion_id);

-- name: CountPromotionRedemptionsByPhone :one
-- Per-customer limits are keyed by phone so a guest cannot dodge them by
-- not signing in.
SELECT COUNT(*)::bigint FROM promotion_redemptions
WHERE promotion_id = sqlc.arg(promotion_id) AND phone = sqlc.arg(phone);

-- name: CountOrdersByPhone :one
SELECT COUNT(*)::bigint FROM orders
WHERE tenant_id = sqlc.arg(tenant_id) AND customer_phone = sqlc.arg(phone) AND status <> 'CANCELLED';

-- name: InsertPromotionRedemption :exec
INSERT INTO promotion_redemptions (tenant_id, promotion_id, order_id, customer_id, phone, discount)
VALUES (sqlc.arg(tenant_id), sqlc.arg(promotion_id), sqlc.arg(order_id), sqlc.arg(customer_id), sqlc.arg(phone), sqlc.arg(discount))
ON CONFLICT (order_id) DO NOTHING;

/* ---------- loyalty ---------- */

-- name: GetLoyaltyProgram :one
SELECT * FROM loyalty_programs WHERE tenant_id = sqlc.arg(tenant_id);

-- name: UpsertLoyaltyProgram :one
INSERT INTO loyalty_programs (tenant_id, enabled, points_per_100, point_value, min_redeem_points, max_redeem_percent)
VALUES (sqlc.arg(tenant_id), sqlc.arg(enabled), sqlc.arg(points_per_100), sqlc.arg(point_value), sqlc.arg(min_redeem_points), sqlc.arg(max_redeem_percent))
ON CONFLICT (tenant_id) DO UPDATE SET
    enabled = EXCLUDED.enabled, points_per_100 = EXCLUDED.points_per_100, point_value = EXCLUDED.point_value,
    min_redeem_points = EXCLUDED.min_redeem_points, max_redeem_percent = EXCLUDED.max_redeem_percent, updated_at = now()
RETURNING *;

-- name: LoyaltySummary :one
SELECT
    COUNT(*)::bigint AS members,
    COALESCE(SUM(balance), 0)::bigint AS outstanding_points,
    COALESCE(SUM(lifetime_earned), 0)::bigint AS lifetime_points
FROM loyalty_accounts WHERE tenant_id = sqlc.arg(tenant_id);

-- name: GetLoyaltyAccount :one
SELECT * FROM loyalty_accounts WHERE tenant_id = sqlc.arg(tenant_id) AND customer_id = sqlc.arg(customer_id);

-- name: LockLoyaltyAccount :one
INSERT INTO loyalty_accounts (tenant_id, customer_id)
VALUES (sqlc.arg(tenant_id), sqlc.arg(customer_id))
ON CONFLICT (tenant_id, customer_id) DO UPDATE SET updated_at = loyalty_accounts.updated_at
RETURNING *;

-- name: ApplyLoyaltyDelta :one
UPDATE loyalty_accounts
SET balance = balance + sqlc.arg(delta),
    lifetime_earned = lifetime_earned + GREATEST(sqlc.arg(delta), 0),
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: InsertLoyaltyTransaction :execrows
-- The partial unique index makes EARN/REDEEM for one order idempotent; zero
-- rows means "already recorded" and the caller must not move the balance.
INSERT INTO loyalty_transactions (tenant_id, account_id, kind, points, order_id, note)
VALUES (sqlc.arg(tenant_id), sqlc.arg(account_id), sqlc.arg(kind), sqlc.arg(points), sqlc.arg(order_id), sqlc.arg(note))
ON CONFLICT DO NOTHING;

-- name: ListLoyaltyTransactions :many
SELECT t.*, o.order_number
FROM loyalty_transactions t
LEFT JOIN orders o ON o.id = t.order_id
WHERE t.account_id = sqlc.arg(account_id) AND t.tenant_id = sqlc.arg(tenant_id)
ORDER BY t.created_at DESC
LIMIT sqlc.arg(limit_count);

-- name: ListLoyaltyMembers :many
SELECT a.*, c.name AS customer_name, c.phone AS customer_phone
FROM loyalty_accounts a
JOIN customers c ON c.id = a.customer_id
WHERE a.tenant_id = sqlc.arg(tenant_id)
ORDER BY a.balance DESC, a.updated_at DESC
LIMIT sqlc.arg(limit_count);
