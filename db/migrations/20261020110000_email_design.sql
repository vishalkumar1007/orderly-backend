-- +goose Up
-- +goose StatementBegin

-- Emails are sent as branded HTML. A template keeps its plain body and gains the
-- pieces the HTML layout places: a heading, inbox preview text and one button.
-- All are optional; an empty heading falls back to the subject.
ALTER TABLE notification_templates
    ADD COLUMN heading      TEXT NOT NULL DEFAULT '',
    ADD COLUMN preheader    TEXT NOT NULL DEFAULT '',
    ADD COLUMN button_label TEXT NOT NULL DEFAULT '',
    ADD COLUMN button_url   TEXT NOT NULL DEFAULT '';

-- The look shared by every email of one sender: the platform (tenant_id NULL)
-- or a business. A business without a row uses its storefront logo and name.
CREATE TABLE email_branding (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID REFERENCES tenants (id) ON DELETE CASCADE,
    show_logo   BOOLEAN NOT NULL DEFAULT TRUE,
    logo_url    TEXT NOT NULL DEFAULT '',
    brand_color TEXT NOT NULL DEFAULT '#4F46E5',
    footer_text TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT email_branding_color_check CHECK (brand_color ~ '^#[0-9A-Fa-f]{6}$')
);
CREATE UNIQUE INDEX email_branding_tenant_idx ON email_branding (tenant_id) WHERE tenant_id IS NOT NULL;
CREATE UNIQUE INDEX email_branding_platform_idx ON email_branding ((tenant_id IS NULL)) WHERE tenant_id IS NULL;

INSERT INTO email_branding (tenant_id, footer_text)
VALUES (NULL, 'You are receiving this email because of your account on Orderly.');

-- Account emails lead with a button instead of a pasted link. Only the
-- untouched seed (version 1) is rewritten; an edited template is left alone.
UPDATE notification_templates SET
    heading = 'You''re invited to {{business_name}}',
    preheader = 'Choose a password to get started.',
    body = E'You have been invited to set up {{business_name}} on Orderly. Choose a password to sign in and get started.\n\nIf you were not expecting this message, you can ignore it.',
    button_label = 'Set up your account',
    button_url = '{{setup_url}}'
WHERE tenant_id IS NULL AND event_code = 'ACCOUNT_SETUP_INVITE' AND channel = 'EMAIL' AND version = 1;

UPDATE notification_templates SET
    heading = 'Welcome to the Orderly console',
    preheader = 'Choose a password to get started.',
    body = E'You have been given access to the Orderly platform console. Choose a password to sign in.\n\nIf you were not expecting this message, you can ignore it.',
    button_label = 'Set up your account',
    button_url = '{{setup_url}}'
WHERE tenant_id IS NULL AND event_code = 'CONSOLE_INVITE' AND channel = 'EMAIL' AND version = 1;

UPDATE notification_templates SET
    heading = 'Your verification code: {{code}}',
    preheader = 'Your code expires in {{minutes}} minutes.',
    body = E'Enter this code to finish signing in. It expires in {{minutes}} minutes.\n\nIf you did not request this, you can ignore this message: someone may have typed your email address by mistake.'
WHERE tenant_id IS NULL AND event_code = 'ACCOUNT_VERIFICATION_CODE' AND channel = 'EMAIL' AND version = 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS email_branding;
ALTER TABLE notification_templates
    DROP COLUMN IF EXISTS heading,
    DROP COLUMN IF EXISTS preheader,
    DROP COLUMN IF EXISTS button_label,
    DROP COLUMN IF EXISTS button_url;
-- +goose StatementEnd
