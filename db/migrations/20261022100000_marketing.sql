-- +goose Up
-- +goose StatementBegin

-- Marketing framework, Phase 1 and 2 (campaigns, consent, trackable links).
--
-- One engine, several channels. A campaign is tenant-scoped, picks a built-in
-- audience segment and one channel, and is executed by a worker that
-- materialises a recipient snapshot and sends through the same configsvc
-- providers (SMTP / SMS) transactional notifications use. Marketing never
-- writes to notification_* tables: a promotional send has consent, pause and
-- unsubscribe semantics that an order confirmation does not.

-- Plan gate. MARKETING is a Business-and-up feature; SMS campaigns also need
-- NOTIFY_SMS, which the worker re-checks at run time.
UPDATE plans SET entitlements = array_append(entitlements, 'MARKETING')
WHERE name = 'BUSINESS' AND NOT ('MARKETING' = ANY (entitlements));

-- Channel-and-purpose consent. One row per (customer, channel); REVOKED doubles
-- as the suppression list (unsubscribe, complaint, do-not-contact).
CREATE TABLE marketing_consents (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    customer_id UUID NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    channel     TEXT NOT NULL CHECK (channel IN ('EMAIL', 'SMS')),
    status      TEXT NOT NULL CHECK (status IN ('GRANTED', 'REVOKED')),
    -- Where the consent came from: OWNER (attested in the console),
    -- CUSTOMER (opted in on the storefront), UNSUBSCRIBE (link in a message).
    source      TEXT NOT NULL DEFAULT 'OWNER',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT marketing_consents_unique UNIQUE (tenant_id, customer_id, channel)
);
CREATE INDEX marketing_consents_tenant_channel_status_idx
    ON marketing_consents (tenant_id, channel, status);

-- Trackable links. The code is globally unique so /api/v1/go/{code} needs no
-- tenant host: the row names the tenant. target_path is a path on the
-- tenant's storefront, never an arbitrary URL, so a link can only ever send
-- someone to the shop that created it.
CREATE TABLE marketing_links (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    code        TEXT NOT NULL UNIQUE,
    label       TEXT NOT NULL DEFAULT '',
    target_path TEXT NOT NULL DEFAULT '/',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT marketing_links_target_path_check CHECK (target_path LIKE '/%')
);
CREATE INDEX marketing_links_tenant_idx ON marketing_links (tenant_id, created_at DESC);

