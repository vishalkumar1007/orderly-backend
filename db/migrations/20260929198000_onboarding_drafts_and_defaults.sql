-- +goose Up
-- +goose StatementBegin

-- The onboarding wizard (HLD §8: Business Type → ... → Review → Provision →
-- Complete) accumulates answers here, step by step, and touches `tenants`
-- only once, in a single transaction, on the Provision step. Nothing before
-- that point creates a queryable tenant — this is what makes provisioning
-- idempotent and prevents a partially-active tenant (POC requirement) if any
-- step fails: the draft just stays IN_PROGRESS and can be resumed or retried.
CREATE TABLE tenant_onboarding_drafts (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_by             UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    step                   TEXT NOT NULL DEFAULT 'BUSINESS_TYPE' CHECK (step IN (
        'BUSINESS_TYPE', 'BUSINESS_INFO', 'OWNER', 'PLAN', 'LICENSE', 'TERMS',
        'TYPE_CONFIG', 'REVIEW', 'PROVISION', 'COMPLETE'
    )),
    payload                JSONB NOT NULL DEFAULT '{}'::jsonb,
    status                 TEXT NOT NULL DEFAULT 'IN_PROGRESS'
                           CHECK (status IN ('IN_PROGRESS', 'PROVISIONED', 'ABANDONED')),
    provisioned_tenant_id  UUID REFERENCES tenants (id),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tenant_onboarding_drafts_status_idx ON tenant_onboarding_drafts (status);

-- Starter content per business type, applied by the Provision step so a new
-- tenant isn't a blank screen on day one. This is data, not Go seed logic, so
-- adding a business type's defaults never requires a code change.
CREATE TABLE business_type_defaults (
    business_type_code TEXT NOT NULL REFERENCES tenant_types (code),
    capability_code     TEXT NOT NULL REFERENCES capabilities (code),
    seed                JSONB NOT NULL,
    PRIMARY KEY (business_type_code, capability_code)
);

INSERT INTO business_type_defaults (business_type_code, capability_code, seed) VALUES
    ('FOOD_SHOP', 'CATALOG',  '{"categories": ["Starters", "Mains", "Beverages"]}'),
    ('CAFE',      'CATALOG',  '{"categories": ["Coffee", "Tea", "Pastries", "Snacks"]}'),
    ('RESTAURANT','CATALOG',  '{"categories": ["Starters", "Main Course", "Desserts", "Beverages"]}'),
    ('GROCERY',   'CATALOG',  '{"categories": ["Fruits & Vegetables", "Dairy", "Snacks", "Beverages"]}'),
    ('BARBER',    'SERVICES', '{"services": [
        {"name": "Haircut", "duration_minutes": 30, "price": 0},
        {"name": "Beard Trim", "duration_minutes": 15, "price": 0},
        {"name": "Shave", "duration_minutes": 20, "price": 0}
    ]}'),
    ('HOTEL',     'ROOMS',    '{"room_types": [
        {"name": "Standard", "base_price": 0, "max_guests": 2}
    ]}');
    -- GENERAL intentionally has no default row: it is the blank-canvas type,
    -- the owner picks capabilities and content during TYPE_CONFIG.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS business_type_defaults;
DROP TABLE IF EXISTS tenant_onboarding_drafts;
-- +goose StatementEnd
