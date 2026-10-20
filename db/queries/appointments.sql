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
-- The end time is fixed from the service's duration at booking time.
INSERT INTO appointments (tenant_id, customer_id, service_id, staff_id, scheduled_at, ends_at, status)
SELECT sqlc.arg(tenant_id)::uuid, sqlc.narg(customer_id)::uuid, s.id, sqlc.narg(staff_id)::uuid,
       sqlc.arg(scheduled_at)::timestamptz,
       sqlc.arg(scheduled_at)::timestamptz + make_interval(mins => s.duration_minutes), 'REQUESTED'
FROM services s
WHERE s.id = sqlc.arg(service_id)::uuid AND s.tenant_id = sqlc.arg(tenant_id)::uuid
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
WHERE id = $1 AND tenant_id = $2 AND status = sqlc.arg(expected_status)
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
WHERE id = $1 AND tenant_id = $2 AND status = sqlc.arg(expected_status)
RETURNING *;

-- Storefront booking. A customer books a service with a staff member at a
-- start time the slot engine offered; the booking transaction takes a
-- per-staff advisory lock and re-checks the slot before inserting.

-- name: ListActiveServices :many
SELECT * FROM services WHERE tenant_id = $1 AND is_active ORDER BY price ASC, name ASC;

-- name: ListBookableStaff :many
SELECT DISTINCT u.id, u.name
FROM staff_availability sa
JOIN users u ON u.id = sa.staff_id AND u.tenant_id = sa.tenant_id
WHERE sa.tenant_id = $1 AND u.status = 'ACTIVE'
ORDER BY u.name ASC;

-- name: ListAvailabilityForWeekday :many
SELECT * FROM staff_availability
WHERE tenant_id = $1 AND weekday = $2
ORDER BY staff_id, start_time;

-- name: ListStaffBusy :many
SELECT id, staff_id, scheduled_at, ends_at FROM appointments
WHERE tenant_id = sqlc.arg(tenant_id)
  AND staff_id IS NOT NULL
  AND status NOT IN ('CANCELLED', 'NO_SHOW')
  AND scheduled_at < sqlc.arg(until)
  AND ends_at > sqlc.arg(since);

-- name: LockStaffSchedule :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: CreateOnlineAppointment :one
INSERT INTO appointments (tenant_id, customer_id, service_id, staff_id, scheduled_at, ends_at,
                          status, source, client_token, notes)
SELECT sqlc.arg(tenant_id)::uuid, sqlc.arg(customer_id)::uuid, s.id, sqlc.arg(staff_id)::uuid,
       sqlc.arg(scheduled_at)::timestamptz,
       sqlc.arg(scheduled_at)::timestamptz + make_interval(mins => s.duration_minutes),
       'REQUESTED', 'ONLINE', sqlc.arg(client_token)::text, sqlc.arg(notes)::text
FROM services s
WHERE s.id = sqlc.arg(service_id)::uuid AND s.tenant_id = sqlc.arg(tenant_id)::uuid AND s.is_active
RETURNING *;

-- name: GetAppointmentByClientToken :one
SELECT * FROM appointments WHERE tenant_id = $1 AND client_token = $2 AND client_token <> '' LIMIT 1;

-- name: ListCustomerAppointments :many
SELECT a.*, s.name AS service_name, s.price AS service_price, s.duration_minutes,
       COALESCE(u.name, '') AS staff_name
FROM appointments a
JOIN services s ON s.id = a.service_id
LEFT JOIN users u ON u.id = a.staff_id
WHERE a.tenant_id = $1 AND a.customer_id = $2
ORDER BY a.scheduled_at DESC
LIMIT 50;

-- name: GetCustomerAppointment :one
SELECT a.*, s.name AS service_name, s.price AS service_price, s.duration_minutes,
       COALESCE(u.name, '') AS staff_name
FROM appointments a
JOIN services s ON s.id = a.service_id
LEFT JOIN users u ON u.id = a.staff_id
WHERE a.tenant_id = $1 AND a.id = $2;

-- name: RescheduleAppointment :one
UPDATE appointments a SET
    scheduled_at = sqlc.arg(scheduled_at)::timestamptz,
    ends_at = sqlc.arg(scheduled_at)::timestamptz + make_interval(mins => s.duration_minutes),
    staff_id = sqlc.arg(staff_id)::uuid,
    updated_at = now()
FROM services s
WHERE a.id = sqlc.arg(id) AND a.tenant_id = sqlc.arg(tenant_id)
  AND s.id = a.service_id
  AND a.status IN ('REQUESTED', 'CONFIRMED')
RETURNING a.*;
