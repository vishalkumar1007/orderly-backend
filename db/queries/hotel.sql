-- Hotel capability group: room types, rooms, reservations, folios/charges and
-- housekeeping. Reservation and room status are two separate state machines
-- (POC requirement) — never write both from one statement.

-- name: CreateRoomType :one
INSERT INTO room_types (tenant_id, name, base_price, max_guests)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListRoomTypesByTenant :many
SELECT * FROM room_types WHERE tenant_id = $1 ORDER BY name ASC;

-- name: UpdateRoomType :one
UPDATE room_types SET
    name = COALESCE(sqlc.narg(name), name),
    base_price = COALESCE(sqlc.narg(base_price), base_price),
    max_guests = COALESCE(sqlc.narg(max_guests), max_guests)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DeleteRoomType :exec
DELETE FROM room_types WHERE id = $1 AND tenant_id = $2;

-- name: CreateRoom :one
INSERT INTO rooms (tenant_id, room_type_id, number, status)
VALUES ($1, $2, $3, 'AVAILABLE')
RETURNING *;

-- name: ListRoomsByTenant :many
SELECT * FROM rooms WHERE tenant_id = $1 ORDER BY number ASC;

-- name: GetRoomByID :one
SELECT * FROM rooms WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateRoomStatus :one
UPDATE rooms SET status = $3
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CreateReservation :one
INSERT INTO reservations (tenant_id, customer_id, room_type_id, check_in_date, check_out_date, guests, status)
VALUES ($1, $2, $3, $4, $5, $6, 'REQUESTED')
RETURNING *;

-- name: ListReservationsByTenant :many
SELECT * FROM reservations WHERE tenant_id = $1 ORDER BY check_in_date DESC LIMIT $2;

-- name: GetReservationByID :one
SELECT * FROM reservations WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateReservationStatus :one
UPDATE reservations SET status = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CheckInReservation :one
UPDATE reservations
SET status = 'CHECKED_IN', room_id = $3, checked_in_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CheckOutReservation :one
UPDATE reservations
SET status = 'CHECKED_OUT', checked_out_at = now(), updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CreateFolio :one
INSERT INTO folios (tenant_id, reservation_id, status)
VALUES ($1, $2, 'OPEN')
RETURNING *;

-- name: GetFolioByReservation :one
SELECT * FROM folios WHERE reservation_id = $1 AND tenant_id = $2 LIMIT 1;

-- name: SetFolioStatus :one
UPDATE folios SET status = $3
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CreateCharge :one
INSERT INTO charges (tenant_id, folio_id, description, amount)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListChargesByFolio :many
SELECT * FROM charges WHERE folio_id = $1 AND tenant_id = $2 ORDER BY created_at ASC;

-- name: FolioTotal :one
SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM charges WHERE folio_id = $1 AND tenant_id = $2;

-- name: CreateHousekeepingTask :one
INSERT INTO housekeeping_tasks (tenant_id, room_id, task_type, assigned_to)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListHousekeepingTasksByTenant :many
SELECT * FROM housekeeping_tasks WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: ListOpenHousekeepingTasks :many
SELECT * FROM housekeeping_tasks
WHERE tenant_id = $1 AND status != 'DONE'
ORDER BY created_at ASC;

-- name: UpdateHousekeepingTaskStatus :one
UPDATE housekeeping_tasks
SET status = $3, completed_at = CASE WHEN $3 = 'DONE' THEN now() ELSE completed_at END
WHERE id = $1 AND tenant_id = $2
RETURNING *;
