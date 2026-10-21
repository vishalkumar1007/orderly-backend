-- +goose Up
-- +goose StatementBegin

-- The business profile an owner fills in under Settings. These are platform
-- facts about the business (its legal identity, where to find it online, its
-- tax registration), so they live on tenants next to currency and timezone
-- rather than on the storefront document, which is what customers see.
ALTER TABLE tenants
    ADD COLUMN legal_name           TEXT    NOT NULL DEFAULT '',
    ADD COLUMN website              TEXT    NOT NULL DEFAULT '',
    ADD COLUMN tax_id               TEXT    NOT NULL DEFAULT '',
    ADD COLUMN social_links         JSONB   NOT NULL DEFAULT '{}'::jsonb,
    -- Whether the console rail shows the business logo beside its name.
    ADD COLUMN show_logo_in_console BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE tenants
    DROP COLUMN IF EXISTS legal_name,
    DROP COLUMN IF EXISTS website,
    DROP COLUMN IF EXISTS tax_id,
    DROP COLUMN IF EXISTS social_links,
    DROP COLUMN IF EXISTS show_logo_in_console;

-- +goose StatementEnd
