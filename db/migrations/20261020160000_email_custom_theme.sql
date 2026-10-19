-- +goose Up
-- A custom look: the sender picks the background and card colours; text
-- colours are derived from the card so they always read.
ALTER TABLE email_branding
    ADD COLUMN page_color TEXT NOT NULL DEFAULT '',
    ADD COLUMN card_color TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT email_branding_theme_check,
    ADD CONSTRAINT email_branding_theme_check CHECK (theme IN ('classic', 'minimal', 'bold', 'dark', 'custom'));

-- +goose Down
UPDATE email_branding SET theme = 'classic' WHERE theme = 'custom';
ALTER TABLE email_branding
    DROP CONSTRAINT email_branding_theme_check,
    ADD CONSTRAINT email_branding_theme_check CHECK (theme IN ('classic', 'minimal', 'bold', 'dark')),
    DROP COLUMN IF EXISTS page_color,
    DROP COLUMN IF EXISTS card_color;
