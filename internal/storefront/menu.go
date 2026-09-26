package storefront

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Addon is a purchasable extra attached to a product.
//
// Prices live on the product, not in the cart. The client sends only an addon
// id and a quantity; the amount is always read from here at order time.
type Addon struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Price  float64 `json:"price"`
	MaxQty int     `json:"max_qty"`
}

// Product is the public projection of a menu product.
type Product struct {
	ID          string  `json:"id"`
	CategoryID  string  `json:"category_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	ImageURL    string  `json:"image_url,omitempty"`
	Available   bool    `json:"is_available"`
	Vegetarian  bool    `json:"is_vegetarian"`
	Featured    bool    `json:"is_featured"`
	Popular     bool    `json:"is_popular"`
	AllowsNotes bool    `json:"allow_special_instructions"`
	Addons      []Addon `json:"addons"`
	SortOrder   int32   `json:"sort_order"`
}

// HasAddons reports whether the product needs a customisation step.
func (p Product) HasAddons() bool { return len(p.Addons) > 0 }

// Category groups products for the storefront menu.
type Category struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SortOrder   int32     `json:"sort_order"`
	Products    []Product `json:"products"`
}

// Menu is the full public menu for one tenant.
type Menu struct {
	Categories []Category `json:"categories"`
	Products   []Product  `json:"products"`
}

// Find returns a category by id.
func (m Menu) Find(id string) (Category, bool) {
	for _, c := range m.Categories {
		if c.ID == id {
			return c, true
		}
	}
	return Category{}, false
}

// Product returns a product by id.
func (m Menu) Product(id string) (Product, bool) {
	for _, p := range m.Products {
		if p.ID == id {
			return p, true
		}
	}
	return Product{}, false
}

// ProductsIn returns the products of one category, or every product when the
// id is empty.
func (m Menu) ProductsIn(categoryID string) []Product {
	if categoryID == "" {
		return m.Products
	}
	if c, ok := m.Find(categoryID); ok {
		return c.Products
	}
	return nil
}

// ErrProductUnavailable means the product is missing, belongs to another
// tenant, or has been taken off sale. The three cases are deliberately
// indistinguishable to the caller.
var ErrProductUnavailable = errors.New("product unavailable")

// Catalog reads menu data for a tenant. Every query is scoped by tenant id.
type Catalog struct {
	q *sqlc.Queries
}

// NewCatalog builds a Catalog over a pool.
func NewCatalog(q *sqlc.Queries) *Catalog { return &Catalog{q: q} }

// LoadMenu reads every active category with its available products.
func (c *Catalog) LoadMenu(ctx context.Context, tenantID uuid.UUID) (*Menu, error) {
	cats, err := c.q.ListActiveMenuCategories(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, err
	}
	prods, err := c.q.ListAvailableProductsByTenant(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, err
	}
	menu := &Menu{Categories: make([]Category, 0, len(cats)), Products: make([]Product, 0, len(prods))}
	byCategory := map[string][]Product{}
	for _, p := range prods {
		pub := publicProduct(p)
		menu.Products = append(menu.Products, pub)
		cid := pgutil.UUIDString(p.CategoryID)
		byCategory[cid] = append(byCategory[cid], pub)
	}
	for _, cat := range cats {
		cid := pgutil.UUIDString(cat.ID)
		items := byCategory[cid]
		if items == nil {
			items = []Product{}
		}
		menu.Categories = append(menu.Categories, Category{
			ID:          cid,
			Name:        cat.Name,
			Description: cat.Description,
			SortOrder:   cat.SortOrder,
			Products:    items,
		})
	}
	return menu, nil
}

// LoadProduct reads one available product.
func (c *Catalog) LoadProduct(ctx context.Context, tenantID, productID uuid.UUID) (Product, error) {
	row, err := c.q.GetProductByID(ctx, sqlc.GetProductByIDParams{
		ID:       pgutil.UUID(productID),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Product{}, ErrProductUnavailable
		}
		return Product{}, err
	}
	if !row.IsAvailable {
		return Product{}, ErrProductUnavailable
	}
	return publicProduct(row), nil
}

// LoadFeatured reads products flagged as featured, falling back to the newest
// products when a tenant has flagged none — a home page must never render an
// empty shelf.
func (c *Catalog) LoadFeatured(ctx context.Context, tenantID uuid.UUID, limit int32) ([]Product, error) {
	rows, err := c.q.ListFeaturedProducts(ctx, sqlc.ListFeaturedProductsParams{
		TenantID:   pgutil.UUID(tenantID),
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		rows, err = c.q.ListAvailableProductsByTenant(ctx, pgutil.UUID(tenantID))
		if err != nil {
			return nil, err
		}
		if int32(len(rows)) > limit {
			rows = rows[:limit]
		}
	}
	return toPublicProducts(rows), nil
}

// LoadPopular reads products flagged as popular, falling back to featured so
// the section still has content.
func (c *Catalog) LoadPopular(ctx context.Context, tenantID uuid.UUID, limit int32) ([]Product, error) {
	rows, err := c.q.ListPopularProducts(ctx, sqlc.ListPopularProductsParams{
		TenantID:   pgutil.UUID(tenantID),
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return c.LoadFeatured(ctx, tenantID, limit)
	}
	return toPublicProducts(rows), nil
}

// LoadForOrder re-reads the products named in a cart, dropping anything that
// has gone off sale. Prices come from these rows, never from the request.
func (c *Catalog) LoadForOrder(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[string]Product, error) {
	if len(ids) == 0 {
		return map[string]Product{}, nil
	}
	pgIDs := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		pgIDs[i] = pgutil.UUID(id)
	}
	rows, err := c.q.GetProductsByIDs(ctx, sqlc.GetProductsByIDsParams{
		TenantID: pgutil.UUID(tenantID),
		Ids:      pgIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]Product, len(rows))
	for _, p := range rows {
		pub := publicProduct(p)
		out[pub.ID] = pub
	}
	return out, nil
}

func toPublicProducts(rows []sqlc.Product) []Product {
	out := make([]Product, 0, len(rows))
	for _, p := range rows {
		out = append(out, publicProduct(p))
	}
	return out
}

func publicProduct(p sqlc.Product) Product {
	return Product{
		ID:          pgutil.UUIDString(p.ID),
		CategoryID:  pgutil.UUIDString(p.CategoryID),
		Name:        p.Name,
		Description: p.Description,
		Price:       pgutil.NumericToFloat(p.Price),
		ImageURL:    p.ImageUrl.String,
		Available:   p.IsAvailable,
		Vegetarian:  p.IsVegetarian,
		Featured:    p.IsFeatured,
		Popular:     p.IsPopular,
		AllowsNotes: p.AllowSpecialInstructions,
		Addons:      ParseAddons(p.Addons),
		SortOrder:   p.SortOrder,
	}
}

// ParseAddons reads a product's add-on catalogue, dropping malformed entries.
func ParseAddons(raw []byte) []Addon {
	out := []Addon{}
	if len(raw) == 0 {
		return out
	}
	var docs []struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Price  float64 `json:"price"`
		MaxQty int     `json:"max_qty"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return out
	}
	for i, d := range docs {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			continue
		}
		if d.Price < 0 {
			continue
		}
		maxQty := d.MaxQty
		if maxQty <= 0 {
			maxQty = 1
		}
		if maxQty > 20 {
			maxQty = 20
		}
		id := strings.TrimSpace(d.ID)
		if id == "" {
			id = slugify(name, i)
		}
		out = append(out, Addon{ID: id, Name: name, Price: round2(d.Price), MaxQty: maxQty})
	}
	return out
}

// FindAddon looks up an add-on by id on a product.
func (p Product) FindAddon(id string) (Addon, bool) {
	for _, a := range p.Addons {
		if a.ID == id {
			return a, true
		}
	}
	return Addon{}, false
}

func slugify(s string, i int) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "addon-" + itoa(i+1)
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5*sign(v))) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// EstimatedReady returns when an order placed now should be ready.
func EstimatedReady(prepMinutes int, from time.Time) time.Time {
	if prepMinutes < 0 {
		prepMinutes = 0
	}
	return from.Add(time.Duration(prepMinutes) * time.Minute)
}
