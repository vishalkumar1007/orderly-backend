-- +goose Up
-- +goose StatementBegin
CREATE TABLE theme_presets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    tokens     JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO theme_presets (id, name, tokens) VALUES
    ('indigo-violet', 'Indigo',  '{"accent":"#4f46e5","accent2":"#6366f1","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('emerald',       'Emerald', '{"accent":"#059669","accent2":"#10b981","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('rose',          'Rose',    '{"accent":"#e11d48","accent2":"#f43f5e","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('cyan',          'Cyan',    '{"accent":"#0891b2","accent2":"#06b6d4","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('orange',        'Orange',  '{"accent":"#ea580c","accent2":"#f97316","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('pink',          'Pink',    '{"accent":"#db2777","accent2":"#ec4899","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb),
    ('lime',          'Lime',    '{"accent":"#65a30d","accent2":"#84cc16","radius_sm":"6px","radius":"8px","radius_lg":"12px","font_display":"Inter","font_body":"Inter"}'::jsonb);

ALTER TABLE tenants
    ADD COLUMN theme_preset_id TEXT NOT NULL DEFAULT 'indigo-violet' REFERENCES theme_presets (id),
    ADD COLUMN theme_color_mode TEXT NOT NULL DEFAULT 'system',
    ADD COLUMN theme_overrides JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE tenants
    ADD CONSTRAINT tenants_theme_color_mode_check CHECK (
        theme_color_mode IN ('light', 'dark', 'system')
    );

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_business_type_check;

CREATE TABLE tenant_types (
    code       TEXT PRIMARY KEY,
    label      TEXT NOT NULL,
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO tenant_types (code, label, active, sort_order) VALUES
    ('MOMO', 'Momo', TRUE, 10),
    ('MANCHURIAN', 'Manchurian', TRUE, 20),
    ('FAST_FOOD', 'Fast food', TRUE, 30),
    ('ROLLS', 'Rolls', TRUE, 40),
    ('TEA', 'Tea', TRUE, 50),
    ('OTHER', 'Other', TRUE, 60);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_types;

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_theme_color_mode_check;
ALTER TABLE tenants
    DROP COLUMN IF EXISTS theme_overrides,
    DROP COLUMN IF EXISTS theme_color_mode,
    DROP COLUMN IF EXISTS theme_preset_id;

DROP TABLE IF EXISTS theme_presets;

ALTER TABLE tenants
    ADD CONSTRAINT tenants_business_type_check CHECK (
        business_type IN ('MOMO', 'MANCHURIAN', 'FAST_FOOD', 'ROLLS', 'TEA', 'OTHER')
    );
-- +goose StatementEnd
