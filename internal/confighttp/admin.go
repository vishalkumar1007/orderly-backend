// Package confighttp holds the HTTP surface for provider configuration. It
// lives beside configsvc rather than inside it so the transport layer stays
// separate from the domain layer.
package confighttp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/pkg/response"
)

// AuditSink records configuration changes. The platform package satisfies it,
// which keeps confighttp free of a dependency on the handler that owns audit
// logging.
type AuditSink interface {
	AuditConfiguration(ctx context.Context, tenantID *uuid.UUID, action, service, provider string)
}

// Handler serves both the Super Admin and tenant configuration endpoints.
type Handler struct {
	svc   *configsvc.Service
	store *configsvc.Store
	box   *secretbox.Box
	audit AuditSink
	log   *slog.Logger
}

// NewHandler builds a Handler.
func NewHandler(svc *configsvc.Service, box *secretbox.Box, audit AuditSink, log *slog.Logger) *Handler {
	return &Handler{svc: svc, store: svc.Store(), box: box, audit: audit, log: log}
}

/* ------------------------------------------------------------------ *
 * Shared request handling
 * ------------------------------------------------------------------ */

// writeRequest is the body every configuration save accepts. Config carries the
// service's non-secret fields; a secret left as MaskSentinel or "" is
// preserved rather than blanked.
type writeRequest struct {
	Provider     string         `json:"provider"`
	Config       map[string]any `json:"config"`
	Enabled      *bool          `json:"enabled"`
	AllowTenants *bool          `json:"allow_tenants"`
}

// serviceParam validates the :service URL segment.
func serviceParam(w http.ResponseWriter, r *http.Request) (configsvc.ServiceType, bool) {
	service, err := configsvc.ParseServiceType(chi.URLParam(r, "service"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "service must be one of smtp, storage, ai")
		return "", false
	}
	return service, true
}

// writeFailure maps a domain error onto the right HTTP status. An
// UnavailableError is a configuration problem, not a server fault, so it must
// not surface as a 500.
func writeFailure(w http.ResponseWriter, err error) {
	var unavailable *configsvc.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		response.JSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "configuration_unavailable",
				"message": unavailable.Error(),
				"hint":    unavailable.Hint(),
				"service": unavailable.Service,
				"source":  unavailable.Source,
			},
		})
	case errors.Is(err, configsvc.ErrInvalidRequest):
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, configsvc.ErrNotFound):
		response.Error(w, http.StatusNotFound, "not_found", "configuration not found")
	case errors.Is(err, configsvc.ErrNotPermitted):
		response.Error(w, http.StatusForbidden, "forbidden", err.Error())
	default:
		response.Error(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

// validateRequest normalises and validates an incoming save.
func validateRequest(service configsvc.ServiceType, req writeRequest, existingEnabled bool) (configsvc.WriteRequest, bool, error) {
	// Defaults are preserved when the field is absent, so a partial save from a
	// form that only touched one field does not silently disable the service.
	enabled := existingEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	out, err := configsvc.NormalizeWriteRequest(service, req.Provider, req.Config)
	if err != nil {
		return configsvc.WriteRequest{}, false, err
	}
	out.Enabled = enabled
	return out, enabled, nil
}

// testTimeout bounds a provider test so a hung endpoint cannot pin a request
// goroutine.
const testTimeout = 20 * time.Second

/* ------------------------------------------------------------------ *
 * Super Admin
 * ------------------------------------------------------------------ */

// ListConfigurations returns every platform service in one call.
//
// GET /api/v1/admin/configurations
func (h *Handler) ListConfigurations(w http.ResponseWriter, r *http.Request) {
	records, err := h.store.ListPlatform(r.Context())
	if err != nil {
		writeFailure(w, err)
		return
	}
	out := make([]configsvc.PublicView, 0, len(configsvc.AllServices))
	for _, service := range configsvc.AllServices {
		if rec, ok := records[service]; ok {
			out = append(out, configsvc.NewPublicView(rec, configsvc.SourcePlatform))
			continue
		}
		out = append(out, configsvc.NotConfiguredView(service, configsvc.SourcePlatform))
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"configurations": out,
		"encryption":     map[string]any{"enabled": !h.box.Disabled()},
	})
}

