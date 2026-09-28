package platform

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/brand"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
Console appearance.

Two layers, and the difference between them is the whole point:

  - the *business default* (`tenants.theme_*`) is shared. The owner sets it, it
    paints the sign-in screen and the setup link, and it is what a colleague
    sees the first time they open the console. Writing it changes the console
    for everyone who works there.
  - the *personal* layer (`users.console_theme`) belongs to one person. It is
    NULL until they choose something, and clearing it puts them back on the
    business default rather than freezing a copy of today's values.

Everyone signed in to a business may set their own, including staff: it is a
preference about their own screen, not a capability over the business. Writing
the business default stays owner-only and lives on `PATCH /tenant/theme`.
*/

// personalTheme is the stored shape of users.console_theme.
//
// It is deliberately the same shape the tenant stores, so `brand.Payload` can
// resolve either layer without a second code path.
type personalTheme struct {
	PresetID  string            `json:"preset_id"`
	ColorMode string            `json:"color_mode"`
	Overrides map[string]string `json:"overrides,omitempty"`
}

// appearanceWrite is the request body for PATCH /tenant/me/appearance.
//
// Pointers because a partial write must be able to say "leave the colour mode
// alone" without a client having to echo back a value it did not touch.
type appearanceWrite struct {
	PresetID  *string            `json:"preset_id"`
	ColorMode *string            `json:"color_mode"`
	Overrides *map[string]string `json:"overrides"`
	// UseBusinessDefault clears the personal layer. Sending it with other
	// fields is meaningless, so it wins: "reset" is what the user asked for.
	UseBusinessDefault bool `json:"use_business_default"`
}

// defaultLayer resolves "what this console looks like before you personalise
// it", and the signed-in row to personalise. Two implementations — a business
// and the platform — so the three verbs below are written once.
type defaultLayer func(http.ResponseWriter, *http.Request) (map[string]any, sqlc.User, bool)

// GetMyConsoleAppearance returns the theme this user's console should paint
// with, alongside the default it is layered over.
func (h *Handler) GetMyConsoleAppearance(w http.ResponseWriter, r *http.Request) {
	h.readAppearance(w, r, h.tenantDefaultTheme)
}

// UpdateMyConsoleAppearance saves — or clears — this user's personal theme.
func (h *Handler) UpdateMyConsoleAppearance(w http.ResponseWriter, r *http.Request) {
	h.writeAppearance(w, r, h.tenantDefaultTheme)
}

// ResetMyConsoleAppearance drops the personal layer.
func (h *Handler) ResetMyConsoleAppearance(w http.ResponseWriter, r *http.Request) {
	h.clearAppearance(w, r, h.tenantDefaultTheme)
}

// The same three, for the platform console. The personal layer is the identical
// column and the identical rules; only the default underneath it differs.
func (h *Handler) GetMyPlatformAppearance(w http.ResponseWriter, r *http.Request) {
	h.readAppearance(w, r, h.platformDefaultTheme)
}

func (h *Handler) UpdateMyPlatformAppearance(w http.ResponseWriter, r *http.Request) {
	h.writeAppearance(w, r, h.platformDefaultTheme)
}

func (h *Handler) ResetMyPlatformAppearance(w http.ResponseWriter, r *http.Request) {
	h.clearAppearance(w, r, h.platformDefaultTheme)
}

func (h *Handler) readAppearance(w http.ResponseWriter, r *http.Request, layer defaultLayer) {
	base, user, ok := layer(w, r)
	if !ok {
		return
	}
	respondAppearance(w, h.resolveAppearance(r, base, user.ConsoleTheme))
}

func (h *Handler) writeAppearance(w http.ResponseWriter, r *http.Request, layer defaultLayer) {
	base, user, ok := layer(w, r)
	if !ok {
		return
	}

	var req appearanceWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "Could not read those appearance settings")
		return
	}

	var stored []byte
	if !req.UseBusinessDefault {
		next, err := h.mergePersonalTheme(r, user.ConsoleTheme, req, base)
		if err != nil {
			writeThemeError(w, err)
			return
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your appearance")
			return
		}
		stored = encoded
	}

	saved, err := h.q.SetUserConsoleTheme(r.Context(), sqlc.SetUserConsoleThemeParams{
		ID:           user.ID,
		ConsoleTheme: stored,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your appearance")
		return
	}
	respondAppearance(w, h.resolveAppearance(r, base, saved.ConsoleTheme))
}

