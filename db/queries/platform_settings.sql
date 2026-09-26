-- name: GetPlatformSettings :one
SELECT config, updated_at FROM platform_settings WHERE id = 1;

-- name: UpsertPlatformSettings :one
INSERT INTO platform_settings (id, config)
VALUES (1, $1)
ON CONFLICT (id) DO UPDATE
SET config = EXCLUDED.config, updated_at = now()
RETURNING config, updated_at;
