-- Capability, license and terms queries. These back the tenant-isolation
-- middleware (tenantctx) and the onboarding transaction (platform.CreateTenant)
-- — see planing/business_saas_lld.md and business_saas_db_map.md.

-- name: ListEnabledCapabilities :many
SELECT capability_code FROM tenant_capabilities
WHERE tenant_id = $1 AND enabled = TRUE;

-- name: GetTenantLicenseStatus :one
SELECT status FROM licenses WHERE tenant_id = $1;

-- name: GetBusinessTypeCapabilities :many
SELECT capability_code, default_enabled, configurable FROM business_type_capabilities
WHERE business_type_code = $1;

-- name: GetBusinessTypeCapabilitiesWithLabels :many
-- Same as GetBusinessTypeCapabilities, with the human label joined in — for
-- the onboarding wizard, which has no reason to carry its own copy of the
-- capability catalogue's labels.
SELECT btc.capability_code, c.label, btc.default_enabled, btc.configurable
FROM business_type_capabilities btc
JOIN capabilities c ON c.code = btc.capability_code
WHERE btc.business_type_code = $1
ORDER BY btc.capability_code;

-- name: UpsertTenantCapability :exec
INSERT INTO tenant_capabilities (tenant_id, capability_code, enabled, source)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, capability_code) DO UPDATE
    SET enabled = EXCLUDED.enabled, source = EXCLUDED.source, updated_at = now();

-- name: ListTenantCapabilities :many
SELECT capability_code, enabled, source, updated_at FROM tenant_capabilities
WHERE tenant_id = $1
ORDER BY capability_code;

-- name: SetTenantCapability :one
-- Caller must have already checked business_type_capabilities.configurable —
-- this statement does not, since it has no way to know the tenant's business
-- type without a join the caller already has cheaper access to.
UPDATE tenant_capabilities
SET enabled = $3, source = 'OVERRIDE', updated_at = now()
WHERE tenant_id = $1 AND capability_code = $2
RETURNING *;

-- name: GetBusinessTypeDefaults :many
SELECT capability_code, seed FROM business_type_defaults
WHERE business_type_code = $1;

-- name: GetLatestTermsDocument :one
SELECT * FROM terms_documents
ORDER BY published_at DESC
LIMIT 1;

-- name: CreateTermsAcceptance :one
INSERT INTO terms_acceptances (tenant_id, user_id, document_id, metadata)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetLicenseTemplateByPlan :one
SELECT * FROM license_templates WHERE plan_id = $1;

-- name: CreateLicense :one
INSERT INTO licenses (tenant_id, template_id, status, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetLicenseByTenant :one
SELECT * FROM licenses WHERE tenant_id = $1;

-- name: SetLicenseStatus :one
UPDATE licenses
SET status = $2, revoked_reason = $3, updated_at = now()
WHERE tenant_id = $1
RETURNING *;
