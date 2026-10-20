-- Marketing framework. Every query is tenant-scoped except the worker claims
-- and the public /go and unsubscribe lookups, which address a globally unique
-- id and read the tenant from the row.

/* ---------- audience ---------- */

-- name: PreviewAudience :one
-- Who a campaign would reach and why the rest are left out. The segment rule
-- lives in SQL once (here and in InsertCampaignRecipients, kept identical).
SELECT
    COUNT(*)::bigint AS matched,
    COUNT(*) FILTER (WHERE is_blocked)::bigint AS blocked,
    COUNT(*) FILTER (WHERE NOT is_blocked
        AND ((sqlc.arg(channel)::text = 'EMAIL' AND email = '')
          OR (sqlc.arg(channel)::text <> 'EMAIL' AND phone = '')))::bigint AS no_address,
    COUNT(*) FILTER (WHERE NOT is_blocked
        AND ((sqlc.arg(channel)::text = 'EMAIL'    AND email <> '' AND email_consent    <> 'GRANTED')
          OR (sqlc.arg(channel)::text = 'SMS'      AND phone <> '' AND sms_consent      <> 'GRANTED')
          OR (sqlc.arg(channel)::text = 'WHATSAPP' AND phone <> '' AND whatsapp_consent <> 'GRANTED')))::bigint AS no_consent,
    COUNT(*) FILTER (WHERE NOT is_blocked
        AND ((sqlc.arg(channel)::text = 'EMAIL'    AND email <> '' AND email_consent    = 'GRANTED')
          OR (sqlc.arg(channel)::text = 'SMS'      AND phone <> '' AND sms_consent      = 'GRANTED')
          OR (sqlc.arg(channel)::text = 'WHATSAPP' AND phone <> '' AND whatsapp_consent = 'GRANTED')))::bigint AS eligible
FROM marketing_audience
WHERE tenant_id = sqlc.arg(tenant_id)
  AND CASE sqlc.arg(audience)::text
        WHEN 'NEW'        THEN created_at >= now() - interval '30 days'
        WHEN 'REPEAT'     THEN visit_count >= 2
        WHEN 'INACTIVE'   THEN visit_count >= 1 AND last_visit_at < now() - interval '60 days'
        WHEN 'HIGH_VALUE' THEN visit_count >= 4
        ELSE TRUE
      END;

-- name: InsertCampaignRecipients :execrows
-- Materialise the recipient snapshot. ON CONFLICT makes a retried
-- materialisation a no-op, so a crash between this and the status update
-- cannot double a recipient.
INSERT INTO marketing_recipients (campaign_id, tenant_id, customer_id, address)
SELECT sqlc.arg(campaign_id), a.tenant_id, a.customer_id,
       CASE WHEN sqlc.arg(channel)::text = 'EMAIL' THEN a.email ELSE a.phone END
FROM marketing_audience a
WHERE a.tenant_id = sqlc.arg(tenant_id)
  AND NOT a.is_blocked
  AND ((sqlc.arg(channel)::text = 'EMAIL'    AND a.email <> '' AND a.email_consent    = 'GRANTED')
    OR (sqlc.arg(channel)::text = 'SMS'      AND a.phone <> '' AND a.sms_consent      = 'GRANTED')
    OR (sqlc.arg(channel)::text = 'WHATSAPP' AND a.phone <> '' AND a.whatsapp_consent = 'GRANTED'))
  AND CASE sqlc.arg(audience)::text
        WHEN 'NEW'        THEN a.created_at >= now() - interval '30 days'
        WHEN 'REPEAT'     THEN a.visit_count >= 2
        WHEN 'INACTIVE'   THEN a.visit_count >= 1 AND a.last_visit_at < now() - interval '60 days'
        WHEN 'HIGH_VALUE' THEN a.visit_count >= 4
        ELSE TRUE
      END
ON CONFLICT (campaign_id, customer_id) DO NOTHING;

/* ---------- consent ---------- */

