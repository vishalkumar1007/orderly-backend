-- +goose Up
-- +goose StatementBegin

-- The event catalog: every real, wired event in the system. Data-driven on
-- purpose — adding a new event anywhere else in the codebase is a row here,
-- never a new table or a new column. `category` groups events for the
-- settings UI (Orders/Payments/Bookings/Staff/Security/Platform); whether an
-- event shows under "Admin" or "Storefront" is derived from each rule's
-- recipient_policy below, not from the event itself, since the same event
-- (e.g. an order being ready) legitimately notifies both audiences.
CREATE TABLE notification_events (
    code                TEXT PRIMARY KEY,
    category            TEXT NOT NULL,
    label               TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    default_channels    TEXT[] NOT NULL DEFAULT '{}',
    required_capability TEXT REFERENCES capabilities (code),
    variables           JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_events_category_check
        CHECK (category IN ('ORDERS', 'PAYMENTS', 'BOOKINGS', 'STAFF', 'SECURITY', 'PLATFORM'))
);

-- One row per (tenant, event, channel). tenant_id NULL is the platform
-- default every tenant inherits until it writes its own row for that
-- (event, channel) — the same override shape as tenant_service_preferences.
-- `locked` rows (mandatory security/critical alerts) can be read but never
-- written by a tenant; only a platform default may set it.
CREATE TABLE notification_rules (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID REFERENCES tenants (id) ON DELETE CASCADE,
    event_code       TEXT NOT NULL REFERENCES notification_events (code),
    channel          TEXT NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    recipient_policy TEXT NOT NULL,
    priority         TEXT NOT NULL DEFAULT 'NORMAL',
    locked           BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_rules_channel_check
        CHECK (channel IN ('IN_APP', 'EMAIL', 'SMS')),
    CONSTRAINT notification_rules_priority_check
        CHECK (priority IN ('LOW', 'NORMAL', 'HIGH', 'CRITICAL')),
    CONSTRAINT notification_rules_unique
        UNIQUE (tenant_id, event_code, channel, recipient_policy)
);

CREATE INDEX notification_rules_event_idx ON notification_rules (event_code);
-- Partial unique index cannot express "one default row per
-- event+channel+audience" via the UNIQUE constraint above when tenant_id is
-- NULL (NULLs are distinct in a composite unique constraint), so it is
-- enforced separately.
CREATE UNIQUE INDEX notification_rules_platform_default_idx
    ON notification_rules (event_code, channel, recipient_policy) WHERE tenant_id IS NULL;

CREATE TABLE notification_templates (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID REFERENCES tenants (id) ON DELETE CASCADE,
    event_code TEXT NOT NULL REFERENCES notification_events (code),
    channel    TEXT NOT NULL,
    subject    TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    is_active  BOOLEAN NOT NULL DEFAULT TRUE,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_templates_channel_check
        CHECK (channel IN ('IN_APP', 'EMAIL', 'SMS'))
);

CREATE UNIQUE INDEX notification_templates_active_idx
    ON notification_templates (tenant_id, event_code, channel) WHERE is_active;
CREATE UNIQUE INDEX notification_templates_platform_active_idx
    ON notification_templates (event_code, channel) WHERE is_active AND tenant_id IS NULL;

-- Per-channel delivery attempts for EMAIL/SMS. The IN_APP channel has no row
-- here — it is written directly to `notifications`, which already is its own
-- delivery record (read_at is its only "status"). `notification_id` links
-- back to that row only when an IN_APP copy of the same event was also
-- published, purely for cross-reference in the UI.
CREATE TABLE notification_deliveries (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id    UUID REFERENCES notifications (id) ON DELETE SET NULL,
    tenant_id          UUID REFERENCES tenants (id) ON DELETE CASCADE,
    event_code         TEXT NOT NULL REFERENCES notification_events (code),
    channel            TEXT NOT NULL,
    recipient          TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'PENDING',
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    max_attempts       INTEGER NOT NULL DEFAULT 5,
    last_error         TEXT NOT NULL DEFAULT '',
    provider_message_id TEXT NOT NULL DEFAULT '',
    idempotency_key    TEXT NOT NULL,
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_deliveries_channel_check
        CHECK (channel IN ('EMAIL', 'SMS')),
    CONSTRAINT notification_deliveries_status_check
        CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'RETRYING', 'DEAD')),
    CONSTRAINT notification_deliveries_idempotency_unique
        UNIQUE (idempotency_key)
);

-- The worker's claim query: due rows, oldest first.
CREATE INDEX notification_deliveries_claim_idx
    ON notification_deliveries (status, next_attempt_at)
    WHERE status IN ('PENDING', 'RETRYING');
CREATE INDEX notification_deliveries_tenant_idx
    ON notification_deliveries (tenant_id, created_at DESC);

