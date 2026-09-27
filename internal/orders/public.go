package orders

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// maxPublicBody caps a public write. A checkout payload is a few kilobytes at
// most, so anything larger is a mistake or an attack.
const maxPublicBody = 64 << 10

// hostTenant resolves the shop for public storefront calls. The tenant is
// derived from the request hostname, so a storefront can only ever address its
// own data no matter what the request body claims.
func hostTenant(r *http.Request) (uuid.UUID, bool) {
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		return uuid.Nil, false
	}
	return info.ID, true
}

// loadStorefront reads the tenant's storefront configuration, bootstrapping the
// row from the tenant's own identity on first request.
func (h *Handler) loadStorefront(r *http.Request) (*storefront.Storefront, error) {
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		return nil, storefront.ErrNotFound
	}
	sf, err := h.store.Ensure(r.Context(), info.ID, storefront.Seed{
		Name:       info.Name,
		Phone:      "",
		Address:    "",
		LogoURL:    "",
		FaviconURL: "",
	})
	if err != nil {
		return nil, err
	}
	return sf, nil
}

// PublicStore serves the storefront configuration: identity, theme tokens,
// homepage layout, available payment methods, workflow summary and the live
// open/closed state. This is the only config payload a customer page needs.
func (h *Handler) PublicStore(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		h.log.Error("storefront load failed", "error", err, "tenant", tenantID)
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this store")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenantID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}

	status := sf.OpeningHours.Status(time.Now())
	preview := !sf.IsPublished
	payload := map[string]any{
		"store": map[string]any{
			"name":          sf.Name,
			"slug":          sf.Slug,
			"logo_url":      sf.LogoURL,
			"favicon_url":   sf.FaviconURL,
			"tagline":       sf.Tagline,
			"description":   sf.About,
			"phone":         sf.Phone,
			"address":       sf.Address,
			"business_type": sf.BusinessTy,
			"currency":      currencyOr(sf.Currency),
		},
		"theme": map[string]any{
			"preset":         sf.Theme.Preset,
			"mode":           sf.Theme.Mode,
			"font":           sf.Theme.Font,
			"radius":         sf.Theme.Radius,
			"button":         sf.Theme.Button,
			"card":           sf.Theme.Card,
			"header":         sf.Theme.Header,
			"hero":           sf.Theme.Hero,
			"product_layout": sf.Theme.Layout,
			"filter_style":   sf.Theme.Filter,
			"primary":        sf.Theme.Primary,
			"secondary":      sf.Theme.Secondary,
			"accent":         sf.Theme.Accent,
			"hero_image_url": sf.HeroImageURL,
			"vars":           sf.Theme.CSSVars(),
			"font_stack":     storefront.FontStacks[sf.Theme.Font],
			"font_import":    storefront.FontImports[sf.Theme.Font],
		},
		"homepage": map[string]any{
			"sections": sf.Homepage.Sections,
		},
		"payments": map[string]any{
			"online_payment_enabled": sf.Payments.OnlineEnabled,
			"cash_enabled":           sf.Payments.CashEnabled,
			"pay_at_pickup_enabled":  sf.Payments.AtPickupEnable,
			"default_payment_method": sf.Payments.DefaultMethod,
			"methods":                sf.Payments.Methods(),
		},
		"ordering": map[string]any{
			"enabled":             sf.OrderingAllowed(),
			"closed_reason":       sf.ClosedReason(),
			"prep_time_minutes":   sf.PrepTimeMinutes,
			"customer_login":      sf.CustomerLogin,
			"customer_login_mode": sf.CustomerLoginMode,
			"payment_requirement": sf.Workflow.PaymentRequirement,
			"auto_accept":         sf.Workflow.AutoAccept(),
		},
		"hours": map[string]any{
			"always_open":  status.AlwaysOpen,
			"is_open":      status.Open,
			"label":        status.Label,
			"detail":       status.Detail,
			"timezone":     status.Timezone,
			"schedule":     sf.OpeningHours.Schedule,
			"today_closes": status.TodayCloses,
		},
		"preview": preview,
	}
	setStoreCacheHeaders(w, sf.UpdatedAt)
	response.JSON(w, http.StatusOK, payload)
}

// previewAllowed lets a signed-in member of this tenant read an unpublished
// storefront, which is what the admin "Preview store" button needs. It is
// gated on a real staff token for the same tenant — never on a query flag.
func (h *Handler) previewAllowed(r *http.Request, tenantID uuid.UUID) bool {
	principal, ok := auth.CustomerFromContext(r.Context())
	if ok {
		return principal.TenantID == tenantID
	}
	return false
}

