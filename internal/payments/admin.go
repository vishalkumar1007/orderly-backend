package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// AuditSink records mode changes on the platform audit log.
type AuditSink interface {
	AuditConfiguration(ctx context.Context, tenantID *uuid.UUID, action, service, provider string)
}

// AdminHandler serves the Super Admin mode switch and the business-side
// read-only view of it.
type AdminHandler struct {
	svc   *Service
	audit AuditSink
}

// NewAdminHandler wires the handler.
func NewAdminHandler(svc *Service, audit AuditSink) *AdminHandler {
	return &AdminHandler{svc: svc, audit: audit}
}

type modeBody struct {
	// Mode is SANDBOX or LIVE; for a business, INHERIT clears its override.
	Mode string `json:"mode"`
	// Confirm must be true to go LIVE: it is the "are you sure" the UI collects.
	Confirm bool `json:"confirm"`
}

// GetPlatform returns the platform default and its readiness.
func (h *AdminHandler) GetPlatform(w http.ResponseWriter, r *http.Request) {
	platform, err := h.svc.PlatformMode(r.Context())
	if err != nil {
		internal(w)
		return
	}
	ready, err := h.svc.Readiness(r.Context(), nil)
	if err != nil {
		internal(w)
		return
	}
	mode := ModeSandbox
	if platform != nil {
		mode = platform.Mode
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"v2_enabled":     h.svc.V2(),
		"live_available": h.svc.LiveAvailable(),
		"mode":           mode,
		"setting":        platform,
		"readiness":      ready,
	})
}

// PutPlatform sets the platform default.
func (h *AdminHandler) PutPlatform(w http.ResponseWriter, r *http.Request) {
	mode, ok := h.parse(w, r, false)
	if !ok {
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	if _, ready, err := h.svc.SetPlatformMode(r.Context(), *mode, actor.ID); err != nil {
		h.writeModeError(w, err, ready)
		return
	}
	h.auditChange(r, nil, "Payments default mode set to "+string(*mode), string(*mode))
	h.GetPlatform(w, r)
}

// GetTenant returns one business's override, its effective mode and readiness.
func (h *AdminHandler) GetTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := tenantParam(w, r)
	if !ok {
		return
	}
	h.writeTenant(w, r, id)
}

// PutTenant sets or clears one business's override.
func (h *AdminHandler) PutTenant(w http.ResponseWriter, r *http.Request) {
	id, ok := tenantParam(w, r)
	if !ok {
		return
	}
	mode, ok := h.parse(w, r, true)
	if !ok {
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	if _, ready, err := h.svc.SetTenantMode(r.Context(), id, mode, actor.ID); err != nil {
		h.writeModeError(w, err, ready)
		return
	}
	label := "INHERIT"
	if mode != nil {
		label = string(*mode)
	}
	h.auditChange(r, &id, "Payments mode set to "+label, label)
	h.writeTenant(w, r, id)
}

func (h *AdminHandler) writeTenant(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	override, err := h.svc.TenantMode(r.Context(), id)
	if err != nil {
		internal(w)
		return
	}
	effective, source, err := h.svc.EffectiveMode(r.Context(), id)
	if err != nil {
		internal(w)
		return
	}
	ready, err := h.svc.Readiness(r.Context(), &id)
	if err != nil {
		internal(w)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"tenant_id":      id,
		"v2_enabled":     h.svc.V2(),
		"live_available": h.svc.LiveAvailable(),
		"override":       override,
		"effective_mode": effective,
		"mode_source":    source,
		"readiness":      ready,
	})
}

// GetOwn is the business console's read-only view: which mode its customers
// are paying in, and whether that can actually take payments.
func (h *AdminHandler) GetOwn(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	mode, source, err := h.svc.EffectiveMode(r.Context(), tenant.ID)
	if err != nil {
		internal(w)
		return
	}
	onlineReady := true
	if mode == ModeLive {
		ready, err := h.svc.Readiness(r.Context(), &tenant.ID)
		if err != nil {
			internal(w)
			return
		}
		onlineReady = ready.Ready && h.svc.LiveAvailable()
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"mode":         mode,
		"mode_source":  source,
		"is_test":      mode != ModeLive,
		"online_ready": onlineReady,
		"v2_enabled":   h.svc.V2(),
	})
}

