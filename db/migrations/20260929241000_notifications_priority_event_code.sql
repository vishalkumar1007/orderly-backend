-- +goose Up
-- +goose StatementBegin

-- Enriches the existing in-app feed row rather than duplicating it: the
-- notification engine now sets both columns when it writes an IN_APP
-- delivery, so the bell can render a priority-appropriate icon/sound and
-- link a row back to its catalog entry.
ALTER TABLE notifications
    ADD COLUMN priority TEXT NOT NULL DEFAULT 'NORMAL',
    ADD COLUMN event_code TEXT;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_priority_check
        CHECK (priority IN ('LOW', 'NORMAL', 'HIGH', 'CRITICAL'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE notifications
    DROP CONSTRAINT IF EXISTS notifications_priority_check,
    DROP COLUMN IF EXISTS priority,
    DROP COLUMN IF EXISTS event_code;
-- +goose StatementEnd