// PublicMenu serves the published menu for the host tenant.
func (h *Handler) PublicMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load the menu")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenantID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	menu, err := h.catalog.LoadMenu(r.Context(), tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load the menu")
		return
	}
	setStoreCacheHeaders(w, sf.UpdatedAt)
	response.JSON(w, http.StatusOK, map[string]any{
		"store":      map[string]any{"name": sf.Name, "slug": sf.Slug},
		"categories": menu.Categories,
		"products":   menu.Products,
		"ordering":   map[string]any{"enabled": sf.OrderingAllowed(), "closed_reason": sf.ClosedReason()},
	})
}

// PublicProduct serves one product, including its add-on catalogue.
func (h *Handler) PublicProduct(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this item")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenantID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "item not found")
		return
	}
	product, err := h.catalog.LoadProduct(r.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, storefront.ErrProductUnavailable) {
			response.Error(w, http.StatusNotFound, "not_found", "item not found")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this item")
		return
	}
	setStoreCacheHeaders(w, sf.UpdatedAt)
	response.JSON(w, http.StatusOK, map[string]any{
		"product":  product,
		"ordering": map[string]any{"enabled": sf.OrderingAllowed(), "closed_reason": sf.ClosedReason()},
	})
}

// PublicQuote re-prices a cart server-side so the cart and checkout screens
// never have to guess at tax or packaging. The returned totals are the same
// numbers the order will be created with.
func (h *Handler) PublicQuote(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not price your cart")
		return
	}
	var req struct {
		Items []RequestedLine `json:"items"`
	}
	if err := decodeBody(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	products, lines, verr := h.resolveCart(r.Context(), tenantID, req.Items)
	if verr != nil {
		writeValidationError(w, verr)
		return
	}
	priced, totals, costErr := PriceCart(products, lines, Costing{
		TaxPercent: sf.TaxPercent, PackagingFee: sf.PackagingFee,
	})
	if costErr != nil {
		writeValidationError(w, &validationError{Code: costErr.Code, Message: costErr.Message, Status: 400})
		return
	}
	linesOut := make([]map[string]any, 0, len(priced))
	for _, line := range priced {
		linesOut = append(linesOut, map[string]any{
			"product_id": line.Product.ID,
			"name":       line.Product.Name,
			"quantity":   line.Quantity,
			"unit_price": line.UnitBase,
			"unit_total": line.UnitTotal,
			"line_total": line.LineTotal,
			"addons":     line.Addons,
			"notes":      line.Notes,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"items":    linesOut,
		"totals":   totals,
		"currency": currencyOr(sf.Currency),
	})
}

// PublicCreateOrder places an order for a guest or a signed-in customer.
func (h *Handler) PublicCreateOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this store")
		return
	}
	var req CreateRequest
	if err := decodeBody(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	// A customer token is optional when login is off or optional. When the
	// shop requires sign-in, guest checkout is rejected here so the rule
	// cannot be bypassed by skipping the UI gate.
	principal, signedIn := auth.CustomerFromContext(r.Context())
	if sf.LoginRequired() && (!signedIn || principal.TenantID != tenantID) {
		response.Error(w, http.StatusUnauthorized, "login_required",
			"Sign in with your phone to place an order at this store")
		return
	}
	var principalPtr *auth.CustomerPrincipal
	if signedIn && principal.TenantID == tenantID {
		principalPtr = &principal
	}

	result, err := h.Create(r.Context(), tenantID, sf, req, principalPtr)
	if err != nil {
		writeValidationError(w, asValidation(h, err))
		return
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	response.JSON(w, status, h.orderResponse(result, sf))
}

// PublicTrackOrder returns one order to its owner: a signed-in customer, or a
// guest who supplies the phone number used at checkout.
func (h *Handler) PublicTrackOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this order")
		return
	}
	number, err := ParseOrderNumber(chi.URLParam(r, "orderNumber"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order number")
		return
	}
	viewer := h.Viewer()

	if principal, signedIn := auth.CustomerFromContext(r.Context()); signedIn && principal.TenantID == tenantID {
		view, err := viewer.ByNumberForCustomer(r.Context(), tenantID, principal.ID, number)
		if err != nil {
			writeLookupError(w, err)
			return
		}
		response.JSON(w, http.StatusOK, h.trackResponse(view, sf))
		return
	}

	phone := normalisePhoneInput(r.URL.Query().Get("phone"))
	if phone == "" {
		response.Error(w, http.StatusBadRequest, "phone_required",
			"Enter the phone number you used to place this order")
		return
	}
	view, err := viewer.ByNumberAndPhone(r.Context(), tenantID, number, phone)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, h.trackResponse(view, sf))
}