-- name: ConsentSummary :one
SELECT
    COUNT(*)::bigint AS customers,
    COUNT(*) FILTER (WHERE email <> '')::bigint AS with_email,
    COUNT(*) FILTER (WHERE phone <> '')::bigint AS with_phone,
    COUNT(*) FILTER (WHERE email_consent = 'GRANTED')::bigint AS email_granted,
    COUNT(*) FILTER (WHERE sms_consent = 'GRANTED')::bigint AS sms_granted,
    COUNT(*) FILTER (WHERE whatsapp_consent = 'GRANTED')::bigint AS whatsapp_granted,
    COUNT(*) FILTER (WHERE email_consent = 'REVOKED' OR sms_consent = 'REVOKED' OR whatsapp_consent = 'REVOKED')::bigint AS revoked
FROM marketing_audience
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: UpsertConsent :exec
INSERT INTO marketing_consents (tenant_id, customer_id, channel, status, source)
VALUES (sqlc.arg(tenant_id), sqlc.arg(customer_id), sqlc.arg(channel), sqlc.arg(status), sqlc.arg(source))
ON CONFLICT (tenant_id, customer_id, channel)
DO UPDATE SET status = EXCLUDED.status, source = EXCLUDED.source, updated_at = now();

-- name: GrantConsentForAll :execrows
-- Owner attestation: every customer with an address on this channel who has
-- not revoked. A REVOKED row is never overwritten by a bulk grant.
INSERT INTO marketing_consents (tenant_id, customer_id, channel, status, source)
SELECT a.tenant_id, a.customer_id, sqlc.arg(channel), 'GRANTED', 'OWNER'
FROM marketing_audience a
WHERE a.tenant_id = sqlc.arg(tenant_id)
  AND ((sqlc.arg(channel)::text = 'EMAIL' AND a.email <> '') OR (sqlc.arg(channel)::text <> 'EMAIL' AND a.phone <> ''))
ON CONFLICT (tenant_id, customer_id, channel) DO NOTHING;

-- name: ListConsentsForCustomer :many
SELECT channel, status FROM marketing_consents
WHERE tenant_id = sqlc.arg(tenant_id) AND customer_id = sqlc.arg(customer_id);

-- name: RevokeConsentByPhone :execrows
-- Inbound STOP on WhatsApp or SMS: the sender's number is the identity.
INSERT INTO marketing_consents (tenant_id, customer_id, channel, status, source)
SELECT c.tenant_id, c.id, sqlc.arg(channel), 'REVOKED', 'UNSUBSCRIBE'
FROM customers c
WHERE c.tenant_id = sqlc.arg(tenant_id) AND c.phone = sqlc.arg(phone)::text
ON CONFLICT (tenant_id, customer_id, channel)
DO UPDATE SET status = 'REVOKED', source = 'UNSUBSCRIBE', updated_at = now();

-- name: GetConsent :one
SELECT status FROM marketing_consents
WHERE tenant_id = sqlc.arg(tenant_id) AND customer_id = sqlc.arg(customer_id) AND channel = sqlc.arg(channel);

/* ---------- links ---------- */

-- name: CreateLink :one
INSERT INTO marketing_links (tenant_id, code, label, target_path)
VALUES (sqlc.arg(tenant_id), sqlc.arg(code), sqlc.arg(label), sqlc.arg(target_path))
RETURNING *;

-- name: ListLinks :many
SELECT l.*, COALESCE(v.visits, 0)::bigint AS visits
FROM marketing_links l
LEFT JOIN (
    SELECT link_id, COUNT(*) AS visits FROM marketing_events
    WHERE kind = 'VISIT' GROUP BY link_id
) v ON v.link_id = l.id
WHERE l.tenant_id = sqlc.arg(tenant_id)
ORDER BY l.created_at DESC;

-- name: GetLink :one
SELECT * FROM marketing_links WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: DeleteLink :execrows
DELETE FROM marketing_links WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: GetLinkByCode :one
-- Public redirect: the code is the credential, the tenant comes from the row.
SELECT l.id, l.tenant_id, l.target_path, t.slug AS tenant_slug
FROM marketing_links l
JOIN tenants t ON t.id = l.tenant_id
WHERE l.code = sqlc.arg(code) AND t.status = 'ACTIVE';

-- name: InsertEvent :exec
INSERT INTO marketing_events (tenant_id, kind, link_id, campaign_id)
VALUES (sqlc.arg(tenant_id), sqlc.arg(kind), sqlc.arg(link_id), sqlc.arg(campaign_id));

/* ---------- campaigns ---------- */

