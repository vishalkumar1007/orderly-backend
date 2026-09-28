package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/brand"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Handler struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries

	// configs is the provider-configuration stack. It is attached during
	// construction by the HTTP server; a nil value only happens in unit tests
	// that exercise unrelated handlers.
	configs       *configsvc.Service
	secretBox     *secretbox.Box
	notifications *configsvc.Notifications
	log           *slog.Logger
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool, q: sqlc.New(pool)}
}

type createTenantRequest struct {
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	BusinessType     string          `json:"business_type"`
	OwnerName        string          `json:"owner_name"`
	Phone            string          `json:"phone"`
	Email            string          `json:"email"`
	Address          string          `json:"address"`
	Plan             string          `json:"plan"`
	ThemePresetID    string          `json:"theme_preset_id"`
	ThemeColorMode   string          `json:"theme_color_mode"`
	ThemeOverrides   json.RawMessage `json:"theme_overrides"`
	AdminName        string          `json:"admin_name"`
	AdminEmail       string          `json:"admin_email"`
	AdminPhone       string          `json:"admin_phone"`
	LogoURL          string          `json:"logo_url"`
	FaviconURL       string          `json:"favicon_url"`
	ShortDescription string          `json:"short_description"`
	Currency         string          `json:"currency"`
	Timezone         string          `json:"timezone"`
	Language         string          `json:"language"`
	StoreStatus      string          `json:"store_status"`
	StatusMessage    string          `json:"status_message"`

	// Configuration is the business-type template the Super Admin confirmed
	// during onboarding. It seeds the tenant's storefront row inside the same
	// transaction, so a new business arrives configured rather than blank.
	Configuration *tenantConfigurationRequest `json:"configuration"`
}

// tenantConfigurationRequest mirrors storefront.Defaults over the wire. Every
// field is optional; an omitted or unrecognised value leaves the storefront
// schema default in place.
type tenantConfigurationRequest struct {
	ThemePreset       string          `json:"theme_preset"`
	ThemeMode         string          `json:"theme_mode"`
	PrimaryColor      string          `json:"primary_color"`
	SecondaryColor    string          `json:"secondary_color"`
	AccentColor       string          `json:"accent_color"`
	FilterStyle       string          `json:"filter_style"`
	ProductLayout     string          `json:"product_layout"`
	HeroStyle         string          `json:"hero_style"`
	FontFamily        string          `json:"font_family"`
	Radius            string          `json:"radius"`
	CardStyle         string          `json:"card_style"`
	ButtonStyle       string          `json:"button_style"`
	OrderingEnabled   *bool           `json:"ordering_enabled"`
	CustomerLoginMode string          `json:"customer_login_mode"`
	PrepTimeMinutes   *int            `json:"prep_time_minutes"`
	Payments          json.RawMessage `json:"payments"`
	Workflow          json.RawMessage `json:"workflow"`
}

// defaults converts the request into the storefront package's own type. The
// validation lives there, beside the schema it has to satisfy.
func (c *tenantConfigurationRequest) defaults() storefront.Defaults {
	if c == nil {
		return storefront.Defaults{}
	}
	return storefront.Defaults{
		ThemePreset:       c.ThemePreset,
		ThemeMode:         c.ThemeMode,
		PrimaryColor:      c.PrimaryColor,
		SecondaryColor:    c.SecondaryColor,
		AccentColor:       c.AccentColor,
		FilterStyle:       c.FilterStyle,
		ProductLayout:     c.ProductLayout,
		HeroStyle:         c.HeroStyle,
		FontFamily:        c.FontFamily,
		Radius:            c.Radius,
		CardStyle:         c.CardStyle,
		ButtonStyle:       c.ButtonStyle,
		OrderingEnabled:   c.OrderingEnabled,
		CustomerLoginMode: c.CustomerLoginMode,
		PrepTimeMinutes:   c.PrepTimeMinutes,
		Payments:          c.Payments,
		Workflow:          c.Workflow,
	}
}

type updateTenantRequest struct {
	Name             *string `json:"name"`
	BusinessType     *string `json:"business_type"`
	OwnerName        *string `json:"owner_name"`
	Phone            *string `json:"phone"`
	Email            *string `json:"email"`
	Address          *string `json:"address"`
	LogoURL          *string `json:"logo_url"`
	FaviconURL       *string `json:"favicon_url"`
	ShortDescription *string `json:"short_description"`
	Currency         *string `json:"currency"`
	Timezone         *string `json:"timezone"`
	Language         *string `json:"language"`
	StoreStatus      *string `json:"store_status"`
	StatusMessage    *string `json:"status_message"`
}

