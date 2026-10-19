-- +goose Up
-- The default brand colour moves from indigo to a calmer teal. Rows still on
-- the old default are moved too; an explicitly chosen colour is kept.
ALTER TABLE email_branding ALTER COLUMN brand_color SET DEFAULT '#0F766E';
UPDATE email_branding SET brand_color = '#0F766E' WHERE brand_color = '#4F46E5';

-- +goose Down
ALTER TABLE email_branding ALTER COLUMN brand_color SET DEFAULT '#4F46E5';
