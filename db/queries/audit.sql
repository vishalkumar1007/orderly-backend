-- name: InsertAuditLog :one
INSERT INTO audit_logs (tenant_id, user_id, action, entity_type, entity_id, metadata, result)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListAuditLogs :many
SELECT * FROM audit_logs
ORDER BY created_at DESC
LIMIT $1;


-- name: CountAuditLogsByResult :many
SELECT result, COUNT(*)::bigint AS count
FROM audit_logs
WHERE tenant_id IS NULL OR tenant_id = sqlc.narg(tenant_id)
GROUP BY result;
