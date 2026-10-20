package customers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

func shopTenantID(r *http.Request) (uuid.UUID, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, false
	}
	return *user.TenantID, true
}

func tsOrNil(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}

// ShopListCustomers returns one page of registered customers and guest order
// phones for the shop CRM screen. Search and paging run in SQL.
func (h *Handler) ShopListCustomers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	qs := r.URL.Query()
	limit := clampInt(qs.Get("limit"), 50, 1, 200)
	offset := clampInt(qs.Get("offset"), 0, 0, 1_000_000)
	rows, err := h.q.ListShopCustomers(r.Context(), sqlc.ListShopCustomersParams{
		TenantID:    pgutil.UUID(tenantID),
		Q:           strings.TrimSpace(qs.Get("q")),
		LimitCount:  int32(limit),
		OffsetCount: int32(offset),
	})
	if err != nil {
		h.log.Error("list customers", "err", err, "tenant_id", tenantID)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load customers")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	var total, registered int64
	for _, row := range rows {
		total, registered = row.TotalCount, row.RegisteredCount
		var id any
		if row.ID.Valid {
			id = pgutil.UUIDString(row.ID)
		}
		out = append(out, map[string]any{
			"id":            id,
			"kind":          row.Kind,
			"name":          row.Name,
			"phone":         row.Phone,
			"email":         row.Email,
			"is_blocked":    row.IsBlocked,
			"last_login_at": tsOrNil(row.LastLoginAt),
			"created_at":    tsOrNil(row.CreatedAt),
			"order_count":   row.OrderCount,
			"total_spend":   row.TotalSpend,
			"last_order_at": tsOrNil(row.LastOrderAt),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"customers": out,
		"counts": map[string]any{
			"registered": registered,
			"guest":      total - registered,
			"total":      total,
		},
		"has_more": int64(offset+len(rows)) < total,
	})
}

func clampInt(raw string, def, lo, hi int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

// customerStats is the money summary on a profile, over every order, not
// just the recent ones listed.
func (h *Handler) customerStats(ctx context.Context, tenantID uuid.UUID, customerID *uuid.UUID, phone string) map[string]any {
	params := sqlc.CustomerOrderStatsParams{TenantID: pgutil.UUID(tenantID), Phone: phone}
	if customerID != nil {
		params.CustomerID = pgutil.UUID(*customerID)
	}
	st, err := h.q.CustomerOrderStats(ctx, params)
	if err != nil {
		return map[string]any{"order_count": 0, "total_spend": 0.0, "average_order": 0.0}
	}
	avg := 0.0
	if st.OrderCount > 0 {
		avg = st.TotalSpend / float64(st.OrderCount)
	}
	return map[string]any{
		"order_count":    st.OrderCount,
		"total_spend":    st.TotalSpend,
		"average_order":  avg,
		"first_order_at": tsOrNil(st.FirstOrderAt),
		"last_order_at":  tsOrNil(st.LastOrderAt),
	}
}

// ShopGetCustomer returns one registered customer plus recent orders.
func (h *Handler) ShopGetCustomer(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer id")
		return
	}
	ctx := r.Context()
	cust, err := h.q.GetCustomerByID(ctx, sqlc.GetCustomerByIDParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}

	phone := ""
	if cust.Phone.Valid {
		phone = cust.Phone.String
	}

	stats := h.customerStats(ctx, tenantID, &id, phone)
	response.JSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":            pgutil.UUIDString(cust.ID),
			"kind":          "registered",
			"name":          cust.Name,
			"phone":         phone,
			"email":         cust.Email.String,
			"is_blocked":    cust.IsBlocked,
			"last_login_at": tsOrNil(cust.LastLoginAt),
			"created_at":    tsOrNil(cust.CreatedAt),
			"order_count":   stats["order_count"],
			"total_spend":   stats["total_spend"],
		},
		"stats":  stats,
		"orders": h.shopOrdersFor(ctx, tenantID, &id, phone),
	})
}