-- name: CreateCampaign :one
INSERT INTO marketing_campaigns (tenant_id, name, goal, audience, channel, subject, body, button_label, link_id, created_by,
                                 template_name, template_lang, promotion_id)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), sqlc.arg(goal), sqlc.arg(audience), sqlc.arg(channel),
        sqlc.arg(subject), sqlc.arg(body), sqlc.arg(button_label), sqlc.arg(link_id), sqlc.arg(created_by),
        sqlc.arg(template_name), sqlc.arg(template_lang), sqlc.arg(promotion_id))
RETURNING *;

-- name: UpdateCampaignDraft :one
-- Content and audience are only editable before the campaign is scheduled.
UPDATE marketing_campaigns
SET name = sqlc.arg(name), goal = sqlc.arg(goal), audience = sqlc.arg(audience), channel = sqlc.arg(channel),
    subject = sqlc.arg(subject), body = sqlc.arg(body), button_label = sqlc.arg(button_label),
    link_id = sqlc.arg(link_id), template_name = sqlc.arg(template_name), template_lang = sqlc.arg(template_lang),
    promotion_id = sqlc.arg(promotion_id), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status = 'DRAFT'
RETURNING *;

-- name: ListCampaigns :many
SELECT * FROM marketing_campaigns
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
ORDER BY created_at DESC
LIMIT sqlc.arg(limit_count);

-- name: CampaignAttribution :one
-- Orders placed through this campaign's tracked link (observed attribution).
SELECT COUNT(*)::bigint AS orders, COALESCE(SUM(total), 0)::numeric AS revenue
FROM orders
WHERE marketing_campaign_id = sqlc.arg(campaign_id) AND tenant_id = sqlc.arg(tenant_id) AND status <> 'CANCELLED';

-- name: GetCampaign :one
SELECT * FROM marketing_campaigns WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: SetCampaignStatus :one
-- A guarded transition: the caller names the states it may move from, so two
-- concurrent actions cannot both win.
UPDATE marketing_campaigns
SET status = sqlc.arg(status), status_reason = sqlc.arg(status_reason),
    -- "Send now" takes the database clock, so a worker on the same database
    -- always sees it as due, whatever the API host's clock says.
    scheduled_at = CASE WHEN sqlc.arg(send_now)::bool THEN now() ELSE COALESCE(sqlc.narg(scheduled_at), scheduled_at) END,
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
  AND status = ANY (sqlc.arg(from_statuses)::text[])
RETURNING *;

-- name: DeleteDraftCampaign :execrows
DELETE FROM marketing_campaigns
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status = 'DRAFT';

-- name: SkipPendingRecipients :exec
-- On cancel: nothing still queued goes out.
UPDATE marketing_recipients
SET status = 'SKIPPED', last_error = 'campaign cancelled'
WHERE campaign_id = sqlc.arg(campaign_id) AND status IN ('PENDING', 'RETRYING');

-- name: ListCampaignRecipients :many
SELECT r.id, r.address, r.status, r.attempt_count, r.last_error, r.sent_at, c.name AS customer_name
FROM marketing_recipients r
JOIN customers c ON c.id = r.customer_id
WHERE r.campaign_id = sqlc.arg(campaign_id) AND r.tenant_id = sqlc.arg(tenant_id)
ORDER BY r.status, r.sent_at DESC NULLS LAST
LIMIT sqlc.arg(limit_count);

-- name: CampaignVisits :one
SELECT COUNT(*)::bigint FROM marketing_events
WHERE campaign_id = sqlc.arg(campaign_id) AND tenant_id = sqlc.arg(tenant_id) AND kind = 'VISIT';

/* ---------- overview ---------- */

