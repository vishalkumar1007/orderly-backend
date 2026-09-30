package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/response"
)

type storedPlatformConfig struct {
	PlatformName    string `json:"platform_name"`
	SupportEmail    string `json:"support_email"`
	Timezone        string `json:"timezone"`
	DefaultLocale   string `json:"default_locale"`
	DefaultCurrency string `json:"default_currency"`

	// The console's own theme, never a tenant's. A tenant's brand lives on its
	// own row and never reads these.
	//
	// A logo and favicon URL used to live here too. They were stored,
	// validated and returned, and nothing ever rendered either of them — the
	// rail shows the platform name as text and the favicon comes from the
	// tenant's storefront or the bundled asset. A field that cannot change
	// anything is worse than a missing one, because somebody fills it in and
	// believes it worked.
	BrandPrimary   string `json:"brand_primary_color"`
	BrandSecondary string `json:"brand_secondary_color"`
	// The console's default theme: the preset everyone starts from and whether
	// it opens light, dark or with the operating system. A person can override
	// both for themselves (users.console_theme); these are what they override.
	BrandPresetID  string `json:"brand_preset_id"`
	BrandColorMode string `json:"brand_color_mode"`

	SessionTimeoutMinutes int    `json:"session_timeout_minutes"`
	RequireMFAForAdmins   bool   `json:"require_mfa_for_admins"`
	PasswordMinLength     int    `json:"password_min_length"`
	InviteExpiryHours     int    `json:"invite_expiry_hours"`
	AllowSelfServe        bool   `json:"allow_self_serve"`
	DefaultPlan           string `json:"default_plan"`
	MaintenanceMode       bool   `json:"maintenance_mode"`
}

func defaultStoredConfig() storedPlatformConfig {
	return storedPlatformConfig{
		PlatformName:          "Orderly",
		SupportEmail:          "support@orderly.local",
		Timezone:              "Asia/Kolkata",
		DefaultLocale:         "en-IN",
		DefaultCurrency:       "INR",
		SessionTimeoutMinutes: 15,
		RequireMFAForAdmins:   false,
		PasswordMinLength:     8,
		InviteExpiryHours:     168,
		AllowSelfServe:        false,
		DefaultPlan:           "TRIAL",
		MaintenanceMode:       false,
		BrandPresetID:         "indigo-violet",
		BrandColorMode:        "system",
	}
}

func (h *Handler) loadStoredConfig(ctx context.Context) (storedPlatformConfig, error) {
	row, err := h.q.GetPlatformSettings(ctx)
	if err != nil {
		return defaultStoredConfig(), err
	}
	cfg := defaultStoredConfig()
	if len(row.Config) > 0 {
		_ = json.Unmarshal(row.Config, &cfg)
	}
	normalizeStoredConfig(&cfg)
	return cfg, nil
}

func normalizeStoredConfig(c *storedPlatformConfig) {
	if c.PlatformName == "" {
		c.PlatformName = "Orderly"
	}
	if c.PasswordMinLength < 6 {
		c.PasswordMinLength = 8
	}
	if c.SessionTimeoutMinutes < 5 {
		c.SessionTimeoutMinutes = 15
	}
	if c.InviteExpiryHours < 1 {
		c.InviteExpiryHours = 168
	}
	if c.DefaultPlan == "" {
		c.DefaultPlan = "TRIAL"
	}
	if c.DefaultCurrency == "" {
		c.DefaultCurrency = "INR"
	}
	c.DefaultCurrency = strings.ToUpper(strings.TrimSpace(c.DefaultCurrency))
	// A malformed colour is dropped rather than stored: the console falls back
	// to its built-in accent, which is always better than an unreadable one.
	if !hexColorRe.MatchString(c.BrandPrimary) {
		c.BrandPrimary = ""
	}
	if !hexColorRe.MatchString(c.BrandSecondary) {
		c.BrandSecondary = ""
	}
}

