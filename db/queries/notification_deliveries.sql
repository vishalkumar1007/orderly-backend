-- Fire-and-forget enqueue. A duplicate idempotency_key (a retried or
-- re-published event) is silently skipped — the caller sees zero rows
-- rather than an error, since "already enqueued" is success, not failure.
-- name: EnqueueNotificationDelivery :one
INSERT INTO notification_deliveries (
    notification_id, tenant_id, event_code, channel, recipient, idempotency_key
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;

-- The worker's claim: due rows get their lease (next_attempt_at) pushed
-- forward immediately, inside this single statement, so the lock is held
-- only for the UPDATE itself rather than for the lifetime of a provider
-- call. A worker that crashes mid-send leaves the row reclaimable once the
-- lease expires.
-- name: ClaimDueDeliveries :many
UPDATE notification_deliveries
SET next_attempt_at = now() + interval '2 minutes'
WHERE id IN (
    SELECT id FROM notification_deliveries
    WHERE status IN ('PENDING', 'RETRYING') AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkDeliverySent :exec
UPDATE notification_deliveries
SET status = 'SENT', provider_message_id = $2, delivered_at = now()
WHERE id = $1;

-- name: UpdateDeliveryAttempt :exec
UPDATE notification_deliveries
SET status = $2, attempt_count = $3, last_error = $4, next_attempt_at = $5
WHERE id = $1;

-- name: ListDeliveriesForTenant :many
SELECT * FROM notification_deliveries
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListAllDeliveries :many
SELECT * FROM notification_deliveries
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- Re-queues a failed/dead delivery for the next worker tick. Scoped by
-- tenant_id so a tenant can only retry its own deliveries; a platform-scoped
-- row (tenant_id NULL) is retried with tenant_id passed as NULL.
-- name: RetryDelivery :exec
UPDATE notification_deliveries
SET status = 'PENDING', next_attempt_at = now()
WHERE id = $1 AND tenant_id IS NOT DISTINCT FROM sqlc.narg(tenant_id)::uuid
  AND status IN ('FAILED', 'DEAD');

-- A Super Admin may retry any delivery, tenant-owned or platform-scoped.
-- name: AdminRetryDelivery :exec
UPDATE notification_deliveries
SET status = 'PENDING', next_attempt_at = now()
WHERE id = $1 AND status IN ('FAILED', 'DEAD');
