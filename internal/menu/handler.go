package menu

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool)}
}

func tenantID(r *http.Request) uuid.UUID {
	u, _ := identity.UserFromContext(r.Context())
	return *u.TenantID
}

type categoryReq struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	ImageURL    *string `json:"image_url"`
	SortOrder   *int32  `json:"sort_order"`
	IsActive    *bool   `json:"is_active"`
}

type productReq struct {
	CategoryID               string          `json:"category_id"`
	Name                     string          `json:"name"`
	Description              string          `json:"description"`
	Price                    *float64        `json:"price"`
	ImageURL                 *string         `json:"image_url"`
	SortOrder                *int32          `json:"sort_order"`
	IsAvailable              *bool           `json:"is_available"`
	IsVegetarian             *bool           `json:"is_vegetarian"`
	IsFeatured               *bool           `json:"is_featured"`
	IsPopular                *bool           `json:"is_popular"`
	AllowSpecialInstructions *bool           `json:"allow_special_instructions"`
	OptionGroups             json.RawMessage `json:"option_groups"`
	Addons                   json.RawMessage `json:"addons"`
}

type reorderReq struct {
	IDs []string `json:"ids"`
}

func (h *Handler) ListCategories(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListCategoriesByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list categories")
		return
	}
	out := make([]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, categoryJSON(c))
	}
	response.JSON(w, http.StatusOK, map[string]any{"categories": out})
}

func (h *Handler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	var req categoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name required")
		return
	}
	sortOrder := int32(0)
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	c, err := h.q.CreateCategory(r.Context(), sqlc.CreateCategoryParams{
		TenantID:    pgutil.UUID(tenantID(r)),
		Name:        strings.TrimSpace(req.Name),
		Description: req.Description,
		SortOrder:   sortOrder,
		IsActive:    isActive,
		ImageUrl:    pgutil.NullText(req.ImageURL),
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create category")
		return
	}
	response.JSON(w, http.StatusCreated, categoryJSON(c))
}

func (h *Handler) UpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req categoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateCategoryParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID(r)),
	}
	if strings.TrimSpace(req.Name) != "" {
		params.Name = pgtype.Text{String: strings.TrimSpace(req.Name), Valid: true}
	}
	params.Description = pgtype.Text{String: req.Description, Valid: true}
	if req.SortOrder != nil {
		params.SortOrder = pgtype.Int4{Int32: *req.SortOrder, Valid: true}
	}
	if req.IsActive != nil {
		params.IsActive = pgtype.Bool{Bool: *req.IsActive, Valid: true}
	}
	if req.ImageURL != nil {
		params.ImageUrl = pgutil.Text(*req.ImageURL)
	}
	c, err := h.q.UpdateCategory(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "category not found")
		return
	}
	response.JSON(w, http.StatusOK, categoryJSON(c))
}

func (h *Handler) DeleteCategory(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	tid := tenantID(r)
	moveTo := strings.TrimSpace(r.URL.Query().Get("move_to"))

	count, err := h.q.CountProductsInCategory(r.Context(), sqlc.CountProductsInCategoryParams{
		TenantID:   pgutil.UUID(tid),
		CategoryID: pgutil.UUID(id),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to check category")
		return
	}

	if count > 0 {
		if moveTo == "" {
			response.JSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{
					"code":          "category_in_use",
					"message":       "This category contains products. Move them first.",
					"product_count": count,
				},
			})
			return
		}
		destID, err := uuid.Parse(moveTo)
		if err != nil || destID == id {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid move_to category")
			return
		}
		if _, err := h.q.GetCategoryByID(r.Context(), sqlc.GetCategoryByIDParams{
			ID: pgutil.UUID(destID), TenantID: pgutil.UUID(tid),
		}); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "destination category not found")
			return
		}
		tx, err := h.pool.Begin(r.Context())
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete category")
			return
		}
		defer tx.Rollback(r.Context())
		qtx := h.q.WithTx(tx)
		if err := qtx.MoveProductsToCategory(r.Context(), sqlc.MoveProductsToCategoryParams{
			TenantID:     pgutil.UUID(tid),
			CategoryID:   pgutil.UUID(id),
			CategoryID_2: pgutil.UUID(destID),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to move products")
			return
		}
		if err := qtx.DeleteCategory(r.Context(), sqlc.DeleteCategoryParams{
			ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid),
		}); err != nil {
			response.Error(w, http.StatusConflict, "conflict", "cannot delete category")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete category")
			return
		}
		response.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}

	if err := h.q.DeleteCategory(r.Context(), sqlc.DeleteCategoryParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid),
	}); err != nil {
		response.Error(w, http.StatusConflict, "conflict", "cannot delete category")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) ReorderCategories(w http.ResponseWriter, r *http.Request) {
	h.reorder(w, r, true)
}