func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := h.q.AdminDashboardStats(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load dashboard")
		return
	}
	ordersByDay, err := h.q.AdminOrdersByDay(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load order series")
		return
	}
	tenantsByWeek, err := h.q.AdminTenantsByWeek(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load tenant series")
		return
	}
	recentAudit, err := h.q.ListAuditLogsEnriched(ctx, 8)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load activity")
		return
	}
	activity := make([]any, 0, len(recentAudit))
	for _, a := range recentAudit {
		activity = append(activity, auditLogJSON(rowOf(a)))
	}
	orderSeries := make([]any, 0, len(ordersByDay))
	for _, row := range ordersByDay {
		orderSeries = append(orderSeries, map[string]any{
			"day":         pgDateString(row.Day),
			"order_count": row.OrderCount,
			"revenue":     row.Revenue,
		})
	}
	tenantSeries := make([]any, 0, len(tenantsByWeek))
	for _, row := range tenantsByWeek {
		tenantSeries = append(tenantSeries, map[string]any{
			"week_start":   pgDateString(row.WeekStart),
			"tenant_count": row.TenantCount,
		})
	}
	// "Needs attention" is answered by the database, not by the browser: a
	// client-side scan would need every subscription and every tenant on the
	// wire to find the handful that matter.
	expiring, err := h.q.ListExpiringSubscriptions(ctx, 14)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load expiring subscriptions")
		return
	}
	expiringOut := make([]any, 0, len(expiring))
	for _, row := range expiring {
		expiringOut = append(expiringOut, map[string]any{
			"subscription_id": pgutil.UUIDString(row.ID),
			"tenant_id":       pgutil.UUIDString(row.TenantID),
			"tenant_name":     row.TenantName,
			"tenant_slug":     row.TenantSlug,
			"plan":            row.PlanName,
			"status":          row.Status,
			"ends_at":         timestampOrEmpty(row.EndAt),
		})
	}

	awaiting, err := h.q.ListTenantsAwaitingSetup(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load pending setups")
		return
	}
	awaitingOut := make([]any, 0, len(awaiting))
	for _, row := range awaiting {
		awaitingOut = append(awaitingOut, map[string]any{
			"tenant_id":   pgutil.UUIDString(row.ID),
			"tenant_name": row.Name,
			"tenant_slug": row.Slug,
			"status":      row.Status,
			"created_at":  row.CreatedAt.Time.Format(time.RFC3339),
		})
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"expiring_subscriptions": expiringOut,
		"awaiting_setup":         awaitingOut,
		"total_tenants":          stats.TotalTenants,
		"active_tenants":         stats.ActiveTenants,
		"suspended_tenants":      stats.SuspendedTenants,
		"trial_tenants":          stats.TrialTenants,
		"total_orders":           stats.TotalOrders,
		"total_revenue":          stats.TotalRevenue,
		"orders_today":           stats.OrdersToday,
		"order_value_today":      stats.OrderValueToday,
		"active_users":           stats.ActiveUsers,
		"pending_setup":          stats.PendingSetup,
		"orders_by_day":          orderSeries,
		"tenants_by_week":        tenantSeries,
		"recent_activity":        activity,
	})
}

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListPlans(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list plans")
		return
	}
	out := make([]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, map[string]any{
			"id": pgutil.UUIDString(p.ID), "name": p.Name, "description": p.Description,
			"price": pgutil.NumericToFloat(p.Price), "max_staff": p.MaxStaff,
			"max_products": p.MaxProducts,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"plans": out})
}

func (h *Handler) ListTenants(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListTenantsWithPlans(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list tenants")
		return
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, tenantJSONEnriched(row.Tenant, row.PlanName, row.PlanPrice, row.ThemeName, row.ThemeTokens))
	}
	response.JSON(w, http.StatusOK, map[string]any{"tenants": out})
}

