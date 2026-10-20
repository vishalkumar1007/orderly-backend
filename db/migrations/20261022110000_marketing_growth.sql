-- +goose Up
-- +goose StatementBegin

-- Marketing framework, Phases 3 to 5: WhatsApp channel (Meta Cloud API),
-- offers and loyalty, automations, social publishing, order attribution and
-- storefront consent capture.

/* ---------- META as a configurable service (WhatsApp, Facebook, Instagram) ---------- */

ALTER TABLE platform_configurations
    DROP CONSTRAINT platform_configurations_service_check,
    ADD CONSTRAINT platform_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS', 'META'));
ALTER TABLE tenant_configurations
    DROP CONSTRAINT tenant_configurations_service_check,
    ADD CONSTRAINT tenant_configurations_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS', 'META'));
ALTER TABLE tenant_service_preferences
    DROP CONSTRAINT tenant_service_preferences_service_check,
    ADD CONSTRAINT tenant_service_preferences_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS', 'META'));
ALTER TABLE tenant_service_access
    DROP CONSTRAINT tenant_service_access_service_check,
    ADD CONSTRAINT tenant_service_access_service_check
        CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS', 'META'));

-- The webhook maps an inbound WhatsApp event to a tenant by phone_number_id.
CREATE INDEX tenant_configurations_meta_phone_idx
    ON tenant_configurations ((config->>'phone_number_id')) WHERE service_type = 'META';

/* ---------- WhatsApp channel ---------- */

ALTER TABLE marketing_consents DROP CONSTRAINT marketing_consents_channel_check;
ALTER TABLE marketing_consents ADD CONSTRAINT marketing_consents_channel_check
    CHECK (channel IN ('EMAIL', 'SMS', 'WHATSAPP'));

ALTER TABLE marketing_campaigns DROP CONSTRAINT marketing_campaigns_channel_check;
ALTER TABLE marketing_campaigns ADD CONSTRAINT marketing_campaigns_channel_check
    CHECK (channel IN ('EMAIL', 'SMS', 'WHATSAPP'));
-- WhatsApp only sends pre-approved templates; body fills the template's {{1}}.
ALTER TABLE marketing_campaigns
    ADD COLUMN template_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN template_lang TEXT NOT NULL DEFAULT 'en',
    -- CAMPAIGN is owner-made; AUTOMATION rows are created by a rule run.
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'CAMPAIGN' CHECK (kind IN ('CAMPAIGN', 'AUTOMATION')),
    ADD COLUMN promotion_id UUID;

-- Provider delivery receipts (WhatsApp statuses) land here by message id.
ALTER TABLE marketing_recipients DROP CONSTRAINT marketing_recipients_status_check;
ALTER TABLE marketing_recipients ADD CONSTRAINT marketing_recipients_status_check
    CHECK (status IN ('PENDING', 'RETRYING', 'SENT', 'DELIVERED', 'READ', 'FAILED', 'SKIPPED'));
CREATE INDEX marketing_recipients_provider_msg_idx
    ON marketing_recipients (provider_message_id) WHERE provider_message_id <> '';

/* ---------- offers ---------- */

CREATE TABLE promotions (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL CHECK (kind IN ('PERCENT', 'FIXED')),
    value              NUMERIC(12, 2) NOT NULL CHECK (value > 0),
    min_subtotal       NUMERIC(12, 2) NOT NULL DEFAULT 0 CHECK (min_subtotal >= 0),
    max_discount       NUMERIC(12, 2) NOT NULL DEFAULT 0 CHECK (max_discount >= 0),
    starts_at          TIMESTAMPTZ,
    ends_at            TIMESTAMPTZ,
    max_redemptions    INT NOT NULL DEFAULT 0 CHECK (max_redemptions >= 0),
    per_customer_limit INT NOT NULL DEFAULT 0 CHECK (per_customer_limit >= 0),
    first_order_only   BOOLEAN NOT NULL DEFAULT FALSE,
    active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT promotions_code_unique UNIQUE (tenant_id, code),
    CONSTRAINT promotions_percent_range CHECK (kind <> 'PERCENT' OR value <= 100)
);
CREATE INDEX promotions_tenant_idx ON promotions (tenant_id, created_at DESC);

