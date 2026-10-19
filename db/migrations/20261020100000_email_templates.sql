-- +goose Up
-- +goose StatementBegin

-- Account emails (setup invitations, verification codes) were hard-coded Go
-- strings. They become catalog events so their wording is editable like any
-- other message. They have no notification_rules: they are sent directly by the
-- flow that needs them, never routed, so they never appear in the rule grids.
ALTER TABLE notification_events DROP CONSTRAINT notification_events_category_check;
ALTER TABLE notification_events ADD CONSTRAINT notification_events_category_check
    CHECK (category IN ('ORDERS', 'PAYMENTS', 'BOOKINGS', 'STAFF', 'SECURITY', 'PLATFORM', 'INVENTORY', 'ACCOUNT'));

INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('ACCOUNT_SETUP_INVITE', 'ACCOUNT', 'Account setup invitation',
     'Sent to a new business owner or staff member with the link to choose a password.',
     ARRAY['EMAIL'], NULL, '["business_name","setup_url"]'),
    ('ACCOUNT_VERIFICATION_CODE', 'ACCOUNT', 'Sign-in verification code',
     'The one-time code emailed for two-step verification.',
     ARRAY['EMAIL'], NULL, '["code","minutes"]'),
    -- PLATFORM category with no tenant rule: only the superadmin can edit it.
    ('CONSOLE_INVITE', 'PLATFORM', 'Platform console invitation',
     'Sent to a new platform console user with the link to choose a password.',
     ARRAY['EMAIL'], NULL, '["setup_url"]');

