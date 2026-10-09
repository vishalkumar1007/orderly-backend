-- name: GetNotificationPreferences :one
SELECT * FROM notification_preferences WHERE user_id = $1;

-- name: UpsertNotificationPreferences :one
INSERT INTO notification_preferences (user_id, overrides, sound_enabled, quiet_hours_start, quiet_hours_end, quiet_hours_tz)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id) DO UPDATE SET
    overrides = EXCLUDED.overrides,
    sound_enabled = EXCLUDED.sound_enabled,
    quiet_hours_start = EXCLUDED.quiet_hours_start,
    quiet_hours_end = EXCLUDED.quiet_hours_end,
    quiet_hours_tz = EXCLUDED.quiet_hours_tz,
    updated_at = now()
RETURNING *;
