-- name: CreateNotification :one
INSERT INTO notifications (
    tenant_id, user_id, type, title, body, data, priority, event_code
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, sqlc.narg(event_code)
)
RETURNING *;

-- user_id alone is a sufficient, safe scope: a user has exactly one fixed
-- tenant_id (or NULL for a platform account), enforced by
-- users_tenant_role_check, so one query serves both the shop and the console.
-- name: ListNotificationsForUser :many
SELECT * FROM notifications
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- Cursor pagination for the "view all" page: everything strictly older than
-- a given row, by (created_at, id) so same-timestamp rows never repeat or
-- get skipped across pages.
-- name: ListNotificationsForUserBefore :many
SELECT * FROM notifications
WHERE user_id = $1
  AND (created_at, id) < (sqlc.arg(before_created_at)::timestamptz, sqlc.arg(before_id)::uuid)
ORDER BY created_at DESC
LIMIT $2;

-- name: CountUnreadForUser :one
SELECT count(*) FROM notifications
WHERE user_id = $1 AND read_at IS NULL;

-- The user_id guard keeps a user from marking someone else's notification read.
-- name: MarkNotificationRead :exec
UPDATE notifications
SET read_at = now()
WHERE id = $1 AND user_id = $2 AND read_at IS NULL;

-- name: MarkNotificationUnread :exec
UPDATE notifications
SET read_at = NULL
WHERE id = $1 AND user_id = $2;

-- name: MarkAllNotificationsRead :exec
UPDATE notifications
SET read_at = now()
WHERE user_id = $1 AND read_at IS NULL;

-- Recipient resolution for a platform-wide event. Tenant-wide recipient
-- resolution reuses the existing ListTenantUsers query (users.sql) instead of
-- a second copy — it already returns every column this needs.
-- name: ListActivePlatformUsers :many
SELECT * FROM users
WHERE tenant_id IS NULL AND status = 'ACTIVE';