-- Platform-default email wording for every event that has none yet, so each
-- business starts with real copy and can override any of it. Templates are per
-- event and channel, shared by every recipient of that event.
INSERT INTO notification_templates (tenant_id, event_code, channel, subject, body)
SELECT NULL, v.event_code, 'EMAIL', v.subject, v.body
FROM (VALUES
  ('ACCOUNT_SETUP_INVITE', 'Set up {{business_name}} on Orderly',
     E'You have been invited to set up {{business_name}} on Orderly.\n\nOpen this link to choose a password and sign in:\n{{setup_url}}\n\nIf you were not expecting this message, you can ignore it.'),
  ('ACCOUNT_VERIFICATION_CODE', 'Your Orderly verification code',
     E'Your verification code is {{code}}. It expires in {{minutes}} minutes.\n\nIf you did not request this, you can ignore this message.'),
  ('CONSOLE_INVITE', 'You have been given access to the Orderly platform console',
     E'You have been invited to the Orderly platform console.\n\nOpen this link to choose a password and sign in:\n{{setup_url}}\n\nIf you were not expecting this message, you can ignore it.'),

  ('APPOINTMENT_CREATED', 'Appointment booked for {{starts_at}}',
     'Hi {{customer_name}}, your appointment for {{starts_at}} has been booked.'),
  ('APPOINTMENT_CONFIRMED', 'Your appointment on {{starts_at}} is confirmed',
     'Hi {{customer_name}}, your appointment on {{starts_at}} is confirmed. See you then!'),
  ('APPOINTMENT_CANCELLED', 'Appointment on {{starts_at}} cancelled',
     'Hi {{customer_name}}, the appointment on {{starts_at}} has been cancelled. If this is unexpected, please contact us.'),
  ('APPOINTMENT_NO_SHOW', 'No-show: {{customer_name}}',
     '{{customer_name}} did not arrive for the appointment on {{starts_at}}.'),
  ('RESERVATION_CREATED', 'Reservation received',
     'Hi {{customer_name}}, we have received your reservation from {{check_in}} to {{check_out}}.'),
  ('RESERVATION_CONFIRMED', 'Your reservation is confirmed',
     'Hi {{customer_name}}, your reservation from {{check_in}} to {{check_out}} is confirmed. We look forward to welcoming you.'),
  ('RESERVATION_CANCELLED', 'Reservation cancelled',
     'Hi {{customer_name}}, your reservation from {{check_in}} to {{check_out}} has been cancelled. If this is unexpected, please contact us.'),
  ('RESERVATION_NO_SHOW', 'No-show: {{customer_name}}',
     '{{customer_name}} did not arrive for the reservation starting {{check_in}}.'),
  ('RESERVATION_CHECKED_IN', 'Checked in: {{customer_name}}',
     '{{customer_name}} has checked in.'),
  ('RESERVATION_CHECKED_OUT', 'Checked out: {{customer_name}}',
     '{{customer_name}} has checked out.'),

  ('PAYMENT_REFUNDED', 'Refund for order #{{order_number}}',
     'A refund of ₹{{amount}} for order #{{order_number}} has been processed. It can take a few working days to reach the original payment method.'),
  ('PAYMENT_REFUND_FAILED', 'Refund failed for order #{{order_number}}',
     'A refund of ₹{{amount}} for order #{{order_number}} failed: {{failure_reason}}. Retry it or follow up with the customer.'),

  ('STAFF_INVITED', 'Team member invited: {{user_name}}',
     '{{user_name}} has been invited to join the team.'),
  ('STAFF_ROLE_CHANGED', 'Role changed for {{user_name}}',
     '{{user_name}} now has the {{new_role}} role.'),
  ('STAFF_DISABLED', 'Account disabled: {{user_name}}',
     'The account for {{user_name}} has been disabled.'),
  ('STAFF_ENABLED', 'Account re-enabled: {{user_name}}',
     'The account for {{user_name}} has been enabled again.'),

  ('SECURITY_LOGIN_FAILED', 'Failed sign-in attempt',
     'There was a failed sign-in attempt for {{user_email}}. If this was not expected, review the account''s security.'),
  ('SECURITY_PASSWORD_RESET', 'Password set for {{user_name}}',
     '{{user_name}} has set a new password.'),
  ('SECURITY_PASSWORD_CHANGED', 'Password changed for {{user_name}}',
     'The password for {{user_name}} was changed. If this was not expected, review the account''s security.'),
  ('SECURITY_MFA_ENABLED', 'Two-step verification turned on for {{user_name}}',
     '{{user_name}} turned on two-step verification.'),
  ('SECURITY_MFA_DISABLED', 'Two-step verification turned off for {{user_name}}',
     '{{user_name}} turned off two-step verification. If this was not expected, review the account''s security.'),
  ('SECURITY_MFA_METHOD_ADDED', '{{user_name}} added a verification method',
     '{{user_name}} added {{method}} as a two-step verification method.'),
  ('SECURITY_MFA_RECOVERY_USED', '{{user_name}} used a recovery code',
     '{{user_name}} signed in with a recovery code. If this was not expected, review the account''s security.'),
  ('SECURITY_MFA_ADMIN_RESET', 'Two-step verification reset for {{user_name}}',
     'An administrator reset two-step verification for {{user_name}}.'),
  ('SECURITY_MFA_POLICY_CHANGED', 'Two-step verification policy changed',
     'The two-step verification policy for {{tenant_name}} was changed.'),

  ('BUSINESS_ONBOARDED', 'New business onboarded: {{tenant_name}}',
     '{{tenant_name}} has been onboarded on Orderly.'),
  ('SUBSCRIPTION_CHANGED', 'Subscription changed for {{tenant_name}}',
     'The subscription for {{tenant_name}} is now on the {{plan_name}} plan.'),
  ('PLATFORM_OPERATIONAL_ALERT', 'Orderly operational alert',
     '{{message}}'),
  ('PLATFORM_SYSTEM_ERROR', 'Orderly system error',
     E'A system error was reported:\n\n{{message}}')
) AS v(event_code, subject, body)
WHERE NOT EXISTS (
    SELECT 1 FROM notification_templates t
    WHERE t.tenant_id IS NULL AND t.event_code = v.event_code AND t.channel = 'EMAIL' AND t.is_active
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM notification_templates WHERE event_code IN
    ('ACCOUNT_SETUP_INVITE','ACCOUNT_VERIFICATION_CODE','CONSOLE_INVITE');
DELETE FROM notification_templates WHERE tenant_id IS NULL AND channel = 'EMAIL' AND version = 1 AND event_code IN
    ('APPOINTMENT_CREATED','APPOINTMENT_CONFIRMED','APPOINTMENT_CANCELLED','APPOINTMENT_NO_SHOW',
     'RESERVATION_CREATED','RESERVATION_CONFIRMED','RESERVATION_CANCELLED','RESERVATION_NO_SHOW',
     'RESERVATION_CHECKED_IN','RESERVATION_CHECKED_OUT','PAYMENT_REFUNDED','PAYMENT_REFUND_FAILED',
     'STAFF_INVITED','STAFF_ROLE_CHANGED','STAFF_DISABLED','STAFF_ENABLED',
     'SECURITY_LOGIN_FAILED','SECURITY_PASSWORD_RESET','SECURITY_PASSWORD_CHANGED','SECURITY_MFA_ENABLED',
     'SECURITY_MFA_DISABLED','SECURITY_MFA_METHOD_ADDED','SECURITY_MFA_RECOVERY_USED','SECURITY_MFA_ADMIN_RESET',
     'SECURITY_MFA_POLICY_CHANGED','BUSINESS_ONBOARDED','SUBSCRIPTION_CHANGED',
     'PLATFORM_OPERATIONAL_ALERT','PLATFORM_SYSTEM_ERROR');
DELETE FROM notification_events WHERE code IN ('ACCOUNT_SETUP_INVITE','ACCOUNT_VERIFICATION_CODE','CONSOLE_INVITE');
ALTER TABLE notification_events DROP CONSTRAINT notification_events_category_check;
ALTER TABLE notification_events ADD CONSTRAINT notification_events_category_check
    CHECK (category IN ('ORDERS', 'PAYMENTS', 'BOOKINGS', 'STAFF', 'SECURITY', 'PLATFORM', 'INVENTORY'));
-- +goose StatementEnd
