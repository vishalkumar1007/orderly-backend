-- +goose Up
-- +goose StatementBegin

-- Two of the platform's six committed business types (barber, general) were
-- never actually rows in tenant_types — onboarding could not have offered
-- them. Add them now; deactivate OTHER in favour of GENERAL as the real
-- catch-all (same pattern as the cuisine-code deprecation: deactivated, not
-- deleted, so any tenant already stored as OTHER keeps a legal value).
INSERT INTO tenant_types (code, label, active, sort_order) VALUES
    ('BARBER',  'Barber shop', TRUE, 45),
    ('GENERAL', 'General',     TRUE, 998)
ON CONFLICT (code) DO UPDATE
    SET label = EXCLUDED.label, active = TRUE, sort_order = EXCLUDED.sort_order;

UPDATE tenant_types SET active = FALSE WHERE code = 'OTHER';

-- Fixed, platform-defined capability list (HLD §5). Not tenant-editable.
CREATE TABLE capabilities (
    code       TEXT PRIMARY KEY,
    label      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO capabilities (code, label) VALUES
    ('CUSTOMERS',     'Customers'),
    ('STAFF',         'Staff'),
    ('PAYMENTS',      'Payments'),
    ('BILLING',       'Billing'),
    ('CATALOG',       'Catalog'),
    ('CART',          'Cart'),
    ('ORDERS',        'Orders'),
    ('KITCHEN',       'Kitchen'),
    ('SERVICES',      'Services'),
    ('APPOINTMENTS',  'Appointments'),
    ('QUEUE',         'Queue'),
    ('RESERVATIONS',  'Reservations'),
    ('TABLES',        'Tables'),
    ('ROOMS',         'Rooms'),
    ('HOUSEKEEPING',  'Housekeeping');

-- The HLD §6 capability matrix, as data. Absence of a row for a
-- (business_type, capability) pair is a hard boundary: no tenant of that
-- business type can ever enable it, regardless of who tries. `configurable`
-- says whether a tenant/super admin may toggle `enabled` in
-- tenant_capabilities at all; `default_enabled` is what provisioning writes
-- for a brand-new tenant of that type.
CREATE TABLE business_type_capabilities (
    business_type_code TEXT NOT NULL REFERENCES tenant_types (code),
    capability_code     TEXT NOT NULL REFERENCES capabilities (code),
    default_enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    configurable        BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (business_type_code, capability_code)
);

INSERT INTO business_type_capabilities (business_type_code, capability_code, default_enabled, configurable) VALUES
    -- FOOD_SHOP
    ('FOOD_SHOP','CUSTOMERS',TRUE,FALSE), ('FOOD_SHOP','STAFF',TRUE,FALSE),
    ('FOOD_SHOP','PAYMENTS',TRUE,FALSE),  ('FOOD_SHOP','BILLING',TRUE,FALSE),
    ('FOOD_SHOP','CATALOG',TRUE,FALSE),   ('FOOD_SHOP','CART',TRUE,FALSE),
    ('FOOD_SHOP','ORDERS',TRUE,FALSE),    ('FOOD_SHOP','KITCHEN',TRUE,FALSE),
    ('FOOD_SHOP','QUEUE',FALSE,TRUE),

    -- CAFE
    ('CAFE','CUSTOMERS',TRUE,FALSE), ('CAFE','STAFF',TRUE,FALSE),
    ('CAFE','PAYMENTS',TRUE,FALSE),  ('CAFE','BILLING',TRUE,FALSE),
    ('CAFE','CATALOG',TRUE,FALSE),   ('CAFE','CART',TRUE,FALSE),
    ('CAFE','ORDERS',TRUE,FALSE),    ('CAFE','KITCHEN',TRUE,FALSE),
    ('CAFE','TABLES',TRUE,FALSE),
    ('CAFE','SERVICES',FALSE,TRUE), ('CAFE','APPOINTMENTS',FALSE,TRUE),
    ('CAFE','RESERVATIONS',FALSE,TRUE), ('CAFE','QUEUE',FALSE,TRUE),

    -- RESTAURANT
    ('RESTAURANT','CUSTOMERS',TRUE,FALSE), ('RESTAURANT','STAFF',TRUE,FALSE),
    ('RESTAURANT','PAYMENTS',TRUE,FALSE),  ('RESTAURANT','BILLING',TRUE,FALSE),
    ('RESTAURANT','CATALOG',TRUE,FALSE),   ('RESTAURANT','ORDERS',TRUE,FALSE),
    ('RESTAURANT','KITCHEN',TRUE,FALSE),   ('RESTAURANT','TABLES',TRUE,FALSE),
    ('RESTAURANT','RESERVATIONS',TRUE,FALSE),
    ('RESTAURANT','CART',FALSE,TRUE), ('RESTAURANT','SERVICES',FALSE,TRUE),
    ('RESTAURANT','APPOINTMENTS',FALSE,TRUE), ('RESTAURANT','QUEUE',FALSE,TRUE),

    -- BARBER
    ('BARBER','CUSTOMERS',TRUE,FALSE), ('BARBER','STAFF',TRUE,FALSE),
    ('BARBER','PAYMENTS',TRUE,FALSE),  ('BARBER','BILLING',TRUE,FALSE),
    ('BARBER','SERVICES',TRUE,FALSE),  ('BARBER','APPOINTMENTS',TRUE,FALSE),
    ('BARBER','QUEUE',TRUE,FALSE),
    ('BARBER','CATALOG',FALSE,TRUE), ('BARBER','RESERVATIONS',FALSE,TRUE),

    -- HOTEL
    ('HOTEL','CUSTOMERS',TRUE,FALSE), ('HOTEL','STAFF',TRUE,FALSE),
    ('HOTEL','PAYMENTS',TRUE,FALSE),  ('HOTEL','BILLING',TRUE,FALSE),
    ('HOTEL','SERVICES',TRUE,FALSE),  ('HOTEL','ROOMS',TRUE,FALSE),
    ('HOTEL','HOUSEKEEPING',TRUE,FALSE), ('HOTEL','RESERVATIONS',TRUE,FALSE),
    ('HOTEL','CATALOG',FALSE,TRUE), ('HOTEL','APPOINTMENTS',FALSE,TRUE),

    -- GENERAL — capability-driven: only the platform-wide four default on
    ('GENERAL','CUSTOMERS',TRUE,FALSE), ('GENERAL','STAFF',TRUE,FALSE),
    ('GENERAL','PAYMENTS',TRUE,FALSE),  ('GENERAL','BILLING',FALSE,TRUE),
    ('GENERAL','CATALOG',FALSE,TRUE),  ('GENERAL','SERVICES',FALSE,TRUE),
    ('GENERAL','CART',FALSE,TRUE),     ('GENERAL','ORDERS',FALSE,TRUE),
    ('GENERAL','APPOINTMENTS',FALSE,TRUE), ('GENERAL','RESERVATIONS',FALSE,TRUE),
    ('GENERAL','QUEUE',FALSE,TRUE),    ('GENERAL','TABLES',FALSE,TRUE),
    ('GENERAL','ROOMS',FALSE,TRUE),    ('GENERAL','KITCHEN',FALSE,TRUE),
    ('GENERAL','HOUSEKEEPING',FALSE,TRUE),

    -- GROCERY — real, currently-offered type with no HLD matrix row: same
    -- operational shape as Food Shop minus a kitchen (retail, not prep).
    ('GROCERY','CUSTOMERS',TRUE,FALSE), ('GROCERY','STAFF',TRUE,FALSE),
    ('GROCERY','PAYMENTS',TRUE,FALSE),  ('GROCERY','BILLING',TRUE,FALSE),
    ('GROCERY','CATALOG',TRUE,FALSE),   ('GROCERY','CART',TRUE,FALSE),
    ('GROCERY','ORDERS',TRUE,FALSE),
    ('GROCERY','QUEUE',FALSE,TRUE);

-- Legacy/deactivated codes (cuisine-specific FOOD_SHOP variants, and the old
-- OTHER catch-all) must still resolve to a capability set: a tenant created
-- before this migration may still carry one of these values, and it must not
-- become capability-less. They inherit FOOD_SHOP's set verbatim.
INSERT INTO business_type_capabilities (business_type_code, capability_code, default_enabled, configurable)
SELECT legacy.code, fs.capability_code, fs.default_enabled, fs.configurable
FROM (VALUES ('MOMO'), ('MANCHURIAN'), ('FAST_FOOD'), ('ROLLS'), ('TEA'), ('OTHER')) AS legacy(code)
CROSS JOIN (
    SELECT capability_code, default_enabled, configurable
    FROM business_type_capabilities WHERE business_type_code = 'FOOD_SHOP'
) AS fs;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS business_type_capabilities;
DROP TABLE IF EXISTS capabilities;

UPDATE tenant_types SET active = TRUE WHERE code = 'OTHER';
DELETE FROM tenant_types
WHERE code IN ('BARBER', 'GENERAL')
  AND NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.business_type = tenant_types.code);

-- +goose StatementEnd