func (h *Handler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
	if req.Slug == "" {
		req.Slug = slugify(req.Name)
	}
	req.BusinessType = normalizeTypeCode(req.BusinessType)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.AdminEmail = strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if req.AdminEmail == "" {
		req.AdminEmail = req.Email
	}
	if req.AdminName == "" {
		req.AdminName = req.OwnerName
	}
	if req.Plan == "" {
		req.Plan = "TRIAL"
	}
	req.Plan = strings.ToUpper(req.Plan)

	if req.Name == "" || req.Slug == "" || req.OwnerName == "" || req.AdminEmail == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name, slug, owner_name, and admin email required")
		return
	}
	if code, errCode, msg := slugValidationError(req.Slug); code != 0 {
		response.Error(w, code, errCode, msg)
		return
	}
	if err := h.requireActiveType(r.Context(), req.BusinessType); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid business_type")
		return
	}
	presetID, mode, overrides, err := h.normalizeTheme(r.Context(), themeWrite{
		ThemePresetID: req.ThemePresetID, ThemeColorMode: req.ThemeColorMode, ThemeOverrides: req.ThemeOverrides,
	})
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid theme")
		return
	}

	plan, err := h.q.GetPlanByName(r.Context(), req.Plan)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "unknown plan")
		return
	}

	// Storefront defaults — normalise with safe fallbacks.
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "INR"
	}
	storeStatus := strings.ToUpper(strings.TrimSpace(req.StoreStatus))
	switch storeStatus {
	case "OPEN", "BUSY", "AWAY", "CLOSED":
		// valid
	default:
		storeStatus = "OPEN"
	}
	adminPhone := strings.TrimSpace(req.AdminPhone)
	if adminPhone == "" {
		adminPhone = strings.TrimSpace(req.Phone)
	}
	req.AdminName = strings.TrimSpace(req.AdminName)
	req.LogoURL = strings.TrimSpace(req.LogoURL)
	req.FaviconURL = strings.TrimSpace(req.FaviconURL)
	req.ShortDescription = strings.TrimSpace(req.ShortDescription)
	req.Timezone = strings.TrimSpace(req.Timezone)
	req.Language = strings.TrimSpace(req.Language)

	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create invite")
		return
	}
	// Unusable password until invite completed
	placeholderHash, _ := bcrypt.GenerateFromPassword([]byte(uuid.NewString()), bcrypt.DefaultCost)

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "transaction failed")
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	tenant, err := qtx.CreateTenant(ctx, sqlc.CreateTenantParams{
		Name: req.Name, Slug: req.Slug, BusinessType: req.BusinessType,
		OwnerName: req.OwnerName, Phone: req.Phone, Email: req.Email,
		Address: req.Address, Status: "ACTIVE", PlanID: plan.ID, SetupStatus: "PENDING",
		ThemePresetID: presetID, ThemeColorMode: mode, ThemeOverrides: overrides,
		LogoUrl: req.LogoURL, FaviconUrl: req.FaviconURL,
		ShortDescription: req.ShortDescription, Currency: currency,
		Timezone: req.Timezone, Language: req.Language, StoreStatus: storeStatus,
		StatusMessage: "",
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "tenants_slug") {
			response.Error(w, http.StatusConflict, "slug_taken", "slug is already in use")
			return
		}
		response.Error(w, http.StatusConflict, "conflict", "could not create tenant")
		return
	}

	subStatus := "TRIAL"
	if req.Plan != "TRIAL" {
		subStatus = "ACTIVE"
	}
	endAt := pgtype.Timestamptz{Time: time.Now().Add(14 * 24 * time.Hour), Valid: true}
	_, err = qtx.CreateSubscription(ctx, sqlc.CreateSubscriptionParams{
		TenantID: tenant.ID, PlanID: plan.ID, Status: subStatus,
		StartAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, EndAt: endAt,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create subscription")
		return
	}

	_ = qtx.EnsureOrderCounter(ctx, tenant.ID)

	// Seed the storefront from the chosen business type. This runs inside the
	// transaction: a business that cannot be configured is not created, rather
	// than created and left half-set-up.
	if err := storefront.Provision(ctx, qtx, uuid.UUID(tenant.ID.Bytes), storefront.Seed{
		Name:       req.Name,
		Phone:      req.Phone,
		Address:    req.Address,
		LogoURL:    req.LogoURL,
		FaviconURL: req.FaviconURL,
	}, req.Configuration.defaults()); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to configure the storefront")
		return
	}

	admin, err := qtx.CreateUser(ctx, sqlc.CreateUserParams{
		TenantID: tenant.ID, Name: req.AdminName, Email: req.AdminEmail, Phone: adminPhone,
		PasswordHash: string(placeholderHash), Role: identity.RoleTenantAdmin, Status: "ACTIVE",
		MustSetPassword: true,
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusConflict, "conflict", "admin email already exists")
		return
	}

	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, qtx, &tenant.ID, &actor.ID, "Business Created", "tenant", tenant.ID)
	_ = insertAudit(ctx, qtx, &tenant.ID, &actor.ID, "Admin Created", "user", admin.ID)

	if err := tx.Commit(ctx); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "commit failed")
		return
	}

	port := frontendPort()
	base := "http://" + tenant.Slug + ".localhost:" + port
	setupPath := "/setup-password?token=" + inviteToken
	emailSent, emailErr := h.deliverInvite(r.Context(), admin.Email, tenant.Name, base+setupPath)
	enriched, err := h.q.GetTenantWithPlanByID(r.Context(), tenant.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "tenant created but failed to load")
		return
	}
	response.JSON(w, http.StatusCreated, map[string]any{
		"tenant":       tenantJSONFromRow(enriched),
		"admin_email":  admin.Email,
		"invite_token": inviteToken,
		"setup_path":   setupPath,
		"tenant_url":   base,
		"login_url":    base + "/login",
		"email_sent":   emailSent,
		"email_error":  emailErr,
	})
}