func (h *Handler) settingsResponse(ctx context.Context) (map[string]any, error) {
	cfg, err := h.loadStoredConfig(ctx)
	if err != nil {
		return nil, err
	}
	baseDomain := strings.TrimSpace(os.Getenv("BASE_DOMAIN"))
	if baseDomain == "" {
		baseDomain = "localhost"
	}
	port := frontendPort()
	return map[string]any{
		"general": map[string]any{
			"platform_name":    cfg.PlatformName,
			"support_email":    cfg.SupportEmail,
			"timezone":         cfg.Timezone,
			"default_locale":   cfg.DefaultLocale,
			"default_currency": cfg.DefaultCurrency,
		},
		"branding": map[string]any{
			"primary_color":   cfg.BrandPrimary,
			"secondary_color": cfg.BrandSecondary,
			"preset_id":       cfg.BrandPresetID,
			"color_mode":      cfg.BrandColorMode,
		},
		"security": map[string]any{
			"session_timeout_minutes": cfg.SessionTimeoutMinutes,
			"require_mfa_for_admins":  cfg.RequireMFAForAdmins,
			"password_min_length":     cfg.PasswordMinLength,
			"invite_expiry_hours":     cfg.InviteExpiryHours,
		},
		"platform": map[string]any{
			"base_domain":        baseDomain,
			"frontend_port":      port,
			"admin_host":         "localhost:" + port,
			"allow_self_serve":   cfg.AllowSelfServe,
			"maintenance_mode":   cfg.MaintenanceMode,
			"read_only_settings": false,
		},
		"app_env": strings.TrimSpace(os.Getenv("APP_ENV")),
	}, nil
}

func (h *Handler) Settings(w http.ResponseWriter, r *http.Request) {
	out, err := h.settingsResponse(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load settings")
		return
	}
	response.JSON(w, http.StatusOK, out)
}

type patchSettingsRequest struct {
	General *struct {
		PlatformName    *string `json:"platform_name"`
		SupportEmail    *string `json:"support_email"`
		Timezone        *string `json:"timezone"`
		DefaultLocale   *string `json:"default_locale"`
		DefaultCurrency *string `json:"default_currency"`
	} `json:"general"`
	Branding *struct {
		PrimaryColor   *string `json:"primary_color"`
		SecondaryColor *string `json:"secondary_color"`
		PresetID       *string `json:"preset_id"`
		ColorMode      *string `json:"color_mode"`
		// Reset puts the console's theme back to what a fresh install has.
		//
		// The client could send those four values itself, but then "factory"
		// would be written down in two places and the copy in the browser would
		// be the one that rots. It asks; the server answers from the same
		// defaults it boots with.
		Reset *bool `json:"reset"`
	} `json:"branding"`
	Security *struct {
		SessionTimeoutMinutes *int  `json:"session_timeout_minutes"`
		RequireMFAForAdmins   *bool `json:"require_mfa_for_admins"`
		PasswordMinLength     *int  `json:"password_min_length"`
		InviteExpiryHours     *int  `json:"invite_expiry_hours"`
	} `json:"security"`
	Platform *struct {
		AllowSelfServe  *bool `json:"allow_self_serve"`
		MaintenanceMode *bool `json:"maintenance_mode"`
	} `json:"platform"`
}

