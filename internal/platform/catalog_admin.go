package platform

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/brand"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/* ------------------------------------------------------------------ *
 * Plans — the catalog the onboarding picker draws from.
 * ------------------------------------------------------------------ */

type planWriteRequest struct {
	Name        *string  `json:"name"`
	Description *string  `json:"description"`
	Price       *float64 `json:"price"`
	MaxStaff    *int     `json:"max_staff"`
	MaxProducts *int     `json:"max_products"`
	IsActive    *bool    `json:"is_active"`

	// Commercial terms. The plans table has columns for price and the two hard
	// limits only, so everything else an offer needs lives in the features
	// document: how often it bills, how long the trial runs, what is included,
	// and which business types may buy it.
	BillingPeriod *string   `json:"billing_period"`
	TrialDays     *int      `json:"trial_days"`
	Features      *[]string `json:"features"`
	BusinessTypes *[]string `json:"business_types"`
}

// planTerms is the parsed shape of plans.features.
//
// It is deliberately a closed struct rather than a free map: the console reads
// these four fields, and an unrecognised key in an old row should not survive a
// round trip and become a de-facto schema.
type planTerms struct {
	BillingPeriod string   `json:"billing_period"`
	TrialDays     int      `json:"trial_days"`
	Features      []string `json:"features"`
	// Empty means the plan is offered to every business type.
	BusinessTypes []string `json:"business_types"`
}

// validBillingPeriods is the closed set the console offers. Billing is not
// charged anywhere in this build; the period describes the offer, and the
// subscription's end date is derived from it at provisioning time.
var validBillingPeriods = map[string]bool{
	"trial": true, "monthly": true, "yearly": true, "one_time": true,
}

// parsePlanTerms reads the features document, tolerating the legacy shape
// ({"trial_days": 14}) and a document that was never written at all.
func parsePlanTerms(raw []byte, planName string, price float64) planTerms {
	terms := planTerms{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &terms)
	}
	if !validBillingPeriods[terms.BillingPeriod] {
		// Infer from what the row already says rather than inventing a period:
		// a free plan named TRIAL is a trial, a priced plan bills monthly.
		switch {
		case strings.EqualFold(planName, "TRIAL"):
			terms.BillingPeriod = "trial"
		case price == 0:
			terms.BillingPeriod = "one_time"
		default:
			terms.BillingPeriod = "monthly"
		}
	}
	if terms.TrialDays < 0 {
		terms.TrialDays = 0
	}
	if terms.BillingPeriod == "trial" && terms.TrialDays == 0 {
		terms.TrialDays = defaultTrialDays
	}
	if terms.Features == nil {
		terms.Features = []string{}
	}
	if terms.BusinessTypes == nil {
		terms.BusinessTypes = []string{}
	}
	return terms
}

// planOffersBusinessType mirrors the console's offeredTo(): a plan with no
// business types listed is offered to every business type, so the
// server-side check matches what the picker already filtered to.
func planOffersBusinessType(plan sqlc.Plan, businessType string) bool {
	terms := parsePlanTerms(plan.Features, plan.Name, pgutil.NumericToFloat(plan.Price))
	if businessType == "" || len(terms.BusinessTypes) == 0 {
		return true
	}
	for _, bt := range terms.BusinessTypes {
		if strings.EqualFold(bt, businessType) {
			return true
		}
	}
	return false
}

// defaultTrialDays matches the window CreateTenant writes onto a new
// subscription when the plan does not state its own.
const defaultTrialDays = 14