// PublicLookupOrder lets a guest find their orders from the orders page: the
// phone number returns every order placed with it.
func (h *Handler) PublicLookupOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := hostTenant(r)
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.loadStorefront(r)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this store")
		return
	}
	phone := normalisePhoneInput(r.URL.Query().Get("phone"))
	if phone == "" {
		response.Error(w, http.StatusBadRequest, "phone_required", "Enter your phone number")
		return
	}
	rows, listErr := h.customerOrdersByPhone(r, tenantID, phone, sf)
	if listErr != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not look up your orders")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, o.Summary())
	}
	response.JSON(w, http.StatusOK, map[string]any{"orders": out})
}

func (h *Handler) orderResponse(result CreateResult, sf *storefront.Storefront) map[string]any {
	payload := h.trackResponse(result.View, sf)
	payload["duplicate"] = result.Duplicate
	payload["payment_methods"] = result.Methods
	payload["can_pay_online"] = result.CanPayOnline
	if result.View.NeedsOnlinePayment() && result.CanPayOnline {
		payload["next_step"] = "pay"
		payload["payment_path"] = "/payment?order=" + strconv.Itoa(int(result.View.OrderNumber))
	} else {
		payload["next_step"] = "confirmation"
		payload["confirmation_path"] = "/order/" + strconv.Itoa(int(result.View.OrderNumber))
	}
	return payload
}

func (h *Handler) trackResponse(view OrderView, sf *storefront.Storefront) map[string]any {
	payload := view.Detail()
	payload["payment_methods"] = sf.Payments.Methods()
	payload["can_pay_online"] = sf.Payments.PayOnline()
	if view.NeedsOnlinePayment() {
		payload["payment_path"] = "/payment?order=" + strconv.Itoa(int(view.OrderNumber))
	}
	return payload
}

// customerOrdersByPhone lists a guest's orders. The lookup is by exact phone
// match, so knowing someone's number reveals only orders they placed with it.
func (h *Handler) customerOrdersByPhone(r *http.Request, tenantID uuid.UUID, phone string, sf *storefront.Storefront) ([]OrderView, error) {
	rows, err := h.q.ListOrdersByTenantAndPhone(r.Context(), sqlc.ListOrdersByTenantAndPhoneParams{
		TenantID:      pgutil.UUID(tenantID),
		CustomerPhone: phone,
		LimitCount:    50,
	})
	if err != nil {
		return nil, err
	}
	viewer := h.Viewer()
	out := make([]OrderView, 0, len(rows))
	for _, row := range rows {
		view, err := viewer.hydrate(r.Context(), row, *sf)
		if err != nil {
			continue
		}
		out = append(out, view)
	}
	return out, nil
}

func decodeBody(r *http.Request, target any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxPublicBody))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func writeValidationError(w http.ResponseWriter, err error) {
	var ve *validationError
	if errors.As(err, &ve) {
		status := ve.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		response.Error(w, status, ve.Code, ve.Message)
		return
	}
	var te *ErrInvalidTransition
	if errors.As(err, &te) {
		response.Error(w, http.StatusConflict, "invalid_transition", te.Message())
		return
	}
	var pe *PriceError
	if errors.As(err, &pe) {
		response.Error(w, http.StatusBadRequest, pe.Code, pe.Message)
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong. Please try again.")
}

func asValidation(h *Handler, err error) error {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve
	}
	var te *ErrInvalidTransition
	if errors.As(err, &te) {
		return te
	}
	var pe *PriceError
	if errors.As(err, &pe) {
		return &validationError{Code: pe.Code, Message: pe.Message, Status: http.StatusBadRequest}
	}
	h.log.Error("order create failed", "error", err)
	return &validationError{Code: "internal_error", Message: "We could not place your order. Please try again.", Status: http.StatusInternalServerError}
}

func writeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrOrderNotFound) {
		response.Error(w, http.StatusNotFound, "order_not_found",
			"We could not find that order. Check the order number and phone number.")
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your order")
}

func currencyOr(v string) string {
	if v == "" {
		return "INR"
	}
	return v
}

func setStoreCacheHeaders(w http.ResponseWriter, updated time.Time) {
	// The storefront config changes rarely and is public, so let a browser or
	// edge hold it briefly. The ETag changes the moment a tenant saves.
	if !updated.IsZero() {
		w.Header().Set("ETag", `"`+strconv.FormatInt(updated.UTC().UnixNano(), 10)+`"`)
	}
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
}
