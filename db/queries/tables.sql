-- Cafe/Restaurant dine-in tables.

-- name: CreateTable :one
INSERT INTO tables (tenant_id, label, seats, status)
VALUES ($1, $2, $3, 'AVAILABLE')
RETURNING *;

-- name: ListTablesByTenant :many
SELECT * FROM tables WHERE tenant_id = $1 ORDER BY label ASC;

-- name: GetTableByID :one
SELECT * FROM tables WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateTable :one
UPDATE tables SET
    label = COALESCE(sqlc.narg(label), label),
    seats = COALESCE(sqlc.narg(seats), seats)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateTableStatus :one
UPDATE tables SET status = $3
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: DeleteTable :exec
DELETE FROM tables WHERE id = $1 AND tenant_id = $2;
