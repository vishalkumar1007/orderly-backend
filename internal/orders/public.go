package orders

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// PublicStore returns the storefront configuration a customer needs to browse
// and check out. Unpublished shops are hidden unless the caller is allowed to
// preview them.
func (h *Handler) PublicStore(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Load(r.Context(), tenant.ID)
	if err != nil || sf == nil {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenant.ID) {
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
			"preset":                       sf.Theme.Preset,
			"mode":                         sf.Theme.Mode,
			"font":                         sf.Theme.Font,
			"radius":                       sf.Theme.Radius,
			"button":                       sf.Theme.Button,
			"card":                         sf.Theme.Card,
			"header":                       sf.Theme.Header,
			"hero":                         sf.Theme.Hero,
			"product_layout":               sf.Theme.Layout,
			"filter_style":                 sf.Theme.Filter,
			"primary":                      sf.Theme.Primary,
			"secondary":                    sf.Theme.Secondary,
			"accent":                       sf.Theme.Accent,
			"hero_image_url":               sf.HeroImageURL,
			"vars":                         sf.Theme.CSSVars(),
			"font_stack":                   storefront.FontStacks[sf.Theme.Font],
			"font_import":                  storefront.FontImports[sf.Theme.Font],
			"customer_mode_switch_enabled": sf.Theme.CustomerModeSwitch,
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
			"ready_notification":  sf.Workflow.ReadyNotification,
			"order_ready_sound":   sf.Workflow.OrderReadySound,
			"store_status":             sf.StoreStatus,
			"store_status_label":       storefront.StoreStatusLabel(sf.StoreStatus),
			"status_message":           sf.StatusMessage,
			"status_message_display":   sf.DisplayStatusMessage(),
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

// PublicMenu returns the tenant's catalogue for browsing.
func (h *Handler) PublicMenu(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	if err := h.requirePublishedStore(w, r, tenant.ID); err != nil {
		return
	}
	menu, err := h.catalog.LoadMenu(r.Context(), tenant.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load the menu")
		return
	}
	flat := make([]storefront.Product, 0)
	for _, c := range menu.Categories {
		flat = append(flat, c.Products...)
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"categories": menu.Categories,
		"products":   flat,
	})
}

// PublicProduct returns one product with its customisation options.
func (h *Handler) PublicProduct(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	if err := h.requirePublishedStore(w, r, tenant.ID); err != nil {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid product id")
		return
	}
	product, err := h.catalog.LoadProduct(r.Context(), tenant.ID, id)
	if err != nil || !product.Available {
		response.Error(w, http.StatusNotFound, "not_found", "product not found")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"product": product})
}

// PublicQuote prices a cart without placing an order.
func (h *Handler) PublicQuote(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Load(r.Context(), tenant.ID)
	if err != nil || sf == nil {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenant.ID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	var req struct {
		Items []RequestedLine `json:"items"`
	}
	if err := decodeBody(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	if len(req.Items) == 0 {
		writeValidationError(w, badRequest("empty_cart", "Your cart is empty"))
		return
	}
	products, lines, perr := h.resolveCart(r.Context(), tenant.ID, req.Items)
	if perr != nil {
		writeValidationError(w, perr)
		return
	}
	_, totals, costErr := PriceCart(products, lines, Costing{
		TaxPercent:   sf.TaxPercent,
		PackagingFee: sf.PackagingFee,
	})
	if costErr != nil {
		writeValidationError(w, &validationError{Code: costErr.Code, Message: costErr.Message, Status: 400})
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"totals": totals})
}

// PublicCreateOrder places a customer order. Prices are always recomputed on
// the server inside the same transaction that writes the order.
func (h *Handler) PublicCreateOrder(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	sf, err := h.store.Ensure(r.Context(), tenant.ID, storefront.Seed{Name: tenant.Name})
	if err != nil || sf == nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load this store")
		return
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenant.ID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return
	}
	var req CreateRequest
	if err := decodeBody(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	var principal *auth.CustomerPrincipal
	if p, ok := auth.CustomerFromContext(r.Context()); ok && p.TenantID == tenant.ID {
		principal = &p
	}
	if err := h.enforceLoginPolicy(sf, principal); err != nil {
		writeValidationError(w, err)
		return
	}
	result, err := h.Create(r.Context(), tenant.ID, sf, req, principal)
	if err != nil {
		writeValidationError(w, asValidation(h, err))
		return
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	response.JSON(w, status, checkoutPayload(result))
}

// PublicTrackOrder lets a guest or signed-in customer read one order.
func (h *Handler) PublicTrackOrder(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	if err := h.requirePublishedStore(w, r, tenant.ID); err != nil {
		return
	}
	number, err := strconv.Atoi(chi.URLParam(r, "orderNumber"))
	if err != nil || number <= 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid order number")
		return
	}
	viewer := h.Viewer()
	if principal, ok := auth.CustomerFromContext(r.Context()); ok && principal.TenantID == tenant.ID {
		view, err := viewer.ByNumberForCustomer(r.Context(), tenant.ID, principal.ID, int32(number))
		if err != nil {
			writeLookupError(w, err)
			return
		}
		sf, _ := h.store.Load(r.Context(), tenant.ID)
		if sf != nil {
			view = withStoreRef(view, *sf)
		}
		response.JSON(w, http.StatusOK, view.Detail())
		return
	}
	phone := normalisePhoneInput(r.URL.Query().Get("phone"))
	if phone == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "phone is required")
		return
	}
	view, err := viewer.ByNumberAndPhone(r.Context(), tenant.ID, int32(number), phone)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	sf, _ := h.store.Load(r.Context(), tenant.ID)
	if sf != nil {
		view = withStoreRef(view, *sf)
	}
	response.JSON(w, http.StatusOK, view.Detail())
}