-- name: MarketingOverview :one
SELECT
    (SELECT COUNT(*) FROM marketing_campaigns mc WHERE mc.tenant_id = sqlc.arg(tenant_id))::bigint AS campaigns,
    (SELECT COUNT(*) FROM marketing_campaigns mc WHERE mc.tenant_id = sqlc.arg(tenant_id)
        AND mc.status IN ('SCHEDULED', 'QUEUED', 'RUNNING', 'PAUSED'))::bigint AS active_campaigns,
    (SELECT COALESCE(SUM(mc.sent_count), 0) FROM marketing_campaigns mc WHERE mc.tenant_id = sqlc.arg(tenant_id))::bigint AS messages_sent,
    (SELECT COALESCE(SUM(mc.failed_count), 0) FROM marketing_campaigns mc WHERE mc.tenant_id = sqlc.arg(tenant_id))::bigint AS messages_failed,
    (SELECT COUNT(*) FROM marketing_events me WHERE me.tenant_id = sqlc.arg(tenant_id) AND me.kind = 'VISIT'
        AND me.created_at >= now() - interval '30 days')::bigint AS visits_30d,
    (SELECT COUNT(*) FROM marketing_events me WHERE me.tenant_id = sqlc.arg(tenant_id) AND me.kind = 'UNSUBSCRIBE'
        AND me.created_at >= now() - interval '30 days')::bigint AS unsubscribes_30d,
    (SELECT COUNT(*) FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status <> 'CANCELLED'
        AND (o.marketing_campaign_id IS NOT NULL OR o.marketing_link_id IS NOT NULL OR o.promotion_id IS NOT NULL)
        AND o.created_at >= now() - interval '30 days')::bigint AS attributed_orders_30d,
    (SELECT COALESCE(SUM(o.total), 0) FROM orders o WHERE o.tenant_id = sqlc.arg(tenant_id) AND o.status <> 'CANCELLED'
        AND (o.marketing_campaign_id IS NOT NULL OR o.marketing_link_id IS NOT NULL OR o.promotion_id IS NOT NULL)
        AND o.created_at >= now() - interval '30 days')::numeric AS attributed_revenue_30d;

/* ---------- worker ---------- */

