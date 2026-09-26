-- Platform-wide provider configuration, one row per service.

-- name: GetPlatformConfiguration :one
SELECT * FROM platform_configurations
WHERE service_type = sqlc.arg(service_type)
LIMIT 1;

-- name: ListPlatformConfigurations :many
SELECT * FROM platform_configurations
ORDER BY service_type ASC;

-- name: UpsertPlatformConfiguration :one
INSERT INTO platform_configurations (
    service_type, provider, config, secret_config, status,
    enabled, allow_tenants, last_error, last_tested_at
) VALUES (
    sqlc.arg(service_type), sqlc.arg(provider), sqlc.arg(config), sqlc.arg(secret_config),
    sqlc.arg(status), sqlc.arg(enabled), sqlc.arg(allow_tenants), sqlc.arg(last_error),
    sqlc.narg(last_tested_at)
)
ON CONFLICT (service_type) DO UPDATE SET
    provider      = EXCLUDED.provider,
    config        = EXCLUDED.config,
    secret_config = EXCLUDED.secret_config,
    status        = EXCLUDED.status,
    enabled       = EXCLUDED.enabled,
    allow_tenants = EXCLUDED.allow_tenants,
    last_error    = EXCLUDED.last_error,
    last_tested_at = EXCLUDED.last_tested_at,
    updated_at    = now()
RETURNING *;

-- name: SetPlatformConfigurationAllowTenants :one
UPDATE platform_configurations
SET allow_tenants = sqlc.arg(allow_tenants), updated_at = now()
WHERE service_type = sqlc.arg(service_type)
RETURNING *;

-- name: SetPlatformConfigurationStatus :one
-- Records the outcome of a connection test without touching credentials.
UPDATE platform_configurations
SET
    status        = sqlc.arg(status),
    last_error    = sqlc.arg(last_error),
    last_tested_at = now(),
    updated_at    = now()
WHERE service_type = sqlc.arg(service_type)
RETURNING *;

-- name: DeletePlatformConfiguration :exec
DELETE FROM platform_configurations
WHERE service_type = sqlc.arg(service_type);

-- name: CountPlatformTenantsUsingService :one
SELECT COUNT(*)::bigint
FROM tenant_service_preferences
WHERE service_type = sqlc.arg(service_type)
  AND source = 'PLATFORM';

-- Per-tenant configuration.

-- name: GetTenantConfiguration :one
SELECT * FROM tenant_configurations
WHERE tenant_id = sqlc.arg(tenant_id)
  AND service_type = sqlc.arg(service_type)
LIMIT 1;

-- name: ListTenantConfigurations :many
SELECT * FROM tenant_configurations
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY service_type ASC;

-- name: UpsertTenantConfiguration :one
INSERT INTO tenant_configurations (
    tenant_id, service_type, provider, config, secret_config,
    status, enabled, last_error, last_tested_at
) VALUES (
    sqlc.arg(tenant_id), sqlc.arg(service_type), sqlc.arg(provider), sqlc.arg(config),
    sqlc.arg(secret_config), sqlc.arg(status), sqlc.arg(enabled), sqlc.arg(last_error),
    sqlc.narg(last_tested_at)
)
ON CONFLICT (tenant_id, service_type) DO UPDATE SET
    provider      = EXCLUDED.provider,
    config        = EXCLUDED.config,
    secret_config = EXCLUDED.secret_config,
    status        = EXCLUDED.status,
    enabled       = EXCLUDED.enabled,
    last_error    = EXCLUDED.last_error,
    last_tested_at = EXCLUDED.last_tested_at,
    updated_at    = now()
RETURNING *;

-- name: SetTenantConfigurationStatus :one
UPDATE tenant_configurations
SET
    status        = sqlc.arg(status),
    last_error    = sqlc.arg(last_error),
    last_tested_at = now(),
    updated_at    = now()
WHERE tenant_id = sqlc.arg(tenant_id)
  AND service_type = sqlc.arg(service_type)
RETURNING *;

-- name: DeleteTenantConfiguration :exec
DELETE FROM tenant_configurations
WHERE tenant_id = sqlc.arg(tenant_id)
  AND service_type = sqlc.arg(service_type);

-- Source preference.

-- name: GetTenantServicePreference :one
SELECT * FROM tenant_service_preferences
WHERE tenant_id = sqlc.arg(tenant_id)
  AND service_type = sqlc.arg(service_type)
LIMIT 1;

-- name: ListTenantServicePreferences :many
SELECT * FROM tenant_service_preferences
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY service_type ASC;

-- name: UpsertTenantServicePreference :one
INSERT INTO tenant_service_preferences (tenant_id, service_type, source)
VALUES (sqlc.arg(tenant_id), sqlc.arg(service_type), sqlc.arg(source))
ON CONFLICT (tenant_id, service_type) DO UPDATE SET
    source = EXCLUDED.source,
    updated_at = now()
RETURNING *;

-- Per-tenant permission to borrow the platform configuration.

-- name: GetTenantServiceAccess :one
SELECT * FROM tenant_service_access
WHERE tenant_id = sqlc.arg(tenant_id)
  AND service_type = sqlc.arg(service_type)
LIMIT 1;

-- name: ListTenantServiceAccess :many
SELECT * FROM tenant_service_access
WHERE tenant_id = sqlc.arg(tenant_id)
ORDER BY service_type ASC;

-- name: UpsertTenantServiceAccess :one
INSERT INTO tenant_service_access (tenant_id, service_type, allow_platform)
VALUES (sqlc.arg(tenant_id), sqlc.arg(service_type), sqlc.arg(allow_platform))
ON CONFLICT (tenant_id, service_type) DO UPDATE SET
    allow_platform = EXCLUDED.allow_platform,
    updated_at    = now()
RETURNING *;


-- name: ListServiceAccessForAllTenants :many
-- Ensures every (tenant, service) pair appears, even when unset, so the Super
-- Admin table can render a complete grid.
SELECT
    t.id AS tenant_id,
    t.name AS tenant_name,
    t.slug AS tenant_slug,
    sv.service_type::text AS service_type,
    COALESCE(a.allow_platform, FALSE) AS allow_platform,
    p.source,
    tc.status  AS own_status,
    tc.enabled AS own_enabled,
    pc.status  AS platform_status,
    pc.enabled AS platform_enabled,
    pc.allow_tenants AS platform_allow_tenants
FROM tenants t
CROSS JOIN (VALUES ('SMTP'), ('STORAGE'), ('AI')) AS sv(service_type)
LEFT JOIN tenant_service_access a
       ON a.tenant_id = t.id AND a.service_type = sv.service_type
LEFT JOIN tenant_service_preferences p
       ON p.tenant_id = t.id AND p.service_type = sv.service_type
LEFT JOIN tenant_configurations tc
       ON tc.tenant_id = t.id AND tc.service_type = sv.service_type
LEFT JOIN platform_configurations pc
       ON pc.service_type = sv.service_type
-- Service ordering is applied in Go: sqlc cannot resolve a VALUES alias in ORDER BY.
ORDER BY t.name ASC;