func (h *Handler) GetTenant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	row, err := h.q.GetTenantWithPlanByID(r.Context(), pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	sub, _ := h.q.GetSubscriptionByTenant(r.Context(), row.Tenant.ID)
	out := tenantJSONFromRow(row)
	if sub.ID.Valid {
		out["subscription"] = subscriptionJSON(sub, row.PlanName)
	}
	response.JSON(w, http.StatusOK, out)
}

func (h *Handler) TenantMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	tenantUUID := pgutil.UUID(id)
	ctx := r.Context()
	metrics, err := h.q.TenantAdminMetrics(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load metrics")
		return
	}
	ordersByDay, err := h.q.TenantOrdersByDay(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load order series")
		return
	}
	statusRows, err := h.q.TenantOrderStatusBreakdown(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load status breakdown")
		return
	}
	security, err := h.q.TenantSecuritySummary(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load security summary")
		return
	}
	row, err := h.q.GetTenantWithPlanByID(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	sub, _ := h.q.GetSubscriptionByTenant(ctx, tenantUUID)
	series := make([]any, 0, len(ordersByDay))
	for _, s := range ordersByDay {
		series = append(series, map[string]any{
			"day":         pgDateString(s.Day),
			"order_count": s.OrderCount,
			"revenue":     s.Revenue,
		})
	}
	breakdown := make([]any, 0, len(statusRows))
	for _, r := range statusRows {
		breakdown = append(breakdown, map[string]any{
			"status": r.Status,
			"count":  r.Count,
		})
	}
	planName := textOrEmpty(row.PlanName)
	out := map[string]any{
		"orders":           metrics.TotalOrders,
		"revenue":          metrics.TotalRevenue,
		"active_users":     metrics.ActiveUsers,
		"current_plan":     planName,
		"orders_today":     metrics.OrdersToday,
		"revenue_today":    metrics.RevenueToday,
		"cancelled_orders": metrics.CancelledOrders,
		"avg_order_value":  metrics.AvgOrderValue,
		"first_order_at":   timestampOrEmpty(metrics.FirstOrderAt),
		"last_order_at":    timestampOrEmpty(metrics.LastOrderAt),
		"status_breakdown": breakdown,
		"orders_by_day":    series,
		"security": map[string]any{
			"users_total":            security.UsersTotal,
			"users_active":           security.UsersActive,
			"users_pending_password": security.UsersPendingPassword,
			"active_sessions":        security.ActiveSessions,
			"audit_events":           security.AuditEvents,
			"audit_events_7d":        security.AuditEvents7d,
		},
	}
	if sub.ID.Valid {
		out["subscription_status"] = sub.Status
		out["subscription"] = subscriptionJSON(sub, row.PlanName)
	}
	response.JSON(w, http.StatusOK, out)
}

type changePlanRequest struct {
	Plan string `json:"plan"`
}

