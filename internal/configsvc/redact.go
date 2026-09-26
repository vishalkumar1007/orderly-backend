package configsvc

import (
	"strings"
)

// PublicView is the only configuration shape that may reach an HTTP response.
// It carries display metadata and boolean has_* flags, never values.
//
// A test asserts that marshalling a PublicView cannot produce any key named
// like a secret, so adding a secret field to a config struct cannot silently
// start leaking it.
type PublicView struct {
	Service ServiceType `json:"service"`
	Label   string      `json:"label"`
	// Scope is PLATFORM for platform rows, ORGANIZATION for tenant rows.
	Scope Source `json:"scope"`
	// Provider is the selected provider id.
	Provider string `json:"provider"`
	// Config holds non-secret fields only.
	Config map[string]any `json:"config"`
	// HasSecret is true when the service's secret field is stored.
	HasSecret bool `json:"has_secret"`
	// HasAnySecret is true when at least one secret of any name is stored.
	HasAnySecret bool `json:"has_any_secret"`
	// Status is the lifecycle badge.
	Status Status `json:"status"`
	// Enabled reports whether the row is switched on.
	Enabled bool `json:"enabled"`
	// AllowTenants is only meaningful at platform scope.
	AllowTenants bool `json:"allow_tenants"`
	// LastError is the message from the most recent failed test.
	LastError string `json:"last_error,omitempty"`
	// LastTestedAt is RFC3339 or "" when never tested.
	LastTestedAt string `json:"last_tested_at,omitempty"`
	// SecretFields names the secret keys, so the UI knows which inputs to mask
	// without being told their values.
	SecretFields []string `json:"secret_fields"`
	// Providers is the catalogue for this service.
	Providers []ProviderInfo `json:"providers"`
	// UpdatedAt is RFC3339.
	UpdatedAt string `json:"updated_at"`
}

// NewPublicView projects a stored Record into its safe form. The secrets are
// represented only by their presence.
func NewPublicView(rec *Record, scope Source) PublicView {
	service := rec.ServiceType
	view := PublicView{
		Service:      service,
		Label:        service.Label(),
		Scope:        scope,
		Provider:     rec.Provider,
		Config:       redactConfig(service, rec.Config),
		HasSecret:    rec.HasSecret(primarySecretKey(service)),
		HasAnySecret: rec.HasAnySecret(),
		Status:       rec.Status,
		Enabled:      rec.Enabled,
		AllowTenants: rec.AllowTenants,
		LastError:    rec.LastError,
		SecretFields: secretFields[service],
		Providers:    ProvidersFor(service),
		UpdatedAt:    rec.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if rec.LastTestedAt != nil {
		view.LastTestedAt = rec.LastTestedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

// NotConfiguredView is what an absent row looks like, so the UI can render one
// shape for both cases.
func NotConfiguredView(service ServiceType, scope Source) PublicView {
	return PublicView{
		Service:      service,
		Label:        service.Label(),
		Scope:        scope,
		Config:       map[string]any{},
		Status:       StatusUnconfigured,
		SecretFields: secretFields[service],
		Providers:    ProvidersFor(service),
	}
}

// primarySecretKey is the one secret field whose presence drives HasSecret.
func primarySecretKey(service ServiceType) string {
	if fields := secretFields[service]; len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// redactConfig returns a copy of cfg with every secret key removed. It is
// belt-and-braces: secrets are stripped before they are written, so a stored
// config should never contain one, but this guarantees a leak cannot happen if
// a row predates that rule.
func redactConfig(service ServiceType, cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		// Check the union rather than this service's own list, so a foreign or
		// mislabelled secret key cannot survive into a response.
		if isSecretName(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// allSecretFieldNames is the union of every secret key across every service.
//
// Redaction uses the union rather than the current service's list. A row should
// never hold a foreign secret, but if one does — a bug, a hand-edited row, or a
// config saved under the wrong service — the response still must not carry it.
// Over-redacting a stray key costs nothing; under-redacting a credential is a
// disclosure.
var allSecretFieldNames = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, fields := range secretFields {
		for _, f := range fields {
			m[f] = struct{}{}
		}
	}
	return m
}()

// isSecretName matches a config key against the union of secret names, ignoring
// case and surrounding space so "Password" and " api_key " are both caught.
func isSecretName(key string) bool {
	normalised := strings.ToLower(strings.TrimSpace(key))
	if _, ok := allSecretFieldNames[normalised]; ok {
		return true
	}
	// Also catch compound names such as "smtp_password" or "openaiApiKey".
	for name := range allSecretFieldNames {
		if strings.Contains(normalised, name) {
			return true
		}
	}
	return false
}

// maskSecrets replaces secret values in a config map with MaskSentinel. The
// console uses it to build an editable form: a masked field round-trips as
// "unchanged" instead of blanking the stored secret.
func maskSecrets(service ServiceType, cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg)+len(secretFields[service]))
	for k, v := range cfg {
		out[k] = v
	}
	for _, key := range secretFields[service] {
		out[key] = MaskSentinel
	}
	return out
}

// TestActionRequest asks a provider to perform a side-effecting check, such as
// sending a test email or uploading a probe object.
type TestActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// SafeErrorMessage strips anything that looks like a credential out of a
// provider error before it is shown or stored. Provider libraries occasionally
// echo the request URL or headers back in an error string.
func SafeErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, key := range []string{"password", "secret_key", "api_key", "token", "authorization"} {
		if idx := strings.Index(strings.ToLower(msg), key); idx >= 0 {
			// Truncate from the first credential-ish token rather than trying to
			// reconstruct the sentence around it.
			msg = msg[:idx] + "[redacted]"
			break
		}
	}
	return truncate(msg, 300)
}
