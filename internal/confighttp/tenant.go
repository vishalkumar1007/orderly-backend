package confighttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/response"
)

// TenantIDFunc supplies the authenticated caller's tenant. The HTTP layer never
// reads a tenant id from the request, so a tenant admin cannot address another
// tenant's configuration by forging a path or body.
type TenantIDFunc func(r *http.Request) *uuid.UUID

// ListTenantServices returns every service's posture for the calling tenant,
// including whether the platform option is selectable.
//
// GET /api/v1/tenant/configurations
func (h *Handler) ListTenantServices(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	records, err := h.store.ListTenant(r.Context(), *id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	prefs, err := h.store.ListPreferences(r.Context(), *id)
	if err != nil {
		writeFailure(w, err)
		return
	}

	out := make([]map[string]any, 0, len(configsvc.AllServices))
	for _, service := range configsvc.AllServices {
		opts, err := h.svc.Resolver().OptionsFor(r.Context(), *id, service)
		if err != nil {
			writeFailure(w, err)
			return
		}
		if pref, ok := prefs[service]; ok {
			opts.Source = pref.Source
			opts.SourceSet = true
		}
		if rec, ok := records[service]; ok {
			opts.OrganizationConfigured = true
			opts.OrganizationStatus = rec.Status
		}

		// The tenant's own values are shown only for the organization level, and
		// only as safe metadata.
		own := configsvc.NotConfiguredView(service, configsvc.SourceOrganization)
		if rec, ok := records[service]; ok {
			own = configsvc.NewPublicView(rec, configsvc.SourceOrganization)
		}
		out = append(out, map[string]any{
			"options":    opts,
			"own_config": own,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"services":   out,
		"encryption": map[string]any{"enabled": !h.box.Disabled()},
	})
}

// GetTenantService returns one service's configuration view for the tenant,
// including the source selector options.
//
// GET /api/v1/tenant/configurations/{service}
func (h *Handler) GetTenantService(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	opts, err := h.svc.Resolver().OptionsFor(r.Context(), *id, service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	rec, err := h.store.GetTenant(r.Context(), *id, service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	own := configsvc.NotConfiguredView(service, configsvc.SourceOrganization)
	if rec != nil {
		own = configsvc.NewPublicView(rec, configsvc.SourceOrganization)
	}

	// A tenant pointed at the platform is told the effective level and, when the
	// platform is unusable, exactly why — never a silent substitution.
	payload := map[string]any{
		"service":    service,
		"options":    opts,
		"own_config": own,
		"in_use":     opts.Source == configsvc.SourcePlatform && opts.PlatformAvailable,
		"source":     opts.Source,
	}
	if opts.Source == configsvc.SourcePlatform && !opts.PlatformAvailable {
		payload["unavailable_reason"] = opts.PlatformReason
		payload["hint"] = (&configsvc.UnavailableError{
			Service: service, Source: configsvc.SourcePlatform, Reason: opts.PlatformReason,
		}).Hint()
	}
	response.JSON(w, http.StatusOK, payload)
}

// PutTenantService saves the tenant's own configuration for a service.
//
// PUT /api/v1/tenant/configurations/{service}
func (h *Handler) PutTenantService(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	existing, err := h.store.GetTenant(r.Context(), *id, service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	existingEnabled := existing != nil && existing.Enabled
	wasEnabled := existingEnabled

	write, enabled, err := validateRequest(service, req, existingEnabled)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if h.box.Disabled() && configsvc.HasSecretInput(service, req.Config) {
		response.Error(w, http.StatusServiceUnavailable, "encryption_unavailable",
			"CONFIG_ENCRYPTION_KEY is not set, so secrets cannot be stored safely. Contact your platform administrator.")
		return
	}

	rec, err := h.store.SaveTenant(r.Context(), *id, write)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if wasEnabled != enabled {
		verb := "Disabled"
		if enabled {
			verb = "Enabled"
		}
		if h.audit != nil {
			h.audit.AuditConfiguration(r.Context(), id, "Organization "+string(service)+" Configuration "+verb, string(service), rec.Provider)
		}
	}
	if h.audit != nil {
		h.audit.AuditConfiguration(r.Context(), id, "Organization Configuration Saved", string(service), rec.Provider)
	}
	response.JSON(w, http.StatusOK, configsvc.NewPublicView(rec, configsvc.SourceOrganization))
}

// DeleteTenantService removes the tenant's own configuration. A tenant that is
// using the platform configuration cannot delete it, because that would silently
// change nothing while making the source meaningless.
//
// DELETE /api/v1/tenant/configurations/{service}
func (h *Handler) DeleteTenantService(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteTenant(r.Context(), *id, service); err != nil {
		writeFailure(w, err)
		return
	}
	if h.audit != nil {
		h.audit.AuditConfiguration(r.Context(), id, "Organization Configuration Deleted", string(service), "")
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// TestTenantService verifies the tenant's own configuration, or the submitted
// values without saving.
//
// POST /api/v1/tenant/configurations/{service}/test
func (h *Handler) TestTenantService(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	resolved, err := h.probeFor(w, r, service, id, true)
	if err != nil {
		return
	}
	h.runTest(w, r, service, id, resolved)
}

// TestTenantServiceAction performs a side-effecting check.
//
// POST /api/v1/tenant/configurations/{service}/test-action
func (h *Handler) TestTenantServiceAction(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
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
	resolved, err := h.probeFor(w, r, service, id, true)
	if err != nil {
		return
	}
	label := "your organization"
	if resolved.Source == configsvc.SourcePlatform {
		label = "your platform administrator"
	}

	action := strings.ToLower(strings.TrimSpace(body.Action))
	var outcome configsvc.TestOutcome
	switch {
	case service == configsvc.ServiceSMTP && (action == "" || action == "send_email"):
		provider, perr := h.svc.Factory().Email(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome = configsvc.SendTestMessage(ctx, provider, body.To, label)

	case service == configsvc.ServiceStorage && action == "upload":
		provider, perr := h.svc.Factory().Storage(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome = configsvc.UploadProbe(ctx, provider)

	case service == configsvc.ServiceAI && (action == "" || action == "test_model"):
		provider, perr := h.svc.Factory().AI(resolved)
		if perr != nil {
			writeFailure(w, perr)
			return
		}
		ctx, cancel := contextWithTimeout(r, testTimeout)
		defer cancel()
		outcome = configsvc.TestModel(ctx, provider)

	default:
		response.Error(w, http.StatusBadRequest, "invalid_request", "unsupported test action for this service")
		return
	}
	h.recordTestResult(r, service, id, outcome)
	response.JSON(w, http.StatusOK, outcome)
}

/* ---------- preferences ---------- */

// ListPreferences returns the tenant's elected source for every service.
//
// GET /api/v1/tenant/service-preferences
func (h *Handler) ListPreferences(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	out := []map[string]any{}
	for _, service := range configsvc.AllServices {
		opts, err := h.svc.Resolver().OptionsFor(r.Context(), *id, service)
		if err != nil {
			writeFailure(w, err)
			return
		}
		out = append(out, map[string]any{
			"service":            service,
			"source":             opts.Source,
			"source_set":         opts.SourceSet,
			"platform_available": opts.PlatformAvailable,
			"platform_reason":    opts.PlatformReason,
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"preferences": out})
}

// SetPreference records which level the tenant will use.
//
// PUT /api/v1/tenant/service-preferences/{service}
func (h *Handler) SetPreference(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	source, err := configsvc.ParseSource(body.Source)
	if err != nil {
		writeFailure(w, err)
		return
	}

	// Refuse a source the resolver would reject, rather than storing a
	// preference that can never be honoured.
	if source == configsvc.SourcePlatform {
		perm, err := h.svc.Resolver().PlatformPermitted(r.Context(), *id, service)
		if err != nil {
			writeFailure(w, err)
			return
		}
		if !perm.Allowed {
			response.JSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{
					"code":    "platform_not_available",
					"message": perm.Reason,
					"hint":    "Ask your platform administrator to enable platform " + string(service) + " for your organization.",
				},
			})
			return
		}
	}

	previous, err := h.store.GetPreference(r.Context(), *id, service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if err := h.store.SetPreference(r.Context(), *id, service, source); err != nil {
		writeFailure(w, err)
		return
	}
	if h.audit != nil && previous.Source != source {
		h.audit.AuditConfiguration(r.Context(), id, "Configuration Source Changed to "+string(source), string(service), "")
	}

	opts, err := h.svc.Resolver().OptionsFor(r.Context(), *id, service)
	if err != nil {
		writeFailure(w, err)
		return
	}
	response.JSON(w, http.StatusOK, opts)
}

/* ---------- effective configuration ---------- */

// GetEffective reports which configuration the tenant will actually use, and
// why, without exposing any value. This is what the UI's "current
// configuration" panel reads.
//
// GET /api/v1/tenant/configurations/{service}/effective
func (h *Handler) GetEffective(w http.ResponseWriter, r *http.Request, tenantID TenantIDFunc) {
	id := tenantID(r)
	if id == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "no tenant is associated with this account")
		return
	}
	service, ok := serviceParam(w, r)
	if !ok {
		return
	}
	resolved, err := h.svc.Resolver().Resolve(r.Context(), configsvc.ScopeTenant, id, service)
	if err != nil {
		var unavailable *configsvc.UnavailableError
		if errors.As(err, &unavailable) {
			response.JSON(w, http.StatusOK, map[string]any{
				"service":   service,
				"available": false,
				"source":    unavailable.Source,
				"reason":    unavailable.Reason,
				"hint":      unavailable.Hint(),
			})
			return
		}
		writeFailure(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"service":   service,
		"available": true,
		"source":    resolved.Source,
		"provider":  resolved.Provider,
		"status":    resolved.Status,
	})
}
