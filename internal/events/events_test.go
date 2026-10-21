package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/database"
)

type recorder struct {
	msgs []struct{ subject, id string }
	fail bool
}

func (r *recorder) Publish(_ context.Context, subject, id string, _ []byte) error {
	if r.fail {
		return errDisabled
	}
	r.msgs = append(r.msgs, struct{ subject, id string }{subject, id})
	return nil
}

func TestSubjectAndValidation(t *testing.T) {
	tid := uuid.MustParse("2d4f3f4e-9c7a-4a0f-9f2b-0b1c6f4d5e6a")
	s := Subject("commerce.order.status_changed", 1, &tid)
	if s != "commerce.order.status_changed.v1."+itoa(bucket(&tid)) {
		t.Fatalf("subject %q", s)
	}
	if Subject("platform.tenant.created", 2, nil) != "platform.tenant.created.v2.0" {
		t.Fatal("nil tenant must land in bucket 0")
	}
	if (Event{Type: "order.status_changed"}).Context() != "order" {
		t.Fatal("context is the first token")
	}
}

func itoa(n uint32) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// Publish, relay and consume once, against a real database.
func TestOutboxRoundTrip(t *testing.T) {
	_ = godotenv.Load("../../.env")
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL is not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := database.NewPool(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := sqlc.New(pool)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Drain anything earlier tests left so counts below are exact.
	rec := &recorder{}
	relay := NewRelay(pool, rec, log)
	for {
		n, err := relay.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n < relayBatch {
			break
		}
	}
	rec.msgs = nil

	tid := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Publish(ctx, q.WithTx(tx), Event{
		Type: "commerce.order.status_changed", AggregateType: "order", AggregateID: "o1",
		TenantID: &tid, Data: map[string]any{"to": "ACCEPTED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Rolled back: the event must never be relayed.
	_ = tx.Rollback(ctx)
	if _, err := relay.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 0 {
		t.Fatalf("rolled-back event was relayed: %+v", rec.msgs)
	}

	tx, _ = pool.Begin(ctx)
	id, err = Publish(ctx, q.WithTx(tx), Event{
		Type: "commerce.order.status_changed", AggregateType: "order", AggregateID: "o1",
		TenantID: &tid, Data: map[string]any{"to": "ACCEPTED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A failing broker leaves the row unpublished; a working one publishes it once.
	rec.fail = true
	if _, err := relay.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rec.fail = false
	if _, err := relay.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.msgs) != 1 || rec.msgs[0].id != id.String() || rec.msgs[0].subject != Subject("commerce.order.status_changed", 1, &tid) {
		t.Fatalf("relay published %+v, want one message for %s", rec.msgs, id)
	}

	// Consume the same event twice: the handler runs once.
	env := Envelope{ID: id, Type: "commerce.order.status_changed", TenantID: &tid, Data: json.RawMessage(`{"to":"ACCEPTED"}`)}
	runs := 0
	h := func(_ context.Context, _ *sqlc.Queries, e Envelope) error {
		runs++
		var d struct{ To string }
		if err := json.Unmarshal(e.Data, &d); err != nil || d.To != "ACCEPTED" {
			t.Fatalf("payload %s", e.Data)
		}
		return nil
	}
	consumer := "test-" + uuid.NewString()
	for i := 0; i < 2; i++ {
		if err := applyOnce(ctx, pool, q, consumer, env, h); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 {
		t.Fatalf("handler ran %d times, want 1", runs)
	}
	// A handler error keeps nothing, so the next delivery runs it again.
	failing := func(context.Context, *sqlc.Queries, Envelope) error { return errDisabled }
	if err := applyOnce(ctx, pool, q, consumer+"-b", env, failing); err == nil {
		t.Fatal("expected handler error")
	}
	runs = 0
	if err := applyOnce(ctx, pool, q, consumer+"-b", env, h); err != nil || runs != 1 {
		t.Fatalf("retry after failure: err=%v runs=%d", err, runs)
	}
}
