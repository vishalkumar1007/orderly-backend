package customers

import (
	"context"
	"encoding/json"
	"net/http"
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

func anyTimeOrNil(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.Format(time.RFC3339)
	case pgtype.Timestamptz:
		return tsOrNil(t)
	case *time.Time:
		if t == nil {
			return nil
		}
		return t.Format(time.RFC3339)
	default:
		return nil
	}
}

func anyString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// ShopListCustomers returns known customers plus guest order phones for the
// shop CRM screen.
func (h *Handler) ShopListCustomers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := shopTenantID(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	rows, err := h.q.ListCustomersByTenant(ctx, sqlc.ListCustomersByTenantParams{
		TenantID:    pgutil.UUID(tenantID),
		LimitCount:  200,
		OffsetCount: 0,
	})
	if err != nil {
		h.log.Error("list customers", "err", err, "tenant_id", tenantID)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load customers")
		return
	}
	guests, err := h.q.ListGuestCustomersFromOrders(ctx, sqlc.ListGuestCustomersFromOrdersParams{
		TenantID:   pgutil.UUID(tenantID),
		LimitCount: 200,
	})
	if err != nil {
		h.log.Error("list guest customers", "err", err, "tenant_id", tenantID)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load guest customers")
		return
	}

	out := make([]map[string]any, 0, len(rows)+len(guests))
	for _, row := range rows {
		phone := ""
		if row.Phone.Valid {
			phone = row.Phone.String
		}
		item := map[string]any{
			"id":            pgutil.UUIDString(row.ID),
			"kind":          "registered",
			"name":          row.Name,
			"phone":         phone,
			"is_blocked":    row.IsBlocked,
			"last_login_at": tsOrNil(row.LastLoginAt),
			"created_at":    tsOrNil(row.CreatedAt),
			"order_count":   row.OrderCount,
			"total_spend":   row.TotalSpend,
			"last_order_at": anyTimeOrNil(row.LastOrderAt),
		}
		if q == "" || matchCustomer(q, row.Name, phone) {
			out = append(out, item)
		}
	}
	for _, g := range guests {
		name := anyString(g.Name)
		item := map[string]any{
			"id":            nil,
			"kind":          "guest",
			"name":          name,
			"phone":         g.Phone,
			"is_blocked":    false,
			"last_login_at": nil,
			"created_at":    anyTimeOrNil(g.FirstSeenAt),
			"order_count":   g.OrderCount,
			"total_spend":   g.TotalSpend,
			"last_order_at": anyTimeOrNil(g.LastOrderAt),
		}
		if q == "" || matchCustomer(q, name, g.Phone) {
			out = append(out, item)
		}
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"customers": out,
		"counts": map[string]any{
			"registered": len(rows),
			"guest":      len(guests),
			"total":      len(out),
		},
	})
}

func matchCustomer(q, name, phone string) bool {
	return strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(phone), q)
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

	ordersOut := h.shopOrdersFor(ctx, tenantID, &id, phone)

	response.JSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":            pgutil.UUIDString(cust.ID),
			"kind":          "registered",
			"name":          cust.Name,
			"phone":         phone,
			"is_blocked":    cust.IsBlocked,
			"last_login_at": tsOrNil(cust.LastLoginAt),
			"created_at":    tsOrNil(cust.CreatedAt),
			"order_count":   len(ordersOut),
			"total_spend":   sumOrderTotals(ordersOut),
		},
		"orders": ordersOut,
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
			"order_count":   len(ordersOut),
			"total_spend":   sumOrderTotals(ordersOut),
		},
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

func sumOrderTotals(orders []map[string]any) float64 {
	var sum float64
	for _, o := range orders {
		if v, ok := o["total"].(float64); ok {
			sum += v
		}
	}
	return sum
}