func (h *Handler) ChangeTenantPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	var req changePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Plan = strings.ToUpper(strings.TrimSpace(req.Plan))
	if req.Plan == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "plan required")
		return
	}
	plan, err := h.q.GetPlanByName(r.Context(), req.Plan)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "unknown plan")
		return
	}
	tenantUUID := pgutil.UUID(id)
	sub, err := h.q.GetSubscriptionByTenant(r.Context(), tenantUUID)
	if err != nil || !sub.ID.Valid {
		response.Error(w, http.StatusNotFound, "not_found", "subscription not found")
		return
	}
	subStatus := "TRIAL"
	if req.Plan != "TRIAL" {
		subStatus = "ACTIVE"
	}
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "transaction failed")
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)
	if _, err := qtx.UpdateTenantPlanID(ctx, sqlc.UpdateTenantPlanIDParams{
		ID: tenantUUID, PlanID: plan.ID,
	}); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	if _, err := qtx.UpdateSubscriptionPlan(ctx, sqlc.UpdateSubscriptionPlanParams{
		ID: sub.ID, PlanID: plan.ID, Status: subStatus,
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update subscription")
		return
	}
	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, qtx, &tenantUUID, &actor.ID, "Plan Changed to "+plan.Name, "tenant", tenantUUID)
	if err := tx.Commit(ctx); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "commit failed")
		return
	}
	row, err := h.q.GetTenantWithPlanByID(ctx, tenantUUID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load tenant")
		return
	}
	response.JSON(w, http.StatusOK, tenantJSONFromRow(row))
}

func (h *Handler) UpdateTenant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	var req updateTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateTenantParams{ID: pgutil.UUID(id)}
	if req.Name != nil {
		params.Name = pgtype.Text{String: *req.Name, Valid: true}
	}
	if req.BusinessType != nil {
		bt := normalizeTypeCode(*req.BusinessType)
		current, curErr := h.q.GetTenantByID(r.Context(), pgutil.UUID(id))
		if curErr != nil {
			response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}
		if bt != current.BusinessType {
			if err := h.requireActiveType(r.Context(), bt); err != nil {
				response.Error(w, http.StatusBadRequest, "invalid_request", "invalid business_type")
				return
			}
		}
		params.BusinessType = pgtype.Text{String: bt, Valid: true}
	}
	if req.OwnerName != nil {
		params.OwnerName = pgtype.Text{String: *req.OwnerName, Valid: true}
	}
	if req.Phone != nil {
		params.Phone = pgtype.Text{String: *req.Phone, Valid: true}
	}
	if req.Email != nil {
		params.Email = pgtype.Text{String: strings.ToLower(*req.Email), Valid: true}
	}
	if req.Address != nil {
		params.Address = pgtype.Text{String: *req.Address, Valid: true}
	}
	if req.LogoURL != nil {
		params.LogoUrl = pgtype.Text{String: strings.TrimSpace(*req.LogoURL), Valid: true}
	}
	if req.FaviconURL != nil {
		params.FaviconUrl = pgtype.Text{String: strings.TrimSpace(*req.FaviconURL), Valid: true}
	}
	if req.ShortDescription != nil {
		params.ShortDescription = pgtype.Text{String: strings.TrimSpace(*req.ShortDescription), Valid: true}
	}
	if req.Currency != nil {
		params.Currency = pgtype.Text{String: strings.ToUpper(strings.TrimSpace(*req.Currency)), Valid: true}
	}
	if req.Timezone != nil {
		params.Timezone = pgtype.Text{String: strings.TrimSpace(*req.Timezone), Valid: true}
	}
	if req.Language != nil {
		params.Language = pgtype.Text{String: strings.TrimSpace(*req.Language), Valid: true}
	}
	if req.StoreStatus != nil {
		st := strings.ToUpper(strings.TrimSpace(*req.StoreStatus))
		switch st {
		case "OPEN", "BUSY", "AWAY", "CLOSED":
			// valid
		default:
			st = "OPEN"
		}
		params.StoreStatus = pgtype.Text{String: st, Valid: true}
	}
	if req.StatusMessage != nil {
		msg := strings.TrimSpace(*req.StatusMessage)
		if len([]rune(msg)) > 200 {
			msg = string([]rune(msg)[:200])
		}
		params.StatusMessage = pgtype.Text{String: msg, Valid: true}
	}
	if _, err := h.q.UpdateTenant(r.Context(), params); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	tenantUUID := pgutil.UUID(id)
	_ = insertAudit(r.Context(), h.q, &tenantUUID, &actor.ID, "Business Updated", "tenant", tenantUUID)
	row, err := h.q.GetTenantWithPlanByID(r.Context(), pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	response.JSON(w, http.StatusOK, tenantJSONFromRow(row))
}

func (h *Handler) ActivateTenant(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, "ACTIVE")
}

