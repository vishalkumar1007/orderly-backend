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
}

func planJSON(p sqlc.Plan) map[string]any {
	return map[string]any{
		"id":           pgutil.UUIDString(p.ID),
		"name":         p.Name,
		"description":  p.Description,
		"price":        pgutil.NumericToFloat(p.Price),
		"max_staff":    p.MaxStaff,
		"max_products": p.MaxProducts,
		"is_active":    p.IsActive,
	}
}

// ListAllPlans includes deactivated plans so the console can manage them.
func (h *Handler) ListAllPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListAllPlans(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list plans")
		return
	}
	out := make([]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, planJSON(p))
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
	plan, err := h.q.CreatePlan(r.Context(), sqlc.CreatePlanParams{
		Name:        name,
		Description: strings.TrimSpace(deref(req.Description)),
		Price:       numericOrZero(req.Price),
		MaxStaff:    int32(derefInt(req.MaxStaff, 5)),
		MaxProducts: int32(derefInt(req.MaxProducts, 100)),
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

	params := sqlc.UpdatePlanParams{ID: pgutil.UUID(id)}
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
