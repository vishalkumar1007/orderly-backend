-- +goose Up
-- +goose StatementBegin

-- The worker used to re-render with no data and no fallback copy, so an event
-- without a template row sent an empty message. Keep the caller's title/body on
-- the delivery so the worker always has something to send, and the event's data
-- so {{vars}} can be filled at send time.
ALTER TABLE notification_deliveries
    ADD COLUMN subject TEXT  NOT NULL DEFAULT '',
    ADD COLUMN body    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN data    JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Plan features for notifications. Email and in-app are baseline for every
-- plan; SMS and custom message templates are Business-and-up.
UPDATE plans
SET entitlements = (
    SELECT ARRAY(SELECT DISTINCT e FROM unnest(entitlements || ARRAY['NOTIFY_SMS', 'NOTIFY_TEMPLATES']) e ORDER BY e)
)
WHERE name = 'BUSINESS';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE plans SET entitlements = array_remove(array_remove(entitlements, 'NOTIFY_SMS'), 'NOTIFY_TEMPLATES');
ALTER TABLE notification_deliveries DROP COLUMN IF EXISTS data, DROP COLUMN IF EXISTS body, DROP COLUMN IF EXISTS subject;
-- +goose StatementEnd
