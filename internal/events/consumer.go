package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Handler applies one event. q belongs to a transaction that also holds the
// inbox row: return an error and nothing is kept, the event is redelivered.
type Handler func(ctx context.Context, q *sqlc.Queries, env Envelope) error

// ErrSkip tells Consume to acknowledge the event without applying it (an
// event the handler does not care about, or one that can never succeed).
var ErrSkip = errors.New("events: skip")

const (
	maxDeliveries = 5
	ackWait       = 30 * time.Second
)

// Consume runs a durable consumer named name on the subjects filter (for
// example "commerce.order.>") until ctx is cancelled. Delivery is at least
// once; the inbox makes the handler's effect exactly once per consumer name.
// After maxDeliveries failures the event is terminated and logged: the
// stream keeps it, so it can be replayed once the cause is fixed.
func (b *Broker) Consume(ctx context.Context, pool *pgxpool.Pool, name, filter string, h Handler) error {
	if !b.Enabled() {
		return errDisabled
	}
	stream, _, _ := strings.Cut(filter, ".")
	cons, err := b.js.CreateOrUpdateConsumer(ctx, strings.ToUpper(stream), jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliveries,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("consumer %s: %w", name, err)
	}
	q := sqlc.New(pool)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		id, err := uuid.Parse(msg.Headers().Get(jetstream.MsgIDHeader))
		if err != nil {
			b.log.Error("consumer: message without id", "consumer", name, "subject", msg.Subject())
			_ = msg.Term()
			return
		}
		env, err := Decode(id, msg.Data())
		if err != nil {
			b.log.Error("consumer: bad envelope", "consumer", name, "event", id, "error", err)
			_ = msg.Term()
			return
		}
		if err := applyOnce(ctx, pool, q, name, env, h); err != nil {
			meta, _ := msg.Metadata()
			attempt := uint64(1)
			if meta != nil {
				attempt = meta.NumDelivered
			}
			if attempt >= maxDeliveries {
				b.log.Error("consumer: giving up", "consumer", name, "event", id, "type", env.Type, "error", err)
				_ = msg.Term()
				return
			}
			b.log.Warn("consumer: retry", "consumer", name, "event", id, "attempt", attempt, "error", err)
			_ = msg.NakWithDelay(time.Duration(attempt) * 5 * time.Second)
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}

// applyOnce runs the handler and the inbox insert in one transaction.
func applyOnce(ctx context.Context, pool *pgxpool.Pool, q *sqlc.Queries, name string, env Envelope, h Handler) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)
	n, err := qtx.InsertInbox(ctx, sqlc.InsertInboxParams{Consumer: name, EventID: pgutil.UUID(env.ID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return nil // already applied
	}
	if err := h(ctx, qtx, env); err != nil {
		if errors.Is(err, ErrSkip) {
			return tx.Commit(ctx) // keep the inbox row so it is not retried
		}
		return err
	}
	return tx.Commit(ctx)
}
