-- name: GetEmailBranding :one
SELECT * FROM email_branding
WHERE tenant_id IS NOT DISTINCT FROM sqlc.narg(tenant_id)::uuid;

-- name: UpdateEmailBranding :execrows
UPDATE email_branding SET
    show_logo = sqlc.arg(show_logo), logo_url = sqlc.arg(logo_url),
    brand_color = sqlc.arg(brand_color), footer_text = sqlc.arg(footer_text),
    theme = sqlc.arg(theme), page_color = sqlc.arg(page_color), card_color = sqlc.arg(card_color), layout = sqlc.arg(layout),
    updated_at = now()
WHERE tenant_id IS NOT DISTINCT FROM sqlc.narg(tenant_id)::uuid;

-- name: InsertTenantEmailBranding :exec
INSERT INTO email_branding (tenant_id, show_logo, logo_url, brand_color, footer_text, theme, page_color, card_color, layout)
VALUES (sqlc.arg(tenant_id), sqlc.arg(show_logo), sqlc.arg(logo_url), sqlc.arg(brand_color), sqlc.arg(footer_text), sqlc.arg(theme), sqlc.arg(page_color), sqlc.arg(card_color), sqlc.arg(layout));

-- A business's identity for its emails: display name and storefront logo,
-- used when it has not set an email logo of its own.
-- name: GetTenantEmailIdentity :one
SELECT t.name,
       COALESCE(NULLIF(s.business_name, ''), t.name)::text AS display_name,
       COALESCE(NULLIF(s.logo_url, ''), t.logo_url)::text AS logo_url
FROM tenants t
LEFT JOIN tenant_storefront_settings s ON s.tenant_id = t.id
WHERE t.id = $1;