func (h *Handler) SuspendTenant(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, "SUSPENDED")
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	tenantUUID := pgutil.UUID(id)
	ctx := r.Context()
	tenant, err := h.q.SetTenantStatus(ctx, sqlc.SetTenantStatusParams{
		ID: tenantUUID, Status: status,
	})
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	action := "Tenant Activated"
	if status == "SUSPENDED" {
		action = "Tenant Suspended"
	}
	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, h.q, &tenantUUID, &actor.ID, action, "tenant", tenant.ID)
	row, err := h.q.GetTenantWithPlanByID(ctx, tenant.ID)
	if err != nil {
		response.JSON(w, http.StatusOK, tenantJSON(tenant))
		return
	}
	response.JSON(w, http.StatusOK, tenantJSONFromRow(row))
}

func (h *Handler) ListTenantAdmins(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	rows, err := h.q.ListTenantAdmins(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list admins")
		return
	}
	out := make([]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, map[string]any{
			"id": pgutil.UUIDString(u.ID), "name": u.Name, "email": u.Email,
			"role": u.Role, "status": u.Status, "must_set_password": u.MustSetPassword,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"admins": out})
}

func (h *Handler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := int32(100)
	tenantFilter := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	resultFilter := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("result")))
	out := []any{}

	switch {
	case tenantFilter != "" && resultFilter != "":
		tid, err := uuid.Parse(tenantFilter)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant_id")
			return
		}
		rows, err := h.q.ListAuditLogsByTenantAndResult(ctx, sqlc.ListAuditLogsByTenantAndResultParams{
			TenantID: pgutil.UUID(tid), Result: resultFilter, RowLimit: limit,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list audit logs")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOfTenantResult(a)))
		}

	case tenantFilter != "":
		tid, err := uuid.Parse(tenantFilter)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant_id")
			return
		}
		rows, err := h.q.ListAuditLogsEnrichedByTenant(ctx, sqlc.ListAuditLogsEnrichedByTenantParams{
			TenantID: pgutil.UUID(tid), RowLimit: limit,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list audit logs")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOfTenant(a)))
		}

	case resultFilter != "":
		rows, err := h.q.ListAuditLogsByResult(ctx, sqlc.ListAuditLogsByResultParams{
			Result: resultFilter, RowLimit: limit,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list audit logs")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOfResult(a)))
		}

	default:
		rows, err := h.q.ListAuditLogsEnriched(ctx, limit)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list audit logs")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOf(a)))
		}
	}

	response.JSON(w, http.StatusOK, map[string]any{"audit_logs": out})
}

func (h *Handler) ListPlatformUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListPlatformUsers(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list users")
		return
	}
	out := make([]any, 0, len(rows))
	for _, u := range rows {
		// A user who has never set a password is invited, not active. The
		// distinction matters on this page: an "active" row nobody can sign in
		// as is the thing an operator most often needs to chase.
		status := u.Status
		if u.MustSetPassword {
			status = "INVITED"
		}
		row := map[string]any{
			"id":            pgutil.UUIDString(u.ID),
			"name":          u.Name,
			"email":         u.Email,
			"phone":         u.Phone,
			"role":          u.Role,
			"status":        status,
			"scope":         "TENANT",
			"tenant_id":     pgutil.UUIDPtr(u.TenantID),
			"tenant_name":   textOrEmpty(u.TenantName),
			"tenant_slug":   textOrEmpty(u.TenantSlug),
			"tenant_status": textOrEmpty(u.TenantStatus),
			"last_activity": nil,
			"created_at":    u.CreatedAt.Time.Format(time.RFC3339),
		}
		if !u.TenantID.Valid {
			row["scope"] = "PLATFORM"
		}
		if ts := timestampOrEmpty(u.LastActivity); ts != "" {
			row["last_activity"] = ts
		}
		out = append(out, row)
	}
	response.JSON(w, http.StatusOK, map[string]any{"users": out})
}

