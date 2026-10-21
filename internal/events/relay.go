package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Publisher is the broker side of the relay. Broker implements it for
// JetStream; tests use a recorder.
type Publisher interface {
	// Publish sends one message. msgID is the dedupe key (the outbox row id).
	Publish(ctx context.Context, subject string, msgID string, payload []byte) error
}

const (
	relayBatch = 200
	// relayKeep is how long published rows stay for inspection and replay.
	relayKeep = 7 * 24 * time.Hour
)

// Relay moves outbox rows to the broker. Same shape as the other workers: a
// ticker, Postgres row locks as the only coordination, so several API
// processes can run it. A row is stamped published only after the broker
// acknowledged it; a crash in between republishes, and JetStream drops the
// duplicate by message id.
type Relay struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	pub  Publisher
	log  *slog.Logger
}

func NewRelay(pool *pgxpool.Pool, pub Publisher, log *slog.Logger) *Relay {
	return &Relay{pool: pool, q: sqlc.New(pool), pub: pub, log: log}
}

// Start blocks until ctx is cancelled.
func (r *Relay) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Drain: keep going while batches are full so a backlog clears
			// faster than one batch per tick.
			for {
				n, err := r.Tick(ctx)
				if err != nil {
					r.log.Error("outbox relay", "error", err)
					break
				}
				if n < relayBatch {
					break
				}
			}
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				_, _ = r.q.DeleteOldOutbox(ctx, pgtype.Timestamptz{Time: time.Now().Add(-relayKeep), Valid: true})
			}
		}
	}
}

// Tick relays one batch and returns how many rows it claimed. Exported so a
// test can drive it.
func (r *Relay) Tick(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := r.q.WithTx(tx)

	rows, err := q.ClaimUnpublishedOutbox(ctx, relayBatch)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	var done, failed []pgtype.UUID
	for _, row := range rows {
		id := pgutil.UUIDString(row.ID)
		if err := r.pub.Publish(ctx, row.Subject, id, row.Payload); err != nil {
			r.log.Warn("outbox relay: publish failed", "event", id, "subject", row.Subject, "error", err)
			failed = append(failed, row.ID)
			continue
		}
		done = append(done, row.ID)
	}
	if len(done) > 0 {
		if err := q.MarkOutboxPublished(ctx, done); err != nil {
			return 0, err
		}
	}
	if len(failed) > 0 {
		if err := q.BumpOutboxAttempts(ctx, failed); err != nil {
			return 0, err
		}
	}
	return len(rows), tx.Commit(ctx)
}

// Backlog is the number of unpublished rows, for health and metrics.
func (r *Relay) Backlog(ctx context.Context) (int, error) {
	n, err := r.q.CountUnpublishedOutbox(ctx)
	return int(n), err
}