// mergePlanTerms applies a write request onto the stored terms. A field the
// request omits keeps its stored value, so a form that edits only the feature
// list cannot silently reset the billing period.
func mergePlanTerms(current planTerms, req planWriteRequest) (planTerms, error) {
	next := current
	if req.BillingPeriod != nil {
		period := strings.ToLower(strings.TrimSpace(*req.BillingPeriod))
		if !validBillingPeriods[period] {
			return next, errors.New("billing_period must be trial, monthly, yearly or one_time")
		}
		next.BillingPeriod = period
	}
	if req.TrialDays != nil {
		if *req.TrialDays < 0 || *req.TrialDays > 365 {
			return next, errors.New("trial_days must be between 0 and 365")
		}
		next.TrialDays = *req.TrialDays
	}
	if req.Features != nil {
		next.Features = cleanStrings(*req.Features, 24, 120)
	}
	if req.BusinessTypes != nil {
		codes := cleanStrings(*req.BusinessTypes, 24, 32)
		for i, c := range codes {
			codes[i] = normalizeTypeCode(c)
		}
		next.BusinessTypes = codes
	}
	if next.Features == nil {
		next.Features = []string{}
	}
	if next.BusinessTypes == nil {
		next.BusinessTypes = []string{}
	}
	return next, nil
}

// cleanStrings trims, drops blanks and duplicates, and bounds both the list and
// each entry so a plan document cannot grow without limit.
func cleanStrings(in []string, maxItems, maxLen int) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		if len([]rune(v)) > maxLen {
			v = string([]rune(v)[:maxLen])
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

func planJSON(p sqlc.Plan) map[string]any {
	return planJSONWithUsage(p, -1, -1)
}

// planJSONWithUsage renders a plan. Negative counts mean "not counted", which
// keeps the single-plan responses free of a query they do not need.
func planJSONWithUsage(p sqlc.Plan, tenantCount, activeSubscriptions int64) map[string]any {
	price := pgutil.NumericToFloat(p.Price)
	terms := parsePlanTerms(p.Features, p.Name, price)
	out := map[string]any{
		"id":             pgutil.UUIDString(p.ID),
		"name":           p.Name,
		"description":    p.Description,
		"price":          price,
		"max_staff":      p.MaxStaff,
		"max_products":   p.MaxProducts,
		"is_active":      p.IsActive,
		"billing_period": terms.BillingPeriod,
		"trial_days":     terms.TrialDays,
		"features":       terms.Features,
		"business_types": terms.BusinessTypes,
	}
	if tenantCount >= 0 {
		out["tenant_count"] = tenantCount
	}
	if activeSubscriptions >= 0 {
		out["active_subscriptions"] = activeSubscriptions
	}
	return out
}

// ListAllPlans includes deactivated plans so the console can manage them, and
// carries how many businesses are on each one — deactivating a plan that
// tenants are still subscribed to is a decision, not an accident.
func (h *Handler) ListAllPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListPlansWithUsage(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list plans")
		return
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		plan := sqlc.Plan{
			ID: row.ID, Name: row.Name, Description: row.Description, Price: row.Price,
			MaxStaff: row.MaxStaff, MaxProducts: row.MaxProducts, Features: row.Features,
			IsActive: row.IsActive, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}
		out = append(out, planJSONWithUsage(plan, row.TenantCount, row.ActiveSubscriptions))
	}
	response.JSON(w, http.StatusOK, map[string]any{"plans": out})
}

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	var req planWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	name := strings.ToUpper(strings.TrimSpace(deref(req.Name)))
	if name == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "plan name is required")
		return
	}
	if _, err := h.q.GetPlanByNameAny(r.Context(), name); err == nil {
		response.Error(w, http.StatusConflict, "duplicate", "a plan with that name already exists")
		return
	}
	if req.Price != nil && *req.Price < 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "price cannot be negative")
		return
	}
	if req.MaxStaff != nil && *req.MaxStaff < 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "max_staff cannot be negative")
		return
	}
	if req.MaxProducts != nil && *req.MaxProducts < 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "max_products cannot be negative")
		return
	}
	price := 0.0
	if req.Price != nil {
		price = *req.Price
	}
	terms, err := mergePlanTerms(parsePlanTerms(nil, name, price), req)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	featuresDoc, err := json.Marshal(terms)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to encode plan terms")
		return
	}

	plan, err := h.q.CreatePlan(r.Context(), sqlc.CreatePlanParams{
		Name:        name,
		Description: strings.TrimSpace(deref(req.Description)),
		Price:       numericOrZero(req.Price),
		MaxStaff:    int32(derefInt(req.MaxStaff, 5)),
		MaxProducts: int32(derefInt(req.MaxProducts, 100)),
		Features:    featuresDoc,
		IsActive:    boolOrTrue(req.IsActive),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create plan")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Plan Created: "+name, "plan", plan.ID)
	response.JSON(w, http.StatusCreated, planJSON(plan))
}

