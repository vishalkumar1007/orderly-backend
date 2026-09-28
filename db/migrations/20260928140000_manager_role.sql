-- +goose Up
-- +goose StatementBegin

-- The product defines three business roles: owner, manager and staff. Only two
-- existed, which left every shop with a cliff — a trusted person either ran
-- orders and nothing else, or could rewrite the storefront and grant access.
--
-- MANAGER sits between them. What it may reach is decided in one place in Go
-- (pkg/identity/permissions.go); the database's job is only to accept the value
-- and keep the tenant pairing honest.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('SUPER_ADMIN', 'TENANT_ADMIN', 'MANAGER', 'STAFF'));

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_tenant_role_check;
ALTER TABLE users ADD CONSTRAINT users_tenant_role_check
    CHECK (
        (role = 'SUPER_ADMIN' AND tenant_id IS NULL)
        OR (role IN ('TENANT_ADMIN', 'MANAGER', 'STAFF') AND tenant_id IS NOT NULL)
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Managers become staff rather than being deleted: the rollback must not
-- remove someone's access to the shop they work in.
UPDATE users SET role = 'STAFF' WHERE role = 'MANAGER';

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('SUPER_ADMIN', 'TENANT_ADMIN', 'STAFF'));

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_tenant_role_check;
ALTER TABLE users ADD CONSTRAINT users_tenant_role_check
    CHECK (
        (role = 'SUPER_ADMIN' AND tenant_id IS NULL)
        OR (role IN ('TENANT_ADMIN', 'STAFF') AND tenant_id IS NOT NULL)
    );

-- +goose StatementEnd
