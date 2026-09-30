-- name: CreateCategory :one
INSERT INTO categories (tenant_id, name, description, sort_order, is_active, image_url)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListCategoriesByTenant :many
SELECT * FROM categories
WHERE tenant_id = $1
ORDER BY sort_order ASC, name ASC;

-- name: GetCategoryByID :one
SELECT * FROM categories
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: UpdateCategory :one
UPDATE categories
SET
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    sort_order = COALESCE(sqlc.narg(sort_order), sort_order),
    is_active = COALESCE(sqlc.narg(is_active), is_active),
    image_url = COALESCE(sqlc.narg(image_url), image_url),
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DeleteCategory :exec
DELETE FROM categories
WHERE id = $1 AND tenant_id = $2;

-- name: UpdateCategorySortOrder :exec
UPDATE categories
SET sort_order = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2;

-- name: CountProductsInCategory :one
SELECT COUNT(*)::bigint AS count
FROM products
WHERE tenant_id = $1 AND category_id = $2;

-- name: MoveProductsToCategory :exec
UPDATE products
SET category_id = $3, updated_at = now()
WHERE tenant_id = $1 AND category_id = $2;

-- name: CreateProduct :one
INSERT INTO products (
    tenant_id, category_id, name, description, price, image_url, sort_order, is_available,
    is_vegetarian, is_featured, is_popular, allow_special_instructions, addons
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING *;

-- name: ListProductsByTenant :many
SELECT * FROM products
WHERE tenant_id = $1
ORDER BY sort_order ASC, name ASC;

-- name: TenantHasActiveCategory :one
-- Launch checklist only needs existence — never pull the full catalogue.
SELECT EXISTS(
    SELECT 1 FROM categories
    WHERE tenant_id = $1 AND is_active = true
);

-- name: TenantHasAvailableProduct :one
SELECT EXISTS(
    SELECT 1 FROM products
    WHERE tenant_id = $1 AND is_available = true
);

-- name: GetProductByID :one
SELECT * FROM products
WHERE id = $1 AND tenant_id = $2
LIMIT 1;

-- name: UpdateProduct :one
UPDATE products
SET
    category_id = COALESCE(sqlc.narg(category_id), category_id),
    name = COALESCE(sqlc.narg(name), name),
    description = COALESCE(sqlc.narg(description), description),
    price = COALESCE(sqlc.narg(price), price),
    image_url = COALESCE(sqlc.narg(image_url), image_url),
    sort_order = COALESCE(sqlc.narg(sort_order), sort_order),
    is_available = COALESCE(sqlc.narg(is_available), is_available),
    is_vegetarian = COALESCE(sqlc.narg(is_vegetarian), is_vegetarian),
    is_featured = COALESCE(sqlc.narg(is_featured), is_featured),
    is_popular = COALESCE(sqlc.narg(is_popular), is_popular),
    allow_special_instructions = COALESCE(sqlc.narg(allow_special_instructions), allow_special_instructions),
    addons = COALESCE(sqlc.narg(addons)::jsonb, addons),
    updated_at = now()
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: UpdateProductSortOrder :exec
UPDATE products
SET sort_order = $3, updated_at = now()
WHERE id = $1 AND tenant_id = $2;

-- name: DeleteProduct :exec
DELETE FROM products
WHERE id = $1 AND tenant_id = $2;

-- name: ListActiveMenuCategories :many
SELECT * FROM categories
WHERE tenant_id = $1 AND is_active = TRUE
ORDER BY sort_order ASC, name ASC;

-- name: ListAvailableProductsByTenant :many
SELECT * FROM products
WHERE tenant_id = $1 AND is_available = TRUE
ORDER BY sort_order ASC, name ASC;

-- Home page merchandising queries. Both are scoped to the tenant, so a
-- storefront can only ever see its own products.
-- name: ListFeaturedProducts :many
SELECT * FROM products
WHERE tenant_id = sqlc.arg(tenant_id)
  AND is_available = TRUE
  AND is_featured = TRUE
ORDER BY sort_order ASC, name ASC
LIMIT sqlc.arg(limit_count);

-- name: ListPopularProducts :many
SELECT * FROM products
WHERE tenant_id = sqlc.arg(tenant_id)
  AND is_available = TRUE
  AND is_popular = TRUE
ORDER BY sort_order ASC, name ASC
LIMIT sqlc.arg(limit_count);
