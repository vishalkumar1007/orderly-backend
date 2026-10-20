-- Staff attendance (punch in / out). Every query is tenant-scoped.

-- name: GetAttendanceSettings :one
SELECT * FROM tenant_attendance_settings WHERE tenant_id = $1;

-- name: UpsertAttendanceSettings :one
INSERT INTO tenant_attendance_settings (tenant_id, enabled, require_location, geo_lat, geo_lng, geo_radius_m)
VALUES (sqlc.arg(tenant_id), sqlc.arg(enabled), sqlc.arg(require_location), sqlc.narg(geo_lat), sqlc.narg(geo_lng), sqlc.arg(geo_radius_m))
ON CONFLICT (tenant_id) DO UPDATE SET
    enabled = EXCLUDED.enabled,
    require_location = EXCLUDED.require_location,
    geo_lat = EXCLUDED.geo_lat,
    geo_lng = EXCLUDED.geo_lng,
    geo_radius_m = EXCLUDED.geo_radius_m,
    updated_at = now()
RETURNING *;

-- name: GetOpenPunch :one
SELECT * FROM staff_attendance
WHERE tenant_id = $1 AND user_id = $2 AND clock_out_at IS NULL;

-- name: PunchIn :one
INSERT INTO staff_attendance (tenant_id, user_id, in_lat, in_lng, in_distance_m, note)
VALUES (sqlc.arg(tenant_id), sqlc.arg(user_id), sqlc.narg(lat), sqlc.narg(lng), sqlc.narg(distance_m), sqlc.arg(note))
RETURNING *;

-- name: PunchOut :one
UPDATE staff_attendance
SET clock_out_at = now(), out_lat = sqlc.narg(lat), out_lng = sqlc.narg(lng),
    out_distance_m = sqlc.narg(distance_m),
    note = CASE WHEN sqlc.arg(note)::text = '' THEN note ELSE sqlc.arg(note)::text END
WHERE tenant_id = sqlc.arg(tenant_id) AND user_id = sqlc.arg(user_id) AND clock_out_at IS NULL
RETURNING *;

-- name: WorkedSeconds :one
-- Time on shift between two instants; an open shift counts up to now.
SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (
    LEAST(COALESCE(clock_out_at, now()), sqlc.arg(to_at)::timestamptz)
    - GREATEST(clock_in_at, sqlc.arg(from_at)::timestamptz)
))), 0)::float8
FROM staff_attendance
WHERE tenant_id = sqlc.arg(tenant_id) AND user_id = sqlc.arg(user_id)
  AND clock_in_at < sqlc.arg(to_at)::timestamptz
  AND COALESCE(clock_out_at, now()) > sqlc.arg(from_at)::timestamptz;

-- name: ListAttendance :many
SELECT a.*, u.name AS user_name, u.role AS user_role
FROM staff_attendance a
JOIN users u ON u.id = a.user_id
WHERE a.tenant_id = sqlc.arg(tenant_id)
  AND a.clock_in_at >= sqlc.arg(from_at)::timestamptz
  AND a.clock_in_at < sqlc.arg(to_at)::timestamptz
  AND (sqlc.narg(user_id)::uuid IS NULL OR a.user_id = sqlc.narg(user_id)::uuid)
ORDER BY a.clock_in_at DESC
LIMIT 2000;

-- name: EditAttendance :one
UPDATE staff_attendance
SET clock_in_at = sqlc.arg(clock_in_at), clock_out_at = sqlc.narg(clock_out_at),
    note = sqlc.arg(note), edited_by = sqlc.arg(edited_by), edited_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: StaffStats :one
-- Per-person figures for the staff profile. Counts start from when the
-- columns they read were added.
SELECT
    (SELECT COUNT(DISTINCT h.order_id) FROM order_status_history h
     WHERE h.tenant_id = sqlc.arg(tenant_id) AND h.actor_user_id = sqlc.arg(user_id)
       AND h.created_at >= sqlc.arg(from_at)::timestamptz AND h.created_at < sqlc.arg(to_at)::timestamptz)::bigint AS orders_handled,
    (SELECT COUNT(*) FROM appointments ap
     WHERE ap.tenant_id = sqlc.arg(tenant_id) AND ap.staff_id = sqlc.arg(user_id) AND ap.status = 'COMPLETED'
       AND ap.scheduled_at >= sqlc.arg(from_at)::timestamptz AND ap.scheduled_at < sqlc.arg(to_at)::timestamptz)::bigint AS appointments_completed;