-- One row per order that used an offer. Written inside the order transaction,
-- so a redemption and its order commit together or not at all.
CREATE TABLE promotion_redemptions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    promotion_id UUID NOT NULL REFERENCES promotions (id) ON DELETE CASCADE,
    order_id     UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    customer_id  UUID REFERENCES customers (id) ON DELETE SET NULL,
    phone        TEXT NOT NULL DEFAULT '',
    discount     NUMERIC(12, 2) NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT promotion_redemptions_order_unique UNIQUE (order_id)
);
CREATE INDEX promotion_redemptions_promo_idx ON promotion_redemptions (promotion_id);
CREATE INDEX promotion_redemptions_phone_idx ON promotion_redemptions (promotion_id, phone);

ALTER TABLE marketing_campaigns
    ADD CONSTRAINT marketing_campaigns_promotion_fkey
        FOREIGN KEY (promotion_id) REFERENCES promotions (id) ON DELETE SET NULL;

/* ---------- loyalty ---------- */

CREATE TABLE loyalty_programs (
    tenant_id              UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE RESTRICT,
    enabled                BOOLEAN NOT NULL DEFAULT FALSE,
    -- Points earned per 100 units of currency spent (completed orders).
    points_per_100         INT NOT NULL DEFAULT 10 CHECK (points_per_100 >= 0),
    -- Currency value of one point at redemption.
    point_value            NUMERIC(12, 4) NOT NULL DEFAULT 0.10 CHECK (point_value >= 0),
    min_redeem_points      INT NOT NULL DEFAULT 100 CHECK (min_redeem_points >= 0),
    -- A redemption may cover at most this share of the subtotal (percent).
    max_redeem_percent     INT NOT NULL DEFAULT 50 CHECK (max_redeem_percent BETWEEN 0 AND 100),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE loyalty_accounts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    customer_id     UUID NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    balance         INT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    lifetime_earned INT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT loyalty_accounts_unique UNIQUE (tenant_id, customer_id)
);

-- The ledger. Balances are derivable from this table; a mismatch is a bug.
CREATE TABLE loyalty_transactions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    account_id UUID NOT NULL REFERENCES loyalty_accounts (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('EARN', 'REDEEM', 'ADJUST', 'REVERSAL')),
    points     INT NOT NULL,
    order_id   UUID REFERENCES orders (id) ON DELETE SET NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX loyalty_transactions_account_idx ON loyalty_transactions (account_id, created_at DESC);
-- An order earns once and is redeemed against once, however many times a
-- retried transition or submit asks.
CREATE UNIQUE INDEX loyalty_transactions_order_kind_unique
    ON loyalty_transactions (order_id, kind) WHERE order_id IS NOT NULL AND kind IN ('EARN', 'REDEEM');

/* ---------- order attribution and consent capture ---------- */

ALTER TABLE orders
    ADD COLUMN marketing_campaign_id UUID REFERENCES marketing_campaigns (id) ON DELETE SET NULL,
    ADD COLUMN marketing_link_id     UUID REFERENCES marketing_links (id) ON DELETE SET NULL,
    ADD COLUMN promotion_id          UUID REFERENCES promotions (id) ON DELETE SET NULL,
    ADD COLUMN coupon_code           TEXT NOT NULL DEFAULT '',
    ADD COLUMN loyalty_points_used   INT NOT NULL DEFAULT 0 CHECK (loyalty_points_used >= 0);
CREATE INDEX orders_marketing_campaign_idx
    ON orders (marketing_campaign_id) WHERE marketing_campaign_id IS NOT NULL;
CREATE INDEX orders_marketing_link_idx
    ON orders (marketing_link_id) WHERE marketing_link_id IS NOT NULL;

/* ---------- automations ---------- */

CREATE TABLE automation_rules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    name           TEXT NOT NULL,
    trigger        TEXT NOT NULL CHECK (trigger IN ('WELCOME', 'ORDER_FOLLOW_UP', 'WIN_BACK')),
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    channel        TEXT NOT NULL CHECK (channel IN ('EMAIL', 'SMS', 'WHATSAPP')),
    -- ORDER_FOLLOW_UP: hours after completion. WIN_BACK: days without a visit.
    delay_hours    INT NOT NULL DEFAULT 24 CHECK (delay_hours BETWEEN 0 AND 720),
    inactive_days  INT NOT NULL DEFAULT 45 CHECK (inactive_days BETWEEN 1 AND 365),
    -- Frequency cap: one message per customer per rule within this many days.
    cooldown_days  INT NOT NULL DEFAULT 30 CHECK (cooldown_days BETWEEN 0 AND 365),
    -- Quiet hours on the business clock (tenants.timezone): no sends between.
    quiet_start    INT NOT NULL DEFAULT 21 CHECK (quiet_start BETWEEN 0 AND 23),
    quiet_end      INT NOT NULL DEFAULT 9 CHECK (quiet_end BETWEEN 0 AND 23),
    subject        TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    button_label   TEXT NOT NULL DEFAULT '',
    link_id        UUID REFERENCES marketing_links (id) ON DELETE SET NULL,
    promotion_id   UUID REFERENCES promotions (id) ON DELETE SET NULL,
    template_name  TEXT NOT NULL DEFAULT '',
    template_lang  TEXT NOT NULL DEFAULT 'en',
    last_run_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX automation_rules_tenant_idx ON automation_rules (tenant_id, created_at DESC);
