-- Inventory (Release V1 §3.6). Every query is tenant-scoped.

-- name: ListInventoryOverview :many
-- Every product with its stock row, if any. Untracked products come back with
-- a NULL item so the console can offer to start tracking them.
SELECT
    p.id            AS product_id,
    p.name          AS product_name,
    p.price         AS price,
    p.is_available  AS is_available,
    c.name          AS category_name,
    i.id            AS item_id,
    i.quantity      AS quantity,
    i.unit          AS unit,
    i.reorder_threshold AS reorder_threshold,
    i.track         AS track,
    i.auto_disabled AS auto_disabled,
    i.updated_at    AS updated_at
FROM products p
JOIN categories c ON c.id = p.category_id
LEFT JOIN inventory_items i ON i.product_id = p.id AND i.tenant_id = p.tenant_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
ORDER BY c.sort_order ASC, c.name ASC, p.sort_order ASC, p.name ASC;

-- name: GetInventoryItemByProduct :one
SELECT * FROM inventory_items
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id);

-- name: LockInventoryItemByProduct :one
-- Row lock for an adjustment: concurrent changes to one product queue up.
SELECT * FROM inventory_items
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id)
FOR UPDATE;

-- name: LockTrackedInventoryForProducts :many
-- Row locks for an order's tracked products, in a fixed order so two orders
-- touching the same products cannot deadlock.
SELECT i.*, p.name AS product_name
FROM inventory_items i
JOIN products p ON p.id = i.product_id
WHERE i.tenant_id = sqlc.arg(tenant_id)
  AND i.track = TRUE
  AND i.product_id = ANY (sqlc.arg(product_ids)::uuid[])
ORDER BY i.product_id
FOR UPDATE OF i;

-- name: PeekTrackedInventoryForProducts :many
-- Unlocked read for the checkout pre-check.
SELECT i.*, p.name AS product_name
FROM inventory_items i
JOIN products p ON p.id = i.product_id
WHERE i.tenant_id = sqlc.arg(tenant_id)
  AND i.track = TRUE
  AND i.product_id = ANY (sqlc.arg(product_ids)::uuid[]);

-- name: CreateInventoryItem :one
INSERT INTO inventory_items (tenant_id, product_id, quantity, unit, reorder_threshold, track)
VALUES (sqlc.arg(tenant_id), sqlc.arg(product_id), sqlc.arg(quantity), sqlc.arg(unit), sqlc.arg(reorder_threshold), TRUE)
RETURNING *;

-- name: UpdateInventorySettings :one
UPDATE inventory_items
SET unit = sqlc.arg(unit),
    reorder_threshold = sqlc.arg(reorder_threshold),
    track = sqlc.arg(track),
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: SetInventoryQuantity :one
UPDATE inventory_items
SET quantity = sqlc.arg(quantity), auto_disabled = sqlc.arg(auto_disabled), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: SetInventoryAutoDisabled :exec
UPDATE inventory_items SET auto_disabled = sqlc.arg(auto_disabled), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: InsertInventoryMovement :one
-- DO NOTHING on the (order, item, reason) unique index: a retried order
-- transition finds its movement already written and changes no stock.
INSERT INTO inventory_movements (tenant_id, item_id, delta, quantity_after, reason, order_id, note, actor)
VALUES (sqlc.arg(tenant_id), sqlc.arg(item_id), sqlc.arg(delta), sqlc.arg(quantity_after), sqlc.arg(reason), sqlc.arg(order_id), sqlc.arg(note), sqlc.arg(actor))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListInventoryMovements :many
SELECT m.*, o.order_number AS order_number
FROM inventory_movements m
LEFT JOIN orders o ON o.id = m.order_id
WHERE m.tenant_id = sqlc.arg(tenant_id) AND m.item_id = sqlc.arg(item_id)
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListOrderStockMovements :many
-- What an order took from stock (reason ORDER), for giving it back.
SELECT * FROM inventory_movements
WHERE tenant_id = sqlc.arg(tenant_id) AND order_id = sqlc.arg(order_id) AND reason = 'ORDER'
ORDER BY item_id;

-- name: LockInventoryItemByID :one
SELECT * FROM inventory_items
WHERE tenant_id = sqlc.arg(tenant_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: SetProductAvailable :exec
UPDATE products SET is_available = sqlc.arg(is_available), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: ListOrderLines :many
SELECT product_id, SUM(quantity)::int AS quantity
FROM order_items
WHERE order_id = sqlc.arg(order_id) AND tenant_id = sqlc.arg(tenant_id)
GROUP BY product_id
ORDER BY product_id;

-- name: HideProductForStock :execrows
-- Hides a product only if it was visible, so the caller learns whether stock
-- (not the owner) is the reason it is hidden.
UPDATE products SET is_available = FALSE, updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND is_available = TRUE;

-- name: ClearInventoryAutoDisabled :exec
-- The owner set availability by hand, so stock no longer owns that decision.
UPDATE inventory_items SET auto_disabled = FALSE, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id) AND product_id = sqlc.arg(product_id) AND auto_disabled;
