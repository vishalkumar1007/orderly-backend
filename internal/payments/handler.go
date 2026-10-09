package payments

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves the storefront payment flow and the tenant's payment
// configuration.
type Handler struct {
	pool   *pgxpool.Pool
	q      *sqlc.Queries
	orders *orders.Handler
	store  *storefront.Loader
	log    *slog.Logger
}

// NewHandler wires the payment handler.
func NewHandler(pool *pgxpool.Pool, orderHandler *orders.Handler, log *slog.Logger) *Handler {
	return &Handler{
		pool:   pool,
		q:      sqlc.New(pool),
		orders: orderHandler,
		store:  storefront.NewLoader(pool),
		log:    log,
	}
}

// sandboxProvider is the recorded provider while no real PSP is configured.
const sandboxProvider = "sandbox"

// Errors surfaced to the storefront.
var (
	ErrPaymentNotFound = errors.New("payment not found")
	ErrAlreadyPaid     = errors.New("this order is already paid")
	ErrOrderNotPayable = errors.New("this order cannot be paid")
	ErrBadIntent       = errors.New("payment session is no longer valid")
)

type httpError struct {
	Code    string
	Message string
	Status  int
}

func (e *httpError) Error() string { return e.Message }

func fail(status int, code, format string, args ...any) *httpError {
	return &httpError{Code: code, Message: fmt.Sprintf(format, args...), Status: status}
}

// ---------------------------------------------------------------------------
// Public payment flow
// ---------------------------------------------------------------------------

// StartRequest asks to pay for an order.
type StartRequest struct {
	Method string `json:"method"`
	// Phone proves ownership for a guest order. Signed-in customers do not send
	// it; the customer token is the proof.
	Phone string `json:"phone"`
}

