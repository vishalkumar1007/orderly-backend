package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// MFA modes, enforcement scopes and method identifiers — a closed set, TEXT
// columns rather than Postgres enums, matching this codebase's convention.
const (
	MFAModeDisabled = "DISABLED"
	MFAModeOptional = "OPTIONAL"
	MFAModeRequired = "REQUIRED"

	MFAScopeAllAdmins     = "ALL_ADMINS"
	MFAScopeSelectedRoles = "SELECTED_ROLES"

	MethodTOTP     = "TOTP"
	MethodEmailOTP = "EMAIL_OTP"
)

// MFAPolicy is the resolved shape shared by tenant and platform scope — same
// fields, same meaning, just a different source (tenant_mfa_policies vs the
// platform settings JSON blob, since internal/platform already imports this
// package and the reverse import would be circular).
type MFAPolicy struct {
	Mode            string
	AllowedMethods  []string
	EnforceScope    string
	EnforceRoles    []string
	GracePeriodDays int
	UpdatedAt       time.Time
}

func defaultMFAPolicy() MFAPolicy {
	return MFAPolicy{
		Mode:            MFAModeDisabled,
		AllowedMethods:  []string{MethodTOTP},
		EnforceScope:    MFAScopeAllAdmins,
		GracePeriodDays: 7,
	}
}

