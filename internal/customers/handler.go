package customers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/signin"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Handler serves storefront customer identity: phone login, the profile and
// order history.
//
// Guest checkout is allowed when the shop sets login to off or optional.
// When login is required, create-order rejects unauthenticated requests.
type Handler struct {
	pool   *pgxpool.Pool
	q      *sqlc.Queries
	orders *orders.Viewer
	store  *storefront.Loader
	auth   *auth.Service
	log    *slog.Logger
	// devMode echoes the OTP in the send-otp response. It is only ever true
	// outside production, so a misconfigured deployment cannot leak live
	// sign-in codes.
	devMode bool
	signin  *signin.Service
}

// NewHandler wires the customer handler.
func NewHandler(pool *pgxpool.Pool, viewer *orders.Viewer, loader *storefront.Loader, cfg config.Config, log *slog.Logger) *Handler {
	return &Handler{
		pool:    pool,
		q:       sqlc.New(pool),
		orders:  viewer,
		store:   loader,
		auth:    auth.NewService(pool, cfg, log),
		log:     log,
		devMode: cfg.AppEnv != "production" && cfg.AppEnv != "prod",
	}
}

// hostTenant resolves the shop from the request hostname. Customer endpoints are
// reachable before an order exists, so the tenant comes from the host and never
// from the request body.
func hostTenant(r *http.Request) (uuid.UUID, string, bool) {
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		return uuid.Nil, "", false
	}
	return info.ID, info.Slug, true
}

// loginEnabled says whether this business offers customer sign-in at all:
// the owner has not switched it off (a booking business always signs people
// in) and at least one method has a working channel.
func (h *Handler) loginEnabled(r *http.Request, tenantID uuid.UUID) (*storefront.Storefront, bool) {
	sf, err := h.store.Load(r.Context(), tenantID)
	if err != nil || sf == nil {
		return nil, false
	}
	booking := false
	if t, ok := tenantctx.FromContext(r.Context()); ok {
		booking = t.HasCapability(tenantctx.CapAppointments) || t.HasCapability(tenantctx.CapRooms)
	}
	if !booking && !sf.LoginAllowed() {
		return sf, false
	}
	return sf, h.signin == nil || len(h.signin.Offered(r.Context(), tenantID, sf)) > 0
}

// AttachSignIn connects the sign-in channel checks and code delivery.
func (h *Handler) AttachSignIn(s *signin.Service) { h.signin = s }

type sendOtpRequest struct {
	Phone string `json:"phone"`
	Email string `json:"email"`
}

// signInID is who a code is for: a normalised phone number or email, and the
// method that reaches it.
type signInID struct {
	method string
	value  string
}

// parseIdentity reads whichever of phone or email the customer gave.
func parseIdentity(phone, email string) (signInID, string) {
	if strings.TrimSpace(email) != "" {
		e, err := NormalizeEmail(email)
		if err != nil {
			return signInID{}, "Enter a valid email address"
		}
		return signInID{method: storefront.LoginMethodEmail, value: e}, ""
	}
	p, err := NormalizePhone(phone)
	if err != nil {
		return signInID{}, "Enter a valid phone number"
	}
	return signInID{method: storefront.LoginMethodPhone, value: p}, ""
}

func (id signInID) masked() map[string]any {
	if id.method == storefront.LoginMethodEmail {
		return map[string]any{"method": id.method, "email": MaskEmail(id.value)}
	}
	return map[string]any{"method": id.method, "phone": MaskPhone(id.value)}
}

