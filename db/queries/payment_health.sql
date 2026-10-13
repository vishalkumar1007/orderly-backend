-- Platform-wide payment health for the Super Admin: what needs a human.

-- name: PaymentHealth :one
SELECT
    (SELECT COUNT(*) FROM payments
      WHERE status = 'PENDING' AND expires_at IS NOT NULL
        AND expires_at < now() - interval '10 minutes')::bigint AS overdue_pending,
    (SELECT COUNT(*) FROM payments
      WHERE status = 'REFUNDING' AND updated_at < now() - interval '1 hour')::bigint AS stuck_refunding,
    (SELECT COUNT(*) FROM payment_refunds
      WHERE status = 'PENDING' AND updated_at < now() - interval '1 hour')::bigint AS stuck_refunds,
    (SELECT COUNT(*) FROM payment_events
      WHERE error NOT IN ('', 'unknown_payment', 'unknown_refund')
        AND received_at > now() - interval '24 hours')::bigint AS failed_events_24h,
    (SELECT COUNT(*) FROM payment_events
      WHERE error = 'amount_mismatch' AND received_at > now() - interval '24 hours')::bigint AS mismatches_24h,
    (SELECT COUNT(*) FROM payment_audit_log
      WHERE event_type = 'DUPLICATE_PAYMENT' AND created_at > now() - interval '24 hours')::bigint AS duplicates_24h,
    (SELECT COUNT(*) FROM payments
      WHERE mode = 'LIVE' AND status = 'PAID' AND paid_at > now() - interval '24 hours')::bigint AS live_paid_24h,
    COALESCE((SELECT SUM(amount) FROM payments
      WHERE mode = 'LIVE' AND status = 'PAID' AND paid_at > now() - interval '24 hours'), 0)::text AS live_paid_amount_24h;

-- name: GetPaymentAnyTenant :one
SELECT * FROM payments WHERE id = $1;

-- Pending refunds the gateway accepted but never confirmed.
-- name: ListStuckRefunds :many
SELECT * FROM payment_refunds
WHERE status = 'PENDING' AND gateway_refund_id <> '' AND updated_at < now() - interval '1 hour'
LIMIT $1;

-- Payments well past their window that the sweeper could not close.
-- name: ListOverduePending :many
SELECT * FROM payments
WHERE status = 'PENDING' AND expires_at IS NOT NULL AND expires_at < now() - interval '30 minutes'
LIMIT $1;
