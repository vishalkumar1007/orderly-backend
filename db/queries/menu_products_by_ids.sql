-- Availability and price are both re-read here at order time. The client's
-- figures are never trusted and `is_available = TRUE` is part of the filter, so
-- a product taken off sale cannot sneak in through a stale cart.
-- name: GetProductsByIDs :many
SELECT * FROM products
WHERE tenant_id = sqlc.arg(tenant_id)
  AND id = ANY(sqlc.arg(ids)::uuid[])
  AND is_available = TRUE;