func (h *Handler) ReorderProducts(w http.ResponseWriter, r *http.Request) {
	h.reorder(w, r, false)
}

func (h *Handler) reorder(w http.ResponseWriter, r *http.Request, categories bool) {
	var req reorderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "ids required")
		return
	}
	tid := tenantID(r)
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reorder")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.q.WithTx(tx)
	for i, raw := range req.IDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
			return
		}
		if categories {
			if err := qtx.UpdateCategorySortOrder(r.Context(), sqlc.UpdateCategorySortOrderParams{
				ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid), SortOrder: int32(i),
			}); err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reorder")
				return
			}
		} else {
			if err := qtx.UpdateProductSortOrder(r.Context(), sqlc.UpdateProductSortOrderParams{
				ID: pgutil.UUID(id), TenantID: pgutil.UUID(tid), SortOrder: int32(i),
			}); err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reorder")
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reorder")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ListProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListProductsByTenant(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list products")
		return
	}
	out := make([]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, productJSON(p))
	}
	response.JSON(w, http.StatusOK, map[string]any{"products": out})
}

func encodeProductAddons(req productReq) ([]byte, error) {
	if len(req.OptionGroups) > 0 && string(req.OptionGroups) != "null" {
		groups, err := storefront.DecodeOptionGroupsWire(req.OptionGroups)
		if err != nil {
			return nil, err
		}
		return storefront.EncodeOptionGroupList(groups)
	}
	if len(req.Addons) > 0 && string(req.Addons) != "null" {
		return storefront.EncodeOptionGroups(req.Addons)
	}
	return []byte("[]"), nil
}

func (h *Handler) CreateProduct(w http.ResponseWriter, r *http.Request) {
	var req productReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.CategoryID == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name and category_id required")
		return
	}
	catID, err := uuid.Parse(req.CategoryID)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid category_id")
		return
	}
	if _, err := h.q.GetCategoryByID(r.Context(), sqlc.GetCategoryByIDParams{
		ID: pgutil.UUID(catID), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "category not found")
		return
	}
	if req.Price == nil || *req.Price < 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid price")
		return
	}
	price, err := pgutil.NumericFromFloat(*req.Price)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid price")
		return
	}
	sortOrder := int32(0)
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	available := true
	if req.IsAvailable != nil {
		available = *req.IsAvailable
	}
	vegetarian := false
	if req.IsVegetarian != nil {
		vegetarian = *req.IsVegetarian
	}
	featured := false
	if req.IsFeatured != nil {
		featured = *req.IsFeatured
	}
	popular := false
	if req.IsPopular != nil {
		popular = *req.IsPopular
	}
	allowNotes := true
	if req.AllowSpecialInstructions != nil {
		allowNotes = *req.AllowSpecialInstructions
	}
	addons, err := encodeProductAddons(req)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid options")
		return
	}
	p, err := h.q.CreateProduct(r.Context(), sqlc.CreateProductParams{
		TenantID:                 pgutil.UUID(tenantID(r)),
		CategoryID:               pgutil.UUID(catID),
		Name:                     strings.TrimSpace(req.Name),
		Description:              req.Description,
		Price:                    price,
		ImageUrl:                 pgutil.NullText(req.ImageURL),
		SortOrder:                sortOrder,
		IsAvailable:              available,
		IsVegetarian:             vegetarian,
		IsFeatured:               featured,
		IsPopular:                popular,
		AllowSpecialInstructions: allowNotes,
		Addons:                   addons,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not create product")
		return
	}
	response.JSON(w, http.StatusCreated, productJSON(p))
}

func (h *Handler) GetProduct(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	p, err := h.q.GetProductByID(r.Context(), sqlc.GetProductByIDParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}
	response.JSON(w, http.StatusOK, productJSON(p))
}

