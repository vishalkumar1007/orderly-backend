-- +goose Up
-- How an email is laid out (header, card, footer) is now its own choice,
-- separate from light or dark. The old Minimal and Bold looks were really
-- layouts, so they move here and their colours become the light theme.
ALTER TABLE email_branding
    ADD COLUMN layout TEXT NOT NULL DEFAULT 'classic',
    ADD CONSTRAINT email_branding_layout_check CHECK (layout IN ('classic', 'header', 'centered', 'plain', 'footerbar'));

UPDATE email_branding SET layout = 'plain',  theme = 'classic' WHERE theme = 'minimal';
UPDATE email_branding SET layout = 'header', theme = 'classic' WHERE theme = 'bold';

-- +goose Down
UPDATE email_branding SET theme = 'minimal' WHERE layout = 'plain' AND theme = 'classic';
UPDATE email_branding SET theme = 'bold' WHERE layout = 'header' AND theme = 'classic';
ALTER TABLE email_branding DROP CONSTRAINT IF EXISTS email_branding_layout_check, DROP COLUMN IF EXISTS layout;
