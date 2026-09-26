package confighttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/response"
)

// tenantParam validates the :tenantId URL segment.
func tenantParam(w http.ResponseWriter, r *http.Request) (*uuid.UUID, bool) {
	raw := strings.TrimSpace(chi.URLParam(r, "tenantId"))
	if raw == "" {
		// Fall back to :id so the handler can serve both shapes.
		raw = strings.TrimSpace(chi.URLParam(r, "id"))
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return nil, false
	}
	return &id, true
}

// contextWithTimeout bounds a provider test.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// probeFor builds an unsaved Resolved value from the request body when one is
// present, falling back to the stored configuration otherwise. This is what
// lets an operator test values before committing them.
//
// On error it has already written the response, so callers must simply return.
func (h *Handler) probeFor(
	w http.ResponseWriter, r *http.Request,
	service configsvc.ServiceType,
	tenantID *uuid.UUID,
	allowUnsaved bool,
) (*configsvc.Resolved, error) {
	contextID := h.contextIDFor(service, tenantID)
	source := configsvc.SourceOrganization
	priorSecrets := map[string]string{}

	// Start from what is stored so a masked or partial body keeps the rest.
	var storedConfig map[string]any
	storedProvider := ""
	storedEnabled := false

	if tenantID == nil {
		rec, err := h.store.GetPlatform(r.Context(), service)
		if err != nil {
			writeFailure(w, err)
			return nil, err
		}
		if rec != nil {
			source = configsvc.SourcePlatform
			storedConfig = rec.Config
			storedProvider = rec.Provider
			storedEnabled = rec.Enabled
			priorSecrets = rec.SealedSecrets
		}
	} else {
		rec, err := h.store.GetTenant(r.Context(), *tenantID, service)
		if err != nil {
			writeFailure(w, err)
			return nil, err
		}
		if rec != nil {
			storedConfig = rec.Config
			storedProvider = rec.Provider
			storedEnabled = rec.Enabled
			priorSecrets = rec.SealedSecrets
		}
	}

	merged := map[string]any{}
	for k, v := range storedConfig {
		merged[k] = v
	}
	provider := storedProvider

	if allowUnsaved {
		var body writeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
			return nil, err
		}
		if strings.TrimSpace(body.Provider) != "" {
			provider = body.Provider
		}
		for k, v := range body.Config {
			merged[k] = v
		}
	}

	// Fill the gap where a previous secret exists but the incoming body is
	// empty, so a test can reuse a stored credential.
	secrets := map[string]string{}
	for k, v := range priorSecrets {
		secrets[k] = v
	}
	// An explicit secret in the body wins, sealed for this exact record.
	open := func() (map[string]string, error) {
		out := map[string]string{}
		for k, v := range secrets {
			plain, err := h.box.OpenString(v, contextID)
			if err != nil {
				return nil, err
			}
			if plain != "" {
				out[k] = plain
			}
		}
		return out, nil
	}
	for _, key := range configsvc.SecretKeys(service) {
		raw, ok := merged[key]
		if !ok {
			continue
		}
		str := strings.TrimSpace(toString(raw))
		if str == "" || str == configsvc.MaskSentinel {
			continue
		}
		sealed, err := h.box.SealString(str, contextID)
		if err != nil {
			writeFailure(w, err)
			return nil, err
		}
		secrets[key] = sealed
	}

	write, err := configsvc.NormalizeWriteRequest(service, provider, merged)
	if err != nil {
		writeFailure(w, err)
		return nil, err
	}
	// A test may run against a configuration that is saved but switched off.
	if !write.Enabled && storedEnabled {
		write.Enabled = true
	}

	resolved := &configsvc.Resolved{
		Service:  service,
		Source:   source,
		Provider: write.Provider,
		Config:   write.Config,
		TenantID: tenantID,
	}
	opened, err := open()
	if err != nil {
		writeFailure(w, err)
		return nil, err
	}
	resolved.Secrets = opened
	return resolved, nil
}

// contextID is the AAD a secret for this record must be sealed with.
func (h *Handler) contextIDFor(service configsvc.ServiceType, tenantID *uuid.UUID) string {
	if tenantID == nil {
		return configsvc.PlatformContextID(service)
	}
	return configsvc.TenantContextID(*tenantID, service)
}

// runTest performs a connection-only test and records the outcome.
func (h *Handler) runTest(
	w http.ResponseWriter, r *http.Request,
	service configsvc.ServiceType,
	tenantID *uuid.UUID,
	resolved *configsvc.Resolved,
) {
	ctx, cancel := contextWithTimeout(r, testTimeout)
	defer cancel()

	outcome, err := h.svc.TestConnection(ctx, resolved)
	if err != nil {
		writeFailure(w, err)
		return
	}
	h.recordTestResult(r, service, tenantID, outcome)
	response.JSON(w, http.StatusOK, outcome)
}

// recordTestResult persists the outcome so the status badge survives a reload.
func (h *Handler) recordTestResult(
	r *http.Request, service configsvc.ServiceType,
	tenantID *uuid.UUID, outcome configsvc.TestOutcome,
) {
	status := configsvc.StatusConnectionFailed
	if outcome.OK {
		status = configsvc.StatusEnabled
	}
	var err error
	if tenantID == nil {
		err = h.store.RecordStatus(r.Context(), service, status, outcome.Detail)
	} else {
		err = h.store.RecordTenantStatus(r.Context(), *tenantID, service, status, outcome.Detail)
	}
	if err != nil && h.log != nil {
		h.log.Warn("could not record configuration test result",
			"service", service, "error", err.Error())
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