// SendOtp issues a sign-in code and delivers it by SMS or email, through the
// business's own account or the platform's. The code is only ever echoed back
// in development, where no message may be delivered.
func (h *Handler) SendOtp(w http.ResponseWriter, r *http.Request) {
	tenantID, slug, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, enabled := h.loginEnabled(r, tenantID)
	if !enabled {
		response.Error(w, http.StatusForbidden, "login_disabled", "Sign-in is not available at this store")
		return
	}
	var req sendOtpRequest
	if err := decode(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	id, msg := parseIdentity(req.Phone, req.Email)
	if msg != "" {
		code := "invalid_phone"
		if id.method == storefront.LoginMethodEmail || strings.TrimSpace(req.Email) != "" {
			code = "invalid_email"
		}
		response.Error(w, http.StatusBadRequest, code, msg)
		return
	}
	if h.signin != nil && !h.signin.Allows(r.Context(), tenantID, sf, id.method) {
		response.Error(w, http.StatusForbidden, "method_unavailable", "This store does not offer that sign-in method")
		return
	}

	code, state, err := IssueCode(r.Context(), h.q, tenantID, id.value, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, ErrResendTooSoon):
			writeOTPError(w, http.StatusTooManyRequests, "resend_too_soon", err.Error(), resendState())
			return
		case errors.Is(err, ErrRateLimited):
			writeOTPError(w, http.StatusTooManyRequests, "rate_limited", err.Error(), OTPState{MaxTries: MaxAttempts})
			return
		}
		h.log.Error("send otp failed", "error", err, "tenant", slug)
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not send the code. Try again shortly.")
		return
	}
	if h.signin != nil {
		if err := h.signin.SendCode(r.Context(), tenantID, sf.Name, id.method, id.value, code, int(CodeTTL.Minutes())); err != nil {
			h.log.Error("deliver otp failed", "error", err, "tenant", slug, "method", id.method)
			response.Error(w, http.StatusBadGateway, "delivery_failed", "We could not send the code just now. Try again in a moment.")
			return
		}
	}
	if h.devMode {
		state.DevCode = code
		h.log.Info("customer otp issued", "tenant", slug, "method", id.method, "code", code)
	}

	out := id.masked()
	out["otp"] = state
	out["next_step"] = "verify"
	out["expires_in"] = state.ExpiresIn
	response.JSON(w, http.StatusOK, out)
}

// resendState is the countdown payload returned alongside a too-soon error, so
// the client restarts its timer from the server's rule rather than its own.
func resendState() OTPState {
	return OTPState{MaxTries: MaxAttempts, ResendIn: int(ResendAfter.Seconds()), CanResend: false}
}

type verifyOtpRequest struct {
	Phone string `json:"phone"`
	Email string `json:"email"`
	Code  string `json:"code"`
}

// VerifyOtp exchanges a phone number or email and its code for a session.
func (h *Handler) VerifyOtp(w http.ResponseWriter, r *http.Request) {
	tenantID, slug, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, enabled := h.loginEnabled(r, tenantID)
	if !enabled {
		response.Error(w, http.StatusForbidden, "login_disabled", "Sign-in is not available at this store")
		return
	}
	var req verifyOtpRequest
	if err := decode(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	id, msg := parseIdentity(req.Phone, req.Email)
	if msg != "" {
		response.Error(w, http.StatusBadRequest, "invalid_identity", msg)
		return
	}
	if h.signin != nil && !h.signin.Allows(r.Context(), tenantID, sf, id.method) {
		response.Error(w, http.StatusForbidden, "method_unavailable", "This store does not offer that sign-in method")
		return
	}
	if len(strings.TrimSpace(req.Code)) != CodeLength {
		response.Error(w, http.StatusBadRequest, "invalid_code", "Enter the 6-digit code")
		return
	}

	if err := Verify(r.Context(), h.q, tenantID, id.value, strings.TrimSpace(req.Code), time.Now()); err != nil {
		status, code := http.StatusBadRequest, "invalid_code"
		switch {
		case errors.Is(err, ErrNoCode):
			code = "no_code"
		case errors.Is(err, ErrExpired):
			code = "otp_expired"
		case errors.Is(err, ErrTooManyAttempts):
			status, code = http.StatusTooManyRequests, "too_many_attempts"
		}
		response.Error(w, status, code, err.Error())
		return
	}

	var customer sqlc.Customer
	var err error
	if id.method == storefront.LoginMethodEmail {
		customer, err = h.q.UpsertCustomerByEmail(r.Context(), sqlc.UpsertCustomerByEmailParams{
			TenantID: pgutil.UUID(tenantID), Name: "", Email: id.value,
		})
	} else {
		customer, err = Upsert(r.Context(), h.q, tenantID, id.value, "")
	}
	if err != nil {
		h.log.Error("customer upsert failed", "error", err, "tenant", slug)
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not sign you in")
		return
	}
	if customer.IsBlocked {
		response.Error(w, http.StatusForbidden, "account_blocked", ErrBlocked.Error())
		return
	}

	phone := customer.Phone.String
	pair, err := h.auth.IssueCustomerTokens(uuid.UUID(customer.ID.Bytes), tenantID, customer.Name, phone)
	if err != nil {
		h.log.Error("customer token issue failed", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not sign you in")
		return
	}

	storeName := h.storeName(r, tenantID)
	isNew := strings.TrimSpace(customer.Name) == ""
	response.JSON(w, http.StatusOK, map[string]any{
		"tokens": pair,
		"customer": map[string]any{
			"id":         pgutil.UUIDString(customer.ID),
			"name":       customer.Name,
			"phone":      MaskPhone(phone),
			"phone_full": phone,
			"email":      customer.Email.String,
			"is_new":     isNew,
		},
		"store_name": storeName,
	})
}

func (h *Handler) storeName(r *http.Request, tenantID uuid.UUID) string {
	if sf, err := h.store.Load(r.Context(), tenantID); err == nil {
		return sf.Name
	}
	if info, ok := tenantctx.FromContext(r.Context()); ok {
		return info.Name
	}
	return ""
}

// Profile returns the signed-in customer's details plus their recent orders.
func (h *Handler) Profile(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to continue")
		return
	}
	row, err := h.q.GetCustomerByID(r.Context(), sqlc.GetCustomerByIDParams{
		ID:       pgutil.UUID(principal.ID),
		TenantID: pgutil.UUID(principal.TenantID),
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "profile not found")
		return
	}
	recent, err := h.orders.CustomerOrders(r.Context(), principal.TenantID, principal.ID, 20)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not load your orders")
		return
	}
	active := make([]map[string]any, 0, 2)
	past := make([]map[string]any, 0, len(recent))
	for _, o := range recent {
		if o.Status == orders.StatusPending || o.Status == orders.StatusAccepted ||
			o.Status == orders.StatusPreparing || o.Status == orders.StatusReady {
			active = append(active, o.Summary())
			continue
		}
		past = append(past, o.Summary())
	}
	total, _ := h.q.ListCustomerOrders(r.Context(), sqlc.ListCustomerOrdersParams{
		TenantID:   pgutil.UUID(principal.TenantID),
		CustomerID: pgutil.UUID(principal.ID),
		LimitCount: 200,
	})
	response.JSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":            pgutil.UUIDString(row.ID),
			"name":          row.Name,
			"phone":         MaskPhone(row.Phone.String),
			"phone_full":    row.Phone.String,
			"joined":        row.CreatedAt.Time.Format(time.RFC3339),
			"last_login_at": formatTime(row.LastLoginAt),
		},
		"active_orders": active,
		"recent_orders": past,
		"total_orders":  len(total),
		"benefits": []string{
			"Faster checkout with your details already filled in",
			"Order history and live tracking from your profile",
		},
	})
}

