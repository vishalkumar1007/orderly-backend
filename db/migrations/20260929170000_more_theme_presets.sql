-- +goose Up
-- +goose StatementBegin

-- More to choose from.
--
-- Seven presets covered warm, green and pink but left the two requests a
-- console actually gets most often — "a normal blue" and "no colour at all" —
-- reachable only by typing a hex code, which is not choosing a theme so much as
-- knowing one. These five fill the gaps: a plain blue, a violet between blue and
-- the indigo default, a warm amber, a teal between green and cyan, and a slate
-- for anyone who wants the console to stay out of the way.
--
-- Presets are data, not code: they are the same rows the console's picker
-- lists, the same ones a business can be onboarded onto, and adding one needs
-- no release. ON CONFLICT so re-running this against a database where an
-- operator already made one of these ids leaves theirs alone.
INSERT INTO theme_presets (id, name, tokens) VALUES
    ('blue',    'Blue',    '{"accent":"#2563eb","accent2":"#3b82f6","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('violet',  'Violet',  '{"accent":"#7c3aed","accent2":"#a78bfa","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('amber',   'Amber',   '{"accent":"#d97706","accent2":"#f59e0b","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('teal',    'Teal',    '{"accent":"#0d9488","accent2":"#14b8a6","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('slate',   'Slate',   '{"accent":"#475569","accent2":"#64748b","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb)
ON CONFLICT (id) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Only the rows nobody is using. A tenant's theme_preset_id is a foreign key,
-- so deleting a preset somebody chose would fail the migration outright —
-- and rolling back a catalogue must never take a shop's theme with it.
DELETE FROM theme_presets
WHERE id IN ('blue', 'violet', 'amber', 'teal', 'slate')
  AND id NOT IN (SELECT theme_preset_id FROM tenants);

-- +goose StatementEnd
