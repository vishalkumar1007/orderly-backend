-- +goose Up
-- +goose StatementBegin

-- A template may be written as raw HTML instead of the guided fields. When
-- html_body is set it is the email; the guided layout is not used.
ALTER TABLE notification_templates ADD COLUMN html_body TEXT NOT NULL DEFAULT '';

-- The guided layout comes in a few looks, chosen per sender.
ALTER TABLE email_branding
    ADD COLUMN theme TEXT NOT NULL DEFAULT 'classic',
    ADD CONSTRAINT email_branding_theme_check CHECK (theme IN ('classic', 'minimal', 'bold', 'dark'));

-- Customer emails lead with a clear heading and inbox preview line. Only the
-- untouched seeds (version 1, no heading yet) are improved.
UPDATE notification_templates t SET heading = v.heading, preheader = v.preheader
FROM (VALUES
  ('ORDER_ACCEPTED',  'We''ve got your order',            'Order #{{order_number}} is confirmed and will be prepared soon.'),
  ('ORDER_PREPARING', 'Your order is being prepared',     'Order #{{order_number}} is in the kitchen.'),
  ('ORDER_READY',     'Your order is ready for pickup',   'Order #{{order_number}} is waiting for you.'),
  ('ORDER_COMPLETED', 'Thanks for your order',            'We hope you enjoyed it. See you again soon.'),
  ('ORDER_CANCELLED', 'Your order was cancelled',         'Order #{{order_number}} will not be prepared.'),
  ('ORDER_PLACED',    'New order #{{order_number}}',      '{{customer_name}} just placed an order.'),
  ('PAYMENT_RECEIVED','Payment received',                 '₹{{amount}} received for order #{{order_number}}.'),
  ('PAYMENT_FAILED',  'A payment did not go through',     'Payment for order #{{order_number}} failed.'),
  ('PAYMENT_REFUNDED','Your refund is on its way',        '₹{{amount}} for order #{{order_number}} has been refunded.'),
  ('APPOINTMENT_CREATED',   'Your appointment is booked',     'See you on {{starts_at}}.'),
  ('APPOINTMENT_CONFIRMED', 'Your appointment is confirmed',  'See you on {{starts_at}}.'),
  ('APPOINTMENT_CANCELLED', 'Your appointment was cancelled', 'The appointment on {{starts_at}} is cancelled.'),
  ('RESERVATION_CREATED',   'We''ve received your reservation', '{{check_in}} to {{check_out}}.'),
  ('RESERVATION_CONFIRMED', 'Your stay is confirmed',          '{{check_in}} to {{check_out}}. We look forward to welcoming you.'),
  ('RESERVATION_CANCELLED', 'Your reservation was cancelled',  'The reservation from {{check_in}} is cancelled.')
) AS v(event_code, heading, preheader)
WHERE t.event_code = v.event_code AND t.tenant_id IS NULL AND t.channel = 'EMAIL'
  AND t.version = 1 AND t.heading = '';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE email_branding DROP CONSTRAINT IF EXISTS email_branding_theme_check, DROP COLUMN IF EXISTS theme;
ALTER TABLE notification_templates DROP COLUMN IF EXISTS html_body;
-- +goose StatementEnd
