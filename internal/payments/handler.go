package payments

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/payable"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves the storefront payment flow and the tenant's payment
// configuration.
type Handler struct {
	pool  *pgxpool.Pool
	q     *sqlc.Queries
	src   payable.Source
	svc   *Service
	store *storefront.Loader
	log   *slog.Logger
}

// NewHandler wires the payment handler.
func NewHandler(pool *pgxpool.Pool, src payable.Source, svc *Service, log *slog.Logger) *Handler {
	return &Handler{
		pool:  pool,
		q:     sqlc.New(pool),
		src:   src,
		svc:   svc,
		store: storefront.NewLoader(pool),
		log:   log,
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
	if !order.Active {
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
	if current.ID.Valid && current.Status == payable.StatusPaid {
		writeError(w, fail(http.StatusConflict, "already_paid", "This order is already paid"))
		return
	}

	intent := newIntentToken()
	var gw Intent
	gwMode := ModeSandbox
	gwProvider := sandboxProvider
	online := h.svc.V2() && method == storefront.MethodOnline
	if h.svc.V2() && !online {
		// Cash at pickup never touches a gateway, and nothing a customer holds
		// can mark it paid: only staff at the counter can.
		intent = ""
	}
	if online {
		// The gateway comes from the business's effective mode; LIVE never
		// falls back to the sandbox. Sandbox keeps the single-use token the
		// storefront already presents; a live gateway issues none.
		var p PaymentProvider
		p, gwMode, err = h.svc.ProviderFor(r.Context(), tenant.ID)
		if err != nil {
			writeError(w, fail(http.StatusServiceUnavailable, "gateway_unavailable",
				"Online payment is unavailable right now. You can pay at pickup instead."))
			return
		}
		gwProvider = p.Name()
		gw, err = p.Initiate(r.Context(), IntentInput{
			PaymentID:   pgutil.UUIDString(order.ID),
			AmountMinor: minorUnits(pgutil.NumericToFloat(order.Amount)),
			Currency:    currencyOr(sf.Currency),
		})
		if err != nil {
			h.log.Error("payments: gateway initiate failed", "provider", gwProvider, "error", err)
			writeError(w, fail(http.StatusBadGateway, "gateway_unavailable",
				"We could not reach the payment provider. Please try again, or pay at pickup instead."))
			return
		}
		intent = gw.ClientToken
	}
	if current.ID.Valid {
		// Retry: same payment row, new attempt. The unique index on order_id
		// guarantees there is never a second one.
		if _, err := h.q.SetPaymentPending(r.Context(), sqlc.SetPaymentPendingParams{
			ID:                current.ID,
			TenantID:          order.TenantID,
			Method:            method,
			Provider:          gwProvider,
			ProviderReference: intent,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
			return
		}
	} else {
		if _, err := h.q.CreatePayment(r.Context(), sqlc.CreatePaymentParams{
			TenantID:          order.TenantID,
			OrderID:           order.ID,
			Amount:            order.Amount,
			Method:            method,
			Status:            payable.StatusPending,
			Provider:          gwProvider,
			ProviderReference: intent,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
			return
		}
	}

	updated, _ := h.reload(r, order)
	if h.svc.V2() {
		withIntent, err := h.q.SetPaymentIntentDetails(r.Context(), sqlc.SetPaymentIntentDetailsParams{
			ID: updated.ID, TenantID: order.TenantID,
			Provider: gwProvider, ProviderOrderID: gw.ProviderOrderID, Mode: string(gwMode),
			ExpiresAt: expiryOf(gw.ExpiresAt),
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not start payment")
			return
		}
		updated = withIntent
		if gw.ProviderOrderID != "" {
			// Remember every gateway order this payment ever had, so a late
			// callback for an earlier attempt still finds it.
			if err := h.q.InsertPaymentAttempt(r.Context(), sqlc.InsertPaymentAttemptParams{
				TenantID: order.TenantID, PaymentID: updated.ID, Provider: gwProvider, ProviderOrderID: gw.ProviderOrderID,
			}); err != nil {
				h.log.Error("payments: could not record attempt", "error", err)
			}
		}
	}
	payload := h.sessionPayload(updated, order, sf, method, intent)
	if gw.Checkout != nil {
		payload["checkout"] = h.checkoutPayload(gw.Checkout, order, sf.Name)
	}
	response.JSON(w, http.StatusOK, payload)
}

// checkoutPayload is what the browser needs to open the gateway's widget. Every
// value is public: the order id, the key id and the store's own display name.
func (h *Handler) checkoutPayload(base map[string]any, order payable.Ref, storeName string) map[string]any {
	out := make(map[string]any, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	out["name"] = storeName
	out["description"] = fmt.Sprintf("Order #%d", order.Number)
	out["prefill"] = map[string]any{"contact": order.CustomerPhone, "email": order.CustomerEmail}
	return out
}

func expiryOf(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
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
// the order is left exactly as it was, so the customer can retry: with the same
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
	if pay.Status == payable.StatusPaid {
		// A duplicate confirmation is a no-op, not an error: gateways do retry
		// and a customer can double-tap.
		h.respondPayment(w, r, order, sf, "already_paid")
		return
	}
	if h.svc.V2() && (pay.Mode == string(ModeLive) || pay.Method == storefront.MethodCash) {
		// A live payment is settled by the gateway alone, and cash by staff at
		// the counter: nothing a customer's browser sends can do either.
		writeError(w, fail(http.StatusConflict, "not_confirmable_here",
			"This payment is confirmed by the payment provider, not from this screen."))
		return
	}
	if trim(req.Token) == "" || trim(req.Token) != pay.ProviderReference {
		writeError(w, fail(http.StatusConflict, "invalid_payment_session",
			"This payment session has expired. Start the payment again."))
		return
	}

	failure := upperTrim(req.Outcome) == "FAILURE"
	reason := ""
	if failure {
		reason = trim(req.Reason)
		if reason == "" {
			reason = "Payment was not completed"
		}
		if len(reason) > 200 {
			reason = reason[:200]
		}
	}
	if h.svc.V2() && pay.ExpiresAt.Valid && time.Now().After(pay.ExpiresAt.Time) {
		writeError(w, fail(http.StatusConflict, "invalid_payment_session",
			"This payment session has expired. Start the payment again."))
		return
	}

	newStatus, label, moveErr, err := h.settle(r.Context(), tenant.ID, order, pay, failure, reason)
	if err != nil {
		if failure {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not record the payment failure")
		} else {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not confirm payment")
		}
		return
	}
	updated, _ := h.reload(r, order)

	if failure {
		payload := h.sessionPayload(updated, order, sf, pay.Method, pay.ProviderReference)
		payload["outcome"] = "FAILURE"
		payload["message"] = reason
		payload["can_retry"] = true
		payload["payment_methods"] = sf.Payments.Methods()
		response.JSON(w, http.StatusOK, payload)
		return
	}
	if updated.Status != payable.StatusPaid {
		// The gateway event was refused (e.g. amount mismatch): never claim
		// success for money that was not recorded.
		writeError(w, fail(http.StatusConflict, "payment_not_confirmed", "We could not confirm this payment."))
		return
	}
	payload := h.sessionPayload(updated, order, sf, updated.Method, pay.ProviderReference)
	payload["outcome"] = "SUCCESS"
	payload["message"] = "Payment received"
	payload["can_retry"] = false
	if moveErr == nil && newStatus != "" {
		payload["order_status"] = newStatus
		payload["order_status_label"] = label
	}
	response.JSON(w, http.StatusOK, payload)
}

// settle records the outcome. With the rollout flag on, a sandbox payment goes
// through the same path a real gateway callback takes: the sandbox "gateway"
// signs an event, it is verified, replay-guarded and applied. A payment that
// predates the flag (no gateway order id) is settled directly.
func (h *Handler) settle(ctx context.Context, tenantID uuid.UUID, order payable.Ref, pay sqlc.Payment, failure bool, reason string) (status, label string, moveErr, err error) {
	if h.svc.V2() && pay.ProviderOrderID != "" {
		kind := EventPaid
		if failure {
			kind = EventFailed
		}
		body, hdr := h.svc.sandbox.Sign(Event{
			ID: "evt_" + pay.ProviderOrderID + "_" + kind, Type: kind,
			ProviderOrderID: pay.ProviderOrderID,
			AmountMinor:     minorUnits(pgutil.NumericToFloat(pay.Amount)), Reason: reason,
		})
		ev, err := h.svc.sandbox.VerifyWebhook(hdr, body)
		if err != nil {
			return "", "", nil, err
		}
		res, err := h.svc.ProcessEvent(ctx, sandboxProvider, ev)
		return res.Status, res.Label, res.MoveErr, err
	}
	to := payable.StatusPaid
	if failure {
		to = payable.StatusFailed
	}
	if _, _, err := h.svc.Apply(ctx, order, pay, to, reason, Actor{Type: "customer"}); err != nil {
		return "", "", nil, err
	}
	if failure {
		return "", "", nil, nil
	}
	// A failure here must not lose the payment: it is already recorded as PAID.
	status, label, moveErr = h.src.AfterPaid(ctx, tenantID, order)
	if moveErr != nil {
		h.log.Error("auto accept after payment failed", "error", moveErr)
	}
	return status, label, moveErr, nil
}

// Webhook receives gateway callbacks. It has no session: the signature is the
// only credential, and the tenant comes from our own payment row, never from
// the payload.
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "provider")
	if !h.svc.KnownProvider(name) {
		response.Error(w, http.StatusNotFound, "not_found", "unknown payment provider")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "could not read body")
		return
	}
	ev, err := h.svc.VerifyIncoming(r.Context(), name, r.Header, body)
	switch {
	case errors.Is(err, ErrUnknownPayment):
		// Nothing of ours to authenticate it against: acknowledge so the
		// gateway stops retrying, change nothing.
		response.JSON(w, http.StatusOK, map[string]any{"status": KindIgnored})
		return
	case errors.Is(err, ErrBadSignature):
		h.log.Warn("payments: webhook rejected", "provider", name, "error", err)
		h.svc.NoteBadSignature(r.Context(), name)
		response.Error(w, http.StatusUnauthorized, "invalid_signature", "invalid signature")
		return
	case err != nil:
		// Our side (credentials, database): 5xx so the gateway retries later.
		h.log.Error("payments: webhook could not be verified", "provider", name, "error", err)
		h.svc.Alert(r.Context(), "unverifiable:"+name, "A payment webhook could not be verified",
			"A genuine-looking callback arrived but this platform could not load the business's gateway credentials to check it (disabled, removed or unreadable). The gateway will retry; fix the credentials soon so no payment is left pending.",
			nil)
		response.Error(w, http.StatusServiceUnavailable, "unavailable", "could not process event")
		return
	}
	res, err := h.svc.ProcessEvent(r.Context(), name, ev)
	if err != nil {
		h.log.Error("payments: webhook failed", "provider", name, "event", ev.ID, "error", err)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not process event")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": res.Kind})
}

// Status returns the current payment state for an order, so the payment screen
// can recover after a refresh without re-driving the flow. For a live payment
// that is still pending it also asks the gateway, so a paid customer whose
// confirmation is late is not left waiting.
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
	if pay.ID.Valid && pay.Status == payable.StatusPending {
		if _, changed, err := h.svc.RefreshPending(r.Context(), pay); err != nil {
			h.log.Warn("payments: could not reconcile with the gateway", "error", err)
		} else if changed {
			pay, _ = h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{OrderID: order.ID, TenantID: order.TenantID})
			// The follow-up (e.g. auto-accept) may have moved the order.
			if fresh, err := h.src.Load(r.Context(), tenant.ID, order.ID); err == nil {
				order = fresh
			}
		}
	}
	if h.svc.V2() && pay.ID.Valid && pay.ProviderOrderID == "" && pay.Status != payable.StatusPaid {
		// Nothing has been started with a gateway yet, so the row's mode is just
		// its default. Show the mode this business's next payment will run in, so
		// the screen offers the right checkout before the customer taps pay.
		if mode, _, err := h.svc.EffectiveMode(r.Context(), tenant.ID); err == nil {
			pay.Mode = string(mode)
		}
	}
	method := storefront.MethodOnline
	reference := ""
	if pay.ID.Valid {
		method = pay.Method
		// The intent token is only echoed while a payment is genuinely pending,
		// so a stale tab cannot replay a captured payment.
		if pay.Status == payable.StatusPending {
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
func (h *Handler) lookupOrder(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, bodyPhone string) (int32, payable.Ref, error) {
	number, err := h.src.ParseNumber(chi.URLParam(r, "orderNumber"))
	if err != nil {
		writeError(w, fail(http.StatusBadRequest, "invalid_request", "invalid order number"))
		return 0, payable.Ref{}, err
	}
	if principal, signedIn := auth.CustomerFromContext(r.Context()); signedIn && principal.TenantID == tenantID {
		ref, err := h.src.Resolve(r.Context(), tenantID, number, &principal.ID, "")
		if err != nil {
			writeError(w, fail(http.StatusNotFound, "order_not_found", "We could not find that order"))
			return 0, payable.Ref{}, err
		}
		return number, ref, nil
	}
	phone := trim(bodyPhone)
	if phone == "" {
		phone = trim(r.URL.Query().Get("phone"))
	}
	if phone == "" {
		writeError(w, fail(http.StatusBadRequest, "phone_required",
			"Enter the phone number you used to place this order"))
		return 0, payable.Ref{}, errors.New("phone_required")
	}
	ref, err := h.src.Resolve(r.Context(), tenantID, number, nil, normalisePhone(phone))
	if err != nil {
		writeError(w, fail(http.StatusNotFound, "order_not_found",
			"We could not find that order for that phone number"))
		return 0, payable.Ref{}, err
	}
	return number, ref, nil
}

func (h *Handler) reload(r *http.Request, order payable.Ref) (sqlc.Payment, error) {
	return h.q.GetPaymentByOrderID(r.Context(), sqlc.GetPaymentByOrderIDParams{
		OrderID:  order.ID,
		TenantID: order.TenantID,
	})
}

func (h *Handler) respondPayment(w http.ResponseWriter, r *http.Request, order payable.Ref, sf *storefront.Storefront, message string) {
	tenantID := uuid.UUID(order.TenantID.Bytes)
	payload, err := h.src.Detail(r.Context(), tenantID, order)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this order")
		return
	}
	payload["message"] = message
	response.JSON(w, http.StatusOK, payload)
}

// sessionPayload is the payment screen's view of the world. It contains no
// gateway credentials and no provider reference beyond the current intent
// token, which is only present while a payment is genuinely in flight.
func (h *Handler) sessionPayload(pay sqlc.Payment, order payable.Ref, sf *storefront.Storefront, method, reference string) map[string]any {
	paymentStatus := payable.StatusPending
	if pay.ID.Valid {
		paymentStatus = pay.Status
	}
	amount := pgutil.NumericToFloat(order.Amount)
	if pay.ID.Valid {
		amount = pgutil.NumericToFloat(pay.Amount)
	}
	mode := string(ModeSandbox)
	if pay.ID.Valid && pay.Mode != "" {
		mode = pay.Mode
	}
	out := map[string]any{
		"order_number":       order.Number,
		"order_status":       order.Status,
		"order_status_label": order.StatusLabel,
		"method":             method,
		"payment_status":     paymentStatus,
		"amount":             amount,
		"currency":           currencyOr(sf.Currency),
		"store_name":         sf.Name,
		"payment_methods":    sf.Payments.Methods(),
		"can_pay_online":     sf.Payments.PayOnline(),
		"payable":            order.Active && paymentStatus != payable.StatusPaid,
		"created_at":         order.CreatedAt.Format(time.RFC3339),
		"payment_mode":       mode,
		"is_test":            mode != string(ModeLive),
	}
	if pay.ID.Valid && pay.FailureReason != "" && paymentStatus == payable.StatusFailed {
		out["failure_reason"] = pay.FailureReason
		out["attempts"] = pay.AttemptCount
	}
	if reference != "" && paymentStatus == payable.StatusPending {
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