// Start begins (or restarts) a payment for an order.
//
// The order's single payment row is updated in place. Because the schema has a
// unique constraint on (order_id), a customer who taps twice, refreshes, or
// retries after a failure can never end up with a second payment record.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Ensure(r.Context(), tenant.ID, storefront.Seed{Name: tenant.Name})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load payment settings")
		return
	}
	var req StartRequest
	// The body is read before the order lookup so a guest can prove ownership
	// with the same JSON payload they already send.
	_ = decodeBody(r, &req)
	_, order, err := h.lookupOrder(w, r, tenant.ID, req.Phone)
	if err != nil {
		return
	}

	method := upperTrim(req.Method)
	if method == "" {
		method = sf.Payments.DefaultMethod
	}
	if !sf.Payments.Allows(method) {
		writeError(w, fail(http.StatusBadRequest, "payment_method_unavailable",
			"That payment method is not available right now"))
		return
	}
	if method == storefront.MethodOnline && !sf.Payments.PayOnline() {
		writeError(w, fail(http.StatusBadRequest, "payment_method_unavailable", "Online payment is off"))
		return
	}
	if !orders.IsActive(order.Status) {
		writeError(w, fail(http.StatusConflict, "order_not_payable", "This order can no longer be paid"))
		return
	}

	current, err := h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{
		OrderID:  order.ID,
		TenantID: order.TenantID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
		return
	}
	if current.ID.Valid && current.Status == orders.PaymentPaid {
		writeError(w, fail(http.StatusConflict, "already_paid", "This order is already paid"))
		return
	}

	intent := newIntentToken()
	if current.ID.Valid {
		// Retry: same payment row, new attempt. The unique index on order_id
		// guarantees there is never a second one.
		if _, err := h.q.SetPaymentPending(r.Context(), sqlc.SetPaymentPendingParams{
			ID:                current.ID,
			TenantID:          order.TenantID,
			Method:            method,
			Provider:          sandboxProvider,
			ProviderReference: intent,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
			return
		}
	} else {
		if _, err := h.q.CreatePayment(r.Context(), sqlc.CreatePaymentParams{
			TenantID:          order.TenantID,
			OrderID:           order.ID,
			Amount:            order.Total,
			Method:            method,
			Status:            orders.PaymentPending,
			Provider:          sandboxProvider,
			ProviderReference: intent,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
			return
		}
	}

	updated, _ := h.reload(r, order)
	response.JSON(w, http.StatusOK, h.sessionPayload(updated, order, sf, method, intent))
}

// ConfirmRequest reports the outcome of a payment attempt.
type ConfirmRequest struct {
	// Outcome is SUCCESS or FAILURE.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Token   string `json:"token"`
	Phone   string `json:"phone"`
}

// Confirm applies a payment outcome.
//
// The caller must present the intent token issued by Start. On success the
// payment is marked PAID and, for a shop that only cooks once money lands, the
// order is accepted automatically. On failure the payment is marked FAILED and
// the order is left exactly as it was, so the customer can retry — with the same
// method or another enabled one.
func (h *Handler) Confirm(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Load(r.Context(), tenant.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load payment settings")
		return
	}
	var req ConfirmRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, fail(http.StatusBadRequest, "invalid_request", "invalid json body"))
		return
	}
	_, order, err := h.lookupOrder(w, r, tenant.ID, req.Phone)
	if err != nil {
		return
	}
	pay, err := h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{
		OrderID:  order.ID,
		TenantID: order.TenantID,
	})
	if err != nil {
		writeError(w, fail(http.StatusNotFound, "payment_not_found", "Start the payment first"))
		return
	}
	if pay.Status == orders.PaymentPaid {
		// A duplicate confirmation is a no-op, not an error: gateways do retry
		// and a customer can double-tap.
		h.respondPayment(w, r, order, sf, "already_paid")
		return
	}
	if trim(req.Token) == "" || trim(req.Token) != pay.ProviderReference {
		writeError(w, fail(http.StatusConflict, "invalid_payment_session",
			"This payment session has expired. Start the payment again."))
		return
	}

	if upperTrim(req.Outcome) == "FAILURE" {
		reason := trim(req.Reason)
		if reason == "" {
			reason = "Payment was not completed"
		}
		if len(reason) > 200 {
			reason = reason[:200]
		}
		if _, err := h.q.MarkPaymentFailed(r.Context(), sqlc.MarkPaymentFailedParams{
			ID:            pay.ID,
			TenantID:      order.TenantID,
			FailureReason: reason,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not record the payment failure")
			return
		}
		tenantID := uuid.UUID(order.TenantID.Bytes)
		notify.Dispatch(r.Context(), notify.Deps{Q: h.q, Log: h.log}, &tenantID, notify.TypePaymentFailed,
			fmt.Sprintf("Payment failed for order #%d", order.OrderNumber),
			reason,
			map[string]any{
				"order_id":       pgutil.UUIDString(order.ID),
				"order_number":   order.OrderNumber,
				"failure_reason": reason,
			},
			notify.Contact{Email: order.CustomerEmail, Phone: order.CustomerPhone})
		updated, _ := h.reload(r, order)
		payload := h.sessionPayload(updated, order, sf, pay.Method, pay.ProviderReference)
		payload["outcome"] = "FAILURE"
		payload["message"] = reason
		payload["can_retry"] = true
		payload["payment_methods"] = sf.Payments.Methods()
		response.JSON(w, http.StatusOK, payload)
		return
	}

	paid, err := h.q.MarkPaymentPaid(r.Context(), sqlc.MarkPaymentPaidParams{
		ID:       pay.ID,
		TenantID: order.TenantID,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not confirm payment")
		return
	}
	// A prepaid order should start moving without waiting for a staff tap when
	// the tenant runs automatic acceptance.
	// A prepaid order should start moving without waiting for a staff tap when
	// the tenant runs automatic acceptance. A failure here must not lose the
	// payment: it is already recorded as PAID.
	view, moveErr := h.orders.AutoAcceptIfNeeded(r.Context(), tenant.ID, order)
	if moveErr != nil {
		h.log.Error("auto accept after payment failed", "error", moveErr, "tenant", tenant.Slug)
	}
	updated, _ := h.reload(r, order)
	payload := h.sessionPayload(updated, order, sf, paid.Method, pay.ProviderReference)
	payload["outcome"] = "SUCCESS"
	payload["message"] = "Payment received"
	payload["can_retry"] = false
	if moveErr == nil && view.Status != "" {
		payload["order_status"] = view.Status
		payload["order_status_label"] = orders.StatusLabel(view.Status)
	}
	response.JSON(w, http.StatusOK, payload)
}

// Status returns the current payment state for an order, so the payment screen
// can recover after a refresh without re-driving the flow.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Load(r.Context(), tenant.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load payment settings")
		return
	}
	_, order, err := h.lookupOrder(w, r, tenant.ID, "")
	if err != nil {
		return
	}
	pay, _ := h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{
		OrderID:  order.ID,
		TenantID: order.TenantID,
	})
	method := storefront.MethodOnline
	reference := ""
	if pay.ID.Valid {
		method = pay.Method
		// The intent token is only echoed while a payment is genuinely pending,
		// so a stale tab cannot replay a captured payment.
		if pay.Status == orders.PaymentPending {
			reference = pay.ProviderReference
		}
	}
	response.JSON(w, http.StatusOK, h.sessionPayload(pay, order, sf, method, reference))
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

