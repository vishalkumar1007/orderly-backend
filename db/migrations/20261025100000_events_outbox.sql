-- +goose Up
-- +goose StatementBegin

-- Transactional outbox (Ch.19 section 7, Ch.38). A module writes its event
-- in the same transaction as the change it describes; the relay publishes
-- rows to NATS JetStream afterwards and stamps published_at. Delivery is at
-- least once: JetStream deduplicates on the event id, and every consumer
-- records what it processed in inbox, so effects happen exactly once.
CREATE TABLE outbox (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID,
    -- NATS subject: <context>.<aggregate>.<event>.v<version>.<bucket>
    subject        TEXT NOT NULL,
    event_type     TEXT NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id   TEXT NOT NULL,
    payload        JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INT NOT NULL DEFAULT 0
);

-- The relay only ever reads the unpublished tail, in arrival order.
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;

-- One row per (consumer, event) that a consumer has already applied.
CREATE TABLE inbox (
    consumer     TEXT NOT NULL,
    event_id     UUID NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS inbox;
DROP TABLE IF EXISTS outbox;
-- +goose StatementEnd