func (h *Handler) clearAppearance(w http.ResponseWriter, r *http.Request, layer defaultLayer) {
	base, user, ok := layer(w, r)
	if !ok {
		return
	}
	saved, err := h.q.SetUserConsoleTheme(r.Context(), sqlc.SetUserConsoleThemeParams{
		ID:           user.ID,
		ConsoleTheme: nil,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not reset your appearance")
		return
	}
	respondAppearance(w, h.resolveAppearance(r, base, saved.ConsoleTheme))
}

// signedInUser loads the row behind the token.
//
// The personal layer is written against a `users` row, so the handlers need the
// row and not just the claims — the claims do not carry console_theme.
func (h *Handler) signedInUser(w http.ResponseWriter, r *http.Request) (sqlc.User, bool) {
	var none sqlc.User
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to change your appearance")
		return none, false
	}
	user, err := h.q.GetUserByID(r.Context(), pgutil.UUID(actor.ID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "account not found")
		return none, false
	}
	return user, true
}

// tenantDefaultTheme is the business default layer: the theme the owner set for
// everyone who works there.
func (h *Handler) tenantDefaultTheme(w http.ResponseWriter, r *http.Request) (map[string]any, sqlc.User, bool) {
	user, ok := h.signedInUser(w, r)
	if !ok {
		return nil, user, false
	}
	info, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return nil, user, false
	}
	// The token already carries the tenant and MatchHostTenant has compared it
	// with the Host. This check is the third leg: the row itself must belong to
	// the business whose console is asking.
	if !user.TenantID.Valid || user.TenantID.Bytes != info.ID {
		response.Error(w, http.StatusForbidden, "forbidden", "that account does not belong to this business")
		return nil, user, false
	}
	row, err := h.q.GetTenantWithPlanByID(r.Context(), pgutil.UUID(info.ID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "business not found")
		return nil, user, false
	}
	return themeFromRow(row), user, true
}

// platformDefaultTheme is the console default layer: the platform's own marks,
// set under Settings → Appearance by an operator who may write platform
// settings.
//
// It resolves to exactly the same shape a tenant's default does, which is what
// lets one client module and one picker serve both consoles.
func (h *Handler) platformDefaultTheme(w http.ResponseWriter, r *http.Request) (map[string]any, sqlc.User, bool) {
	user, ok := h.signedInUser(w, r)
	if !ok {
		return nil, user, false
	}
	cfg, err := h.loadStoredConfig(r.Context())
	if err != nil {
		// A settings row that will not load is not a reason to render an
		// unthemed console; the defaults are a usable theme on their own.
		cfg = defaultStoredConfig()
	}
	presetID := strings.TrimSpace(cfg.BrandPresetID)
	if presetID == "" {
		presetID = "indigo-violet"
	}
	preset, err := h.q.GetThemePreset(r.Context(), presetID)
	if err != nil {
		preset, err = h.q.GetThemePreset(r.Context(), "indigo-violet")
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "no theme presets are installed")
			return nil, user, false
		}
	}
	// The two brand colours are overrides on the preset, in the same shape a
	// tenant stores them, so `brand.Merge` needs no second code path.
	overrides := map[string]string{}
	if hexColorRe.MatchString(cfg.BrandPrimary) {
		overrides["accent"] = strings.ToLower(cfg.BrandPrimary)
	}
	if hexColorRe.MatchString(cfg.BrandSecondary) {
		overrides["accent2"] = strings.ToLower(cfg.BrandSecondary)
	}
	encoded, _ := json.Marshal(overrides)
	return brand.Payload(preset.ID, preset.Name, cfg.BrandColorMode, preset.Tokens, encoded), user, true
}

