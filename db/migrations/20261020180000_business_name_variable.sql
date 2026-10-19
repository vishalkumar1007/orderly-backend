-- +goose Up
-- Every message can name the business that sends it: the email editor offers
-- {{business_name}} on every template and the default copy uses it, so each
-- event now declares it too (substitution only fills declared variables).
UPDATE notification_events
SET variables = variables || '["business_name"]'::jsonb
WHERE NOT variables ? 'business_name';

-- +goose Down
UPDATE notification_events
SET variables = variables - 'business_name'
WHERE code NOT IN ('ACCOUNT_SETUP_INVITE');