func (h *Handler) ListSubscriptions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListSubscriptionsAdmin(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list subscriptions")
		return
	}
	out := make([]any, 0, len(rows))
	for _, s := range rows {
		var trialEnd, renewal *string
		if s.EndAt.Valid {
			end := s.EndAt.Time.Format(time.RFC3339)
			if s.Status == "TRIAL" {
				trialEnd = &end
			}
			renewal = &end
		}
		out = append(out, map[string]any{
			"id":           pgutil.UUIDString(s.ID),
			"tenant_id":    pgutil.UUIDString(s.TenantID),
			"tenant_name":  s.TenantName,
			"plan_code":    s.PlanName,
			"plan_name":    s.PlanName,
			"status":       s.Status,
			"trial_end":    trialEnd,
			"start_date":   s.StartAt.Time.Format(time.RFC3339),
			"renewal_date": renewal,
			"price":        pgutil.NumericToFloat(s.PlanPrice),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

func (h *Handler) ResendTenantInvite(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	ctx := r.Context()
	row, err := h.q.GetTenantWithPlanByID(ctx, pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	if row.Tenant.SetupStatus == "COMPLETED" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "tenant setup already completed")
		return
	}
	admin, err := h.q.GetTenantAdminForTenant(ctx, row.Tenant.ID)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant admin not found")
		return
	}
	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create invite")
		return
	}
	updated, err := h.q.UpdateUserInviteToken(ctx, sqlc.UpdateUserInviteTokenParams{
		ID:              admin.ID,
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update invite")
		return
	}
	port := frontendPort()
	base := "http://" + row.Tenant.Slug + ".localhost:" + port
	setupPath := "/setup-password?token=" + inviteToken
	setupURL := base + setupPath
	emailSent, emailErr := h.deliverInvite(ctx, updated.Email, row.Tenant.Name, setupURL)
	response.JSON(w, http.StatusOK, map[string]any{
		"admin_email":  updated.Email,
		"invite_token": inviteToken,
		"setup_path":   setupPath,
		"setup_url":    setupURL,
		"login_url":    base + "/login",
		"tenant_url":   base,
		"email_sent":   emailSent,
		"email_error":  emailErr,
	})
}

func frontendPort() string {
	if p := strings.TrimSpace(os.Getenv("FRONTEND_PORT")); p != "" {
		return p
	}
	return "5173"
}

func tenantJSON(t sqlc.Tenant) map[string]any {
	return tenantJSONEnriched(t, pgtype.Text{}, pgtype.Numeric{}, pgtype.Text{}, nil)
}

func tenantJSONEnriched(t sqlc.Tenant, planName pgtype.Text, planPrice pgtype.Numeric, themeName pgtype.Text, themeTokens []byte) map[string]any {
	out := map[string]any{
		"id": pgutil.UUIDString(t.ID), "name": t.Name, "slug": t.Slug,
		"business_type": t.BusinessType, "owner_name": t.OwnerName,
		"phone": t.Phone, "email": t.Email, "address": t.Address,
		"status": t.Status, "setup_status": t.SetupStatus, "is_published": t.IsPublished,
		"plan_id":           pgutil.UUIDPtr(t.PlanID),
		"created_at":        t.CreatedAt.Time.Format(time.RFC3339),
		"updated_at":        t.UpdatedAt.Time.Format(time.RFC3339),
		"public_host":       t.Slug + ".localhost:" + frontendPort(),
		"theme_preset_id":   t.ThemePresetID,
		"theme_color_mode":  t.ThemeColorMode,
		"theme":             brand.Payload(t.ThemePresetID, textOrEmpty(themeName), t.ThemeColorMode, themeTokens, t.ThemeOverrides),
		"logo_url":          t.LogoUrl,
		"favicon_url":       t.FaviconUrl,
		"short_description": t.ShortDescription,
		"currency":          t.Currency,
		"timezone":          t.Timezone,
		"language":          t.Language,
		"store_status":      t.StoreStatus,
		"status_message":    t.StatusMessage,
		"shop_type":         t.ShopType,
	}
	if planName.Valid {
		out["plan"] = planName.String
	}
	if planPrice.Valid {
		out["plan_price"] = pgutil.NumericToFloat(planPrice)
	}
	return out
}

func subscriptionJSON(sub sqlc.Subscription, planName pgtype.Text) map[string]any {
	out := map[string]any{
		"id":      pgutil.UUIDString(sub.ID),
		"status":  sub.Status,
		"plan_id": pgutil.UUIDString(sub.PlanID),
	}
	if planName.Valid {
		out["plan_name"] = planName.String
		out["plan_code"] = planName.String
	}
	if sub.StartAt.Valid {
		out["start_date"] = sub.StartAt.Time.Format(time.RFC3339)
	}
	if sub.EndAt.Valid {
		end := sub.EndAt.Time.Format(time.RFC3339)
		out["end_at"] = end
		if sub.Status == "TRIAL" {
			out["trial_end"] = end
		}
		out["renewal_date"] = end
	}
	return out
}

// auditRow is the shape shared by every enriched audit query. sqlc emits a
// distinct struct per query, so we normalise into this before serialising.
type auditRow struct {
	ID         pgtype.UUID
	Action     string
	EntityType string
	EntityID   pgtype.UUID
	Result     string
	Metadata   []byte
	CreatedAt  pgtype.Timestamptz
	TenantID   pgtype.UUID
	ActorName  pgtype.Text
	ActorEmail pgtype.Text
	TenantName pgtype.Text
}

func rowOf(a sqlc.ListAuditLogsEnrichedRow) auditRow {
	return auditRow{a.ID, a.Action, a.EntityType, a.EntityID, a.Result, a.Metadata,
		a.CreatedAt, a.TenantID, a.ActorName, a.ActorEmail, a.TenantName}
}

func rowOfTenant(a sqlc.ListAuditLogsEnrichedByTenantRow) auditRow {
	return auditRow{a.ID, a.Action, a.EntityType, a.EntityID, a.Result, a.Metadata,
		a.CreatedAt, a.TenantID, a.ActorName, a.ActorEmail, a.TenantName}
}

func rowOfResult(a sqlc.ListAuditLogsByResultRow) auditRow {
	return auditRow{a.ID, a.Action, a.EntityType, a.EntityID, a.Result, a.Metadata,
		a.CreatedAt, a.TenantID, a.ActorName, a.ActorEmail, a.TenantName}
}

func rowOfTenantResult(a sqlc.ListAuditLogsByTenantAndResultRow) auditRow {
	return auditRow{a.ID, a.Action, a.EntityType, a.EntityID, a.Result, a.Metadata,
		a.CreatedAt, a.TenantID, a.ActorName, a.ActorEmail, a.TenantName}
}

func auditLogJSON(a auditRow) map[string]any {
	return map[string]any{
		"id":          pgutil.UUIDString(a.ID),
		"timestamp":   a.CreatedAt.Time.Format(time.RFC3339),
		"created_at":  a.CreatedAt.Time.Format(time.RFC3339),
		"action":      a.Action,
		"actor":       textOrEmpty(a.ActorName),
		"actor_email": textOrEmpty(a.ActorEmail),
		"resource":    a.EntityType,
		"resource_id": pgutil.UUIDPtr(a.EntityID),
		"tenant":      textOrEmpty(a.TenantName),
		"tenant_id":   pgutil.UUIDPtr(a.TenantID),
		"result":      a.Result,
		"metadata":    auditMetadata(a.Metadata),
	}
}

// auditMetadata decodes an audit metadata blob for display, tolerating junk.
func auditMetadata(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil
	}
	return v
}

