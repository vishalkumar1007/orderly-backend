// Package events is the transactional outbox and inbox (Ch.19 section 7,
// Ch.38). A module calls Publish inside the transaction that makes the change;
// the Relay moves outbox rows to NATS JetStream; Consume hands each event to a
// handler exactly once per consumer name, with the inbox row written in the
// same transaction as the handler's effect.
//
// Subjects: <context>.<aggregate>.<event>.v<version>.<bucket>, where bucket is
// hash(tenant_id) mod Buckets so one tenant's events stay ordered while
// different tenants are processed in parallel.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Buckets is the number of ordering lanes per stream.
const Buckets = 64

// Contexts (the first subject token). Each has one JetStream stream.
const (
	ContextPlatform  = "platform"
	ContextCommerce  = "commerce"
	ContextInventory = "inventory"
)

// Envelope is what travels on the wire. It follows the Ch.38 proposal plus
// trace_id. Data is the event-specific payload.
type Envelope struct {
	ID            uuid.UUID       `json:"id"`
	Type          string          `json:"type"` // commerce.order.status_changed
	Version       int             `json:"version"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	TenantID      *uuid.UUID      `json:"tenant_id,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Source        string          `json:"source"`
	Data          json.RawMessage `json:"data"`
	Metadata      Metadata        `json:"metadata"`
}

// Metadata is request context carried for tracing and audit.
type Metadata struct {
	TraceID string `json:"trace_id,omitempty"`
	Actor   string `json:"actor,omitempty"`
}

// Event is what a module hands to Publish.
type Event struct {
	// Type is <context>.<aggregate>.<event>; Version defaults to 1.
	Type          string
	Version       int
	AggregateType string
	AggregateID   string
	TenantID      *uuid.UUID
	Data          any
	Metadata      Metadata
}

// Context returns the first token of the type, which names the stream.
func (e Event) Context() string {
	ctx, _, _ := strings.Cut(e.Type, ".")
	return ctx
}

// Subject builds the NATS subject for an event.
func Subject(eventType string, version int, tenantID *uuid.UUID) string {
	return fmt.Sprintf("%s.v%d.%d", eventType, version, bucket(tenantID))
}

func bucket(tenantID *uuid.UUID) uint32 {
	if tenantID == nil {
		return 0
	}
	h := fnv.New32a()
	h.Write(tenantID[:])
	return h.Sum32() % Buckets
}

// Publish writes the event to the outbox. q must belong to the transaction
// that makes the change the event describes, so both commit or neither does.
func Publish(ctx context.Context, q *sqlc.Queries, e Event) (uuid.UUID, error) {
	if e.Type == "" || e.AggregateType == "" || e.AggregateID == "" {
		return uuid.Nil, fmt.Errorf("events: type, aggregate_type and aggregate_id are required")
	}
	if strings.Count(e.Type, ".") != 2 {
		return uuid.Nil, fmt.Errorf("events: type %q must be <context>.<aggregate>.<event>", e.Type)
	}
	if e.Version == 0 {
		e.Version = 1
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		return uuid.Nil, fmt.Errorf("events: marshal data: %w", err)
	}
	env := Envelope{
		ID: uuid.New(), Type: e.Type, Version: e.Version,
		AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		TenantID: e.TenantID, OccurredAt: time.Now().UTC(), Source: "api",
		Data: data, Metadata: e.Metadata,
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return uuid.Nil, err
	}
	tid := pgtype.UUID{}
	if e.TenantID != nil {
		tid = pgutil.UUID(*e.TenantID)
	}
	row, err := q.InsertOutbox(ctx, sqlc.InsertOutboxParams{
		TenantID: tid, Subject: Subject(e.Type, e.Version, e.TenantID),
		EventType: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		Payload: payload,
	})
	if err != nil {
		return uuid.Nil, err
	}
	// The outbox row id is the event id on the wire, so the relay and the
	// JetStream dedupe window agree on identity without a second column.
	env.ID = uuid.UUID(row.ID.Bytes)
	return env.ID, nil
}

// Decode parses an envelope off the wire. The outbox row id is the event id.
func Decode(id uuid.UUID, payload []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return env, err
	}
	env.ID = id
	return env, nil
}
