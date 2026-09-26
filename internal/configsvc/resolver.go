package configsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Resolved is an effective configuration with secrets decrypted. It is
// backend-only: handlers must never serialise it, and providers are the only
// intended consumers.
type Resolved struct {
	Service  ServiceType
	Source   Source
	Provider string
	Config   map[string]any
	Secrets  map[string]string
	// Status mirrors the stored row, so callers can distinguish "not
	// configured" from "configured but disabled".
	Status Status
	// TenantID is nil for the platform scope.
	TenantID *uuid.UUID
}

// Secret returns a decrypted secret, or "" when absent.
func (r *Resolved) Secret(key string) string {
	if r == nil || r.Secrets == nil {
		return ""
	}
	return r.Secrets[key]
}

// String reads a config field as a string.
func (r *Resolved) String(key string) string {
	if r == nil || r.Config == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", r.Config[key]))
}

// Int reads a config field as an int, falling back when absent or unparseable.
func (r *Resolved) Int(key string, fallback int) int {
	if r == nil || r.Config == nil {
		return fallback
	}
	switch v := r.Config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return fallback
}

// Bool reads a config field as a bool, falling back when absent.
func (r *Resolved) Bool(key string, fallback bool) bool {
	if r == nil || r.Config == nil {
		return fallback
	}
	if v, ok := r.Config[key].(bool); ok {
		return v
	}
	return fallback
}

// Resolver turns a tenant's preference plus the stored rows into the one
// configuration that should be used. It is the only component that decides
// which level applies.
type Resolver struct {
	store *Store
	log   Logger
}

// NewResolver builds a Resolver over a Store.
func NewResolver(store *Store, log Logger) *Resolver {
	return &Resolver{store: store, log: log}
}

// Scope identifies who is asking, which decides whether platform-only callers
// are allowed to resolve without a tenant.
type Scope int

const (
	// ScopePlatform resolves the platform configuration directly. Used by
	// platform services such as tenant invitations.
	ScopePlatform Scope = iota
	// ScopeTenant resolves for a tenant, honouring its preference and
	// permission. Used by everything tenant-scoped.
	ScopeTenant
)

// Resolve returns the effective configuration.
//
// Precedence, with no silent fallback between levels:
//
//	tenant selected ORGANIZATION -> tenant row, if present and enabled
//	tenant selected PLATFORM     -> platform row, but only when the platform
//	                                allows tenants *and* this tenant is granted
//	otherwise                    -> UnavailableError explaining why
//
// A nil tenantID is only honoured for ScopePlatform.
func (r *Resolver) Resolve(ctx context.Context, scope Scope, tenantID *uuid.UUID, service ServiceType) (*Resolved, error) {
	if scope == ScopePlatform || tenantID == nil {
		return r.resolvePlatform(ctx, service)
	}
	return r.resolveForTenant(ctx, *tenantID, service)
}

func (r *Resolver) resolvePlatform(ctx context.Context, service ServiceType) (*Resolved, error) {
	rec, err := r.store.GetPlatform(ctx, service)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, &UnavailableError{
			Service: service, Source: SourcePlatform,
			Reason: "no platform configuration has been saved yet",
		}
	}
	if !rec.Enabled {
		return nil, &UnavailableError{
			Service: service, Source: SourcePlatform,
			Reason: "the platform configuration exists but is disabled",
		}
	}
	return r.hydrate(ctx, rec, SourcePlatform, nil, platformContext(service))
}

func (r *Resolver) resolveForTenant(ctx context.Context, tenantID uuid.UUID, service ServiceType) (*Resolved, error) {
	pref, err := r.store.GetPreference(ctx, tenantID, service)
	if err != nil {
		return nil, err
	}

	if pref.Source == SourcePlatform {
		perm, err := r.PlatformPermitted(ctx, tenantID, service)
		if err != nil {
			return nil, err
		}
		if !perm.Allowed {
			return nil, &UnavailableError{
				Service: service, Source: SourcePlatform, Reason: perm.Reason,
			}
		}
		rec, err := r.store.GetPlatform(ctx, service)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, &UnavailableError{
				Service: service, Source: SourcePlatform,
				Reason: "no platform configuration has been saved yet",
			}
		}
		if !rec.Enabled {
			return nil, &UnavailableError{
				Service: service, Source: SourcePlatform,
				Reason: "the platform administrator has disabled this configuration",
			}
		}
		return r.hydrate(ctx, rec, SourcePlatform, &tenantID, platformContext(service))
	}

	// Default and explicit ORGANIZATION both read the tenant's own row.
	rec, err := r.store.GetTenant(ctx, tenantID, service)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, &UnavailableError{
			Service: service, Source: SourceOrganization,
			Reason: "no organization configuration has been saved yet",
		}
	}
	if !rec.Enabled {
		return nil, &UnavailableError{
			Service: service, Source: SourceOrganization,
			Reason: "the organization configuration exists but is disabled",
		}
	}
	return r.hydrate(ctx, rec, SourceOrganization, &tenantID, tenantContext(tenantID, service))
}

