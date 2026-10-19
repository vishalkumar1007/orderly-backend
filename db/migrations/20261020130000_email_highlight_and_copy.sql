-- +goose Up
-- +goose StatementBegin

-- Two more guided parts, matching how a transactional email is read: one
-- highlighted detail (a code, an order number) and a quiet note at the end.
ALTER TABLE notification_templates
    ADD COLUMN highlight TEXT NOT NULL DEFAULT '',
    ADD COLUMN footnote  TEXT NOT NULL DEFAULT '';

-- Plainer default copy. Only untouched seeds (version 1) are rewritten; a
-- template anyone has saved is left exactly as it is.
UPDATE notification_templates t SET
    subject = v.subject, preheader = v.preheader, heading = v.heading, body = v.body,
    highlight = v.highlight, footnote = v.footnote
FROM (VALUES
  ('ACCOUNT_VERIFICATION_CODE',
     '{{code}} is your verification code',
     'It expires in {{minutes}} minutes.',
     'Your verification code',
     'Use this code to finish signing in. It expires in {{minutes}} minutes.',
     '{{code}}',
     'Don''t share this code with anyone. If you didn''t try to sign in, you can ignore this email.'),
  ('ACCOUNT_SETUP_INVITE',
     'You''re invited to {{business_name}} on Orderly',
     'Choose a password to get started.',
     'Set up your account',
     E'You''ve been invited to manage {{business_name}} on Orderly.\n\nChoose a password to sign in. It only takes a minute.',
     '',
     'This link works once. If you weren''t expecting this invitation, you can ignore this email.'),
  ('CONSOLE_INVITE',
     'Your Orderly console access',
     'Choose a password to get started.',
     'Set up your console account',
     'You''ve been given access to the Orderly platform console. Choose a password to sign in.',
     '',
     'This link works once. If you weren''t expecting this, you can ignore this email.'),
  ('ORDER_ACCEPTED',
     'We''ve got your order #{{order_number}}',
     'We''ll start preparing it shortly.',
     'Thanks, {{customer_name}}',
     'We''ve accepted your order and will start preparing it shortly. We''ll let you know when it''s ready.',
     'Order #{{order_number}}',
     ''),
  ('ORDER_PREPARING',
     'Your order #{{order_number}} is being prepared',
     'It''s in the kitchen now.',
     'Your order is being prepared',
     'Hi {{customer_name}}, your order is in the kitchen now. We''ll let you know as soon as it''s ready.',
     'Order #{{order_number}}',
     ''),
  ('ORDER_READY',
     'Your order #{{order_number}} is ready',
     'Come and collect it whenever you''re ready.',
     'Your order is ready',
     'Hi {{customer_name}}, your order is ready for pickup. Show this number at the counter when you collect it.',
     'Order #{{order_number}}',
     ''),
  ('ORDER_COMPLETED',
     'Thanks for ordering from {{business_name}}',
     'We hope you enjoyed it.',
     'Thanks, {{customer_name}}',
     'Your order is complete. We hope you enjoyed it, and we''d love to see you again.',
     '',
     ''),
  ('ORDER_CANCELLED',
     'Your order #{{order_number}} was cancelled',
     'It won''t be prepared.',
     'Your order was cancelled',
     'Hi {{customer_name}}, your order has been cancelled and won''t be prepared.',
     'Order #{{order_number}}',
     'If you didn''t expect this, reply to this email or contact us and we''ll sort it out.'),
  ('PAYMENT_RECEIVED',
     'Payment received for order #{{order_number}}',
     '₹{{amount}} received.',
     'Payment received',
     'We''ve received your payment for order #{{order_number}}. Thank you.',
     '₹{{amount}}',
     ''),
  ('PAYMENT_REFUNDED',
     'Refund for order #{{order_number}}',
     'It can take a few working days to arrive.',
     'Your refund is on its way',
     'We''ve refunded your payment for order #{{order_number}}. It can take a few working days to reach your account.',
     '₹{{amount}}',
     '')
) AS v(event_code, subject, preheader, heading, body, highlight, footnote)
WHERE t.event_code = v.event_code AND t.tenant_id IS NULL AND t.channel = 'EMAIL'
  AND t.is_active AND t.version = 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE notification_templates DROP COLUMN IF EXISTS highlight, DROP COLUMN IF EXISTS footnote;
-- +goose StatementEnd
