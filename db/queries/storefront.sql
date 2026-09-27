-- name: EnsureStorefrontSettings :one
INSERT INTO tenant_storefront_settings (tenant_id, business_name, phone, address, logo_url, favicon_url, tagline)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: GetStorefrontSettings :one
SELECT * FROM tenant_storefront_settings
WHERE tenant_id = $1
LIMIT 1;

-- Full storefront row joined with the tenant, so identity fallbacks (name,
-- phone, address, logo) can be read without a second round trip.
-- name: GetStorefrontConfig :one
SELECT
    s.tenant_id,
    t.name              AS tenant_name,
    t.slug,
    t.business_type,
    t.currency,
    t.timezone,
    t.is_published,
    t.store_status,
    t.status_message,
    s.logo_url,
    s.favicon_url,
    s.business_name,
    s.tagline,
    s.description,
    s.phone,
    s.address,
    s.theme_preset,
    s.primary_color,
    s.secondary_color,
    s.accent_color,
    s.theme_mode,
    s.font_family,
    s.radius,
    s.button_style,
    s.card_style,
    s.header_style,
    s.hero_style,
    s.hero_image_url,
    s.product_layout,
    s.filter_style,
    s.ordering_enabled,
    s.closed_message,
    s.customer_login_enabled,
    s.customer_login_mode,
    s.prep_time_minutes,
    s.tax_percent,
    s.packaging_fee,
    s.opening_hours,
    s.homepage,
    s.payments,
    s.workflow,
    s.updated_at
FROM tenant_storefront_settings s
JOIN tenants t ON t.id = s.tenant_id
WHERE s.tenant_id = sqlc.arg(tenant_id)
LIMIT 1;

-- name: UpdateStorefrontIdentity :one
UPDATE tenant_storefront_settings
SET
    logo_url      = COALESCE(sqlc.narg(logo_url), logo_url),
    favicon_url   = COALESCE(sqlc.narg(favicon_url), favicon_url),
    business_name = COALESCE(sqlc.narg(business_name), business_name),
    tagline       = COALESCE(sqlc.narg(tagline), tagline),
    description   = COALESCE(sqlc.narg(description), description),
    phone         = COALESCE(sqlc.narg(phone), phone),
    address       = COALESCE(sqlc.narg(address), address),
    updated_at    = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontBehaviour :one
UPDATE tenant_storefront_settings
SET
    ordering_enabled       = COALESCE(sqlc.narg(ordering_enabled), ordering_enabled),
    closed_message         = COALESCE(sqlc.narg(closed_message), closed_message),
    customer_login_mode    = COALESCE(sqlc.narg(customer_login_mode), customer_login_mode),
    prep_time_minutes      = COALESCE(sqlc.narg(prep_time_minutes), prep_time_minutes),
    updated_at             = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontTheme :one
UPDATE tenant_storefront_settings
SET
    theme_preset    = COALESCE(sqlc.narg(theme_preset), theme_preset),
    primary_color   = COALESCE(sqlc.narg(primary_color), primary_color),
    secondary_color = COALESCE(sqlc.narg(secondary_color), secondary_color),
    accent_color    = COALESCE(sqlc.narg(accent_color), accent_color),
    theme_mode      = COALESCE(sqlc.narg(theme_mode), theme_mode),
    font_family     = COALESCE(sqlc.narg(font_family), font_family),
    radius          = COALESCE(sqlc.narg(radius), radius),
    button_style    = COALESCE(sqlc.narg(button_style), button_style),
    card_style      = COALESCE(sqlc.narg(card_style), card_style),
    header_style    = COALESCE(sqlc.narg(header_style), header_style),
    hero_style      = COALESCE(sqlc.narg(hero_style), hero_style),
    hero_image_url  = COALESCE(sqlc.narg(hero_image_url), hero_image_url),
    product_layout  = COALESCE(sqlc.narg(product_layout), product_layout),
    filter_style    = COALESCE(sqlc.narg(filter_style), filter_style),
    updated_at      = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontOpeningHours :one
UPDATE tenant_storefront_settings
SET opening_hours = sqlc.arg(opening_hours)::jsonb, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontHomepage :one
UPDATE tenant_storefront_settings
SET homepage = sqlc.arg(homepage)::jsonb, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontPayments :one
UPDATE tenant_storefront_settings
SET payments = sqlc.arg(payments)::jsonb, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontWorkflow :one
UPDATE tenant_storefront_settings
SET workflow = sqlc.arg(workflow)::jsonb, updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateStorefrontCosting :one
UPDATE tenant_storefront_settings
SET tax_percent = COALESCE(sqlc.narg(tax_percent), tax_percent), packaging_fee = COALESCE(sqlc.narg(packaging_fee), packaging_fee), updated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: SetStorefrontPublished :one
UPDATE tenants
SET is_published = sqlc.arg(is_published), updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING *;