// PublicLookupOrder lists recent orders for a phone number (guest self-serve).
func (h *Handler) PublicLookupOrder(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	if err := h.requirePublishedStore(w, r, tenant.ID); err != nil {
		return
	}
	phone := normalisePhoneInput(r.URL.Query().Get("phone"))
	if phone == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "phone is required")
		return
	}
	rows, err := h.q.ListOrdersByTenantAndPhone(r.Context(), sqlc.ListOrdersByTenantAndPhoneParams{
		TenantID:      pgutil.UUID(tenant.ID),
		CustomerPhone: phone,
		LimitCount:    20,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not look up orders")
		return
	}
	sf, _ := h.store.Load(r.Context(), tenant.ID)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		store := storefront.Storefront{}
		if sf != nil {
			store = *sf
		}
		view, err := h.Viewer().hydrate(r.Context(), row, store)
		if err != nil {
			continue
		}
		out = append(out, view.Summary())
	}
	response.JSON(w, http.StatusOK, map[string]any{"orders": out})
}

func (h *Handler) requirePublishedStore(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) error {
	sf, err := h.store.Load(r.Context(), tenantID)
	if err != nil || sf == nil {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return err
	}
	if !sf.IsPublished && !h.previewAllowed(r, tenantID) {
		response.Error(w, http.StatusNotFound, "not_found", "store not found")
		return errors.New("unpublished")
	}
	return nil
}

func (h *Handler) previewAllowed(r *http.Request, tenantID uuid.UUID) bool {
	if principal, ok := auth.CustomerFromContext(r.Context()); ok {
		return principal.TenantID == tenantID
	}
	user, ok := identity.UserFromContext(r.Context())
	return ok && user.TenantID != nil && *user.TenantID == tenantID
}

func (h *Handler) enforceLoginPolicy(sf *storefront.Storefront, principal *auth.CustomerPrincipal) *validationError {
	mode := sf.CustomerLoginMode
	if mode == storefront.LoginRequired && principal == nil {
		return &validationError{
			Code:    "login_required",
			Message: "Sign in with your phone to place this order",
			Status:  http.StatusUnauthorized,
		}
	}
	return nil
}

func checkoutPayload(result CreateResult) map[string]any {
	out := result.View.Detail()
	out["duplicate"] = result.Duplicate
	out["store_name"] = result.StoreName
	out["payment_methods"] = result.Methods
	out["can_pay_online"] = result.CanPayOnline
	if result.View.NeedsOnlinePayment() {
		out["next_step"] = "pay"
	} else {
		out["next_step"] = "confirmation"
	}
	return out
}

func withStoreRef(view OrderView, sf storefront.Storefront) OrderView {
	view.Store = StoreRef{Name: sf.Name, Address: sf.Address, Phone: sf.Phone}
	return view
}

func setStoreCacheHeaders(w http.ResponseWriter, updatedAt time.Time) {
	w.Header().Set("Cache-Control", "public, max-age=60")
	if !updatedAt.IsZero() {
		w.Header().Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))
	}
}

func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	return nil
}

func writeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrOrderNotFound) {
		response.Error(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	response.Error(w, http.StatusInternalServerError, "internal_error", "could not load that order")
}

func writeValidationError(w http.ResponseWriter, ve *validationError) {
	if ve == nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong")
		return
	}
	status := ve.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	response.Error(w, status, ve.Code, ve.Message)
}

func asValidation(h *Handler, err error) *validationError {
	var ve *validationError
	if errors.As(err, &ve) {
		return ve
	}
	return &validationError{Code: "internal_error", Message: "Could not complete that request", Status: 500}
}

func currencyOr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "INR"
	}
	return strings.ToUpper(v)
}