// Health is the Super Admin's view of what needs a human.
func (h *AdminHandler) Health(w http.ResponseWriter, r *http.Request) {
	health, err := h.svc.Health(r.Context())
	if err != nil {
		internal(w)
		return
	}
	response.JSON(w, http.StatusOK, health)
}

// Reconcile asks the gateway about one pending payment right now and applies
// the answer. For the call that starts "the customer says they paid".
func (h *AdminHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid payment id")
		return
	}
	pay, changed, err := h.svc.Reconcile(r.Context(), pgutil.UUID(id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		response.Error(w, http.StatusNotFound, "not_found", "payment not found")
		return
	case err != nil:
		response.Error(w, http.StatusBadGateway, "gateway_unavailable", "The payment provider could not be reached. Try again shortly.")
		return
	}
	tenant := uuid.UUID(pay.TenantID.Bytes)
	h.auditChange(r, &tenant, "Payment reconciled", pay.Status)
	response.JSON(w, http.StatusOK, map[string]any{
		"payment_id": pay.ID, "status": pay.Status, "mode": pay.Mode, "updated": changed,
	})
}

// RefundRequest asks to return a payment.
type RefundRequest struct {
	Reason string `json:"reason"`
}

// Refund returns the money for one online payment. Owner-level, because it
// moves money.
func (h *AdminHandler) Refund(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid payment id")
		return
	}
	var body RefundRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "Refunded by the shop"
	}
	actor := Actor{Type: "staff"}
	if u, ok := identity.UserFromContext(r.Context()); ok {
		actor.ID = u.ID.String()
	}

	res, err := h.svc.RefundPayment(r.Context(), tenant.ID, pgutil.UUID(id), reason, actor)
	switch {
	case errors.Is(err, ErrNotRefundable):
		response.Error(w, http.StatusConflict, "not_refundable", "Only a paid online payment can be refunded here. Cash is handed back at the counter.")
		return
	case errors.Is(err, ErrRefundInProgress):
		response.Error(w, http.StatusConflict, "refund_in_progress", "This payment has already been refunded or is being refunded.")
		return
	case errors.Is(err, ErrRefundRejected):
		response.Error(w, http.StatusBadGateway, "refund_rejected", "The payment provider declined the refund. The payment is unchanged; check your provider dashboard.")
		return
	case errors.Is(err, ErrGatewayUnavailable):
		// Recorded and retried automatically: tell the owner it is on its way.
		response.JSON(w, http.StatusAccepted, h.refundBody(res, false))
		return
	case err != nil:
		internal(w)
		return
	}
	response.JSON(w, http.StatusOK, h.refundBody(res, res.Done))
}

func (h *AdminHandler) refundBody(res RefundResult, done bool) map[string]any {
	return map[string]any{
		"payment_status": res.Payment.Status,
		"done":           done,
		"refund": map[string]any{
			"id":     res.Refund.ID,
			"status": res.Refund.Status,
			"amount": pgutil.NumericToFloat(res.Refund.Amount),
		},
	}
}

// parse reads and validates the body. allowInherit lets a business clear its
// override (returns a nil mode).
func (h *AdminHandler) parse(w http.ResponseWriter, r *http.Request, allowInherit bool) (*Mode, bool) {
	var body modeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return nil, false
	}
	if allowInherit && upperTrim(body.Mode) == "INHERIT" {
		return nil, true
	}
	mode, ok := ParseMode(body.Mode)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_mode", "mode must be SANDBOX or LIVE")
		return nil, false
	}
	if mode == ModeLive && !body.Confirm {
		response.Error(w, http.StatusBadRequest, "confirmation_required",
			"Going live needs explicit confirmation: real customers will be charged real money.")
		return nil, false
	}
	return &mode, true
}

func (h *AdminHandler) writeModeError(w http.ResponseWriter, err error, ready Readiness) {
	if errors.Is(err, ErrNotReady) {
		response.ErrorWithDetails(w, http.StatusConflict, "gateway_not_ready",
			"The payment gateway is not ready for live payments yet.",
			map[string]any{"checks": ready.Checks})
		return
	}
	internal(w)
}

func (h *AdminHandler) auditChange(r *http.Request, tenantID *uuid.UUID, action, mode string) {
	if h.audit != nil {
		h.audit.AuditConfiguration(r.Context(), tenantID, action, "PAYMENTS", mode)
	}
}

func tenantParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid business id")
		return uuid.Nil, false
	}
	return id, true
}

func internal(w http.ResponseWriter) {
	response.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong. Please try again.")
}
