-- +goose Up
-- +goose StatementBegin

-- Four more business types. Each borrows the capability set of the existing
-- type it works like: fashion and electronics shops sell from a catalogue
-- like a grocery, salons and garages take bookings like a barber.
INSERT INTO tenant_types (code, label, active, sort_order) VALUES
    ('FASHION',     'Fashion & apparel', TRUE, 22),
    ('ELECTRONICS', 'Electronics',       TRUE, 24),
    ('SALON',       'Salon & spa',       TRUE, 46),
    ('AUTOMOTIVE',  'Car wash & garage', TRUE, 48)
ON CONFLICT (code) DO UPDATE
    SET label = EXCLUDED.label, active = TRUE, sort_order = EXCLUDED.sort_order;

INSERT INTO business_type_capabilities (business_type_code, capability_code, default_enabled, configurable)
SELECT t.code, base.capability_code, base.default_enabled, base.configurable
FROM (VALUES ('FASHION', 'GROCERY'), ('ELECTRONICS', 'GROCERY'),
             ('SALON', 'BARBER'), ('AUTOMOTIVE', 'BARBER')) AS t(code, like_code)
JOIN business_type_capabilities base ON base.business_type_code = t.like_code
ON CONFLICT DO NOTHING;

INSERT INTO business_type_defaults (business_type_code, capability_code, seed) VALUES
    ('FASHION',     'CATALOG',  '{"categories": ["Men", "Women", "Kids", "Accessories"]}'),
    ('ELECTRONICS', 'CATALOG',  '{"categories": ["Mobiles", "Laptops", "Audio", "Accessories"]}'),
    ('SALON',       'SERVICES', '{"services": [
        {"name": "Haircut", "duration_minutes": 45, "price": 0},
        {"name": "Hair colour", "duration_minutes": 90, "price": 0},
        {"name": "Facial", "duration_minutes": 60, "price": 0},
        {"name": "Manicure", "duration_minutes": 45, "price": 0}
    ]}'),
    ('AUTOMOTIVE',  'SERVICES', '{"services": [
        {"name": "Exterior wash", "duration_minutes": 30, "price": 0},
        {"name": "Full detailing", "duration_minutes": 120, "price": 0},
        {"name": "Oil change", "duration_minutes": 45, "price": 0},
        {"name": "General service", "duration_minutes": 180, "price": 0}
    ]}')
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM business_type_defaults
WHERE business_type_code IN ('FASHION', 'ELECTRONICS', 'SALON', 'AUTOMOTIVE')
  AND NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.business_type = business_type_defaults.business_type_code);
DELETE FROM business_type_capabilities
WHERE business_type_code IN ('FASHION', 'ELECTRONICS', 'SALON', 'AUTOMOTIVE')
  AND NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.business_type = business_type_capabilities.business_type_code);
DELETE FROM tenant_types
WHERE code IN ('FASHION', 'ELECTRONICS', 'SALON', 'AUTOMOTIVE')
  AND NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.business_type = tenant_types.code);

-- +goose StatementEnd