func textOrEmpty(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

func timestampOrEmpty(t interface{}) string {
	switch v := t.(type) {
	case time.Time:
		if v.IsZero() {
			return ""
		}
		return v.Format(time.RFC3339)
	case *time.Time:
		if v == nil || v.IsZero() {
			return ""
		}
		return v.Format(time.RFC3339)
	case pgtype.Timestamptz:
		if !v.Valid || v.Time.IsZero() {
			return ""
		}
		return v.Time.Format(time.RFC3339)
	case nil:
		return ""
	default:
		return ""
	}
}

func pgDateString(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Audit results recorded on every event.
const (
	auditSuccess = "SUCCESS"
	auditFailure = "FAILURE"
	auditDenied  = "DENIED"
)

func insertAudit(ctx context.Context, q *sqlc.Queries, tenantID *pgtype.UUID, userID *uuid.UUID, action, entityType string, entityID pgtype.UUID) error {
	return insertAuditResult(ctx, q, tenantID, userID, action, entityType, entityID, auditSuccess, "{}")
}

func insertAuditResult(
	ctx context.Context,
	q *sqlc.Queries,
	tenantID *pgtype.UUID,
	userID *uuid.UUID,
	action, entityType string,
	entityID pgtype.UUID,
	result, metadata string,
) error {
	var tid, uid pgtype.UUID
	if tenantID != nil {
		tid = *tenantID
	}
	if userID != nil {
		uid = pgutil.UUID(*userID)
	}
	if result == "" {
		result = auditSuccess
	}
	if metadata == "" {
		metadata = "{}"
	}
	_, err := q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		TenantID: tid, UserID: uid, Action: action, EntityType: entityType,
		EntityID: entityID, Result: result, Metadata: []byte(metadata),
	})
	return err
}