-- name: ClaimDueCampaigns :many
-- SCHEDULED -> QUEUED for anything whose time has come. SKIP LOCKED lets a
-- second API process run the same loop without double-claiming.
UPDATE marketing_campaigns
SET status = 'QUEUED', updated_at = now()
WHERE id IN (
    SELECT id FROM marketing_campaigns
    WHERE status = 'SCHEDULED' AND scheduled_at <= now()
    ORDER BY scheduled_at
    LIMIT sqlc.arg(limit_count)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: StartCampaign :exec
UPDATE marketing_campaigns
SET status = 'RUNNING', total_count = sqlc.arg(total_count), started_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'QUEUED';

-- name: FinishCampaign :exec
UPDATE marketing_campaigns
SET status = sqlc.arg(status), status_reason = sqlc.arg(status_reason), finished_at = now(), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ListDueSendGroups :many
-- Which (tenant, channel) pairs have work. The worker claims a capped slice
-- from each, so one shop's big campaign cannot starve the others.
SELECT r.tenant_id, c.channel, COUNT(*)::bigint AS due
FROM marketing_recipients r
JOIN marketing_campaigns c ON c.id = r.campaign_id
WHERE r.status IN ('PENDING', 'RETRYING') AND r.next_attempt_at <= now()
  AND c.status = 'RUNNING'
GROUP BY r.tenant_id, c.channel
ORDER BY MIN(r.next_attempt_at)
LIMIT 50;

-- name: ClaimDueRecipients :many
-- Only recipients of RUNNING campaigns: pausing a campaign stops the next
-- batch without touching its rows. The claim pushes next_attempt_at out so a
-- crashed worker's rows come back after two minutes instead of being lost.
UPDATE marketing_recipients
SET next_attempt_at = now() + interval '2 minutes'
WHERE id IN (
    SELECT r.id FROM marketing_recipients r
    JOIN marketing_campaigns c ON c.id = r.campaign_id
    WHERE r.status IN ('PENDING', 'RETRYING') AND r.next_attempt_at <= now()
      AND c.status = 'RUNNING'
      AND r.tenant_id = sqlc.arg(tenant_id) AND c.channel = sqlc.arg(channel)
    ORDER BY r.next_attempt_at
    LIMIT sqlc.arg(limit_count)
    FOR UPDATE OF r SKIP LOCKED
)
RETURNING *;

-- name: MarkRecipientSent :exec
UPDATE marketing_recipients
SET status = 'SENT', provider_message_id = sqlc.arg(provider_message_id), sent_at = now(),
    attempt_count = attempt_count + 1
WHERE id = sqlc.arg(id);

-- name: MarkRecipientAttempt :exec
UPDATE marketing_recipients
SET status = sqlc.arg(status), attempt_count = sqlc.arg(attempt_count), last_error = sqlc.arg(last_error),
    next_attempt_at = sqlc.arg(next_attempt_at)
WHERE id = sqlc.arg(id);

-- name: BumpCampaignCounters :exec
UPDATE marketing_campaigns
SET sent_count = sent_count + sqlc.arg(sent), failed_count = failed_count + sqlc.arg(failed),
    skipped_count = skipped_count + sqlc.arg(skipped), updated_at = now()
WHERE id = sqlc.arg(id);

-- name: ListRunningCampaignsWithoutWork :many
-- RUNNING campaigns with nothing left to send: the worker closes them.
SELECT c.* FROM marketing_campaigns c
WHERE c.status = 'RUNNING'
  AND NOT EXISTS (
      SELECT 1 FROM marketing_recipients r
      WHERE r.campaign_id = c.id AND r.status IN ('PENDING', 'RETRYING')
  );

-- name: GetRecipientByProviderMessage :one
SELECT r.* FROM marketing_recipients r
WHERE r.provider_message_id = sqlc.arg(provider_message_id) AND r.provider_message_id <> ''
LIMIT 1;

-- name: SetRecipientDeliveryStatus :exec
-- Provider receipts only ever move a delivered message forward.
UPDATE marketing_recipients
SET status = sqlc.arg(status)
WHERE id = sqlc.arg(id)
  AND status IN ('SENT', 'DELIVERED')
  AND (sqlc.arg(status)::text = 'READ' OR status = 'SENT');

-- name: TenantIDByMetaPhoneNumber :one
-- Webhook routing: which tenant owns this WhatsApp number.
SELECT tenant_id FROM tenant_configurations
WHERE service_type = 'META' AND config->>'phone_number_id' = sqlc.arg(phone_number_id)::text
LIMIT 1;

-- name: GetRecipientForUnsubscribe :one
-- Public: the recipient id is the unguessable token in the unsubscribe link.
SELECT r.tenant_id, r.customer_id, r.campaign_id, c.channel
FROM marketing_recipients r
JOIN marketing_campaigns c ON c.id = r.campaign_id
WHERE r.id = sqlc.arg(id);

/* ---------- attribution ---------- */

-- name: SetOrderMarketing :exec
UPDATE orders
SET marketing_campaign_id = sqlc.narg(campaign_id), marketing_link_id = sqlc.narg(link_id),
    promotion_id = sqlc.narg(promotion_id), coupon_code = sqlc.arg(coupon_code),
    loyalty_points_used = sqlc.arg(loyalty_points_used)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

/* ---------- automations ---------- */

-- name: ListAutomationRules :many
SELECT r.*,
       (SELECT COUNT(*) FROM automation_runs ar WHERE ar.rule_id = r.id)::bigint AS runs,
       (SELECT COUNT(*) FROM automation_runs ar WHERE ar.rule_id = r.id AND ar.created_at >= now() - interval '30 days')::bigint AS runs_30d
FROM automation_rules r
WHERE r.tenant_id = sqlc.arg(tenant_id)
ORDER BY r.created_at DESC;

-- name: GetAutomationRule :one
SELECT * FROM automation_rules WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: CreateAutomationRule :one
INSERT INTO automation_rules (tenant_id, name, trigger, enabled, channel, delay_hours, inactive_days, cooldown_days,
                              quiet_start, quiet_end, subject, body, button_label, link_id, promotion_id, template_name, template_lang)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), sqlc.arg(trigger), sqlc.arg(enabled), sqlc.arg(channel), sqlc.arg(delay_hours),
        sqlc.arg(inactive_days), sqlc.arg(cooldown_days), sqlc.arg(quiet_start), sqlc.arg(quiet_end), sqlc.arg(subject),
        sqlc.arg(body), sqlc.arg(button_label), sqlc.arg(link_id), sqlc.arg(promotion_id), sqlc.arg(template_name), sqlc.arg(template_lang))
RETURNING *;

