-- name: ListThemePresets :many
SELECT id, name, tokens, created_at
FROM theme_presets
ORDER BY name ASC;

-- name: GetThemePreset :one
SELECT id, name, tokens, created_at
FROM theme_presets
WHERE id = $1
LIMIT 1;

-- name: CreateThemePreset :one
INSERT INTO theme_presets (id, name, tokens)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(tokens))
RETURNING *;

-- name: UpdateThemePreset :one
UPDATE theme_presets
SET
    name = COALESCE(sqlc.narg(name), name),
    tokens = COALESCE(sqlc.narg(tokens), tokens)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteThemePreset :exec
DELETE FROM theme_presets
WHERE id = sqlc.arg(id);

-- name: CountTenantsOnThemePreset :one
SELECT COUNT(*)::bigint
FROM tenants
WHERE theme_preset_id = sqlc.arg(theme_preset_id);