func (h *Handler) PatchSettings(w http.ResponseWriter, r *http.Request) {
	var req patchSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	ctx := r.Context()
	cfg, err := h.loadStoredConfig(ctx)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load settings")
		return
	}
	if req.General != nil {
		if req.General.PlatformName != nil {
			cfg.PlatformName = strings.TrimSpace(*req.General.PlatformName)
		}
		if req.General.SupportEmail != nil {
			cfg.SupportEmail = strings.TrimSpace(*req.General.SupportEmail)
		}
		if req.General.Timezone != nil {
			cfg.Timezone = strings.TrimSpace(*req.General.Timezone)
		}
		if req.General.DefaultLocale != nil {
			cfg.DefaultLocale = strings.TrimSpace(*req.General.DefaultLocale)
		}
		if req.General.DefaultCurrency != nil {
			cfg.DefaultCurrency = strings.ToUpper(strings.TrimSpace(*req.General.DefaultCurrency))
		}
	}
	if req.Branding != nil {
		if req.Branding.Reset != nil && *req.Branding.Reset {
			fresh := defaultStoredConfig()
			cfg.BrandPresetID = fresh.BrandPresetID
			cfg.BrandColorMode = fresh.BrandColorMode
			cfg.BrandPrimary = fresh.BrandPrimary
			cfg.BrandSecondary = fresh.BrandSecondary
		}
		if req.Branding.PrimaryColor != nil {
			cfg.BrandPrimary = strings.TrimSpace(*req.Branding.PrimaryColor)
		}
		if req.Branding.SecondaryColor != nil {
			cfg.BrandSecondary = strings.TrimSpace(*req.Branding.SecondaryColor)
		}
		if req.Branding.PresetID != nil {
			cfg.BrandPresetID = strings.TrimSpace(*req.Branding.PresetID)
		}
		if req.Branding.ColorMode != nil {
			cfg.BrandColorMode = strings.ToLower(strings.TrimSpace(*req.Branding.ColorMode))
		}
	}
	if req.Security != nil {
		if req.Security.SessionTimeoutMinutes != nil && *req.Security.SessionTimeoutMinutes >= 5 {
			cfg.SessionTimeoutMinutes = *req.Security.SessionTimeoutMinutes
		}
		if req.Security.RequireMFAForAdmins != nil {
			cfg.RequireMFAForAdmins = *req.Security.RequireMFAForAdmins
		}
		if req.Security.PasswordMinLength != nil && *req.Security.PasswordMinLength >= 6 {
			cfg.PasswordMinLength = *req.Security.PasswordMinLength
		}
		if req.Security.InviteExpiryHours != nil && *req.Security.InviteExpiryHours >= 1 {
			cfg.InviteExpiryHours = *req.Security.InviteExpiryHours
		}
	}
	if req.Platform != nil {
		if req.Platform.AllowSelfServe != nil {
			cfg.AllowSelfServe = *req.Platform.AllowSelfServe
		}
		if req.Platform.MaintenanceMode != nil {
			cfg.MaintenanceMode = *req.Platform.MaintenanceMode
		}
	}
	if cfg.PlatformName == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "platform_name required")
		return
	}
	if req.Branding != nil {
		// The preset has to exist, or every console would fall back to the
		// built-in accent and the saved value would look like it did nothing.
		if cfg.BrandPresetID != "" {
			if _, err := h.q.GetThemePreset(ctx, cfg.BrandPresetID); err != nil {
				response.Error(w, http.StatusBadRequest, "invalid_request", "unknown theme")
				return
			}
		}
		switch cfg.BrandColorMode {
		case "", "light", "soft", "dark", "night", "system":
		default:
			response.Error(w, http.StatusBadRequest, "invalid_request", "color mode must be light, soft, dark, night, or system")
			return
		}
		if cfg.BrandPrimary != "" && !hexColorRe.MatchString(cfg.BrandPrimary) {
			response.Error(w, http.StatusBadRequest, "invalid_request", "primary_color must be a 6-digit hex colour")
			return
		}
		if cfg.BrandSecondary != "" && !hexColorRe.MatchString(cfg.BrandSecondary) {
			response.Error(w, http.StatusBadRequest, "invalid_request", "secondary_color must be a 6-digit hex colour")
			return
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "encode failed")
		return
	}
	if _, err := h.q.UpsertPlatformSettings(ctx, raw); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save settings")
		return
	}
	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, h.q, nil, &actor.ID, "platform.settings_updated", "platform_settings", pgtype.UUID{})
	out, _ := h.settingsResponse(ctx)
	response.JSON(w, http.StatusOK, out)
}
