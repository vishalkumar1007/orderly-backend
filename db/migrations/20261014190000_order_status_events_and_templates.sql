-- +goose Up
-- +goose StatementBegin

-- N1: every order status change now publishes an event. Placed, ready and
-- cancelled already existed; accepted, preparing and completed did not.
INSERT INTO notification_events (code, category, label, description, default_channels, required_capability, variables) VALUES
    ('ORDER_ACCEPTED',  'ORDERS', 'Order accepted',  'The shop accepted an order (by staff or automatically).', ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name"]'),
    ('ORDER_PREPARING', 'ORDERS', 'Order preparing', 'The kitchen started preparing an order.',                ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name"]'),
    ('ORDER_COMPLETED', 'ORDERS', 'Order completed', 'An order was collected / finished.',                       ARRAY['IN_APP'], 'ORDERS', '["order_id","order_number","customer_name"]');

-- Defaults are deliberately quiet: staff watch the board, so no in-app noise for
-- these three, and the customer is only emailed on acceptance (reassurance).
-- Every rule is overridable per business from Settings → Notifications.
INSERT INTO notification_rules (event_code, channel, enabled, recipient_policy, priority, locked) VALUES
    ('ORDER_ACCEPTED',  'IN_APP', FALSE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_ACCEPTED',  'EMAIL',  TRUE,  'CUSTOMER',     'NORMAL', FALSE),
    ('ORDER_PREPARING', 'IN_APP', FALSE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_PREPARING', 'EMAIL',  FALSE, 'CUSTOMER',     'NORMAL', FALSE),
    ('ORDER_COMPLETED', 'IN_APP', FALSE, 'ROLE:selling', 'NORMAL', FALSE),
    ('ORDER_COMPLETED', 'EMAIL',  FALSE, 'CUSTOMER',     'NORMAL', FALSE);

-- N3: platform-default message templates for the V1 events, so a business
-- starts with real copy to edit instead of a blank Templates screen. Only the
-- variables each event declares are used ({{name}} is plain substitution; an
-- undeclared token would be sent verbatim).
INSERT INTO notification_templates (tenant_id, event_code, channel, subject, body)
SELECT NULL, v.event_code, c.channel,
       CASE c.channel WHEN 'EMAIL' THEN v.subject ELSE '' END,
       CASE c.channel WHEN 'EMAIL' THEN v.email_body ELSE v.sms_body END
FROM (VALUES
  ('ORDER_PLACED',     'New order #{{order_number}}',
     '{{customer_name}} placed order #{{order_number}} for ₹{{total}}. Open Selling to accept it.',
     'New order #{{order_number}} from {{customer_name}} (₹{{total}})'),
  ('ORDER_ACCEPTED',   'Order #{{order_number}} accepted',
     'Hi {{customer_name}}, we have accepted your order #{{order_number}} and will start preparing it soon.',
     'Order #{{order_number}} accepted. We will tell you when it is ready.'),
  ('ORDER_PREPARING',  'Order #{{order_number}} is being prepared',
     'Hi {{customer_name}}, your order #{{order_number}} is being prepared.',
     'Order #{{order_number}} is being prepared.'),
  ('ORDER_READY',      'Order #{{order_number}} is ready for pickup',
     'Hi {{customer_name}}, your order #{{order_number}} is ready. Please come to collect it.',
     'Order #{{order_number}} is ready for pickup.'),
  ('ORDER_COMPLETED',  'Thank you for order #{{order_number}}',
     'Hi {{customer_name}}, order #{{order_number}} is complete. Thank you, see you again!',
     'Order #{{order_number}} complete. Thank you!'),
  ('ORDER_CANCELLED',  'Order #{{order_number}} was cancelled',
     'Hi {{customer_name}}, order #{{order_number}} was cancelled. If this is unexpected, please contact us.',
     'Order #{{order_number}} was cancelled. Contact us if this is unexpected.'),
  ('PAYMENT_RECEIVED', 'Payment received for order #{{order_number}}',
     '₹{{amount}} was received for order #{{order_number}}.',
     'Payment of ₹{{amount}} received for order #{{order_number}}.'),
  ('PAYMENT_FAILED',   'Payment failed for order #{{order_number}}',
     'A payment for order #{{order_number}} failed: {{failure_reason}}. Follow up with the customer.',
     'Payment for order #{{order_number}} failed: {{failure_reason}}'),
  ('INVENTORY_LOW',    'Low stock: {{product_name}}',
     '{{product_name}} is down to {{quantity}} {{unit}} (reorder level {{threshold}}).',
     'Low stock: {{product_name}} has {{quantity}} {{unit}} left.')
) AS v(event_code, subject, email_body, sms_body)
CROSS JOIN (VALUES ('EMAIL'), ('SMS')) AS c(channel);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM notification_templates WHERE tenant_id IS NULL AND event_code IN
    ('ORDER_PLACED','ORDER_ACCEPTED','ORDER_PREPARING','ORDER_READY','ORDER_COMPLETED','ORDER_CANCELLED','PAYMENT_RECEIVED','PAYMENT_FAILED','INVENTORY_LOW');
DELETE FROM notification_rules WHERE event_code IN ('ORDER_ACCEPTED','ORDER_PREPARING','ORDER_COMPLETED');
DELETE FROM notification_templates WHERE event_code IN ('ORDER_ACCEPTED','ORDER_PREPARING','ORDER_COMPLETED');
DELETE FROM notification_events WHERE code IN ('ORDER_ACCEPTED','ORDER_PREPARING','ORDER_COMPLETED');
-- +goose StatementEnd
