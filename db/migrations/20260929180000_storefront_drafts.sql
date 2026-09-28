-- +goose Up
-- +goose StatementBegin

-- A storefront draft belongs to the shop, not to the browser it was typed in.
--
-- The Studio kept its unpublished work in localStorage. That looked like
-- autosave and was not: the draft could not be opened on another device, it
-- vanished with cleared site data, and because publishing replayed the whole
-- local document over the live one, a draft left open on a laptop would quietly
-- undo whatever a colleague had changed in the meantime.
--
-- One row per tenant, because a storefront has one draft. `document` is the
-- same shape the admin API already returns, so the Studio edits, saves and
-- publishes exactly what it renders — no second schema to keep in step.
--
-- `base_version` is the storefront's `updated_at` at the moment the draft was
-- started. Publishing compares it with the live row: if the shop moved on
-- underneath, the API refuses instead of overwriting, and the Studio can say so
-- rather than silently winning.
CREATE TABLE IF NOT EXISTS tenant_storefront_drafts (
    tenant_id    UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    document     JSONB       NOT NULL,
    base_version TIMESTAMPTZ NOT NULL,
    -- Who has work in progress, so the console can say whose it is. The draft
    -- outlives the person leaving, so this nulls rather than cascading.
    updated_by   UUID        REFERENCES users (id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS tenant_storefront_drafts;

-- +goose StatementEnd