// mergePersonalTheme applies a partial write onto what the user already has,
// falling back to the business default for anything they have never set.
func (h *Handler) mergePersonalTheme(
	r *http.Request,
	current []byte,
	req appearanceWrite,
	base map[string]any,
) (personalTheme, error) {
	next := parsePersonalTheme(current)

	if req.PresetID != nil {
		presetID := strings.TrimSpace(*req.PresetID)
		if presetID == "" {
			return next, errUnknownPreset
		}
		if _, err := h.q.GetThemePreset(r.Context(), presetID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return next, errUnknownPreset
			}
			return next, err
		}
		next.PresetID = presetID
	}
	if req.ColorMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*req.ColorMode))
		if mode != "light" && mode != "dark" && mode != "system" {
			return next, errBadMode
		}
		next.ColorMode = mode
	}
	if req.Overrides != nil {
		clean, err := cleanAccentOverrides(*req.Overrides)
		if err != nil {
			return next, err
		}
		next.Overrides = clean
	}

	// A personal theme with no preset would resolve to nothing. The default this
	// console already paints with is the only sensible starting point, so fill
	// it in rather than rejecting a request that only changed the colour mode.
	if next.PresetID == "" {
		if id, _ := base["preset_id"].(string); id != "" {
			next.PresetID = id
		}
	}
	if next.PresetID == "" {
		next.PresetID = "indigo-violet"
	}
	if next.ColorMode == "" {
		next.ColorMode = "system"
	}
	return next, nil
}

// resolveAppearance layers a person's stored choice over the console default
// and reports which of the two is in force.
func (h *Handler) resolveAppearance(
	r *http.Request,
	base map[string]any,
	stored []byte,
) map[string]any {
	payload := map[string]any{
		"business": base,
		"source":   "BUSINESS",
		"theme":    base,
		"personal": nil,
	}

	personal := parsePersonalTheme(stored)
	if personal.PresetID == "" {
		return payload
	}

	preset, err := h.q.GetThemePreset(r.Context(), personal.PresetID)
	if err != nil {
		// The preset was withdrawn after this person chose it. Falling back to
		// the default is better than painting a console with no accent, and the
		// next save replaces the stale row anyway.
		return payload
	}
	overrides, err := json.Marshal(personal.Overrides)
	if err != nil {
		overrides = []byte("{}")
	}

	payload["source"] = "USER"
	payload["theme"] = brand.Payload(preset.ID, preset.Name, personal.ColorMode, preset.Tokens, overrides)
	payload["personal"] = personal
	return payload
}

func respondAppearance(w http.ResponseWriter, payload map[string]any) {
	response.JSON(w, http.StatusOK, payload)
}

func parsePersonalTheme(raw []byte) personalTheme {
	var t personalTheme
	if len(raw) == 0 || string(raw) == "null" {
		return t
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return personalTheme{}
	}
	t.PresetID = strings.TrimSpace(t.PresetID)
	t.ColorMode = strings.ToLower(strings.TrimSpace(t.ColorMode))
	if t.ColorMode != "light" && t.ColorMode != "dark" && t.ColorMode != "system" {
		t.ColorMode = "system"
	}
	clean, err := cleanAccentOverrides(t.Overrides)
	if err != nil {
		clean = nil
	}
	t.Overrides = clean
	return t
}

// cleanAccentOverrides keeps the two colours that are overridable and rejects
// anything that is not a hex colour, so a stored document can never carry CSS.
func cleanAccentOverrides(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, key := range []string{"accent", "accent2"} {
		v := strings.TrimSpace(in[key])
		if v == "" {
			continue
		}
		if !hexColorRe.MatchString(v) {
			return nil, errBadColor
		}
		out[key] = strings.ToLower(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func writeThemeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnknownPreset):
		response.Error(w, http.StatusBadRequest, "invalid_request", "unknown theme")
	case errors.Is(err, errBadMode):
		response.Error(w, http.StatusBadRequest, "invalid_request", "color mode must be light, dark, or system")
	case errors.Is(err, errBadColor):
		response.Error(w, http.StatusBadRequest, "invalid_request", "accent must be a 6-digit hex color")
	default:
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not save your appearance")
	}
}