// GetConfiguration returns one platform service.
//
// GET /api/v1/admin/configurations/{service}
func (h *Handler) GetConfiguration(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	rec, err := h.store.GetPlatform(r.Context(), service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if rec == nil {
		// A 200 with an UNCONFIGURED view lets the UI render one shape for
		// "never set up" and "set up but broken".
		response.JSON(w, http.StatusOK, configsvc.NotConfiguredView(service, configsvc.SourcePlatform))
		return
	}
	response.JSON(w, http.StatusOK, configsvc.NewPublicView(rec, configsvc.SourcePlatform))
}

// PutConfiguration saves a platform configuration.
//
// PUT /api/v1/admin/configurations/{service}
func (h *Handler) PutConfiguration(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	existing, err := h.store.GetPlatform(r.Context(), service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	existingEnabled := existing != nil && existing.Enabled
	existingAllow := existing != nil && existing.AllowTenants
	wasEnabled := existingEnabled

	write, enabled, err := validateRequest(service, req, existingEnabled)
	if err != nil {
		writeFailure(w, err)
		return
	}
	// Turning sharing on while disabled is meaningless and confusing in the UI.
	write.AllowTenants = existingAllow
	if req.AllowTenants != nil {
		write.AllowTenants = *req.AllowTenants && write.Enabled
	}

	if h.box.Disabled() && configsvc.HasSecretInput(service, req.Config) {
		response.Error(w, http.StatusServiceUnavailable, "encryption_unavailable",
			"CONFIG_ENCRYPTION_KEY is not set, so secrets cannot be stored safely. Set it and restart.")
		return
	}

	rec, err := h.store.SavePlatform(r.Context(), write)
	if err != nil {
		writeFailure(w, err)
		return
	}

	h.auditPlatform(r, service, rec.Provider, "Configuration Saved", wasEnabled, enabled)
	response.JSON(w, http.StatusOK, configsvc.NewPublicView(rec, configsvc.SourcePlatform))
}

// SetPlatformSharing toggles whether tenants may use this configuration.
//
// PATCH /api/v1/admin/configurations/{service}/sharing
func (h *Handler) SetPlatformSharing(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var body struct {
		AllowTenants bool `json:"allow_tenants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	rec, err := h.store.GetPlatform(r.Context(), service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if rec == nil {
		response.Error(w, http.StatusNotFound, "not_found", "configure this service before sharing it")
		return
	}
	if body.AllowTenants && !rec.Enabled {
		response.Error(w, http.StatusBadRequest, "invalid_request",
			"enable this configuration before allowing tenants to use it")
		return
	}
	updated, err := h.store.SetPlatformAllowTenants(r.Context(), service, body.AllowTenants)
	if err != nil {
		writeFailure(w, err)
		return
	}
	action := "Platform Access Revoked for Tenants"
	if body.AllowTenants {
		action = "Platform Access Granted to Tenants"
	}
	h.auditPlatform(r, service, rec.Provider, action, rec.Enabled, rec.Enabled)
	response.JSON(w, http.StatusOK, configsvc.NewPublicView(updated, configsvc.SourcePlatform))
}

// TestConfiguration verifies a platform configuration. With a body it tests the
// submitted values without saving, so an operator can check before committing.
//
// POST /api/v1/admin/configurations/{service}/test
func (h *Handler) TestConfiguration(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	resolved, err := h.probeFor(w, r, service, nil, true)
	if err != nil {
		return
	}
	h.runTest(w, r, service, nil, resolved)
}

// TestConfigurationAction performs a side-effecting check such as sending a
// test email or uploading a probe object.
//
// POST /api/v1/admin/configurations/{service}/test-action
func (h *Handler) TestConfigurationAction(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Action string `json:"action"`
		To     string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}

	action := strings.ToLower(strings.TrimSpace(body.Action))
	switch {
	case service == configsvc.ServiceSMTP && (action == "" || action == "send_email"):
		if strings.TrimSpace(body.To) == "" {
			response.Error(w, http.StatusBadRequest, "invalid_request", "a recipient address is required")
			return
		}
		resolved, err := h.probeFor(w, r, service, nil, true)
		if err != nil {
			return
		}
		provider, perr := h.svc.Factory().Email(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome := configsvc.SendTestMessage(ctx, provider, body.To, "the platform")
		h.recordTestResult(r, service, nil, outcome)
		response.JSON(w, http.StatusOK, outcome)

	case service == configsvc.ServiceStorage && action == "upload":
		resolved, err := h.probeFor(w, r, service, nil, true)
		if err != nil {
			return
		}
		provider, perr := h.svc.Factory().Storage(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome := configsvc.UploadProbe(ctx, provider)
		h.recordTestResult(r, service, nil, outcome)
		response.JSON(w, http.StatusOK, outcome)

	case service == configsvc.ServiceAI && (action == "" || action == "test_model"):
		resolved, err := h.probeFor(w, r, service, nil, true)
		if err != nil {
			return
		}
		provider, perr := h.svc.Factory().AI(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome := configsvc.TestModel(ctx, provider)
		h.recordTestResult(r, service, nil, outcome)
		response.JSON(w, http.StatusOK, outcome)

	default:
		response.Error(w, http.StatusBadRequest, "invalid_request", "unsupported test action for this service")
	}
}

// ListProviders returns the provider catalogue, so the UI never hardcodes it.
//
// GET /api/v1/admin/configurations/{service}/providers
func (h *Handler) ListProviders(w http.ResponseWriter, r *http.Request) {
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"providers": configsvc.ProvidersFor(service),
	})
}

/* ------------------------------------------------------------------ *
 * Super Admin tenant control
 * ------------------------------------------------------------------ */

// ListTenantConfigurations returns every tenant's configuration posture without
// exposing a single secret. Used by the tenant status table.
//
// GET /api/v1/admin/tenants/{tenantId}/configurations
func (h *Handler) ListTenantConfigurations(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantParam(w, r)
	if !ok {
		return
	}
	out, err := h.tenantOverview(r, tenantID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

// SetTenantAccess grants or revokes a tenant's permission to use a platform
// configuration.
//
// PUT /api/v1/admin/tenants/{tenantId}/configurations/{service}/access
func (h *Handler) SetTenantAccess(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantParam(w, r)
	if !ok {
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var body struct {
		AllowPlatform bool `json:"allow_platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	if err := h.store.SetAccess(r.Context(), *tenantID, service, body.AllowPlatform); err != nil {
		writeFailure(w, err)
		return
	}
	action := "Tenant Platform Access Revoked"
	if body.AllowPlatform {
		action = "Tenant Platform Access Granted"
	}
	if h.audit != nil {
		h.audit.AuditConfiguration(r.Context(), tenantID, action, string(service), "")
	}

	// If the tenant is currently pointing at the platform and we just revoked
	// access, its stored preference is now unsatisfiable. Say so rather than
	// silently rewriting the tenant's choice.
	var warning string
	if !body.AllowPlatform {
		pref, err := h.store.GetPreference(r.Context(), *tenantID, service)
		if err == nil && pref.Source == configsvc.SourcePlatform {
			warning = "This organization is currently using the platform configuration and will lose access until it switches back or access is restored."
		}
	}
	out, err := h.tenantOverview(r, tenantID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	payload := out.(map[string]any)
	if warning != "" {
		payload["warning"] = warning
	}
	response.JSON(w, http.StatusOK, payload)
}

// ListAllTenantConfigurations returns the cross-tenant posture table.
//
// GET /api/v1/admin/tenant-configurations
func (h *Handler) ListAllTenantConfigurations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.ListAllTenants(r.Context())
	if err != nil {
		writeFailure(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"tenants": rows,
		"services": []configsvc.ServiceType{
			configsvc.ServiceSMTP, configsvc.ServiceStorage, configsvc.ServiceAI,
		},
	})
}

/* ---------- helpers ---------- */

// tenantOverview builds the per-tenant configuration view.
func (h *Handler) tenantOverview(r *http.Request, tenantID *uuid.UUID) (any, error) {
	records, err := h.store.ListTenant(r.Context(), *tenantID)
	if err != nil {
		return nil, err
	}
	access, err := h.store.ListAccess(r.Context(), *tenantID)
	if err != nil {
		return nil, err
	}
	prefs, err := h.store.ListPreferences(r.Context(), *tenantID)
	if err != nil {
		return nil, err
	}
	platformRecords, err := h.store.ListPlatform(r.Context())
	if err != nil {
		return nil, err
	}

	services := make([]map[string]any, 0, len(configsvc.AllServices))
	for _, service := range configsvc.AllServices {
		view := configsvc.TenantOptions{
			Service:                service,
			Source:                 configsvc.SourceOrganization,
			OrganizationConfigured: records[service] != nil,
			PlatformConfigured:     platformRecords[service] != nil,
		}
		if records[service] != nil {
			view.OrganizationStatus = records[service].Status
		}
		if platformRecords[service] != nil {
			view.PlatformStatus = platformRecords[service].Status
		}
		if pref, ok := prefs[service]; ok {
			view.Source = pref.Source
			view.SourceSet = true
		}
		if grant, ok := access[service]; ok {
			view.PlatformAvailable = grant.AllowPlatform && platformRecords[service] != nil &&
				platformRecords[service].AllowTenants
		}
		services = append(services, map[string]any{
			"service":                 view.Service,
			"source":                  view.Source,
			"source_set":              view.SourceSet,
			"allow_platform":          access[service].AllowPlatform,
			"platform_available":      view.PlatformAvailable,
			"organization_configured": view.OrganizationConfigured,
			"organization_status":     view.OrganizationStatus,
			"platform_configured":     view.PlatformConfigured,
			"platform_status":         view.PlatformStatus,
		})
	}
	return map[string]any{"tenant_id": tenantID.String(), "services": services}, nil
}

func (h *Handler) auditPlatform(r *http.Request, service configsvc.ServiceType, provider, action string, wasEnabled, isEnabled bool) {
	if h.audit == nil {
		return
	}
	// Only the provider and enabled flag are recorded; credentials are not
	// reachable from this package at all.
	h.audit.AuditConfiguration(r.Context(), nil, action, string(service), provider)
	if wasEnabled != isEnabled {
		verb := "Disabled"
		if isEnabled {
			verb = "Enabled"
		}
		h.audit.AuditConfiguration(r.Context(), nil, string(service)+" Configuration "+verb, string(service), provider)
	}
}