CREATE INDEX automation_rules_enabled_idx ON automation_rules (enabled) WHERE enabled;

-- Idempotency for rule evaluation: a (rule, customer, trigger key) fires once.
CREATE TABLE automation_runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id     UUID NOT NULL REFERENCES automation_rules (id) ON DELETE CASCADE,
    tenant_id   UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    customer_id UUID NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    trigger_key TEXT NOT NULL,
    campaign_id UUID REFERENCES marketing_campaigns (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT automation_runs_unique UNIQUE (rule_id, customer_id, trigger_key)
);
CREATE INDEX automation_runs_rule_customer_idx ON automation_runs (rule_id, customer_id, created_at DESC);

ALTER TABLE marketing_campaigns
    ADD COLUMN automation_rule_id UUID REFERENCES automation_rules (id) ON DELETE SET NULL;

/* ---------- social publishing ---------- */

CREATE TABLE social_posts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    caption      TEXT NOT NULL DEFAULT '',
    image_url    TEXT NOT NULL DEFAULT '',
    platforms    TEXT[] NOT NULL DEFAULT '{}',
    link_id      UUID REFERENCES marketing_links (id) ON DELETE SET NULL,
    status       TEXT NOT NULL DEFAULT 'DRAFT' CHECK (
        status IN ('DRAFT', 'SCHEDULED', 'PUBLISHING', 'PUBLISHED', 'PARTIAL', 'FAILED', 'MANUAL')
    ),
    scheduled_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    -- Per platform: {"FACEBOOK": {"ok": true, "id": "..."}, "INSTAGRAM": {"ok": false, "error": "..."}}
    results      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX social_posts_tenant_idx ON social_posts (tenant_id, created_at DESC);
CREATE INDEX social_posts_due_idx ON social_posts (scheduled_at) WHERE status = 'SCHEDULED';

/* ---------- audience view: WhatsApp consent and loyalty balance ---------- */

DROP VIEW marketing_audience;
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
    COALESCE(cs.status, 'UNKNOWN')         AS sms_consent,
    COALESCE(cw.status, 'UNKNOWN')         AS whatsapp_consent,
    COALESCE(la.balance, 0)::int           AS loyalty_points
FROM customers c
LEFT JOIN agg a  ON a.tenant_id = c.tenant_id AND a.customer_id = c.id
LEFT JOIN marketing_consents ce ON ce.tenant_id = c.tenant_id AND ce.customer_id = c.id AND ce.channel = 'EMAIL'
LEFT JOIN marketing_consents cs ON cs.tenant_id = c.tenant_id AND cs.customer_id = c.id AND cs.channel = 'SMS'
LEFT JOIN marketing_consents cw ON cw.tenant_id = c.tenant_id AND cw.customer_id = c.id AND cw.channel = 'WHATSAPP'
LEFT JOIN loyalty_accounts la   ON la.tenant_id = c.tenant_id AND la.customer_id = c.id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS marketing_audience;
CREATE VIEW marketing_audience AS
WITH visits AS (
    SELECT tenant_id, customer_id, created_at FROM orders WHERE customer_id IS NOT NULL AND status <> 'CANCELLED'
    UNION ALL
    SELECT tenant_id, customer_id, created_at FROM appointments WHERE customer_id IS NOT NULL AND status NOT IN ('CANCELLED', 'NO_SHOW')
    UNION ALL
    SELECT tenant_id, customer_id, created_at FROM reservations WHERE customer_id IS NOT NULL AND status NOT IN ('CANCELLED', 'NO_SHOW')
),
agg AS (
    SELECT tenant_id, customer_id, COUNT(*)::bigint AS visit_count, MAX(created_at) AS last_visit_at FROM visits GROUP BY tenant_id, customer_id
)
SELECT c.tenant_id, c.id AS customer_id, c.name, COALESCE(c.email, '') AS email, COALESCE(c.phone, '') AS phone,
       c.is_blocked, c.created_at, COALESCE(a.visit_count, 0)::bigint AS visit_count, a.last_visit_at,
       COALESCE(ce.status, 'UNKNOWN') AS email_consent, COALESCE(cs.status, 'UNKNOWN') AS sms_consent