-- name: UpdateAutomationRule :one
UPDATE automation_rules
SET name = sqlc.arg(name), trigger = sqlc.arg(trigger), enabled = sqlc.arg(enabled), channel = sqlc.arg(channel),
    delay_hours = sqlc.arg(delay_hours), inactive_days = sqlc.arg(inactive_days), cooldown_days = sqlc.arg(cooldown_days),
    quiet_start = sqlc.arg(quiet_start), quiet_end = sqlc.arg(quiet_end), subject = sqlc.arg(subject), body = sqlc.arg(body),
    button_label = sqlc.arg(button_label), link_id = sqlc.arg(link_id), promotion_id = sqlc.arg(promotion_id),
    template_name = sqlc.arg(template_name), template_lang = sqlc.arg(template_lang), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: SetAutomationRuleEnabled :one
UPDATE automation_rules SET enabled = sqlc.arg(enabled), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DeleteAutomationRule :execrows
DELETE FROM automation_rules WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: ListEnabledAutomationRules :many
SELECT r.*, t.timezone AS tenant_timezone
FROM automation_rules r
JOIN tenants t ON t.id = r.tenant_id
WHERE r.enabled AND t.status = 'ACTIVE';

-- name: TouchAutomationRule :exec
UPDATE automation_rules SET last_run_at = now() WHERE id = sqlc.arg(id);

-- name: AutomationCandidates :many
-- Customers a rule should message now, with the trigger key that makes the
-- fire idempotent. One query, three triggers, so the cooldown, consent and
-- address rules are applied identically to each. Consent and address follow
-- the rule's channel.
WITH base AS (
    SELECT a.*
    FROM marketing_audience a
    WHERE a.tenant_id = sqlc.arg(tenant_id) AND NOT a.is_blocked
      AND ((sqlc.arg(channel)::text = 'EMAIL'    AND a.email <> '' AND a.email_consent    = 'GRANTED')
        OR (sqlc.arg(channel)::text = 'SMS'      AND a.phone <> '' AND a.sms_consent      = 'GRANTED')
        OR (sqlc.arg(channel)::text = 'WHATSAPP' AND a.phone <> '' AND a.whatsapp_consent = 'GRANTED'))
),
candidates AS (
    -- WELCOME: consent for this channel granted in the last 7 days.
    SELECT b.customer_id, 'welcome:' || mc.id::text AS trigger_key
    FROM base b
    JOIN marketing_consents mc ON mc.tenant_id = b.tenant_id AND mc.customer_id = b.customer_id
         AND mc.channel = sqlc.arg(channel)::text AND mc.status = 'GRANTED'
    WHERE sqlc.arg(trigger)::text = 'WELCOME' AND mc.updated_at >= now() - interval '7 days'
    UNION ALL
    -- ORDER_FOLLOW_UP: completed delay_hours ago (and not more than 3 days past that).
    SELECT b.customer_id, 'order:' || o.id::text
    FROM base b
    JOIN orders o ON o.tenant_id = b.tenant_id AND o.customer_id = b.customer_id
    WHERE sqlc.arg(trigger)::text = 'ORDER_FOLLOW_UP' AND o.status = 'COMPLETED'
      AND o.completed_at <= now() - make_interval(hours => sqlc.arg(delay_hours)::int)
      AND o.completed_at >  now() - make_interval(hours => sqlc.arg(delay_hours)::int + 72)
    UNION ALL
    -- WIN_BACK: had a visit, none for inactive_days. The key carries the last
    -- visit date, so a customer who comes back and lapses again is eligible again.
    SELECT b.customer_id, 'winback:' || to_char(b.last_visit_at, 'YYYY-MM-DD')
    FROM base b
    WHERE sqlc.arg(trigger)::text = 'WIN_BACK' AND b.visit_count >= 1
      AND b.last_visit_at < now() - make_interval(days => sqlc.arg(inactive_days)::int)
)
SELECT DISTINCT c.customer_id, c.trigger_key::text AS trigger_key
FROM candidates c
WHERE NOT EXISTS (
    SELECT 1 FROM automation_runs ar
    WHERE ar.rule_id = sqlc.arg(rule_id) AND ar.customer_id = c.customer_id AND ar.trigger_key = c.trigger_key
)
AND NOT EXISTS (
    -- Frequency cap: nothing from this rule to this customer within cooldown.
    SELECT 1 FROM automation_runs ar
    WHERE ar.rule_id = sqlc.arg(rule_id) AND ar.customer_id = c.customer_id
      AND ar.created_at >= now() - make_interval(days => sqlc.arg(cooldown_days)::int)
)
LIMIT 500;