func (h *Handler) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	var req productReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateProductParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID(r)),
	}
	if req.CategoryID != "" {
		cid, err := uuid.Parse(req.CategoryID)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid category_id")
			return
		}
		params.CategoryID = pgutil.UUID(cid)
	}
	if req.Name != "" {
		params.Name = pgtype.Text{String: strings.TrimSpace(req.Name), Valid: true}
	}
	params.Description = pgtype.Text{String: req.Description, Valid: true}
	if req.Price != nil {
		price, err := pgutil.NumericFromFloat(*req.Price)
		if err != nil || *req.Price < 0 {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid price")
			return
		}
		params.Price = price
	}
	if req.ImageURL != nil {
		params.ImageUrl = pgutil.Text(*req.ImageURL)
	}
	if req.SortOrder != nil {
		params.SortOrder = pgtype.Int4{Int32: *req.SortOrder, Valid: true}
	}
	if req.IsAvailable != nil {
		params.IsAvailable = pgtype.Bool{Bool: *req.IsAvailable, Valid: true}
	}
	if req.IsVegetarian != nil {
		params.IsVegetarian = pgtype.Bool{Bool: *req.IsVegetarian, Valid: true}
	}
	if req.IsFeatured != nil {
		params.IsFeatured = pgtype.Bool{Bool: *req.IsFeatured, Valid: true}
	}
	if req.IsPopular != nil {
		params.IsPopular = pgtype.Bool{Bool: *req.IsPopular, Valid: true}
	}
	if req.AllowSpecialInstructions != nil {
		params.AllowSpecialInstructions = pgtype.Bool{Bool: *req.AllowSpecialInstructions, Valid: true}
	}
	if (len(req.OptionGroups) > 0 && string(req.OptionGroups) != "null") ||
		(len(req.Addons) > 0 && string(req.Addons) != "null") {
		addons, err := encodeProductAddons(req)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid options")
			return
		}
		params.Addons = addons
	}
	p, err := h.q.UpdateProduct(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}
	response.JSON(w, http.StatusOK, productJSON(p))
}

func (h *Handler) DuplicateProduct(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	src, err := h.q.GetProductByID(r.Context(), sqlc.GetProductByIDParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}
	name := "Copy of " + src.Name
	p, err := h.q.CreateProduct(r.Context(), sqlc.CreateProductParams{
		TenantID:                 src.TenantID,
		CategoryID:               src.CategoryID,
		Name:                     name,
		Description:              src.Description,
		Price:                    src.Price,
		ImageUrl:                 src.ImageUrl,
		SortOrder:                src.SortOrder + 1,
		IsAvailable:              false,
		IsVegetarian:             src.IsVegetarian,
		IsFeatured:               false,
		IsPopular:                false,
		AllowSpecialInstructions: src.AllowSpecialInstructions,
		Addons:                   src.Addons,
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "could not duplicate product")
		return
	}
	response.JSON(w, http.StatusCreated, productJSON(p))
}

func (h *Handler) DeleteProduct(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteProduct(r.Context(), sqlc.DeleteProductParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusConflict, "conflict", "cannot delete product")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	h.setPublished(w, r, true)
}

func (h *Handler) Unpublish(w http.ResponseWriter, r *http.Request) {
	h.setPublished(w, r, false)
}

func (h *Handler) setPublished(w http.ResponseWriter, r *http.Request, published bool) {
	t, err := h.q.SetTenantPublished(r.Context(), sqlc.SetTenantPublishedParams{
		ID:          pgutil.UUID(tenantID(r)),
		IsPublished: published,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update publish state")
		return
	}
	if published {
		_, _ = h.q.SetTenantSetupStatus(r.Context(), sqlc.SetTenantSetupStatusParams{
			ID: t.ID, SetupStatus: "COMPLETED",
		})
		t.SetupStatus = "COMPLETED"
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"is_published": t.IsPublished,
		"setup_status": t.SetupStatus,
		"slug":         t.Slug,
		"status":       t.Status,
		"public_host":  t.Slug + ".localhost:5173",
		"public_path":  "/",
	})
}

func (h *Handler) StoreLink(w http.ResponseWriter, r *http.Request) {
	t, err := h.q.GetTenantByID(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "tenant not found")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"slug":         t.Slug,
		"public_host":  t.Slug + ".localhost:5173",
		"public_path":  "/",
		"is_published": t.IsPublished,
		"setup_status": t.SetupStatus,
		"status":       t.Status,
		"name":         t.Name,
	})
}

