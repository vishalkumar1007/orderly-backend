-- +goose Up
-- +goose StatementBegin

-- Staff punch in / punch out. A plan feature (ATTENDANCE) that each shop also
-- switches on for itself. One open punch per person is enforced here, not in
-- code, so two taps from two devices cannot open two shifts.
CREATE TABLE staff_attendance (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    clock_in_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    clock_out_at   TIMESTAMPTZ,
    in_lat         DOUBLE PRECISION,
    in_lng         DOUBLE PRECISION,
    in_distance_m  INTEGER,
    out_lat        DOUBLE PRECISION,
    out_lng        DOUBLE PRECISION,
    out_distance_m INTEGER,
    note           TEXT NOT NULL DEFAULT '',
    edited_by      UUID REFERENCES users (id) ON DELETE SET NULL,
    edited_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (clock_out_at IS NULL OR clock_out_at >= clock_in_at)
);
CREATE UNIQUE INDEX staff_attendance_one_open
    ON staff_attendance (tenant_id, user_id) WHERE clock_out_at IS NULL;
CREATE INDEX staff_attendance_tenant_time_idx ON staff_attendance (tenant_id, clock_in_at DESC);

-- Per-shop switch and optional location rule (geofence around the shop).
CREATE TABLE tenant_attendance_settings (
    tenant_id        UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    enabled          BOOLEAN NOT NULL DEFAULT FALSE,
    require_location BOOLEAN NOT NULL DEFAULT FALSE,
    geo_lat          DOUBLE PRECISION,
    geo_lng          DOUBLE PRECISION,
    geo_radius_m     INTEGER NOT NULL DEFAULT 200 CHECK (geo_radius_m BETWEEN 20 AND 50000),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

UPDATE plans SET entitlements = array_append(entitlements, 'ATTENDANCE')
WHERE name = 'BUSINESS' AND NOT ('ATTENDANCE' = ANY (entitlements));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE plans SET entitlements = array_remove(entitlements, 'ATTENDANCE');
DROP TABLE IF EXISTS tenant_attendance_settings;
DROP TABLE IF EXISTS staff_attendance;
-- +goose StatementEnd
