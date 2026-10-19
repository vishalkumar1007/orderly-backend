-- +goose Up
-- The console offers nine looks; the business default could store only three,
-- so a chosen Raw or Graphite came back as Dark. Accept them all.
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_theme_color_mode_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_theme_color_mode_check CHECK (
    theme_color_mode IN ('light', 'soft', 'mist', 'dark', 'graphite', 'raw', 'night', 'midnight', 'system')
);

-- +goose Down
UPDATE tenants SET theme_color_mode = CASE
    WHEN theme_color_mode IN ('soft', 'mist') THEN 'light'
    WHEN theme_color_mode IN ('graphite', 'raw', 'night', 'midnight') THEN 'dark'
    ELSE theme_color_mode END;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_theme_color_mode_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_theme_color_mode_check CHECK (theme_color_mode IN ('light', 'dark', 'system'));
