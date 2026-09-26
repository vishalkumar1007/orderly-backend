package storefront

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// nowFunc is swappable in tests.
var nowFunc = time.Now

// BaseURLResolver builds the public storefront origin for a tenant slug. The
// admin QR screen uses it to show customers the exact address they should open.
type BaseURLResolver func(slug string) string

// AdminHandler serves the tenant admin storefront configuration screens.
//
// Every handler takes the tenant from the verified staff token, never from a
// request parameter, so one shop can never read or write another's storefront.
type AdminHandler struct {
	pool   *pgxpool.Pool
	q      *sqlc.Queries
	loader *Loader
	reader *Catalog
	base   BaseURLResolver
}

// NewAdminHandler wires the tenant admin storefront handler.
func NewAdminHandler(pool *pgxpool.Pool, base BaseURLResolver) *AdminHandler {
	q := sqlc.New(pool)
	return &AdminHandler{
		pool:   pool,
		q:      q,
		loader: NewLoader(pool),
		reader: NewCatalog(q),
		base:   base,
	}
}

// tenantOf returns the caller's tenant id.
func tenantOf(r *http.Request) (uuid.UUID, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, false
	}
	return *user.TenantID, true
}

// load returns the tenant's storefront, creating it on first access from the
// tenant's own identity so a new shop is never left without a storefront.
func (a *AdminHandler) load(r *http.Request, tenantID uuid.UUID) (*Storefront, error) {
	tenant, err := a.q.GetTenantByID(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		return nil, err
	}
	return a.loader.Ensure(r.Context(), tenantID, Seed{
		Name:       tenant.Name,
		Phone:      tenant.Phone,
		Address:    tenant.Address,
		LogoURL:    tenant.LogoUrl,
		FaviconURL: tenant.FaviconUrl,
	})
}

// GetStorefront returns everything the admin storefront screens need: resolved
// configuration plus the platform catalogues they render pickers from.
func (a *AdminHandler) GetStorefront(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	response.JSON(w, http.StatusOK, a.payload(sf))
}