-- Personal, per-user settings within whatever the business/platform permits.
-- One JSONB document for per-event overrides, matching this schema's existing
-- convention (tenants.theme_overrides, platform_settings.config) rather than
-- a column or a row per event.
CREATE TABLE notification_preferences (
    user_id           UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    overrides         JSONB NOT NULL DEFAULT '{}'::jsonb,
    sound_enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    quiet_hours_start TIME,
    quiet_hours_end   TIME,
    quiet_hours_tz    TEXT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

/* ------------------------------------------------------------------ *
 * Catalog seed — every event this change wires to a real trigger.
 * ------------------------------------------------------------------ */

INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('ORDER_PLACED', 'ORDERS', 'Order placed', 'A customer places a new order.',
        ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name","total"]'),
    ('ORDER_READY', 'ORDERS', 'Order ready', 'An order is marked ready for pickup.',
        ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name"]'),
    ('ORDER_CANCELLED', 'ORDERS', 'Order cancelled', 'An order is cancelled.',
        ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name"]'),
    ('PAYMENT_FAILED', 'PAYMENTS', 'Payment failed', 'A payment attempt fails.',
        ARRAY['IN_APP'], 'PAYMENTS', '["order_id","order_number","failure_reason"]'),
    ('BUSINESS_ONBOARDED', 'PLATFORM', 'Business onboarded', 'A new business is onboarded to the platform.',
        ARRAY['IN_APP'], NULL, '["tenant_id","tenant_name"]'),
    ('SUBSCRIPTION_CHANGED', 'PLATFORM', 'Subscription changed', 'A business''s plan or subscription status changes.',
        ARRAY['IN_APP'], NULL, '["tenant_id","tenant_name","plan_name"]'),
    ('APPOINTMENT_CREATED', 'BOOKINGS', 'Appointment booked', 'A new appointment is booked.',
        ARRAY['IN_APP'], 'APPOINTMENTS', '["appointment_id","customer_name","starts_at"]'),
    ('APPOINTMENT_CONFIRMED', 'BOOKINGS', 'Appointment confirmed', 'An appointment is confirmed.',
        ARRAY['IN_APP'], 'APPOINTMENTS', '["appointment_id","customer_name","starts_at"]'),
    ('APPOINTMENT_CANCELLED', 'BOOKINGS', 'Appointment cancelled', 'An appointment is cancelled.',
        ARRAY['IN_APP'], 'APPOINTMENTS', '["appointment_id","customer_name","starts_at"]'),
    ('APPOINTMENT_NO_SHOW', 'BOOKINGS', 'Appointment no-show', 'A customer did not show up for an appointment.',
        ARRAY['IN_APP'], 'APPOINTMENTS', '["appointment_id","customer_name","starts_at"]'),
    ('RESERVATION_CREATED', 'BOOKINGS', 'Reservation booked', 'A new hotel reservation is booked.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name","check_in","check_out"]'),
    ('RESERVATION_CONFIRMED', 'BOOKINGS', 'Reservation confirmed', 'A hotel reservation is confirmed.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name","check_in","check_out"]'),
    ('RESERVATION_CANCELLED', 'BOOKINGS', 'Reservation cancelled', 'A hotel reservation is cancelled.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name","check_in","check_out"]'),
    ('RESERVATION_NO_SHOW', 'BOOKINGS', 'Reservation no-show', 'A guest did not show up for a reservation.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name","check_in"]'),
    ('RESERVATION_CHECKED_IN', 'BOOKINGS', 'Guest checked in', 'A guest checks into a reservation.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name"]'),
    ('RESERVATION_CHECKED_OUT', 'BOOKINGS', 'Guest checked out', 'A guest checks out of a reservation.',
        ARRAY['IN_APP'], 'RESERVATIONS', '["reservation_id","customer_name"]'),
    ('STAFF_INVITED', 'STAFF', 'Staff invited', 'A new staff member is invited.',
        ARRAY['IN_APP'], 'STAFF', '["user_id","user_name"]'),
    ('STAFF_ROLE_CHANGED', 'STAFF', 'Staff role changed', 'A staff member''s role changes.',
        ARRAY['IN_APP'], 'STAFF', '["user_id","user_name","new_role"]'),
    ('STAFF_DISABLED', 'STAFF', 'Staff disabled', 'A staff member''s access is disabled.',
        ARRAY['IN_APP'], 'STAFF', '["user_id","user_name"]'),
    ('STAFF_ENABLED', 'STAFF', 'Staff enabled', 'A staff member''s access is re-enabled.',
        ARRAY['IN_APP'], 'STAFF', '["user_id","user_name"]'),
    ('SECURITY_LOGIN_FAILED', 'SECURITY', 'Failed sign-in', 'A sign-in attempt fails.',
        ARRAY['IN_APP'], NULL, '["user_email"]'),
    ('SECURITY_MFA_ENABLED', 'SECURITY', 'Two-factor enabled', 'Two-factor authentication is enabled on an account.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('SECURITY_MFA_DISABLED', 'SECURITY', 'Two-factor disabled', 'Two-factor authentication is disabled on an account.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('SECURITY_PASSWORD_RESET', 'SECURITY', 'Password set via invite', 'A user sets their password through an invite link.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('SECURITY_PASSWORD_CHANGED', 'SECURITY', 'Password changed', 'A user changes their own password.',
        ARRAY['IN_APP'], NULL, '["user_id","user_name"]'),
    ('PLATFORM_SYSTEM_ERROR', 'PLATFORM', 'System error', 'An operational error needs platform attention.',
        ARRAY['IN_APP'], NULL, '["message"]'),
    ('PLATFORM_OPERATIONAL_ALERT', 'PLATFORM', 'Operational alert', 'A general operational alert for the platform.',
        ARRAY['IN_APP'], NULL, '["message"]');

-- Platform-default rules (tenant_id NULL) for every event above. IN_APP is
-- always on for its natural admin/platform audience. Customer-facing EMAIL
-- defaults on for the two order events already proven safe (storefront
-- always captures a contact at checkout); SMS/booking-customer rows exist but
-- start disabled, since no SMS provider is configured anywhere out of the
-- box. Security events are `locked` — a tenant cannot silently turn these
-- off.
INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('ORDER_PLACED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_READY', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_READY', 'EMAIL', TRUE, 'CUSTOMER', 'NORMAL', FALSE),
    ('ORDER_CANCELLED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_CANCELLED', 'EMAIL', TRUE, 'CUSTOMER', 'NORMAL', FALSE),
    ('PAYMENT_FAILED', 'IN_APP', TRUE, 'ROLE:organization', 'HIGH', FALSE),
    ('PAYMENT_FAILED', 'EMAIL', TRUE, 'CUSTOMER', 'HIGH', FALSE),
    ('BUSINESS_ONBOARDED', 'IN_APP', TRUE, 'PLATFORM', 'NORMAL', FALSE),
    ('SUBSCRIPTION_CHANGED', 'IN_APP', TRUE, 'PLATFORM', 'NORMAL', FALSE),
    ('SUBSCRIPTION_CHANGED', 'IN_APP', TRUE, 'ROLE:organization', 'NORMAL', FALSE),
    ('APPOINTMENT_CREATED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('APPOINTMENT_CREATED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('APPOINTMENT_CONFIRMED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('APPOINTMENT_CONFIRMED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('APPOINTMENT_CANCELLED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('APPOINTMENT_CANCELLED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('APPOINTMENT_NO_SHOW', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('RESERVATION_CREATED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('RESERVATION_CREATED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('RESERVATION_CONFIRMED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('RESERVATION_CONFIRMED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('RESERVATION_CANCELLED', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('RESERVATION_CANCELLED', 'SMS', FALSE, 'CUSTOMER', 'NORMAL', FALSE),
    ('RESERVATION_NO_SHOW', 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE),
    ('RESERVATION_CHECKED_IN', 'IN_APP', TRUE, 'ROLE:selling', 'LOW', FALSE),
    ('RESERVATION_CHECKED_OUT', 'IN_APP', TRUE, 'ROLE:selling', 'LOW', FALSE),
    ('STAFF_INVITED', 'IN_APP', TRUE, 'ROLE:staff', 'NORMAL', FALSE),
    ('STAFF_ROLE_CHANGED', 'IN_APP', TRUE, 'ROLE:staff', 'NORMAL', FALSE),
    ('STAFF_DISABLED', 'IN_APP', TRUE, 'ROLE:staff', 'NORMAL', FALSE),
    ('STAFF_ENABLED', 'IN_APP', TRUE, 'ROLE:staff', 'NORMAL', FALSE),
    ('SECURITY_LOGIN_FAILED', 'IN_APP', TRUE, 'ROLE:iam', 'HIGH', TRUE),
    ('SECURITY_MFA_ENABLED', 'IN_APP', TRUE, 'ROLE:iam', 'NORMAL', TRUE),
    ('SECURITY_MFA_DISABLED', 'IN_APP', TRUE, 'ROLE:iam', 'HIGH', TRUE),
    ('SECURITY_PASSWORD_RESET', 'IN_APP', TRUE, 'ROLE:iam', 'NORMAL', TRUE),
    ('SECURITY_PASSWORD_CHANGED', 'IN_APP', TRUE, 'ROLE:iam', 'NORMAL', TRUE),
    ('PLATFORM_SYSTEM_ERROR', 'IN_APP', TRUE, 'PLATFORM', 'CRITICAL', TRUE),
    ('PLATFORM_OPERATIONAL_ALERT', 'IN_APP', TRUE, 'PLATFORM', 'HIGH', TRUE);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS notification_preferences;
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notification_templates;
DROP TABLE IF EXISTS notification_rules;
DROP TABLE IF EXISTS notification_events;
-- +goose StatementEnd