FROM customers c
LEFT JOIN agg a ON a.tenant_id = c.tenant_id AND a.customer_id = c.id
LEFT JOIN marketing_consents ce ON ce.tenant_id = c.tenant_id AND ce.customer_id = c.id AND ce.channel = 'EMAIL'
LEFT JOIN marketing_consents cs ON cs.tenant_id = c.tenant_id AND cs.customer_id = c.id AND cs.channel = 'SMS';

DROP TABLE IF EXISTS social_posts;
ALTER TABLE marketing_campaigns DROP COLUMN IF EXISTS automation_rule_id;
DROP TABLE IF EXISTS automation_runs;
DROP TABLE IF EXISTS automation_rules;
ALTER TABLE orders
    DROP COLUMN IF EXISTS loyalty_points_used,
    DROP COLUMN IF EXISTS coupon_code,
    DROP COLUMN IF EXISTS promotion_id,
    DROP COLUMN IF EXISTS marketing_link_id,
    DROP COLUMN IF EXISTS marketing_campaign_id;
DROP TABLE IF EXISTS loyalty_transactions;
DROP TABLE IF EXISTS loyalty_accounts;
DROP TABLE IF EXISTS loyalty_programs;
ALTER TABLE marketing_campaigns DROP CONSTRAINT IF EXISTS marketing_campaigns_promotion_fkey;
DROP TABLE IF EXISTS promotion_redemptions;
DROP TABLE IF EXISTS promotions;
DROP INDEX IF EXISTS marketing_recipients_provider_msg_idx;
UPDATE marketing_recipients SET status = 'SENT' WHERE status IN ('DELIVERED', 'READ');
ALTER TABLE marketing_recipients DROP CONSTRAINT marketing_recipients_status_check;
ALTER TABLE marketing_recipients ADD CONSTRAINT marketing_recipients_status_check
    CHECK (status IN ('PENDING', 'RETRYING', 'SENT', 'FAILED', 'SKIPPED'));
DELETE FROM marketing_campaigns WHERE channel = 'WHATSAPP';
ALTER TABLE marketing_campaigns
    DROP COLUMN IF EXISTS promotion_id,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS template_lang,
    DROP COLUMN IF EXISTS template_name;
ALTER TABLE marketing_campaigns DROP CONSTRAINT marketing_campaigns_channel_check;
ALTER TABLE marketing_campaigns ADD CONSTRAINT marketing_campaigns_channel_check CHECK (channel IN ('EMAIL', 'SMS'));
DELETE FROM marketing_consents WHERE channel = 'WHATSAPP';
ALTER TABLE marketing_consents DROP CONSTRAINT marketing_consents_channel_check;
ALTER TABLE marketing_consents ADD CONSTRAINT marketing_consents_channel_check CHECK (channel IN ('EMAIL', 'SMS'));
DROP INDEX IF EXISTS tenant_configurations_meta_phone_idx;
DELETE FROM tenant_service_access WHERE service_type = 'META';
DELETE FROM tenant_service_preferences WHERE service_type = 'META';
DELETE FROM tenant_configurations WHERE service_type = 'META';
DELETE FROM platform_configurations WHERE service_type = 'META';
ALTER TABLE platform_configurations DROP CONSTRAINT platform_configurations_service_check,
    ADD CONSTRAINT platform_configurations_service_check CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));
ALTER TABLE tenant_configurations DROP CONSTRAINT tenant_configurations_service_check,
    ADD CONSTRAINT tenant_configurations_service_check CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));
ALTER TABLE tenant_service_preferences DROP CONSTRAINT tenant_service_preferences_service_check,
    ADD CONSTRAINT tenant_service_preferences_service_check CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));
ALTER TABLE tenant_service_access DROP CONSTRAINT tenant_service_access_service_check,
    ADD CONSTRAINT tenant_service_access_service_check CHECK (service_type IN ('SMTP', 'STORAGE', 'AI', 'SMS', 'PAYMENTS'));
-- +goose StatementEnd
