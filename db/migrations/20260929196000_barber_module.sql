-- +goose Up
-- +goose StatementBegin

CREATE TABLE services (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    duration_minutes INT NOT NULL,
    price            NUMERIC(12, 2) NOT NULL,
    is_active        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT services_duration_positive CHECK (duration_minutes > 0),
    CONSTRAINT services_price_non_negative CHECK (price >= 0),
    CONSTRAINT services_tenant_name_unique UNIQUE (tenant_id, name)
);

CREATE INDEX services_tenant_id_idx ON services (tenant_id);

-- Recurring weekly availability per staff member. Time-off/one-off overrides
-- are out of scope for the POC — a real requirement can add an exceptions
-- table later without touching this one.
CREATE TABLE staff_availability (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    staff_id   UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    weekday    INT NOT NULL,
    start_time TIME NOT NULL,
    end_time   TIME NOT NULL,
    CONSTRAINT staff_availability_weekday_check CHECK (weekday BETWEEN 0 AND 6),
    CONSTRAINT staff_availability_range_check CHECK (end_time > start_time)
);

CREATE INDEX staff_availability_tenant_staff_idx ON staff_availability (tenant_id, staff_id);

CREATE TABLE appointments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    customer_id  UUID REFERENCES customers (id) ON DELETE RESTRICT,
    service_id   UUID NOT NULL REFERENCES services (id) ON DELETE RESTRICT,
    staff_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    scheduled_at TIMESTAMPTZ NOT NULL,
    status       TEXT NOT NULL DEFAULT 'REQUESTED' CHECK (
        status IN ('REQUESTED', 'CONFIRMED', 'CHECKED_IN', 'WAITING', 'IN_SERVICE',
                   'COMPLETED', 'CANCELLED', 'NO_SHOW')
    ),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX appointments_tenant_id_idx ON appointments (tenant_id);
CREATE INDEX appointments_tenant_staff_time_idx ON appointments (tenant_id, staff_id, scheduled_at);
CREATE INDEX appointments_tenant_customer_idx ON appointments (tenant_id, customer_id);

-- One queue serves both walk-ins (appointment_id NULL) and checked-in
-- appointment customers waiting their turn, matching the POC's walk-in flow
-- (WAITING → CALLED → IN_SERVICE → COMPLETED). queue_date is a real stored
-- column (not date(created_at)) so the daily-reset uniqueness below can be a
-- plain btree constraint instead of a non-immutable expression index.
CREATE TABLE queue_entries (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    appointment_id UUID REFERENCES appointments (id) ON DELETE SET NULL,
    customer_id    UUID REFERENCES customers (id) ON DELETE RESTRICT,
    queue_date     DATE NOT NULL DEFAULT CURRENT_DATE,
    queue_number   INT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'WAITING'
                   CHECK (status IN ('WAITING', 'CALLED', 'IN_SERVICE', 'COMPLETED', 'CANCELLED')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT queue_entries_tenant_date_number_unique UNIQUE (tenant_id, queue_date, queue_number)
);

CREATE INDEX queue_entries_tenant_status_idx ON queue_entries (tenant_id, queue_date, status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS queue_entries;
DROP TABLE IF EXISTS appointments;
DROP TABLE IF EXISTS staff_availability;
DROP TABLE IF EXISTS services;
-- +goose StatementEnd
