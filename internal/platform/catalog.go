package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/brand"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

var typeCodeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)
var hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func normalizeTypeCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func (h *Handler) requireActiveType(ctx context.Context, code string) error {
	row, err := h.q.GetTenantType(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errInactiveType
		}
		return err
	}
	if !row.Active {
		return errInactiveType
	}
	return nil
}

var errInactiveType = errors.New("invalid business type")

func (h *Handler) ListTenantTypes(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListTenantTypes(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list tenant types")
		return
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, tenantTypeJSON(row))
	}
	response.JSON(w, http.StatusOK, map[string]any{"types": out})
}

// ListBusinessTypeCapabilities returns the capability matrix row for one
// business type — what the onboarding wizard needs to render "included"
// badges for the fixed capabilities and toggles for the configurable ones,
// without carrying its own copy of business_type_capabilities.
func (h *Handler) ListBusinessTypeCapabilities(w http.ResponseWriter, r *http.Request) {
	code := normalizeTypeCode(chi.URLParam(r, "code"))
	rows, err := h.q.GetBusinessTypeCapabilitiesWithLabels(r.Context(), code)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list capabilities")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"code":            row.CapabilityCode,
			"label":           row.Label,
			"default_enabled": row.DefaultEnabled,
			"configurable":    row.Configurable,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"capabilities": out})
}

type tenantTypeWrite struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	Active    *bool  `json:"active"`
	SortOrder *int32 `json:"sort_order"`
}

func (h *Handler) CreateTenantType(w http.ResponseWriter, r *http.Request) {
	var req tenantTypeWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	code := normalizeTypeCode(req.Code)
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = strings.ReplaceAll(code, "_", " ")
	}
	if !typeCodeRe.MatchString(code) || label == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "code and label are required")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	sort := int32(100)
	if req.SortOrder != nil {
		sort = *req.SortOrder
	}
	row, err := h.q.CreateTenantType(r.Context(), sqlc.CreateTenantTypeParams{
		Code: code, Label: label, Active: active, SortOrder: sort,
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "tenant_types_pkey") {
			response.Error(w, http.StatusConflict, "conflict", "type code already exists")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create type")
		return
	}
	response.JSON(w, http.StatusCreated, tenantTypeJSON(row))
}

func (h *Handler) UpdateTenantType(w http.ResponseWriter, r *http.Request) {
	code := normalizeTypeCode(chi.URLParam(r, "code"))
	if !typeCodeRe.MatchString(code) {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid type code")
		return
	}
	current, err := h.q.GetTenantType(r.Context(), code)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "type not found")
		return
	}
	var req tenantTypeWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	label := current.Label
	if strings.TrimSpace(req.Label) != "" {
		label = strings.TrimSpace(req.Label)
	}
	active := current.Active
	if req.Active != nil {
		active = *req.Active
	}
	sort := current.SortOrder
	if req.SortOrder != nil {
		sort = *req.SortOrder
	}
	row, err := h.q.UpdateTenantType(r.Context(), sqlc.UpdateTenantTypeParams{
		Code: code, Label: label, Active: active, SortOrder: sort,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update type")
		return
	}
	response.JSON(w, http.StatusOK, tenantTypeJSON(row))
}

func tenantTypeJSON(row sqlc.TenantType) map[string]any {
	return map[string]any{
		"code": row.Code, "label": row.Label, "active": row.Active, "sort_order": row.SortOrder,
	}
}

func (h *Handler) ListThemePresets(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListThemePresets(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list themes")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"presets": presetList(rows)})
}

func presetList(rows []sqlc.ThemePreset) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID, "name": row.Name, "tokens": brand.ParseTokens(row.Tokens),
		})
	}
	return out
}

type themeWrite struct {
	ThemePresetID  string          `json:"theme_preset_id"`
	ThemeColorMode string          `json:"theme_color_mode"`
	ThemeOverrides json.RawMessage `json:"theme_overrides"`
}

func (h *Handler) normalizeTheme(ctx context.Context, req themeWrite) (string, string, []byte, error) {
	presetID := strings.TrimSpace(req.ThemePresetID)
	if presetID == "" {
		presetID = "indigo-violet"
	}
	if _, err := h.q.GetThemePreset(ctx, presetID); err != nil {
		return "", "", nil, errUnknownPreset
	}
	mode := strings.ToLower(strings.TrimSpace(req.ThemeColorMode))
	if mode == "" {
		mode = "system"
	}
	if mode != "light" && mode != "dark" && mode != "system" {
		return "", "", nil, errBadMode
	}
	overrides, err := sanitizeOverrides(req.ThemeOverrides)
	if err != nil {
		return "", "", nil, err
	}
	return presetID, mode, overrides, nil
}

var (
	errUnknownPreset = errors.New("unknown theme")
	errBadMode       = errors.New("invalid color mode")
	errBadColor      = errors.New("invalid accent color")
)

func sanitizeOverrides(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("{}"), nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errBadColor
	}
	out := map[string]string{}
	for _, key := range []string{"accent", "accent2"} {
		v := strings.TrimSpace(m[key])
		if v == "" {
			continue
		}
		if !hexColorRe.MatchString(v) {
			return nil, errBadColor
		}
		out[key] = strings.ToLower(v)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (h *Handler) UpdateTenantTheme(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	h.writeTheme(w, r, pgutil.UUID(id))
}

func (h *Handler) GetMyTenantTheme(w http.ResponseWriter, r *http.Request) {
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	row, err := h.q.GetTenantWithPlanByID(r.Context(), pgutil.UUID(info.ID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	response.JSON(w, http.StatusOK, themeFromRow(row))
}

func (h *Handler) UpdateMyTenantTheme(w http.ResponseWriter, r *http.Request) {
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	if actor.Role != identity.RoleTenantAdmin {
		response.Error(w, http.StatusForbidden, "forbidden", "only the tenant administrator can change the brand")
		return
	}
	h.writeTheme(w, r, pgutil.UUID(info.ID))
}

func (h *Handler) writeTheme(w http.ResponseWriter, r *http.Request, id pgtype.UUID) {
	var req themeWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	presetID, mode, overrides, err := h.normalizeTheme(r.Context(), req)
	if err != nil {
		msg := "invalid theme"
		if errors.Is(err, errUnknownPreset) {
			msg = "unknown theme"
		} else if errors.Is(err, errBadMode) {
			msg = "color mode must be light, dark, or system"
		} else if errors.Is(err, errBadColor) {
			msg = "accent must be a 6-digit hex color"
		}
		response.Error(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	if _, err := h.q.SetTenantTheme(r.Context(), sqlc.SetTenantThemeParams{
		ID: id, ThemePresetID: presetID, ThemeColorMode: mode, ThemeOverrides: overrides,
	}); err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	row, err := h.q.GetTenantWithPlanByID(r.Context(), id)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load tenant")
		return
	}
	response.JSON(w, http.StatusOK, tenantJSONFromRow(row))
}

func themeFromRow(row sqlc.GetTenantWithPlanByIDRow) map[string]any {
	return brand.Payload(
		row.Tenant.ThemePresetID,
		textOrEmpty(row.ThemeName),
		row.Tenant.ThemeColorMode,
		row.ThemeTokens,
		row.Tenant.ThemeOverrides,
	)
}

func tenantJSONFromRow(row sqlc.GetTenantWithPlanByIDRow) map[string]any {
	out := tenantJSONEnriched(row.Tenant, row.PlanName, row.PlanPrice, row.ThemeName, row.ThemeTokens)
	return out
}