-- name: InsertAutomationRun :execrows
INSERT INTO automation_runs (rule_id, tenant_id, customer_id, trigger_key, campaign_id)
VALUES (sqlc.arg(rule_id), sqlc.arg(tenant_id), sqlc.arg(customer_id), sqlc.arg(trigger_key), sqlc.arg(campaign_id))
ON CONFLICT (rule_id, customer_id, trigger_key) DO NOTHING;

-- name: CreateAutomationCampaign :one
-- A rule run is a campaign of kind AUTOMATION that starts RUNNING: the normal
-- worker delivers it and the normal campaign screens show its history.
INSERT INTO marketing_campaigns (tenant_id, name, goal, audience, channel, subject, body, button_label, link_id,
                                 template_name, template_lang, promotion_id, kind, automation_rule_id,
                                 status, started_at, total_count, created_by)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), 'RETAIN', 'ALL', sqlc.arg(channel), sqlc.arg(subject), sqlc.arg(body),
        sqlc.arg(button_label), sqlc.arg(link_id), sqlc.arg(template_name), sqlc.arg(template_lang), sqlc.arg(promotion_id),
        'AUTOMATION', sqlc.arg(automation_rule_id), 'RUNNING', now(), sqlc.arg(total_count), 'automation')
RETURNING *;

-- name: InsertAutomationRecipient :execrows
INSERT INTO marketing_recipients (campaign_id, tenant_id, customer_id, address)
SELECT sqlc.arg(campaign_id), a.tenant_id, a.customer_id,
       CASE WHEN sqlc.arg(channel)::text = 'EMAIL' THEN a.email ELSE a.phone END
FROM marketing_audience a
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.customer_id = sqlc.arg(customer_id)
ON CONFLICT (campaign_id, customer_id) DO NOTHING;

/* ---------- social ---------- */

-- name: ListSocialPosts :many
SELECT * FROM social_posts WHERE tenant_id = sqlc.arg(tenant_id) ORDER BY COALESCE(scheduled_at, created_at) DESC LIMIT 200;

-- name: GetSocialPost :one
SELECT * FROM social_posts WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: CreateSocialPost :one
INSERT INTO social_posts (tenant_id, caption, image_url, platforms, link_id, status, scheduled_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(caption), sqlc.arg(image_url), sqlc.arg(platforms), sqlc.arg(link_id), sqlc.arg(status), sqlc.arg(scheduled_at))
RETURNING *;

-- name: UpdateSocialPost :one
UPDATE social_posts
SET caption = sqlc.arg(caption), image_url = sqlc.arg(image_url), platforms = sqlc.arg(platforms), link_id = sqlc.arg(link_id),
    status = sqlc.arg(status), scheduled_at = sqlc.arg(scheduled_at), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status IN ('DRAFT', 'SCHEDULED', 'MANUAL', 'FAILED')
RETURNING *;

-- name: PublishSocialPostNow :one
UPDATE social_posts
SET status = 'SCHEDULED', scheduled_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status IN ('DRAFT', 'SCHEDULED', 'MANUAL', 'FAILED')
RETURNING *;

-- name: DeleteSocialPost :execrows
DELETE FROM social_posts WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);

-- name: ClaimDueSocialPosts :many
UPDATE social_posts
SET status = 'PUBLISHING', updated_at = now()
WHERE id IN (
    SELECT id FROM social_posts
    WHERE status = 'SCHEDULED' AND scheduled_at <= now()
    ORDER BY scheduled_at
    LIMIT sqlc.arg(limit_count)
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishSocialPost :one
UPDATE social_posts
SET status = sqlc.arg(status), results = sqlc.arg(results),
    published_at = CASE WHEN sqlc.arg(status)::text IN ('PUBLISHED', 'PARTIAL') THEN now() ELSE published_at END,
    updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetCampaignTotal :exec
UPDATE marketing_campaigns SET total_count = sqlc.arg(total_count), updated_at = now() WHERE id = sqlc.arg(id);

-- name: MetaVerifyTokens :many
-- Webhook subscription handshake: Meta echoes one shared token per app; a
-- platform without its own Meta app lets each tenant register theirs.
SELECT DISTINCT (config->>'verify_token')::text AS verify_token
FROM tenant_configurations
WHERE service_type = 'META' AND COALESCE(config->>'verify_token', '') <> '';