// UpdateProfile saves the display name a customer chose at first sign-in.
func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to continue")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 80 {
		response.Error(w, http.StatusBadRequest, "invalid_name", "Name must be 2 to 80 characters")
		return
	}
	row, err := h.q.UpdateCustomerProfile(r.Context(), sqlc.UpdateCustomerProfileParams{
		Name:     pgutil.Text(name),
		ID:       pgutil.UUID(principal.ID),
		TenantID: pgutil.UUID(principal.TenantID),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not save your details")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"customer": map[string]any{
			"id":         pgutil.UUIDString(row.ID),
			"name":       row.Name,
			"phone":      MaskPhone(row.Phone.String),
			"phone_full": row.Phone.String,
		},
	})
}

// Orders lists the signed-in customer's own orders, newest first.
func (h *Handler) Orders(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to view your orders")
		return
	}
	rows, err := h.orders.CustomerOrders(r.Context(), principal.TenantID, principal.ID, 50)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not load your orders")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, o.Summary())
	}
	response.JSON(w, http.StatusOK, map[string]any{"orders": out})
}

// SingleOrder returns one of the customer's own orders.
func (h *Handler) SingleOrder(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.CustomerFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to view your orders")
		return
	}
	number, err := orders.ParseOrderNumber(chi.URLParam(r, "orderNumber"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order number")
		return
	}
	view, err := h.orders.ByNumberForCustomer(r.Context(), principal.TenantID, principal.ID, number)
	if err != nil {
		if errors.Is(err, orders.ErrOrderNotFound) {
			response.Error(w, http.StatusNotFound, "order_not_found", "We could not find that order")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "We could not load that order")
		return
	}
	response.JSON(w, http.StatusOK, view.Detail())
}

// Logout is a no-op acknowledgement: customer tokens are stateless, so signing
// out is the client discarding them. The endpoint exists so the client has one
// call to make and any future server-side revocation has a home.
func (h *Handler) Logout(w http.ResponseWriter, _ *http.Request) {
	response.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeOTPError(w http.ResponseWriter, status int, code, message string, state OTPState) {
	response.JSON(w, status, response.ErrorBody{
		Error: response.ErrorDetail{Code: code, Message: message},
	})
}

func decode(r *http.Request, target any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func formatTime(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}
