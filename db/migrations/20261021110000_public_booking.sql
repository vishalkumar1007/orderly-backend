-- +goose Up
-- +goose StatementBegin

-- Customers can now book from the storefront. An appointment needs its end
-- time stored so overlap checks do not have to join services (whose duration
-- an owner may change after the booking was made).
ALTER TABLE appointments ADD COLUMN ends_at TIMESTAMPTZ;
UPDATE appointments a
SET ends_at = a.scheduled_at + make_interval(mins => s.duration_minutes)
FROM services s
WHERE s.id = a.service_id;
ALTER TABLE appointments ALTER COLUMN ends_at SET NOT NULL;

ALTER TABLE appointments
    ADD COLUMN source TEXT NOT NULL DEFAULT 'STAFF' CHECK (source IN ('STAFF', 'ONLINE')),
    ADD COLUMN client_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN notes TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX appointments_tenant_client_token_unique
    ON appointments (tenant_id, client_token)
    WHERE client_token <> '';

ALTER TABLE reservations
    ADD COLUMN source TEXT NOT NULL DEFAULT 'STAFF' CHECK (source IN ('STAFF', 'ONLINE')),
    ADD COLUMN client_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN notes TEXT NOT NULL DEFAULT '',
    ADD COLUMN total_amount NUMERIC(12, 2);
CREATE UNIQUE INDEX reservations_tenant_client_token_unique
    ON reservations (tenant_id, client_token)
    WHERE client_token <> '';

-- What a guest sees before choosing a room.
ALTER TABLE room_types
    ADD COLUMN description TEXT NOT NULL DEFAULT '',
    ADD COLUMN image_url TEXT NOT NULL DEFAULT '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE room_types DROP COLUMN IF EXISTS image_url, DROP COLUMN IF EXISTS description;
DROP INDEX IF EXISTS reservations_tenant_client_token_unique;
ALTER TABLE reservations
    DROP COLUMN IF EXISTS total_amount,
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS client_token,
    DROP COLUMN IF EXISTS source;
DROP INDEX IF EXISTS appointments_tenant_client_token_unique;
ALTER TABLE appointments
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS client_token,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS ends_at;

-- +goose StatementEnd
