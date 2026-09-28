-- +goose Up
-- +goose StatementBegin

-- The platform sells to five shapes of business, not to one cuisine. The
-- original catalogue listed momo / manchurian / rolls / tea, which are all the
-- same operational shape — quick-service food — with different menus on top.
--
-- Onboarding now picks the shape, because the shape is what drives real
-- defaults: theme, storefront layout, product terminology, starter categories,
-- order workflow and payment methods. The cuisine never did.
INSERT INTO tenant_types (code, label, active, sort_order) VALUES
    ('FOOD_SHOP',  'Food shop',  TRUE, 10),
    ('GROCERY',    'Grocery',    TRUE, 20),
    ('CAFE',       'Cafe',       TRUE, 30),
    ('RESTAURANT', 'Restaurant', TRUE, 40),
    ('HOTEL',      'Hotel',      TRUE, 50)
ON CONFLICT (code) DO UPDATE
    SET label = EXCLUDED.label, active = TRUE, sort_order = EXCLUDED.sort_order;

-- Cuisine codes stay valid on the tenants that already carry them — a
-- deactivated type is still a legal stored value and still renders — they are
-- simply no longer offered when onboarding a new business.
UPDATE tenant_types
SET active = FALSE, sort_order = 900
WHERE code IN ('MOMO', 'MANCHURIAN', 'FAST_FOOD', 'ROLLS', 'TEA');

-- "Other" remains the catch-all, last in the list.
UPDATE tenant_types SET sort_order = 999 WHERE code = 'OTHER';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

UPDATE tenant_types
SET active = TRUE, sort_order = CASE code
        WHEN 'MOMO' THEN 10
        WHEN 'MANCHURIAN' THEN 20
        WHEN 'FAST_FOOD' THEN 30
        WHEN 'ROLLS' THEN 40
        WHEN 'TEA' THEN 50
        ELSE sort_order
    END
WHERE code IN ('MOMO', 'MANCHURIAN', 'FAST_FOOD', 'ROLLS', 'TEA');

UPDATE tenant_types SET sort_order = 60 WHERE code = 'OTHER';

-- Only the rows this migration introduced are removed; a tenant still on one
-- of them keeps its stored code, so deletion is guarded.
DELETE FROM tenant_types
WHERE code IN ('FOOD_SHOP', 'GROCERY', 'CAFE', 'RESTAURANT', 'HOTEL')
  AND NOT EXISTS (SELECT 1 FROM tenants WHERE tenants.business_type = tenant_types.code);

-- +goose StatementEnd