// payload is the tenant-private configuration document. It includes the theme
// catalogue and section catalogue so the admin UI needs one request to render.
func (a *AdminHandler) payload(sf *Storefront) map[string]any {
	status := sf.OpeningHours.Status(nowFunc())
	return map[string]any{
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
			"currency":      sf.Currency,
			"timezone":      sf.Timezone,
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
			"primary":        sf.Theme.Primary,
			"secondary":      sf.Theme.Secondary,
			"accent":         sf.Theme.Accent,
			"hero_image_url": sf.HeroImageURL,
			"vars":           sf.Theme.CSSVars(),
		},
		"behaviour": map[string]any{
			"ordering_enabled":       sf.OrderingEnabled,
			"closed_message":         sf.ClosedMessage,
			"customer_login_enabled": sf.CustomerLogin,
			"prep_time_minutes":      sf.PrepTimeMinutes,
			"tax_percent":            sf.TaxPercent,
			"packaging_fee":          sf.PackagingFee,
			"published":              sf.IsPublished,
			"store_status":           sf.StoreStatus,
		},
		"payments": map[string]any{
			"online_payment_enabled": sf.Payments.OnlineEnabled,
			"cash_enabled":           sf.Payments.CashEnabled,
			"pay_at_pickup_enabled":  sf.Payments.AtPickupEnable,
			"default_payment_method": sf.Payments.DefaultMethod,
			"methods":                sf.Payments.Methods(),
		},
		"workflow": map[string]any{
			"acceptance_mode":     sf.Workflow.AcceptanceMode,
			"payment_requirement": sf.Workflow.PaymentRequirement,
			"ready_notification":  sf.Workflow.ReadyNotification,
			"auto_complete":       sf.Workflow.AutoComplete,
		},
		"hours": map[string]any{
			"always_open":  sf.OpeningHours.AlwaysOpen,
			"timezone":     sf.OpeningHours.Timezone,
			"schedule":     sf.OpeningHours.Schedule,
			"days":         DayKeys(),
			"is_open":      status.Open,
			"label":        status.Label,
			"detail":       status.Detail,
			"today_closes": status.TodayCloses,
		},
		"homepage": map[string]any{"sections": sf.Homepage.Sections},
		"catalogues": map[string]any{
			"presets":       Presets(),
			"section_types": SectionTypes(),
			"days":          DayKeys(),
		},
		"ordering_available_now": sf.OrderingAllowed(),
		"closed_reason":          sf.ClosedReason(),
		"public_url":             a.base(sf.Slug),
		"updated_at":             sf.UpdatedAt.Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// Identity and behaviour
// ---------------------------------------------------------------------------

// PutStorefront updates store identity and ordering behaviour.
func (a *AdminHandler) PutStorefront(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		Name       *string `json:"name"`
		LogoURL    *string `json:"logo_url"`
		FaviconURL *string `json:"favicon_url"`
		Tagline    *string `json:"tagline"`
		About      *string `json:"description"`
		Phone      *string `json:"phone"`
		Address    *string `json:"address"`

		OrderingEnabled *bool   `json:"ordering_enabled"`
		ClosedMessage   *string `json:"closed_message"`
		CustomerLogin   *bool   `json:"customer_login_enabled"`
		PrepTimeMinutes *int    `json:"prep_time_minutes"`
		TaxPercent      *string `json:"tax_percent"`
		PackagingFee    *string `json:"packaging_fee"`
		Published       *bool   `json:"published"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those settings")
		return
	}

	if req.Name != nil {
		name := trimText(*req.Name, 80)
		if utf8Count(name) < 2 {
			writeAdminError(w, "invalid_name", "Your business name needs at least 2 characters")
			return
		}
		if _, err := a.q.UpdateStorefrontIdentity(r.Context(), sqlc.UpdateStorefrontIdentityParams{
			BusinessName: pgutil.Text(name),
			LogoUrl:      trimmedText(req.LogoURL, 500),
			FaviconUrl:   trimmedText(req.FaviconURL, 500),
			Tagline:      trimmedText(req.Tagline, 140),
			Description:  trimmedText(req.About, 500),
			Phone:        trimmedText(req.Phone, 40),
			Address:      trimmedText(req.Address, 300),
			TenantID:     pgutil.UUID(tenantID),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your details")
			return
		}
	}
	if req.OrderingEnabled != nil || req.ClosedMessage != nil || req.CustomerLogin != nil || req.PrepTimeMinutes != nil {
		minutes := sf.PrepTimeMinutes
		if req.PrepTimeMinutes != nil {
			if *req.PrepTimeMinutes < 0 || *req.PrepTimeMinutes > 240 {
				writeAdminError(w, "invalid_prep_time", "Preparation time must be between 0 and 240 minutes")
				return
			}
			minutes = *req.PrepTimeMinutes
		}
		if _, err := a.q.UpdateStorefrontBehaviour(r.Context(), sqlc.UpdateStorefrontBehaviourParams{
			OrderingEnabled:      boolPtr(req.OrderingEnabled),
			ClosedMessage:        trimmedText(req.ClosedMessage, 200),
			CustomerLoginEnabled: boolPtr(req.CustomerLogin),
			PrepTimeMinutes:      int32Ptr(&minutes),
			TenantID:             pgutil.UUID(tenantID),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your settings")
			return
		}
	}
	if req.TaxPercent != nil || req.PackagingFee != nil {
		tax, taxOK := parseRate(*req.TaxPercent)
		fee, feeOK := parseRate(*req.PackagingFee)
		if (req.TaxPercent != nil && !taxOK) || (req.PackagingFee != nil && !feeOK) {
			writeAdminError(w, "invalid_amount", "Tax and packaging fee must be plain numbers")
			return
		}
		taxParam, feeParam := numericOrNil(tax, sf.TaxPercent), numericOrNil(fee, sf.PackagingFee)
		if _, err := a.q.UpdateStorefrontCosting(r.Context(), sqlc.UpdateStorefrontCostingParams{
			TaxPercent:   taxParam,
			PackagingFee: feeParam,
			TenantID:     pgutil.UUID(tenantID),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your pricing")
			return
		}
	}
	if req.Published != nil {
		if _, err := a.q.SetStorefrontPublished(r.Context(), sqlc.SetStorefrontPublishedParams{
			IsPublished: *req.Published,
			ID:          pgutil.UUID(tenantID),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not change publish state")
			return
		}
	}
	a.respond(w, r, tenantID)
}

// PutTheme updates the controlled design tokens. Values are validated against
// the platform catalogue: an unknown preset, radius or component style is
// rejected rather than stored.
func (a *AdminHandler) PutTheme(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	if _, err := a.load(r, tenantID); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		Preset       *string `json:"preset"`
		Mode         *string `json:"mode"`
		Font         *string `json:"font"`
		Radius       *string `json:"radius"`
		Button       *string `json:"button"`
		Card         *string `json:"card"`
		Header       *string `json:"header"`
		Hero         *string `json:"hero"`
		Primary      *string `json:"primary"`
		Secondary    *string `json:"secondary"`
		Accent       *string `json:"accent"`
		HeroImageURL *string `json:"hero_image_url"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those settings")
		return
	}
	params := sqlc.UpdateStorefrontThemeParams{TenantID: pgutil.UUID(tenantID)}
	if req.Preset != nil {
		if !contains(validPresets, strings.ToLower(strings.TrimSpace(*req.Preset))) {
			writeAdminError(w, "invalid_preset", "Pick one of the available theme presets")
			return
		}
		params.ThemePreset = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Preset))))
	}
	if req.Mode != nil {
		if !contains(validModes, strings.ToLower(strings.TrimSpace(*req.Mode))) {
			writeAdminError(w, "invalid_mode", "Theme must be light, dark or system")
			return
		}
		params.ThemeMode = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Mode))))
	}
	if req.Font != nil {
		if !contains(validFonts, strings.ToLower(strings.TrimSpace(*req.Font))) {
			writeAdminError(w, "invalid_font", "Pick one of the available fonts")
			return
		}
		params.FontFamily = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Font))))
	}
	if req.Radius != nil {
		if !contains(validRadii, strings.ToLower(strings.TrimSpace(*req.Radius))) {
			writeAdminError(w, "invalid_radius", "Pick one of the available corner styles")
			return
		}
		params.Radius = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Radius))))
	}
	if req.Button != nil {
		if !contains(validButtons, strings.ToLower(strings.TrimSpace(*req.Button))) {
			writeAdminError(w, "invalid_button_style", "Pick one of the available button styles")
			return
		}
		params.ButtonStyle = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Button))))
	}
	if req.Card != nil {
		if !contains(validCards, strings.ToLower(strings.TrimSpace(*req.Card))) {
			writeAdminError(w, "invalid_card_style", "Pick one of the available card styles")
			return
		}
		params.CardStyle = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Card))))
	}
	if req.Header != nil {
		if !contains(validHeaders, strings.ToLower(strings.TrimSpace(*req.Header))) {
			writeAdminError(w, "invalid_header_style", "Pick one of the available header styles")
			return
		}
		params.HeaderStyle = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Header))))
	}
	if req.Hero != nil {
		if !contains(validHeros, strings.ToLower(strings.TrimSpace(*req.Hero))) {
			writeAdminError(w, "invalid_hero_style", "Pick one of the available hero styles")
			return
		}
		params.HeroStyle = pgutil.NullText(strPtr(strings.ToLower(strings.TrimSpace(*req.Hero))))
	}
	params.PrimaryColor = trimmedText(req.Primary, 20)
	params.SecondaryColor = trimmedText(req.Secondary, 20)
	params.AccentColor = trimmedText(req.Accent, 20)
	params.HeroImageUrl = trimmedText(req.HeroImageURL, 500)

	if _, err := a.q.UpdateStorefrontTheme(r.Context(), params); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your theme")
		return
	}
	a.respond(w, r, tenantID)
}

