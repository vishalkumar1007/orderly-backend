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
	PlatformName          string `json:"platform_name"`
	SupportEmail          string `json:"support_email"`
	Timezone              string `json:"timezone"`
	DefaultLocale         string `json:"default_locale"`
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
		SessionTimeoutMinutes: 15,
		RequireMFAForAdmins:   false,
		PasswordMinLength:     8,
		InviteExpiryHours:     168,
		AllowSelfServe:        false,
		DefaultPlan:           "TRIAL",
		MaintenanceMode:       false,
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
			"platform_name":  cfg.PlatformName,
			"support_email":  cfg.SupportEmail,
			"timezone":       cfg.Timezone,
			"default_locale": cfg.DefaultLocale,
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
		PlatformName  *string `json:"platform_name"`
		SupportEmail  *string `json:"support_email"`
		Timezone      *string `json:"timezone"`
		DefaultLocale *string `json:"default_locale"`
	} `json:"general"`
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
