-- +goose Up
-- +goose StatementBegin

-- Machine-readable plan -> feature matrix (Release V1, S1).
--
-- plans.features.features is the marketing bullet list shown on a pricing
-- card; it is free text and cannot gate anything. entitlements holds feature
-- codes the API enforces (see internal/entitlement). Limits stay in
-- max_staff / max_products.
ALTER TABLE plans ADD COLUMN entitlements TEXT[] NOT NULL DEFAULT '{}';

-- Everyone keeps what they have today (analytics), so no tenant regresses.
UPDATE plans SET entitlements = ARRAY['ANALYTICS'];
-- Inventory is a Business-and-up feature (Ch.57).
UPDATE plans SET entitlements = ARRAY['ANALYTICS', 'INVENTORY'] WHERE name = 'BUSINESS';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE plans DROP COLUMN IF EXISTS entitlements;
-- +goose StatementEnd