// PutHomepage replaces the homepage layout. Unknown section types and content
// keys are dropped, so a stale client can never store something the storefront
// cannot render.
func (a *AdminHandler) PutHomepage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	if _, err := a.load(r, tenantID); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		Sections []Section `json:"sections"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read that layout")
		return
	}
	normalised := NormalizeHomepage(Homepage{Sections: req.Sections})
	raw, err := json.Marshal(normalised)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save that layout")
		return
	}
	if _, err := a.q.UpdateStorefrontHomepage(r.Context(), sqlc.UpdateStorefrontHomepageParams{
		Homepage: raw,
		TenantID: pgutil.UUID(tenantID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your layout")
		return
	}
	a.respond(w, r, tenantID)
}

// PutPaymentSettings updates which payment methods customers may choose.
func (a *AdminHandler) PutPaymentSettings(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		OnlineEnabled  *bool   `json:"online_payment_enabled"`
		CashEnabled    *bool   `json:"cash_enabled"`
		AtPickupEnable *bool   `json:"pay_at_pickup_enabled"`
		DefaultMethod  *string `json:"default_payment_method"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those settings")
		return
	}
	next := PaymentSettings{
		OnlineEnabled:  sf.Payments.OnlineEnabled,
		CashEnabled:    sf.Payments.CashEnabled,
		AtPickupEnable: sf.Payments.AtPickupEnable,
		DefaultMethod:  sf.Payments.DefaultMethod,
	}
	if req.OnlineEnabled != nil {
		next.OnlineEnabled = *req.OnlineEnabled
	}
	if req.CashEnabled != nil {
		next.CashEnabled = *req.CashEnabled
	}
	if req.AtPickupEnable != nil {
		next.AtPickupEnable = *req.AtPickupEnable
	}
	if req.DefaultMethod != nil {
		method := strings.ToUpper(strings.TrimSpace(*req.DefaultMethod))
		if method != MethodOnline && method != MethodCash {
			writeAdminError(w, "invalid_payment_method", "Default payment must be online or cash")
			return
		}
		next.DefaultMethod = method
	}
	// Turning every method off would leave customers with no way to pay, so the
	// switch that would do it is refused with an explanation instead.
	if !next.OnlineEnabled && !next.CashEnabled {
		writeAdminError(w, "no_payment_method", "Keep at least one payment method switched on")
		return
	}
	if !next.Allows(next.DefaultMethod) {
		if next.OnlineEnabled {
			next.DefaultMethod = MethodOnline
		} else {
			next.DefaultMethod = MethodCash
		}
	}
	if _, err := a.q.UpdateStorefrontPayments(r.Context(), sqlc.UpdateStorefrontPaymentsParams{
		Payments: next.Marshal(),
		TenantID: pgutil.UUID(tenantID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your payment settings")
		return
	}
	a.respond(w, r, tenantID)
}

// PutOrderWorkflow updates the order pipeline behaviour. The state machine
// itself is fixed; only the four settings below can change.
func (a *AdminHandler) PutOrderWorkflow(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		AcceptanceMode     *string `json:"acceptance_mode"`
		PaymentRequirement *string `json:"payment_requirement"`
		ReadyNotification  *bool   `json:"ready_notification"`
		AutoComplete       *bool   `json:"auto_complete"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those settings")
		return
	}
	next := sf.Workflow
	if req.AcceptanceMode != nil {
		mode := strings.ToUpper(strings.TrimSpace(*req.AcceptanceMode))
		if !contains(validAccept, mode) {
			writeAdminError(w, "invalid_acceptance_mode", "Acceptance must be manual or automatic")
			return
		}
		next.AcceptanceMode = mode
	}
	if req.PaymentRequirement != nil {
		timing := strings.ToUpper(strings.TrimSpace(*req.PaymentRequirement))
		if !contains(validPayTiming, timing) {
			writeAdminError(w, "invalid_payment_requirement",
				"Payment timing must be before preparation or at pickup")
			return
		}
		next.PaymentRequirement = timing
	}
	if req.ReadyNotification != nil {
		next.ReadyNotification = *req.ReadyNotification
	}
	if req.AutoComplete != nil {
		next.AutoComplete = *req.AutoComplete
	}
	if _, err := a.q.UpdateStorefrontWorkflow(r.Context(), sqlc.UpdateStorefrontWorkflowParams{
		Workflow: next.Marshal(),
		TenantID: pgutil.UUID(tenantID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your workflow settings")
		return
	}
	a.respond(w, r, tenantID)
}

// PutOpeningHours updates the weekly schedule.
func (a *AdminHandler) PutOpeningHours(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your storefront")
		return
	}
	var req struct {
		AlwaysOpen *bool          `json:"always_open"`
		Timezone   *string        `json:"timezone"`
		Schedule   map[string]any `json:"schedule"`
	}
	if err := decodeAdmin(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those hours")
		return
	}
	next := sf.OpeningHours
	if req.AlwaysOpen != nil {
		next.AlwaysOpen = *req.AlwaysOpen
	}
	if req.Timezone != nil {
		tz := strings.TrimSpace(*req.Timezone)
		if tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				writeAdminError(w, "invalid_timezone", "That timezone is not recognised")
				return
			}
			next.Timezone = tz
		}
	}
	if req.Schedule != nil {
		next.Schedule = normaliseSchedule(req.Schedule)
	}
	raw, err := json.Marshal(map[string]any{
		"always_open": next.AlwaysOpen,
		"timezone":    next.Timezone,
		"schedule":    next.Schedule,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your hours")
		return
	}
	if _, err := a.q.UpdateStorefrontOpeningHours(r.Context(), sqlc.UpdateStorefrontOpeningHoursParams{
		OpeningHours: raw,
		TenantID:     pgutil.UUID(tenantID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your hours")
		return
	}
	a.respond(w, r, tenantID)
}

// respond reloads and returns the whole configuration, so a single save call
// leaves the admin UI in sync with the server.
func (a *AdminHandler) respond(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	sf, err := a.load(r, tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reload your storefront")
		return
	}
	response.JSON(w, http.StatusOK, a.payload(sf))
}

// PreviewMenu returns the menu as the storefront will render it, so the admin
// preview reflects live product data.
func (a *AdminHandler) PreviewMenu(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantOf(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	menu, err := a.reader.LoadMenu(r.Context(), tenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load your menu")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"categories": menu.Categories,
		"products":   menu.Products,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func decodeAdmin(r *http.Request, target any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func writeAdminError(w http.ResponseWriter, code, message string) {
	response.Error(w, http.StatusBadRequest, code, message)
}

func trimText(s string, max int) string {
	out := strings.TrimSpace(s)
	if utf8Count(out) > max {
		out = string([]rune(out)[:max])
	}
	return out
}

func utf8Count(s string) int { return len([]rune(s)) }

// trimmedText returns a nullable column value, or nil to leave the column
// unchanged. This is what makes every PUT a partial update.
func trimmedText(value *string, max int) nullableText {
	if value == nil {
		return nullText()
	}
	trimmed := trimText(*value, max)
	return pgutil.Text(trimmed)
}

func strPtr(s string) *string { return &s }

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// parseRate reads a money or percentage field that arrives as a JSON number or
// a JSON string, so both `12.5` and `"12.5"` are accepted.
func parseRate(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	if v < 0 || v > 1_000_000 {
		return 0, false
	}
	return v, true
}