CREATE TABLE marketing_campaigns (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    name          TEXT NOT NULL,
    goal          TEXT NOT NULL DEFAULT 'PROMOTE' CHECK (
        goal IN ('ACQUIRE', 'RETAIN', 'PROMOTE', 'FILL_SLOW_DAY', 'ANNOUNCE')
    ),
    audience      TEXT NOT NULL DEFAULT 'ALL' CHECK (
        audience IN ('ALL', 'NEW', 'REPEAT', 'INACTIVE', 'HIGH_VALUE')
    ),
    channel       TEXT NOT NULL CHECK (channel IN ('EMAIL', 'SMS')),
    subject       TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL DEFAULT '',
    button_label  TEXT NOT NULL DEFAULT '',
    link_id       UUID REFERENCES marketing_links (id) ON DELETE SET NULL,
    status        TEXT NOT NULL DEFAULT 'DRAFT' CHECK (
        status IN ('DRAFT', 'SCHEDULED', 'QUEUED', 'RUNNING', 'PAUSED',
                   'COMPLETED', 'PARTIALLY_FAILED', 'FAILED', 'CANCELLED')
    ),
    status_reason TEXT NOT NULL DEFAULT '',
    scheduled_at  TIMESTAMPTZ,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    total_count   INT NOT NULL DEFAULT 0,
    sent_count    INT NOT NULL DEFAULT 0,
    failed_count  INT NOT NULL DEFAULT 0,
    skipped_count INT NOT NULL DEFAULT 0,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX marketing_campaigns_tenant_idx ON marketing_campaigns (tenant_id, created_at DESC);
-- The worker's two scans: due schedules and running campaigns.
CREATE INDEX marketing_campaigns_due_idx
    ON marketing_campaigns (scheduled_at) WHERE status = 'SCHEDULED';
CREATE INDEX marketing_campaigns_running_idx
    ON marketing_campaigns (id) WHERE status = 'RUNNING';

-- Recipient snapshot, one row per customer per campaign. The unique key is
-- what makes a retried materialisation or a duplicate worker idempotent.
CREATE TABLE marketing_recipients (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id         UUID NOT NULL REFERENCES marketing_campaigns (id) ON DELETE CASCADE,
    tenant_id           UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    customer_id         UUID NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    address             TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'PENDING' CHECK (
        status IN ('PENDING', 'RETRYING', 'SENT', 'FAILED', 'SKIPPED')
    ),
    attempt_count       INT NOT NULL DEFAULT 0,
    last_error          TEXT NOT NULL DEFAULT '',
    provider_message_id TEXT NOT NULL DEFAULT '',
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at             TIMESTAMPTZ,
    CONSTRAINT marketing_recipients_unique UNIQUE (campaign_id, customer_id)
);
CREATE INDEX marketing_recipients_due_idx
    ON marketing_recipients (next_attempt_at) WHERE status IN ('PENDING', 'RETRYING');
CREATE INDEX marketing_recipients_campaign_status_idx
    ON marketing_recipients (campaign_id, status);

-- Observed events: a link visit or an unsubscribe. Attribution to orders is a
-- later phase; nothing here claims a conversion.
CREATE TABLE marketing_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    kind        TEXT NOT NULL CHECK (kind IN ('VISIT', 'UNSUBSCRIBE')),
    link_id     UUID REFERENCES marketing_links (id) ON DELETE CASCADE,
    campaign_id UUID REFERENCES marketing_campaigns (id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX marketing_events_tenant_kind_created_idx
    ON marketing_events (tenant_id, kind, created_at DESC);
CREATE INDEX marketing_events_link_idx ON marketing_events (link_id) WHERE link_id IS NOT NULL;

-- Per-customer marketing profile, computed on read. Visits are orders,
-- appointments and reservations that were not cancelled or no-shows. The
-- segment rules (internal/marketing) and the recipient snapshot both read
-- this view, so there is exactly one definition of "who is in the audience".
CREATE VIEW marketing_audience AS
WITH visits AS (
    SELECT tenant_id, customer_id, created_at
    FROM orders
    WHERE customer_id IS NOT NULL AND status <> 'CANCELLED'
    UNION ALL
    SELECT tenant_id, customer_id, created_at
    FROM appointments
    WHERE customer_id IS NOT NULL AND status NOT IN ('CANCELLED', 'NO_SHOW')
    UNION ALL
    SELECT tenant_id, customer_id, created_at
    FROM reservations
    WHERE customer_id IS NOT NULL AND status NOT IN ('CANCELLED', 'NO_SHOW')
),
agg AS (
    SELECT tenant_id, customer_id, COUNT(*)::bigint AS visit_count, MAX(created_at) AS last_visit_at
    FROM visits
    GROUP BY tenant_id, customer_id
)
SELECT
    c.tenant_id,
    c.id                                   AS customer_id,
    c.name,
    COALESCE(c.email, '')                  AS email,
    COALESCE(c.phone, '')                  AS phone,
    c.is_blocked,
    c.created_at,
    COALESCE(a.visit_count, 0)::bigint     AS visit_count,
    a.last_visit_at,
    COALESCE(ce.status, 'UNKNOWN')         AS email_consent,
    COALESCE(cs.status, 'UNKNOWN')         AS sms_consent
FROM customers c
LEFT JOIN agg a  ON a.tenant_id = c.tenant_id AND a.customer_id = c.id
LEFT JOIN marketing_consents ce ON ce.tenant_id = c.tenant_id AND ce.customer_id = c.id AND ce.channel = 'EMAIL'
LEFT JOIN marketing_consents cs ON cs.tenant_id = c.tenant_id AND cs.customer_id = c.id AND cs.channel = 'SMS';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS marketing_audience;
DROP TABLE IF EXISTS marketing_events;
DROP TABLE IF EXISTS marketing_recipients;
DROP TABLE IF EXISTS marketing_campaigns;
DROP TABLE IF EXISTS marketing_links;
DROP TABLE IF EXISTS marketing_consents;
UPDATE plans SET entitlements = array_remove(entitlements, 'MARKETING');
-- +goose StatementEnd
