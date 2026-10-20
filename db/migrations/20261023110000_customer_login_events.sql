-- +goose Up
-- +goose StatementBegin

-- Customer sessions are stateless JWTs, so sign-ins are recorded here for the
-- shop's customer profile. identifier is the phone or lower-cased email used.
CREATE TABLE customer_login_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    customer_id UUID REFERENCES customers (id) ON DELETE CASCADE,
    identifier  TEXT NOT NULL,
    method      TEXT NOT NULL CHECK (method IN ('PHONE', 'EMAIL')),
    result      TEXT NOT NULL CHECK (result IN ('SUCCESS', 'FAILURE', 'DENIED')),
    ip          TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX customer_login_events_customer_idx
    ON customer_login_events (tenant_id, customer_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS customer_login_events;
-- +goose StatementEnd