// GetTenantMFAPolicy returns a tenant's saved policy, or the default
// (DISABLED) when it has never set one.
func (s *Service) GetTenantMFAPolicy(ctx context.Context, tenantID uuid.UUID) (MFAPolicy, error) {
	row, err := s.q.GetTenantMfaPolicy(ctx, pgutil.UUID(tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return defaultMFAPolicy(), nil
		}
		return MFAPolicy{}, err
	}
	return MFAPolicy{
		Mode: row.Mode, AllowedMethods: row.AllowedMethods, EnforceScope: row.EnforceScope,
		EnforceRoles: row.EnforceRoles, GracePeriodDays: int(row.GracePeriodDays), UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

// PutTenantMFAPolicy saves a tenant's policy. Callers (the tenant-admin HTTP
// handler, and tenant creation) are responsible for checking
// tenants.mfa_allowed first — this function writes whatever it is given.
func (s *Service) PutTenantMFAPolicy(ctx context.Context, tenantID uuid.UUID, p MFAPolicy) (MFAPolicy, error) {
	row, err := s.q.UpsertTenantMfaPolicy(ctx, sqlc.UpsertTenantMfaPolicyParams{
		TenantID: pgutil.UUID(tenantID), Mode: p.Mode, AllowedMethods: p.AllowedMethods,
		EnforceScope: p.EnforceScope, EnforceRoles: p.EnforceRoles, GracePeriodDays: int32(p.GracePeriodDays),
	})
	if err != nil {
		return MFAPolicy{}, err
	}
	return MFAPolicy{
		Mode: row.Mode, AllowedMethods: row.AllowedMethods, EnforceScope: row.EnforceScope,
		EnforceRoles: row.EnforceRoles, GracePeriodDays: int(row.GracePeriodDays), UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

// platformMFAConfig mirrors just the fields this package needs out of
// internal/platform's storedPlatformConfig JSON blob — the same pattern
// passwordMin already uses to read password_min_length, so this package
// never has to import internal/platform (which imports this one).
type platformMFAConfig struct {
	MFAMode            string   `json:"mfa_mode"`
	MFAAllowedMethods  []string `json:"mfa_allowed_methods"`
	MFAEnforceScope    string   `json:"mfa_enforce_scope"`
	MFAEnforceRoles    []string `json:"mfa_enforce_roles"`
	MFAGracePeriodDays int      `json:"mfa_grace_period_days"`
}

// GetPlatformMFAPolicy returns the console's own policy — governs console
// accounts (SuperAdmin/PlatformAdmin/Support), never tenant staff.
func (s *Service) GetPlatformMFAPolicy(ctx context.Context) (MFAPolicy, error) {
	row, err := s.q.GetPlatformSettings(ctx)
	if err != nil || len(row.Config) == 0 {
		return defaultMFAPolicy(), nil
	}
	var cfg platformMFAConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return defaultMFAPolicy(), nil
	}
	p := defaultMFAPolicy()
	if cfg.MFAMode != "" {
		p.Mode = cfg.MFAMode
	}
	if len(cfg.MFAAllowedMethods) > 0 {
		p.AllowedMethods = cfg.MFAAllowedMethods
	}
	if cfg.MFAEnforceScope != "" {
		p.EnforceScope = cfg.MFAEnforceScope
	}
	p.EnforceRoles = cfg.MFAEnforceRoles
	if cfg.MFAGracePeriodDays > 0 {
		p.GracePeriodDays = cfg.MFAGracePeriodDays
	}
	p.UpdatedAt = row.UpdatedAt.Time
	return p, nil
}

// policyForUser resolves the policy that governs one user: platform policy
// for a console account, tenant policy for tenant staff — and for a tenant,
// nil availability when the Super Admin has not granted mfa_allowed, since no
// tenant policy matters at all until that gate is open.
func (s *Service) policyForUser(ctx context.Context, user sqlc.User) (policy MFAPolicy, available bool, err error) {
	if !user.TenantID.Valid {
		policy, err = s.GetPlatformMFAPolicy(ctx)
		return policy, true, err
	}
	if !s.tenantAllowsMFA(ctx, user.TenantID) {
		return MFAPolicy{}, false, nil
	}
	policy, err = s.GetTenantMFAPolicy(ctx, uuid.UUID(user.TenantID.Bytes))
	if err != nil {
		return MFAPolicy{}, false, err
	}
	return policy, policy.Mode != MFAModeDisabled, nil
}

// mustEnrollNow reports whether a user with zero enrolled methods must be
// routed into the forced-enrollment flow right now, rather than getting a
// normal session. False whenever: MFA isn't permitted/enabled for them,
// policy mode isn't REQUIRED, their role is outside enforce_roles under
// SELECTED_ROLES scope, or the grace period hasn't elapsed yet.
//
// The grace period is anchored at max(policy.UpdatedAt, user.CreatedAt): a
// user added after the policy already flipped to REQUIRED still gets the
// full window, rather than being anchored to a policy change that predates
// them.
func (s *Service) mustEnrollNow(ctx context.Context, user sqlc.User) (bool, error) {
	policy, available, err := s.policyForUser(ctx, user)
	if err != nil || !available {
		return false, err
	}
	if policy.Mode != MFAModeRequired {
		return false, nil
	}
	if policy.EnforceScope == MFAScopeSelectedRoles && !roleInList(user.Role, policy.EnforceRoles) {
		return false, nil
	}
	anchor := policy.UpdatedAt
	if user.CreatedAt.Time.After(anchor) {
		anchor = user.CreatedAt.Time
	}
	deadline := anchor.Add(time.Duration(policy.GracePeriodDays) * 24 * time.Hour)
	return !time.Now().Before(deadline), nil
}

func roleInList(role string, roles []string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func policyJSON(p MFAPolicy) map[string]any {
	return map[string]any{
		"mode":              p.Mode,
		"allowed_methods":   p.AllowedMethods,
		"enforce_scope":     p.EnforceScope,
		"enforce_roles":     p.EnforceRoles,
		"grace_period_days": p.GracePeriodDays,
	}
}

var validMFAModes = map[string]bool{MFAModeDisabled: true, MFAModeOptional: true, MFAModeRequired: true}
var validMFAScopes = map[string]bool{MFAScopeAllAdmins: true, MFAScopeSelectedRoles: true}
var validMFAMethods = map[string]bool{MethodTOTP: true, MethodEmailOTP: true}

/* ------------------------------------------------------------------ *
 * Tenant self-service HTTP handlers — a Tenant Admin's own policy, within
 * whatever the Super Admin's tenants.mfa_allowed permits. Gated at the
 * route level (canOrg) in internal/httpserver/server.go.
 * ------------------------------------------------------------------ */

func (s *Service) HandleGetTenantMFAPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok || actor.TenantID == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	policy, err := s.GetTenantMFAPolicy(r.Context(), *actor.TenantID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load mfa policy")
		return
	}
	out := policyJSON(policy)
	out["allowed"] = s.tenantAllowsMFA(r.Context(), pgutil.UUID(*actor.TenantID))
	response.JSON(w, http.StatusOK, out)
}

type putMFAPolicyRequest struct {
	Mode            string   `json:"mode"`
	AllowedMethods  []string `json:"allowed_methods"`
	EnforceScope    string   `json:"enforce_scope"`
	EnforceRoles    []string `json:"enforce_roles"`
	GracePeriodDays int      `json:"grace_period_days"`
}

func (s *Service) HandlePutTenantMFAPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok || actor.TenantID == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	if !s.tenantAllowsMFA(r.Context(), pgutil.UUID(*actor.TenantID)) {
		response.Error(w, http.StatusForbidden, "mfa_not_allowed", "this business has not been granted MFA access")
		return
	}
	var req putMFAPolicyRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Mode = strings.ToUpper(strings.TrimSpace(req.Mode))
	req.EnforceScope = strings.ToUpper(strings.TrimSpace(req.EnforceScope))
	if !validMFAModes[req.Mode] {
		response.Error(w, http.StatusBadRequest, "invalid_request", "mode must be DISABLED, OPTIONAL, or REQUIRED")
		return
	}
	if !validMFAScopes[req.EnforceScope] {
		response.Error(w, http.StatusBadRequest, "invalid_request", "enforce_scope must be ALL_ADMINS or SELECTED_ROLES")
		return
	}
	if len(req.AllowedMethods) == 0 {
		response.Error(w, http.StatusBadRequest, "invalid_request", "at least one allowed method is required")
		return
	}
	for i, m := range req.AllowedMethods {
		req.AllowedMethods[i] = strings.ToUpper(strings.TrimSpace(m))
		if !validMFAMethods[req.AllowedMethods[i]] {
			response.Error(w, http.StatusBadRequest, "invalid_request", "allowed_methods may only contain TOTP or EMAIL_OTP")
			return
		}
	}
	if req.GracePeriodDays < 0 {
		req.GracePeriodDays = 0
	}
	policy, err := s.PutTenantMFAPolicy(r.Context(), *actor.TenantID, MFAPolicy{
		Mode: req.Mode, AllowedMethods: req.AllowedMethods, EnforceScope: req.EnforceScope,
		EnforceRoles: req.EnforceRoles, GracePeriodDays: req.GracePeriodDays,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to save mfa policy")
		return
	}
	_, _ = s.q.InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		TenantID: pgutil.UUID(*actor.TenantID), UserID: pgutil.UUID(actor.ID), Action: "mfa.policy_changed",
		EntityType: "tenant", EntityID: pgutil.UUID(*actor.TenantID), Result: auditSuccess,
	})
	if tenant, terr := s.q.GetTenantByID(r.Context(), pgutil.UUID(*actor.TenantID)); terr == nil {
		tid := *actor.TenantID
		notify.Dispatch(r.Context(), notify.Deps{Q: s.q, Log: s.log}, &tid, notify.TypeSecurityMFAPolicyChanged,
			fmt.Sprintf("MFA policy changed for %s", tenant.Name), "",
			map[string]any{"tenant_id": tid.String(), "tenant_name": tenant.Name}, notify.Contact{})
	}
	out := policyJSON(policy)
	out["allowed"] = true
	response.JSON(w, http.StatusOK, out)
}
