package menu

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Handler struct {
	q *sqlc.Queries
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{q: sqlc.New(pool)}
}

func tenantID(r *http.Request) uuid.UUID {
	u, _ := identity.UserFromContext(r.Context())
	return *u.TenantID
}

type categoryReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SortOrder   *int32 `json:"sort_order"`
	IsActive    *bool  `json:"is_active"`
}

type productReq struct {
	CategoryID  string   `json:"category_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Price       *float64 `json:"price"`
	ImageURL    *string  `json:"image_url"`
	SortOrder   *int32   `json:"sort_order"`
	IsAvailable *bool    `json:"is_available"`
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
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
		Name:        req.Name,
		Description: req.Description,
		SortOrder:   sortOrder,
		IsActive:    isActive,
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
	if req.Name != "" {
		params.Name = pgtype.Text{String: req.Name, Valid: true}
	}
	if req.Description != "" || true {
		params.Description = pgtype.Text{String: req.Description, Valid: true}
	}
	if req.SortOrder != nil {
		params.SortOrder = pgtype.Int4{Int32: *req.SortOrder, Valid: true}
	}
	if req.IsActive != nil {
		params.IsActive = pgtype.Bool{Bool: *req.IsActive, Valid: true}
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
	if err := h.q.DeleteCategory(r.Context(), sqlc.DeleteCategoryParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID(r)),
	}); err != nil {
		response.Error(w, http.StatusConflict, "conflict", "cannot delete category (in use?)")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
		ID:       pgutil.UUID(catID),
		TenantID: pgutil.UUID(tenantID(r)),
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
	p, err := h.q.CreateProduct(r.Context(), sqlc.CreateProductParams{
		TenantID:    pgutil.UUID(tenantID(r)),
		CategoryID:  pgutil.UUID(catID),
		Name:        req.Name,
		Description: req.Description,
		Price:       price,
		ImageUrl:    pgutil.NullText(req.ImageURL),
		SortOrder:   sortOrder,
		IsAvailable: available,
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
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID(r)),
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
		params.Name = pgtype.Text{String: req.Name, Valid: true}
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
	p, err := h.q.UpdateProduct(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}
	response.JSON(w, http.StatusOK, productJSON(p))
}

func (h *Handler) DeleteProduct(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid id")
		return
	}
	if err := h.q.DeleteProduct(r.Context(), sqlc.DeleteProductParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID(r)),
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

func (h *Handler) SetupStatus(w http.ResponseWriter, r *http.Request) {
	t, err := h.q.GetTenantByID(r.Context(), pgutil.UUID(tenantID(r)))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "tenant not found")
		return
	}
	cats, _ := h.q.ListCategoriesByTenant(r.Context(), t.ID)
	prods, _ := h.q.ListProductsByTenant(r.Context(), t.ID)
	hasMenu := len(cats) > 0 && len(prods) > 0
	response.JSON(w, http.StatusOK, map[string]any{
		"setup_status": t.SetupStatus,
		"is_published": t.IsPublished,
		"steps": map[string]bool{
			"business_info": true,
			"menu":          hasMenu,
			"payment":       true, // POC: payment methods are fixed
			"qr":            false,
			"launch":        t.IsPublished,
		},
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
	return map[string]any{
		"id":          pgutil.UUIDString(c.ID),
		"tenant_id":   pgutil.UUIDString(c.TenantID),
		"name":        c.Name,
		"description": c.Description,
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
	return map[string]any{
		"id":           pgutil.UUIDString(p.ID),
		"tenant_id":    pgutil.UUIDString(p.TenantID),
		"category_id":  pgutil.UUIDString(p.CategoryID),
		"name":         p.Name,
		"description":  p.Description,
		"price":        pgutil.NumericToFloat(p.Price),
		"image_url":    image,
		"sort_order":   p.SortOrder,
		"is_available": p.IsAvailable,
		"created_at":   p.CreatedAt.Time.Format(time.RFC3339),
		"updated_at":   p.UpdatedAt.Time.Format(time.RFC3339),
	}
}

// silence unused strconv in case
var _ = strconv.Itoa