// ShopGetGuestCustomer returns order history for a guest phone number.
func (h *Handler) ShopGetGuestCustomer(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	phone := strings.TrimSpace(r.URL.Query().Get("phone"))
	if phone == "" {
		response.Error(w, http.StatusBadRequest, "phone_required", "phone is required")
		return
	}
	ordersOut := h.shopOrdersFor(r.Context(), tenantID, nil, phone)
	stats := h.customerStats(r.Context(), tenantID, nil, phone)
	name := ""
	if len(ordersOut) > 0 {
		if n, ok := ordersOut[0]["customer_name"].(string); ok {
			name = n
		}
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":            nil,
			"kind":          "guest",
			"name":          name,
			"phone":         phone,
			"is_blocked":    false,
			"last_login_at": nil,
			"order_count":   stats["order_count"],
			"total_spend":   stats["total_spend"],
		},
		"stats":  stats,
		"orders": ordersOut,
	})
}

// ShopSetCustomerBlocked blocks or unblocks a registered customer.
func (h *Handler) ShopSetCustomerBlocked(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer id")
		return
	}
	var req struct {
		Blocked bool `json:"blocked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	cust, err := h.q.SetCustomerBlocked(r.Context(), sqlc.SetCustomerBlockedParams{
		ID:        pgutil.UUID(id),
		TenantID:  pgutil.UUID(tenantID),
		IsBlocked: req.Blocked,
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	action := "customer.unblocked"
	if req.Blocked {
		action = "customer.blocked"
	}
	var actorID pgtype.UUID
	if u, ok := identity.UserFromContext(r.Context()); ok {
		actorID = pgutil.UUID(u.ID)
	}
	_, _ = h.q.InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		TenantID: pgutil.UUID(tenantID), UserID: actorID, Action: action,
		EntityType: "customer", EntityID: cust.ID, Metadata: []byte("{}"), Result: "SUCCESS",
	})
	phone := ""
	if cust.Phone.Valid {
		phone = cust.Phone.String
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"id":         pgutil.UUIDString(cust.ID),
		"name":       cust.Name,
		"phone":      phone,
		"is_blocked": cust.IsBlocked,
	})
}

func (h *Handler) shopOrdersFor(ctx context.Context, tenantID uuid.UUID, customerID *uuid.UUID, phone string) []map[string]any {
	var rows []sqlc.Order
	var err error
	if customerID != nil {
		rows, err = h.q.ListCustomerOrders(ctx, sqlc.ListCustomerOrdersParams{
			TenantID:   pgutil.UUID(tenantID),
			CustomerID: pgutil.UUID(*customerID),
			LimitCount: 50,
		})
	}
	if (err != nil || len(rows) == 0) && phone != "" {
		byPhone, phoneErr := h.q.ListOrdersByTenantAndPhone(ctx, sqlc.ListOrdersByTenantAndPhoneParams{
			TenantID:      pgutil.UUID(tenantID),
			CustomerPhone: phone,
			LimitCount:    50,
		})
		if phoneErr == nil {
			rows = byPhone
			err = nil
		}
	}
	if err != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, map[string]any{
			"id":            pgutil.UUIDString(o.ID),
			"order_number":  o.OrderNumber,
			"status":        o.Status,
			"total":         pgutil.NumericToFloat(o.Total),
			"customer_name": o.CustomerName,
			"created_at":    tsOrNil(o.CreatedAt),
		})
	}
	return out
}

// ShopCustomerActivity is the profile timeline, newest first, paged by time.
func (h *Handler) ShopCustomerActivity(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid customer id")
		return
	}
	cust, err := h.q.GetCustomerByID(r.Context(), sqlc.GetCustomerByIDParams{
		ID: pgutil.UUID(id), TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "customer not found")
		return
	}
	before := time.Now().Add(time.Minute)
	if raw := r.URL.Query().Get("before"); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			before = t
		}
	}
	limit := clampInt(r.URL.Query().Get("limit"), 50, 1, 100)
	rows, err := h.q.CustomerActivity(r.Context(), sqlc.CustomerActivityParams{
		TenantID:   pgutil.UUID(tenantID),
		CustomerID: cust.ID,
		Phone:      cust.Phone.String,
		Before:     pgtype.Timestamptz{Time: before, Valid: true},
		LimitCount: int32(limit),
	})
	if err != nil {
		h.log.Error("customer activity", "err", err, "tenant_id", tenantID)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load activity")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"kind":   row.Kind,
			"status": row.Status,
			"ref":    row.Ref,
			"amount": row.Amount,
			"detail": row.Detail,
			"at":     row.At.Time.Format(time.RFC3339Nano),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"events":   out,
		"has_more": len(rows) == limit,
	})
}
