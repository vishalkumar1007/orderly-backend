-- Transactional outbox and consumer inbox (Ch.19 section 7).

-- name: InsertOutbox :one
INSERT INTO outbox (tenant_id, subject, event_type, aggregate_type, aggregate_id, payload)
VALUES (sqlc.arg(tenant_id), sqlc.arg(subject), sqlc.arg(event_type), sqlc.arg(aggregate_type), sqlc.arg(aggregate_id), sqlc.arg(payload))
RETURNING *;

-- name: ClaimUnpublishedOutbox :many
-- The relay's batch. SKIP LOCKED lets several processes relay at once without
-- handing the same row to two of them; the caller holds the transaction open
-- until it has stamped the rows it published.
SELECT * FROM outbox
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT sqlc.arg(batch)
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxPublished :exec
UPDATE outbox SET published_at = now(), attempts = attempts + 1
WHERE id = ANY (sqlc.arg(ids)::uuid[]);

-- name: BumpOutboxAttempts :exec
UPDATE outbox SET attempts = attempts + 1
WHERE id = ANY (sqlc.arg(ids)::uuid[]);

-- name: CountUnpublishedOutbox :one
SELECT COUNT(*)::int FROM outbox WHERE published_at IS NULL;

-- name: InsertInbox :execrows
-- 0 rows means this consumer already processed the event.
INSERT INTO inbox (consumer, event_id) VALUES (sqlc.arg(consumer), sqlc.arg(event_id))
ON CONFLICT DO NOTHING;

-- name: DeleteOldOutbox :execrows
DELETE FROM outbox WHERE published_at IS NOT NULL AND published_at < sqlc.arg(before);
