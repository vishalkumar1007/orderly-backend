package orders

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// actor identifies who moved an order, for the status log.
type actor struct {
	ID     string
	Name   string
	Role   string
	Source string
}

func actorOf(r *http.Request) actor {
	if user, ok := identity.UserFromContext(r.Context()); ok {
		name := user.Name
		if name == "" {
			name = user.Email
		}
		return actor{ID: user.ID.String(), Name: name, Role: user.Role, Source: "staff"}
	}
	return actor{Source: "system"}
}

// mustTenant returns the caller's tenant. Handlers receive this rather than
// reading an id from the request, so a tenant admin cannot address another
// tenant's orders.
func mustTenant(r *http.Request) uuid.UUID {
	user, _ := identity.UserFromContext(r.Context())
	if user.TenantID == nil {
		return uuid.Nil
	}
	return *user.TenantID
}

// ListOrders returns the tenant's orders, newest first, with the pipeline
// counts the board header shows.
func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	rows, err := h.q.ListOrdersByTenant(r.Context(), sqlc.ListOrdersByTenantParams{
		TenantID: pgutil.UUID(tenantID),
		Limit:    100,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load orders")
		return
	}
	sf, _ := h.store.Load(r.Context(), tenantID)
	if sf == nil {
		sf = &storefront.Storefront{}
	}
	viewer := h.Viewer()
	out := make([]map[string]any, 0, len(rows))
	counts := map[string]int{}
	for _, row := range rows {
		view, err := viewer.hydrate(r.Context(), row, *sf)
		if err != nil {
			continue
		}
		counts[view.Status]++
		out = append(out, view.StaffBoard())
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"orders": out,
		"counts": counts,
	})
}

// GetOrder returns one order with the transitions currently available.
func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order id")
		return
	}
	row, err := h.q.GetOrderByID(r.Context(), sqlc.GetOrderByIDParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		writeLookupError(w, ErrOrderNotFound)
		return
	}
	sf, _ := h.store.Load(r.Context(), tenantID)
	if sf == nil {
		sf = &storefront.Storefront{}
	}
	view, err := h.Viewer().hydrate(r.Context(), row, *sf)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this order")
		return
	}
	response.JSON(w, http.StatusOK, view.StaffBoard())
}

// actionTarget maps a staff action to the state it moves the order to.
var actionTarget = map[string]string{
	"accept":   StatusAccepted,
	"prepare":  StatusPreparing,
	"ready":    StatusReady,
	"complete": StatusCompleted,
	"cancel":   StatusCancelled,
}

// Transition applies a staff order action. The target state comes from a fixed
// table here, never from the request, and the workflow gates in the orders
// package decide whether the move is allowed.
func (h *Handler) Transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target, ok := actionTarget[action]
		if !ok {
			response.Error(w, http.StatusBadRequest, "invalid_request", "unknown action")
			return
		}
		tenantID := mustTenant(r)
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order id")
			return
		}
		row, err := h.q.GetOrderByID(r.Context(), sqlc.GetOrderByIDParams{
			ID:       pgutil.UUID(id),
			TenantID: pgutil.UUID(tenantID),
		})
		if err != nil {
			writeLookupError(w, ErrOrderNotFound)
			return
		}
		who := actorOf(r)
		var view OrderView
		if target == StatusCancelled {
			view, err = h.Cancel(r.Context(), tenantID, row, "", who.Source)
		} else {
			view, err = h.Move(r.Context(), tenantID, row, target, "", who.Source)
		}
		if err != nil {
			writeValidationError(w, asValidation(h, err))
			return
		}
		response.JSON(w, http.StatusOK, view.StaffBoard())
	}
}

