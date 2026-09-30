-- +goose Up
-- +goose StatementBegin

CREATE TABLE room_types (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    base_price NUMERIC(12, 2) NOT NULL,
    max_guests INT NOT NULL DEFAULT 2,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT room_types_base_price_non_negative CHECK (base_price >= 0),
    CONSTRAINT room_types_max_guests_positive CHECK (max_guests > 0),
    CONSTRAINT room_types_tenant_name_unique UNIQUE (tenant_id, name)
);

CREATE INDEX room_types_tenant_id_idx ON room_types (tenant_id);

CREATE TABLE rooms (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    room_type_id UUID NOT NULL REFERENCES room_types (id) ON DELETE RESTRICT,
    number       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'AVAILABLE' CHECK (
        status IN ('AVAILABLE', 'RESERVED', 'OCCUPIED', 'CHECKOUT_PENDING', 'CLEANING', 'INSPECTION')
    ),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT rooms_tenant_number_unique UNIQUE (tenant_id, number)
);

CREATE INDEX rooms_tenant_id_idx ON rooms (tenant_id);
CREATE INDEX rooms_tenant_status_idx ON rooms (tenant_id, status);

-- Reservation and room lifecycles are deliberately two separate state
-- machines on two separate tables (POC: "Reservation and room state machines
-- remain separate") — a room can go to CLEANING while its reservation is
-- already CHECKED_OUT, and a walk-in stay can occupy a room with no
-- reservation at all. room_id is assigned at check-in, not at booking time,
-- so it stays nullable.
CREATE TABLE reservations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    customer_id     UUID REFERENCES customers (id) ON DELETE RESTRICT,
    room_type_id    UUID NOT NULL REFERENCES room_types (id) ON DELETE RESTRICT,
    room_id         UUID REFERENCES rooms (id) ON DELETE SET NULL,
    check_in_date   DATE NOT NULL,
    check_out_date  DATE NOT NULL,
    guests          INT NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'REQUESTED' CHECK (
        status IN ('REQUESTED', 'CONFIRMED', 'CHECKED_IN', 'CHECKED_OUT', 'CANCELLED', 'NO_SHOW')
    ),
    checked_in_at   TIMESTAMPTZ,
    checked_out_at  TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT reservations_guests_positive CHECK (guests > 0),
    CONSTRAINT reservations_date_range_check CHECK (check_out_date > check_in_date)
);

CREATE INDEX reservations_tenant_id_idx ON reservations (tenant_id);
CREATE INDEX reservations_tenant_status_idx ON reservations (tenant_id, status);
CREATE INDEX reservations_tenant_dates_idx ON reservations (tenant_id, check_in_date, check_out_date);

-- One open folio per stay; every charge lines up here, payments settle
-- against it (payments.payable_type = 'FOLIO').
CREATE TABLE folios (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    reservation_id UUID NOT NULL UNIQUE REFERENCES reservations (id) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'SETTLED', 'VOID')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX folios_tenant_id_idx ON folios (tenant_id);

CREATE TABLE charges (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    folio_id    UUID NOT NULL REFERENCES folios (id) ON DELETE CASCADE,
    description TEXT NOT NULL,
    amount      NUMERIC(12, 2) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT charges_amount_non_negative CHECK (amount >= 0)
);

CREATE INDEX charges_folio_id_idx ON charges (folio_id);

CREATE TABLE housekeeping_tasks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    room_id      UUID NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    task_type    TEXT NOT NULL DEFAULT 'CLEANING' CHECK (task_type IN ('CLEANING', 'INSPECTION', 'MAINTENANCE')),
    status       TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'IN_PROGRESS', 'DONE')),
    assigned_to  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX housekeeping_tasks_tenant_room_idx ON housekeeping_tasks (tenant_id, room_id);
CREATE INDEX housekeeping_tasks_tenant_status_idx ON housekeeping_tasks (tenant_id, status);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS housekeeping_tasks;
DROP TABLE IF EXISTS charges;
DROP TABLE IF EXISTS folios;
DROP TABLE IF EXISTS reservations;
DROP TABLE IF EXISTS rooms;
DROP TABLE IF EXISTS room_types;
-- +goose StatementEnd
