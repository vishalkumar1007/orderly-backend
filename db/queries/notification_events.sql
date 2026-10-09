-- name: ListNotificationEvents :many
SELECT * FROM notification_events
ORDER BY category, label;

-- Events available to a tenant: either not capability-gated, or gated by a
-- capability the tenant actually has enabled. Keeps the settings UI from
-- showing "Reservation confirmed" to a food shop that has no RESERVATIONS
-- capability.
-- name: ListNotificationEventsForCapabilities :many
SELECT * FROM notification_events
WHERE required_capability IS NULL OR required_capability = ANY(sqlc.arg(capability_codes)::text[])
ORDER BY category, label;

-- name: GetNotificationEvent :one
SELECT * FROM notification_events WHERE code = $1;
