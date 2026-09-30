-- Barber Shop capability group: services, staff availability, appointments,
-- and the shared walk-in/checked-in queue. Every statement is tenant-scoped.

-- name: CreateService :one
INSERT INTO services (tenant_id, name, duration_minutes, price, is_active)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListServicesByTenant :many
SELECT * FROM services WHERE tenant_id = $1 ORDER BY name ASC;

-- name: GetServiceByID :one
SELECT * FROM services WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateService :one
UPDATE services SET
    name = COALESCE(sqlc.narg(name), name),
    duration_minutes = COALESCE(sqlc.narg(duration_minutes), duration_minutes),
    price = COALESCE(sqlc.narg(price), price),
    is_active = COALESCE(sqlc.narg(is_active), is_active),
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DeleteService :exec
DELETE FROM services WHERE id = $1 AND tenant_id = $2;

-- name: CreateStaffAvailability :one
INSERT INTO staff_availability (tenant_id, staff_id, weekday, start_time, end_time)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListStaffAvailabilityByTenant :many
SELECT * FROM staff_availability WHERE tenant_id = $1 ORDER BY staff_id, weekday;

-- name: DeleteStaffAvailability :exec
DELETE FROM staff_availability WHERE id = $1 AND tenant_id = $2;

-- name: CreateAppointment :one
INSERT INTO appointments (tenant_id, customer_id, service_id, staff_id, scheduled_at, status)
VALUES ($1, $2, $3, $4, $5, 'REQUESTED')
RETURNING *;

-- name: ListAppointmentsByTenant :many
SELECT * FROM appointments WHERE tenant_id = $1 ORDER BY scheduled_at DESC LIMIT $2;

-- name: ListUpcomingAppointments :many
SELECT * FROM appointments
WHERE tenant_id = $1 AND status NOT IN ('COMPLETED', 'CANCELLED', 'NO_SHOW')
ORDER BY scheduled_at ASC;

-- name: GetAppointmentByID :one
SELECT * FROM appointments WHERE id = $1 AND tenant_id = $2 LIMIT 1;

-- name: UpdateAppointmentStatus :one
UPDATE appointments SET status = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: CreateQueueEntry :one
INSERT INTO queue_entries (tenant_id, appointment_id, customer_id, queue_date, queue_number, status)
VALUES ($1, $2, $3, CURRENT_DATE, $4, 'WAITING')
RETURNING *;

-- name: NextQueueNumberToday :one
SELECT COALESCE(MAX(queue_number), 0) + 1 AS next_number
FROM queue_entries
WHERE tenant_id = $1 AND queue_date = CURRENT_DATE;

-- name: ListTodayQueue :many
SELECT * FROM queue_entries
WHERE tenant_id = $1 AND queue_date = CURRENT_DATE
ORDER BY queue_number ASC;

-- name: UpdateQueueEntryStatus :one
UPDATE queue_entries SET status = $3
WHERE id = $1 AND tenant_id = $2
RETURNING *;
