-- +goose Up
-- +goose StatementBegin
-- Branding assets and storefront defaults captured during Super Admin onboarding.
-- Everything here is editable later by the tenant admin from their setup wizard.
ALTER TABLE tenants
    ADD COLUMN logo_url         TEXT NOT NULL DEFAULT '',
    ADD COLUMN favicon_url      TEXT NOT NULL DEFAULT '',
    ADD COLUMN short_description TEXT NOT NULL DEFAULT '',
    ADD COLUMN currency         TEXT NOT NULL DEFAULT 'INR',
    ADD COLUMN timezone         TEXT NOT NULL DEFAULT 'Asia/Kolkata',
    ADD COLUMN language         TEXT NOT NULL DEFAULT 'en',
    ADD COLUMN store_status     TEXT NOT NULL DEFAULT 'OPEN';

ALTER TABLE tenants
    ADD CONSTRAINT tenants_store_status_check CHECK (store_status IN ('OPEN', 'CLOSED'));

-- Public storefronts are looked up by slug; keep that path indexed for the
-- extra columns the public payload now carries.
CREATE INDEX tenants_storefront_idx ON tenants (slug, store_status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS tenants_storefront_idx;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_store_status_check;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS logo_url,
    DROP COLUMN IF EXISTS favicon_url,
    DROP COLUMN IF EXISTS short_description,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS timezone,
    DROP COLUMN IF EXISTS language,
    DROP COLUMN IF EXISTS store_status;
-- +goose StatementEnd
