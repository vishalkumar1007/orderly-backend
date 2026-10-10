-- The merge that implements Platform Defaults -> Business Configuration
-- precedence: one row per (channel, recipient_policy), the tenant's own row
-- winning over the platform default when both exist. `tenant_id NULLS LAST`
-- sorts the non-null (tenant) row first within each DISTINCT ON group.
-- name: ListEffectiveRulesForEvent :many
SELECT DISTINCT ON (channel, recipient_policy) *
FROM notification_rules
WHERE event_code = $1 AND (tenant_id = $2 OR tenant_id IS NULL)
ORDER BY channel, recipient_policy, tenant_id NULLS LAST;

-- name: ListEffectiveRulesForTenant :many
SELECT DISTINCT ON (event_code, channel, recipient_policy) *
FROM notification_rules
WHERE tenant_id = $1 OR tenant_id IS NULL
ORDER BY event_code, channel, recipient_policy, tenant_id NULLS LAST;

-- name: ListPlatformRules :many
SELECT * FROM notification_rules
WHERE tenant_id IS NULL
ORDER BY event_code, channel, recipient_policy;

-- A tenant override. Only ever called with a concrete tenant_id, so it
-- matches the full (tenant_id, event_code, channel, recipient_policy)
-- unique constraint. The caller is responsible for rejecting this when the
-- effective platform rule for the same key is locked.
-- name: UpsertTenantNotificationRule :one
INSERT INTO notification_rules (tenant_id, event_code, channel, enabled, recipient_policy, priority, locked)
VALUES ($1, $2, $3, $4, $5, $6, FALSE)
ON CONFLICT (tenant_id, event_code, channel, recipient_policy)
DO UPDATE SET enabled = EXCLUDED.enabled, priority = EXCLUDED.priority, updated_at = now()
RETURNING *;

-- The platform default. Only ever called with tenant_id NULL, matching the
-- partial unique index rather than the full constraint (which does not
-- de-duplicate NULL tenant_id rows).
-- name: UpsertPlatformNotificationRule :one
INSERT INTO notification_rules (tenant_id, event_code, channel, enabled, recipient_policy, priority, locked)
VALUES (NULL, $1, $2, $3, $4, $5, $6)
ON CONFLICT (event_code, channel, recipient_policy) WHERE tenant_id IS NULL
DO UPDATE SET enabled = EXCLUDED.enabled, priority = EXCLUDED.priority, locked = EXCLUDED.locked, updated_at = now()
RETURNING *;

-- Reverts a tenant to inheriting the platform default for this key.
-- name: DeleteTenantNotificationRuleOverride :exec
DELETE FROM notification_rules
WHERE tenant_id = $1 AND event_code = $2 AND channel = $3 AND recipient_policy = $4;

-- Whether the effective (tenant-or-platform) rule for this exact key is
-- locked, checked before accepting a tenant write.
-- name: IsRuleLockedForTenant :one
SELECT COALESCE(
    (SELECT locked FROM notification_rules
     WHERE event_code = $2 AND channel = $3 AND recipient_policy = $4
       AND (tenant_id = $1 OR tenant_id IS NULL)
     ORDER BY tenant_id NULLS LAST LIMIT 1),
    FALSE
)::boolean AS locked;