// ConfirmPayment marks a cash payment as received at the counter. The tenant
// admin is the only role that can do this; the customer never can.
func (h *Handler) ConfirmPayment(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid payment id")
		return
	}
	pay, err := h.q.MarkPaymentPaid(r.Context(), sqlc.MarkPaymentPaidParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Error(w, http.StatusNotFound, "not_found", "payment not found")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not confirm payment")
		return
	}
	// A shop that only starts cooking once payment lands should pick the order
	// straight up.
	if order, lookupErr := h.orderByPayment(r.Context(), tenantID, pay.OrderID); lookupErr == nil {
		if _, moveErr := h.AutoAcceptIfNeeded(r.Context(), tenantID, order); moveErr != nil {
			h.log.Warn("auto accept after payment failed", "error", moveErr)
		}
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"id":     pgutil.UUIDString(pay.ID),
		"status": pay.Status,
		"method": pay.Method,
		"amount": pgutil.NumericToFloat(pay.Amount),
		"paid_at": func() any {
			if pay.PaidAt.Valid {
				return pay.PaidAt.Time.Format(time.RFC3339)
			}
			return nil
		}(),
	})
}

func (h *Handler) orderByPayment(ctx context.Context, tenantID uuid.UUID, orderID pgtype.UUID) (sqlc.Order, error) {
	return h.q.GetOrderByID(ctx, sqlc.GetOrderByIDParams{
		ID:       orderID,
		TenantID: pgutil.UUID(tenantID),
	})
}

// StaffPaymentView is the tenant-facing payment projection. It carries no
// gateway credentials and no provider callback body.
func (h *Handler) StaffPaymentView(ctx context.Context, tenantID uuid.UUID, orderID pgtype.UUID) (*PaymentView, error) {
	pay, err := h.q.GetPaymentByOrderID(ctx, sqlc.GetPaymentByOrderIDParams{
		OrderID:  orderID,
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	out := &PaymentView{
		ID:            pgutil.UUIDString(pay.ID),
		Method:        pay.Method,
		Status:        pay.Status,
		Amount:        pgutil.NumericToFloat(pay.Amount),
		FailureReason: pay.FailureReason,
		Attempts:      int(pay.AttemptCount),
	}
	if pay.PaidAt.Valid {
		t := pay.PaidAt.Time
		out.PaidAt = &t
	}
	return out, nil
}

// CancelStaffOrder cancels an order on behalf of the shop, with a reason that
// the customer sees on the tracking screen.
func (h *Handler) CancelStaffOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order id")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = decodeBody(r, &req)
	row, err := h.q.GetOrderByID(r.Context(), sqlc.GetOrderByIDParams{
		ID:       pgutil.UUID(id),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		writeLookupError(w, ErrOrderNotFound)
		return
	}
	who := actorOf(r)
	view, err := h.Cancel(r.Context(), tenantID, row, trim(req.Reason), who.Source)
	if err != nil {
		writeValidationError(w, asValidation(h, err))
		return
	}
	response.JSON(w, http.StatusOK, view.StaffBoard())
}

// StaffCreateOrder places a counter / walk-in order. Staff auth is required;
// customer login and the public storefront open gate are not.
func (h *Handler) StaffCreateOrder(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	seedName := "Shop"
	if user, ok := identity.UserFromContext(r.Context()); ok && user.Name != "" {
		seedName = user.Name
	}
	if info, ok := tenantctx.FromContext(r.Context()); ok && info.Name != "" {
		seedName = info.Name
	}
	sf, err := h.store.Ensure(r.Context(), tenantID, storefront.Seed{Name: seedName})
	if err != nil || sf == nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this store")
		return
	}
	var req CreateRequest
	if err := decodeBody(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	if trim(req.PaymentMethod) == "" {
		req.PaymentMethod = storefront.MethodCash
	}
	result, err := h.CreateCounter(r.Context(), tenantID, sf, req)
	if err != nil {
		writeValidationError(w, asValidation(h, err))
		return
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	payload := result.View.StaffBoard()
	payload["duplicate"] = result.Duplicate
	response.JSON(w, status, payload)
}