func (r *Resolver) hydrate(ctx context.Context, rec *Record, source Source, tenantID *uuid.UUID, ctxID string) (*Resolved, error) {
	secrets, err := r.store.OpenSecrets(rec, ctxID)
	if err != nil {
		// A key mismatch means the encryption key changed. Fail loudly rather
		// than silently serving a broken configuration.
		return nil, fmt.Errorf("decrypt %s configuration: %w", rec.ServiceType, err)
	}
	return &Resolved{
		Service:  rec.ServiceType,
		Source:   source,
		Provider: rec.Provider,
		Config:   rec.Config,
		Secrets:  secrets,
		Status:   rec.Status,
		TenantID: tenantID,
	}, nil
}

/* ---------- permission ---------- */

// Permission is the effective answer to "may this tenant use the platform
// configuration for this service?".
type Permission struct {
	Allowed bool
	Reason  string
	// GlobalAllowed is the platform-wide switch.
	GlobalAllowed bool
	// TenantGranted is the per-tenant grant.
	TenantGranted bool
	// PlatformConfigured is whether a platform row exists at all.
	PlatformConfigured bool
	// PlatformEnabled is whether that row is enabled.
	PlatformEnabled bool
}

// PlatformPermitted combines the platform-wide switch with the per-tenant
// grant. Both must be true, and a usable platform row must exist, before a
// tenant may select the platform configuration. The default is deny.
func (r *Resolver) PlatformPermitted(ctx context.Context, tenantID uuid.UUID, service ServiceType) (Permission, error) {
	rec, err := r.store.GetPlatform(ctx, service)
	if err != nil {
		return Permission{}, err
	}
	access, err := r.store.GetAccess(ctx, tenantID, service)
	if err != nil {
		return Permission{}, err
	}

	p := Permission{
		GlobalAllowed:      rec != nil && rec.AllowTenants,
		TenantGranted:      access.AllowPlatform,
		PlatformConfigured: rec != nil,
		PlatformEnabled:    rec != nil && rec.Enabled,
	}
	switch {
	case !p.PlatformConfigured:
		p.Reason = "the platform administrator has not configured this service yet"
	case !p.GlobalAllowed:
		p.Reason = "the platform administrator has not enabled platform " + string(service) + " for tenants"
	case !p.TenantGranted:
		p.Reason = "your organization has not been granted access to the platform " + string(service) + " configuration"
	}
	p.Allowed = p.GlobalAllowed && p.TenantGranted && p.PlatformConfigured
	return p, nil
}

// TenantOptions is everything a tenant's configuration page needs to render its
// source selector. It contains no secret material.
type TenantOptions struct {
	Service ServiceType `json:"service"`
	// Source the tenant has elected.
	Source Source `json:"source"`
	// SourceSet is false when the tenant has never chosen, in which case
	// Source holds the default (ORGANIZATION).
	SourceSet bool `json:"source_set"`
	// PlatformAvailable is true when the platform option is selectable.
	PlatformAvailable bool `json:"platform_available"`
	// PlatformReason explains why it is not selectable.
	PlatformReason string `json:"platform_reason,omitempty"`
	// OrganizationConfigured reports whether the tenant has its own row.
	OrganizationConfigured bool `json:"organization_configured"`
	// OrganizationStatus is the tenant row's status.
	OrganizationStatus Status `json:"organization_status"`
	// PlatformConfigured reports whether a platform row exists.
	PlatformConfigured bool `json:"platform_configured"`
	// PlatformStatus is the platform row's status.
	PlatformStatus Status `json:"platform_status"`
}

// OptionsFor builds the tenant-facing view of a service's configuration
// choices. A tenant may only be *shown* the platform option when it is also
// permitted to select it, so the UI cannot offer a choice the resolver would
// reject.
func (r *Resolver) OptionsFor(ctx context.Context, tenantID uuid.UUID, service ServiceType) (TenantOptions, error) {
	pref, err := r.store.GetPreference(ctx, tenantID, service)
	if err != nil {
		return TenantOptions{}, err
	}
	perm, err := r.PlatformPermitted(ctx, tenantID, service)
	if err != nil {
		return TenantOptions{}, err
	}
	own, err := r.store.GetTenant(ctx, tenantID, service)
	if err != nil {
		return TenantOptions{}, err
	}

	opts := TenantOptions{
		Service:                service,
		Source:                 pref.Source,
		SourceSet:              pref.Set,
		OrganizationConfigured: own != nil,
		PlatformConfigured:     perm.PlatformConfigured,
	}
	// Availability has to mean "actually usable", not merely "permitted". A
	// granted tenant whose platform row has been switched off cannot use it, so
	// the UI must not offer the option as though it could — otherwise the page
	// would show the platform as active while the backend refuses to use it.
	opts.PlatformAvailable = perm.Allowed && perm.PlatformEnabled
	if !opts.PlatformAvailable && perm.Reason == "" {
		perm.Reason = "the platform configuration exists but is disabled"
	}
	opts.PlatformReason = perm.Reason
	if own != nil {
		opts.OrganizationStatus = own.Status
	}
	if perm.PlatformConfigured {
		if rec, err := r.store.GetPlatform(ctx, service); err == nil && rec != nil {
			opts.PlatformStatus = rec.Status
		}
	}
	return opts, nil
}
