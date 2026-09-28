-- +goose Up
-- +goose StatementBegin

-- A console theme belongs to the person looking at the console, not to the
-- business.
--
-- `tenants.theme_*` is the business default: the owner sets it once and it is
-- what a new colleague sees on their first sign-in, what the sign-in screen is
-- painted with, and what the setup link inherits. It is shared, so changing it
-- changes the console for everybody who works there — which is exactly wrong
-- for "I prefer dark mode".
--
-- This column is the personal layer on top of that. NULL means "whatever the
-- business default is", so nothing has to be written for the common case and a
-- reset is a DELETE rather than a second copy of the tenant's values that
-- silently stops tracking it.
--
-- One JSONB rather than three columns: the document is read and written whole
-- by one endpoint, it is never queried by field, and the shape is the same one
-- the tenant already stores (preset id, colour mode, accent overrides). A
-- migration per future token would be noise.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS console_theme JSONB;

COMMENT ON COLUMN users.console_theme IS
    'Per-user console appearance override. NULL = follow the business default.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE users DROP COLUMN IF EXISTS console_theme;

-- +goose StatementEnd