// SetupStatus reports what a new business still has to do before it can trade.
//
// Every step is derived from real state. An earlier version hard-coded
// business_info and payment to true and qr to false, which made the checklist
// worse than useless: it told an owner they had finished something they had
// never opened, and never let them tick off something they had.
func (h *Handler) SetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := tenantID(r)
	t, err := h.q.GetTenantByID(ctx, pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "tenant not found")
		return
	}

	cats, _ := h.q.ListCategoriesByTenant(ctx, t.ID)
	prods, _ := h.q.ListProductsByTenant(ctx, t.ID)
	hasCategory := false
	for _, c := range cats {
		if c.IsActive {
			hasCategory = true
			break
		}
	}
	// A product nobody can order does not make the shop sellable.
	hasSellable := false
	for _, p := range prods {
		if p.IsAvailable {
			hasSellable = true
			break
		}
	}

	// Contact details customers and the platform both depend on.
	hasBusinessInfo := strings.TrimSpace(t.Name) != "" &&
		(strings.TrimSpace(t.Phone) != "" || strings.TrimSpace(t.Email) != "") &&
		strings.TrimSpace(t.Address) != ""

	// The storefront documents answer the remaining three steps. A shop with no
	// storefront row has simply not started, so the zero values are correct.
	var hasPayment, hasHours, hasStorefront bool
	if sf, err := storefront.NewLoaderFromQueries(h.q).Load(ctx, id); err == nil && sf != nil {
		hasPayment = len(sf.Payments.Methods()) > 0
		// "Always open" is the default nobody chose. A real schedule, or an
		// explicit decision to stay always open, both count — the difference is
		// whether the document was ever written.
		hasHours = len(sf.OpeningHours.Schedule) > 0
		// Saved at least once: created_at and updated_at diverge on first write.
		hasStorefront = sf.UpdatedAt.After(t.CreatedAt.Time.Add(time.Second)) &&
			strings.TrimSpace(sf.Name) != ""
	}

	users, _ := h.q.ListTenantUsers(ctx, t.ID)

	response.JSON(w, http.StatusOK, map[string]any{
		"setup_status": t.SetupStatus,
		"is_published": t.IsPublished,
		"steps": map[string]bool{
			"business_info": hasBusinessInfo,
			"menu":          hasCategory && hasSellable,
			"payment":       hasPayment,
			"hours":         hasHours,
			"storefront":    hasStorefront,
			// Optional: a one-person shop is a complete shop.
			"staff":  len(users) > 1,
			"launch": t.IsPublished,
		},
		// The steps the product calls the minimum to launch. The console greys
		// out Publish until these are done rather than letting an owner put an
		// empty shop in front of a customer.
		"required": []string{"business_info", "menu", "payment", "hours", "storefront"},
	})
}

func (h *Handler) CompleteSetupStep(w http.ResponseWriter, r *http.Request) {
	t, err := h.q.SetTenantSetupStatus(r.Context(), sqlc.SetTenantSetupStatusParams{
		ID: pgutil.UUID(tenantID(r)), SetupStatus: "IN_PROGRESS",
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update setup")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"setup_status": t.SetupStatus})
}

func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := h.q.TenantDashboardStats(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load dashboard")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"orders_today":     stats.OrdersToday,
		"revenue_today":    stats.RevenueToday,
		"pending_orders":   stats.PendingOrders,
		"preparing_orders": stats.PreparingOrders,
		"ready_orders":     stats.ReadyOrders,
		"completed_today":  stats.CompletedToday,
	})
}

func categoryJSON(c sqlc.Category) map[string]any {
	var image *string
	if c.ImageUrl.Valid {
		s := c.ImageUrl.String
		image = &s
	}
	return map[string]any{
		"id":          pgutil.UUIDString(c.ID),
		"tenant_id":   pgutil.UUIDString(c.TenantID),
		"name":        c.Name,
		"description": c.Description,
		"image_url":   image,
		"sort_order":  c.SortOrder,
		"is_active":   c.IsActive,
		"created_at":  c.CreatedAt.Time.Format(time.RFC3339),
		"updated_at":  c.UpdatedAt.Time.Format(time.RFC3339),
	}
}

func productJSON(p sqlc.Product) map[string]any {
	var image *string
	if p.ImageUrl.Valid {
		s := p.ImageUrl.String
		image = &s
	}
	groups := storefront.ParseOptionGroups(p.Addons)
	return map[string]any{
		"id":                         pgutil.UUIDString(p.ID),
		"tenant_id":                  pgutil.UUIDString(p.TenantID),
		"category_id":                pgutil.UUIDString(p.CategoryID),
		"name":                       p.Name,
		"description":                p.Description,
		"price":                      pgutil.NumericToFloat(p.Price),
		"image_url":                  image,
		"sort_order":                 p.SortOrder,
		"is_available":               p.IsAvailable,
		"is_vegetarian":              p.IsVegetarian,
		"is_featured":                p.IsFeatured,
		"is_popular":                 p.IsPopular,
		"allow_special_instructions": p.AllowSpecialInstructions,
		"option_groups":              groups,
		"addons":                     storefront.FlattenOptionGroups(groups),
		"created_at":                 p.CreatedAt.Time.Format(time.RFC3339),
		"updated_at":                 p.UpdatedAt.Time.Format(time.RFC3339),
	}
}