func (h *Handler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid plan id")
		return
	}
	var req planWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	// nil leaves plans.features untouched (the query COALESCEs it).
	var planTermsDoc []byte

	// Refuse to rename a plan that tenants are already subscribed to — the
	// subscription row and the plan name must stay in agreement.
	if req.Name != nil {
		newName := strings.ToUpper(strings.TrimSpace(*req.Name))
		existing, err := h.q.GetPlanByID(r.Context(), pgutil.UUID(id))
		if err != nil {
			response.Error(w, http.StatusNotFound, "not_found", "plan not found")
			return
		}
		if newName != existing.Name {
			inUse, err := h.q.CountTenantsOnPlan(r.Context(), pgutil.UUID(id))
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to check plan usage")
				return
			}
			if inUse > 0 {
				response.Error(w, http.StatusConflict, "plan_in_use",
					"cannot rename a plan that active tenants are on")
				return
			}
		}
	}

	// The commercial terms are merged onto what is stored, so a partial write
	// cannot blank the feature list or the billing period.
	if req.BillingPeriod != nil || req.TrialDays != nil || req.Features != nil || req.BusinessTypes != nil {
		existing, err := h.q.GetPlanByID(r.Context(), pgutil.UUID(id))
		if err != nil {
			response.Error(w, http.StatusNotFound, "not_found", "plan not found")
			return
		}
		terms, err := mergePlanTerms(
			parsePlanTerms(existing.Features, existing.Name, pgutil.NumericToFloat(existing.Price)),
			req,
		)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		doc, err := json.Marshal(terms)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to encode plan terms")
			return
		}
		planTermsDoc = doc
	}

	params := sqlc.UpdatePlanParams{ID: pgutil.UUID(id), Features: planTermsDoc}
	if req.Name != nil {
		params.Name = pgtype.Text{String: strings.ToUpper(strings.TrimSpace(*req.Name)), Valid: true}
	}
	if req.Description != nil {
		params.Description = pgtype.Text{String: strings.TrimSpace(*req.Description), Valid: true}
	}
	if req.Price != nil {
		if *req.Price < 0 {
			response.Error(w, http.StatusBadRequest, "invalid_request", "price cannot be negative")
			return
		}
		params.Price = numericOrZero(req.Price)
	}
	if req.MaxStaff != nil {
		if *req.MaxStaff < 0 {
			response.Error(w, http.StatusBadRequest, "invalid_request", "max_staff cannot be negative")
			return
		}
		params.MaxStaff = pgtype.Int4{Int32: int32(*req.MaxStaff), Valid: true}
	}
	if req.MaxProducts != nil {
		if *req.MaxProducts < 0 {
			response.Error(w, http.StatusBadRequest, "invalid_request", "max_products cannot be negative")
			return
		}
		params.MaxProducts = pgtype.Int4{Int32: int32(*req.MaxProducts), Valid: true}
	}
	if req.IsActive != nil {
		params.IsActive = pgtype.Bool{Bool: *req.IsActive, Valid: true}
	}

	plan, err := h.q.UpdatePlan(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update plan")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Plan Updated: "+plan.Name, "plan", plan.ID)
	response.JSON(w, http.StatusOK, planJSON(plan))
}

/* ------------------------------------------------------------------ *
 * Theme presets — the catalog the brand picker draws from.
 * ------------------------------------------------------------------ */