// lookupOrder resolves the order in the URL and proves the caller may act on
// it: a signed-in customer for their own order, or a guest who supplies the
// phone number used at checkout. The guest phone may arrive in the query
// string or in the request body, so a POST does not have to duplicate it in
// both places.
func (h *Handler) lookupOrder(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, bodyPhone string) (int32, sqlc.Order, error) {
	number, err := orders.ParseOrderNumber(chi.URLParam(r, "orderNumber"))
	if err != nil {
		writeError(w, fail(http.StatusBadRequest, "invalid_request", "invalid order number"))
		return 0, sqlc.Order{}, err
	}
	viewer := h.orders.Viewer()
	if principal, signedIn := auth.CustomerFromContext(r.Context()); signedIn && principal.TenantID == tenantID {
		if _, err := viewer.ByNumberForCustomer(r.Context(), tenantID, principal.ID, number); err != nil {
			writeError(w, fail(http.StatusNotFound, "order_not_found", "We could not find that order"))
			return 0, sqlc.Order{}, err
		}
		row, err := h.rawOrder(r, tenantID, number)
		return number, row, err
	}
	phone := trim(bodyPhone)
	if phone == "" {
		phone = trim(r.URL.Query().Get("phone"))
	}
	if phone == "" {
		writeError(w, fail(http.StatusBadRequest, "phone_required",
			"Enter the phone number you used to place this order"))
		return 0, sqlc.Order{}, errors.New("phone_required")
	}
	if _, err := viewer.ByNumberAndPhone(r.Context(), tenantID, number, normalisePhone(phone)); err != nil {
		writeError(w, fail(http.StatusNotFound, "order_not_found",
			"We could not find that order for that phone number"))
		return 0, sqlc.Order{}, err
	}
	row, err := h.rawOrder(r, tenantID, number)
	return number, row, err
}

func (h *Handler) rawOrder(r *http.Request, tenantID uuid.UUID, number int32) (sqlc.Order, error) {
	return h.q.GetOrderByTenantAndNumber(r.Context(), sqlc.GetOrderByTenantAndNumberParams{
		TenantID:    pgutil.UUID(tenantID),
		OrderNumber: number,
	})
}

func (h *Handler) reload(r *http.Request, order sqlc.Order) (sqlc.Payment, error) {
	return h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{
		OrderID:  order.ID,
		TenantID: order.TenantID,
	})
}

func (h *Handler) respondPayment(w http.ResponseWriter, r *http.Request, order sqlc.Order, sf *storefront.Storefront, message string) {
	tenantID := uuid.UUID(order.TenantID.Bytes)
	view, err := h.orders.Viewer().ByNumber(r.Context(), tenantID, order.OrderNumber)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this order")
		return
	}
	payload := view.Detail()
	payload["message"] = message
	response.JSON(w, http.StatusOK, payload)
}

// sessionPayload is the payment screen's view of the world. It contains no
// gateway credentials and no provider reference beyond the current intent
// token, which is only present while a payment is genuinely in flight.
func (h *Handler) sessionPayload(pay sqlc.Payment, order sqlc.Order, sf *storefront.Storefront, method, reference string) map[string]any {
	paymentStatus := orders.PaymentPending
	if pay.ID.Valid {
		paymentStatus = pay.Status
	}
	amount := pgutil.NumericToFloat(order.Total)
	if pay.ID.Valid {
		amount = pgutil.NumericToFloat(pay.Amount)
	}
	out := map[string]any{
		"order_number":       order.OrderNumber,
		"order_status":       order.Status,
		"order_status_label": orders.StatusLabel(order.Status),
		"method":             method,
		"payment_status":     paymentStatus,
		"amount":             amount,
		"currency":           currencyOr(sf.Currency),
		"store_name":         sf.Name,
		"payment_methods":    sf.Payments.Methods(),
		"can_pay_online":     sf.Payments.PayOnline(),
		"payable":            orders.IsActive(order.Status) && paymentStatus != orders.PaymentPaid,
		"created_at":         order.CreatedAt.Time.Format(time.RFC3339),
	}
	if pay.ID.Valid && pay.FailureReason != "" && paymentStatus == orders.PaymentFailed {
		out["failure_reason"] = pay.FailureReason
		out["attempts"] = pay.AttemptCount
	}
	if reference != "" && paymentStatus == orders.PaymentPending {
		out["payment_token"] = reference
	}
	return out
}

// newIntentToken mints the single-use token that stands in for a provider
// signature on the confirm call.
func newIntentToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func writeError(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		response.Error(w, he.Status, he.Code, he.Message)
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong. Please try again.")
}

func decodeBody(r *http.Request, target any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func currencyOr(v string) string {
	if v == "" {
		return "INR"
	}
	return v
}
