-- name: GetEffectiveTemplate :one
SELECT * FROM notification_templates
WHERE event_code = $1 AND channel = $2 AND is_active
  AND (tenant_id = $3 OR tenant_id IS NULL)
ORDER BY tenant_id NULLS LAST
LIMIT 1;

-- name: ListEffectiveTemplatesForTenant :many
SELECT DISTINCT ON (event_code, channel) *
FROM notification_templates
WHERE is_active AND (tenant_id = $1 OR tenant_id IS NULL)
ORDER BY event_code, channel, tenant_id NULLS LAST;

-- name: ListPlatformTemplates :many
SELECT * FROM notification_templates
WHERE is_active AND tenant_id IS NULL
ORDER BY event_code, channel;

-- Deactivates whatever is currently active for this key before a new
-- version is inserted, so "the active template" stays a single row per the
-- partial unique indexes rather than a status flag maintained by hand.
-- name: DeactivateTemplate :exec
UPDATE notification_templates
SET is_active = FALSE, updated_at = now()
WHERE event_code = $1 AND channel = $2 AND is_active
  AND tenant_id IS NOT DISTINCT FROM sqlc.narg(tenant_id)::uuid;

-- name: CreateTemplateVersion :one
INSERT INTO notification_templates (tenant_id, event_code, channel, subject, body, heading, preheader, button_label, button_url, html_body, highlight, footnote, is_active, version)
VALUES (
    sqlc.narg(tenant_id), $1, $2, $3, $4,
    sqlc.arg(heading), sqlc.arg(preheader), sqlc.arg(button_label), sqlc.arg(button_url), sqlc.arg(html_body), sqlc.arg(highlight), sqlc.arg(footnote), TRUE,
    COALESCE((
        SELECT MAX(version) + 1 FROM notification_templates
        WHERE event_code = $1 AND channel = $2
          AND tenant_id IS NOT DISTINCT FROM sqlc.narg(tenant_id)::uuid
    ), 1)
)
RETURNING *;

-- A business may word any message except the platform's own (PLATFORM events
-- that no business rule routes to, e.g. console invitations and system alerts).
-- name: TenantMayEditTemplate :one
SELECT EXISTS (
    SELECT 1 FROM notification_events e
    WHERE e.code = $1
      AND (e.category <> 'PLATFORM' OR EXISTS (
          SELECT 1 FROM notification_rules r
          WHERE r.event_code = e.code AND r.recipient_policy <> 'PLATFORM'))
)::bool;