type themePresetWriteRequest struct {
	Name   *string         `json:"name"`
	Tokens *map[string]any `json:"tokens"`
}

func themePresetJSON(t sqlc.ThemePreset) map[string]any {
	return map[string]any{
		"id":         t.ID,
		"name":       t.Name,
		"tokens":     brand.ParseTokens(t.Tokens),
		"created_at": t.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func (h *Handler) CreateThemePreset(w http.ResponseWriter, r *http.Request) {
	var req themePresetWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	id := slugify(name)
	if id == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "preset name is required")
		return
	}
	if _, err := h.q.GetThemePreset(r.Context(), id); err == nil {
		response.Error(w, http.StatusConflict, "duplicate", "a preset with that id already exists")
		return
	}
	tokens, err := marshalThemeTokens(req.Tokens)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid theme tokens")
		return
	}
	preset, err := h.q.CreateThemePreset(r.Context(), sqlc.CreateThemePresetParams{
		ID: id, Name: name, Tokens: tokens,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create preset")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Theme Preset Created: "+name, "theme_preset", pgtype.UUID{})
	response.JSON(w, http.StatusCreated, themePresetJSON(preset))
}

func (h *Handler) UpdateThemePreset(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid preset id")
		return
	}
	var req themePresetWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	params := sqlc.UpdateThemePresetParams{ID: id}
	if req.Name != nil {
		params.Name = pgtype.Text{String: strings.TrimSpace(*req.Name), Valid: true}
	}
	if req.Tokens != nil {
		tokens, err := marshalThemeTokens(req.Tokens)
		if err != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid theme tokens")
			return
		}
		params.Tokens = tokens
	}
	preset, err := h.q.UpdateThemePreset(r.Context(), params)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "preset not found")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Theme Preset Updated: "+preset.Name, "theme_preset", pgtype.UUID{})
	response.JSON(w, http.StatusOK, themePresetJSON(preset))
}

func (h *Handler) DeleteThemePreset(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	if id == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid preset id")
		return
	}
	// Tenants keep working on a deleted preset only if we block it outright.
	inUse, err := h.q.CountTenantsOnThemePreset(r.Context(), id)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to check preset usage")
		return
	}
	if inUse > 0 {
		response.Error(w, http.StatusConflict, "preset_in_use",
			strconv.FormatInt(inUse, 10)+" tenants still use this preset")
		return
	}
	if err := h.q.DeleteThemePreset(r.Context(), id); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to delete preset")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	_ = insertAudit(r.Context(), h.q, nil, &actor.ID, "Theme Preset Deleted: "+id, "theme_preset", pgtype.UUID{})
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* ---------- helpers ---------- */

// marshalThemeTokens validates the subset of token keys a preset may set.
func marshalThemeTokens(in *map[string]any) ([]byte, error) {
	allowed := map[string]bool{
		"accent": true, "accent2": true, "radius_sm": true, "radius": true,
		"radius_lg": true, "font_display": true, "font_body": true,
	}
	out := map[string]string{}
	if in != nil {
		for k, v := range *in {
			if !allowed[k] {
				continue
			}
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out[k] = strings.TrimSpace(s)
			}
		}
	}
	if c, ok := out["accent"]; ok && !isHexColor(c) {
		return nil, errors.New("invalid colour")
	}
	if c, ok := out["accent2"]; ok && !isHexColor(c) {
		return nil, errors.New("invalid colour")
	}
	return json.Marshal(out)
}

func isHexColor(v string) bool {
	if len(v) != 7 || v[0] != '#' {
		return false
	}
	for _, r := range v[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, name))
}

func numericOrZero(v *float64) pgtype.Numeric {
	if v == nil {
		return pgtype.Numeric{}
	}
	n, err := pgutil.NumericFromFloat(*v)
	if err != nil {
		return pgtype.Numeric{}
	}
	return n
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(v *int, fallback int) int {
	if v == nil {
		return fallback
	}
	return *v
}

func boolOrTrue(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}
