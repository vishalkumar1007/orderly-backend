package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
)

const (
	workerBatchSize  = 20
	workerMaxAttempt = 5
)

// backoffSchedule is the delay before each retry, capped at the last entry.
// Index 0 is the delay after the FIRST failed attempt.
var backoffSchedule = []time.Duration{
	1 * time.Minute, 5 * time.Minute, 15 * time.Minute, 1 * time.Hour, 6 * time.Hour,
}

func backoffFor(attempt int) time.Duration {
	if attempt <= 0 {
		return backoffSchedule[0]
	}
	if attempt >= len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[attempt]
}

// StartWorker runs the EMAIL/SMS delivery loop until ctx is cancelled. One
// goroutine, polling on a plain time.Ticker — this codebase has no queue
// broker anywhere, and Postgres (via ClaimDueDeliveries's SELECT ... FOR
// UPDATE SKIP LOCKED) is already the only shared infrastructure every
// deployment has, so that is what the queue is built on rather than adding
// one.
func StartWorker(ctx context.Context, q *sqlc.Queries, log *slog.Logger, cfg *configsvc.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce(ctx, q, log, cfg)
		}
	}
}

func runOnce(ctx context.Context, q *sqlc.Queries, log *slog.Logger, cfg *configsvc.Service) {
	claimed, err := q.ClaimDueDeliveries(ctx, workerBatchSize)
	if err != nil {
		if log != nil {
			log.Error("notify worker: claim failed", "error", err)
		}
		return
	}
	for _, d := range claimed {
		deliverOne(ctx, q, log, cfg, d)
	}
}

func deliverOne(ctx context.Context, q *sqlc.Queries, log *slog.Logger, cfg *configsvc.Service, d sqlc.NotificationDelivery) {
	var tenantID *uuid.UUID
	if d.TenantID.Valid {
		id := uuid.UUID(d.TenantID.Bytes)
		tenantID = &id
	}

	rendered := render(ctx, q, tenantID, d.EventCode, d.Channel, "", "", nil)
	var sendErr error
	var providerMessageID string

	switch d.Channel {
	case ChannelEmail:
		sendErr = cfg.Notifications().Send(ctx, tenantID, configsvc.MailMessage{
			To:      []string{d.Recipient},
			Subject: rendered.Subject,
			Body:    rendered.Body,
		})
	case ChannelSMS:
		providerMessageID, sendErr = cfg.SMSService().Send(ctx, tenantID, d.Recipient, rendered.Body)
	}

	if sendErr == nil {
		if err := q.MarkDeliverySent(ctx, sqlc.MarkDeliverySentParams{ID: d.ID, ProviderMessageID: providerMessageID}); err != nil && log != nil {
			log.Error("notify worker: mark sent failed", "id", d.ID, "error", err)
		}
		return
	}

	attempt := d.AttemptCount + 1
	status := "RETRYING"
	nextAttempt := time.Now().Add(backoffFor(int(attempt)))
	if attempt >= d.MaxAttempts {
		status = "DEAD"
		if log != nil {
			log.Error("notify worker: delivery exhausted retries", "id", d.ID, "event", d.EventCode, "channel", d.Channel, "error", sendErr)
		}
	}
	if err := q.UpdateDeliveryAttempt(ctx, sqlc.UpdateDeliveryAttemptParams{
		ID:            d.ID,
		Status:        status,
		AttemptCount:  attempt,
		LastError:     configsvc.SafeErrorMessage(sendErr),
		NextAttemptAt: pgtype.Timestamptz{Time: nextAttempt, Valid: true},
	}); err != nil && log != nil {
		log.Error("notify worker: update attempt failed", "id", d.ID, "error", err)
	}
}
