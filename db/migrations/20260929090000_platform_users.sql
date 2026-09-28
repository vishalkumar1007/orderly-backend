-- +goose Up
-- +goose StatementBegin

-- The platform console needs more than one person.
--
-- Until now exactly one SUPER_ADMIN could exist, which meant the only way to
-- give a colleague access was to share a password. These two roles fix that
-- without weakening the owner account: PLATFORM_ADMIN runs the platform
-- alongside the owner, SUPPORT can look but not change. Both are platform
-- identities — tenant_id stays NULL — and neither grants anything inside a
-- business, which is still a separate authentication context entirely.
--
-- The single-owner rule is kept: the SUPER_ADMIN is the account that cannot be
-- removed or demoted, so a console can never be left with nobody able to
-- restore access.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('SUPER_ADMIN', 'PLATFORM_ADMIN', 'SUPPORT', 'TENANT_ADMIN', 'MANAGER', 'STAFF'));

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_tenant_role_check;
ALTER TABLE users ADD CONSTRAINT users_tenant_role_check
    CHECK (
        (role IN ('SUPER_ADMIN', 'PLATFORM_ADMIN', 'SUPPORT') AND tenant_id IS NULL)
        OR (role IN ('TENANT_ADMIN', 'MANAGER', 'STAFF') AND tenant_id IS NOT NULL)
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Platform staff become nothing on rollback: there is no tenant to move them
-- to, and leaving rows that violate the restored constraint would break every
-- later migration. Their audit history is preserved by nulling the actor.
UPDATE audit_logs SET user_id = NULL
WHERE user_id IN (SELECT id FROM users WHERE role IN ('PLATFORM_ADMIN', 'SUPPORT'));
DELETE FROM refresh_tokens
WHERE user_id IN (SELECT id FROM users WHERE role IN ('PLATFORM_ADMIN', 'SUPPORT'));
DELETE FROM users WHERE role IN ('PLATFORM_ADMIN', 'SUPPORT');

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
