// Package entitlement enforces what a tenant's plan allows: feature codes
// (plans.entitlements) and hard limits (plans.max_staff, plans.max_products).
//
// Three layers gate a tenant route, outermost first:
//
//	license      tenantctx.Middleware   is the business allowed to use Orderly at all
//	capability   RequireCapability      does its business type have this module
//	plan         this package           does its plan include it / is it under its limit
//
// A denial is always a 403 with a stable code and machine-readable details, so
// the console can show an upgrade prompt instead of a broken page.
package entitlement

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Feature codes stored in plans.entitlements.
const (
	FeatureAnalytics = "ANALYTICS"
	FeatureInventory = "INVENTORY"
	// Email and in-app notifications are baseline for every plan; these two are not.
	FeatureNotifySMS       = "NOTIFY_SMS"
	FeatureNotifyTemplates = "NOTIFY_TEMPLATES"
	// Marketing: campaigns, consent, trackable links (internal/marketing).
	// SMS campaigns additionally need FeatureNotifySMS.
	FeatureMarketing = "MARKETING"
	// Staff punch in / punch out (internal/attendance).
	FeatureAttendance = "ATTENDANCE"
)

// Feature is one row of the plan matrix the Super Admin edits.
type Feature struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// Catalog is every feature code a plan can carry. A new gated module adds a
// code here and a RequireFeature on its routes; nothing else.
var Catalog = []Feature{
	{FeatureAnalytics, "Analytics & trends"},
	{FeatureInventory, "Inventory"},
	{FeatureNotifySMS, "SMS notifications"},
	{FeatureNotifyTemplates, "Custom message templates"},
	{FeatureMarketing, "Marketing campaigns"},
	{FeatureAttendance, "Staff attendance"},
}

func label(code string) string {
	for _, f := range Catalog {
		if f.Code == code {
			return f.Label
		}
	}
	return code
}

// Clean normalises feature codes from a request: upper-cased, de-duplicated,
// and rejected when not in the catalog.
func Clean(codes []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range codes {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if code == "" || seen[code] {
			continue
		}
		known := false
		for _, f := range Catalog {
			known = known || f.Code == code
		}
		if !known {
			return nil, fmt.Errorf("unknown entitlement %q", raw)
		}
		seen[code] = true
		out = append(out, code)
	}
	sort.Strings(out)
	return out, nil
}

// Resource is something a plan caps.
type Resource string

const (
	Staff    Resource = "staff"
	Products Resource = "products"
)

// noun is the resource's name for n of them: "1 product", "5 products",
// "1 staff member", "3 staff members".
func (r Resource) noun(n int64) string {
	singular, plural := "product", "products"
	if r == Staff {
		singular, plural = "staff member", "staff members"
	}
	if n == 1 {
		return singular
	}
	return plural
}

// Usage is how much of a limit a tenant has used.
type Usage struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
	// Over is true when usage is above the limit, which happens after a
	// downgrade. Existing rows are kept; only new ones are refused.
	Over bool `json:"over"`
}

// Used counts the tenant's rows for a resource and reads its plan limit.
func Used(ctx context.Context, q *sqlc.Queries, tenantID pgtype.UUID, res Resource) (Usage, string, error) {
	plan, err := q.GetTenantPlanLimits(ctx, tenantID)
	if err != nil {
		return Usage{}, "", err
	}
	var used, limit int64
	if res == Staff {
		used, err = q.CountActiveTenantUsers(ctx, tenantID)
		limit = int64(plan.MaxStaff)
	} else {
		used, err = q.CountTenantProducts(ctx, tenantID)
		limit = int64(plan.MaxProducts)
	}
	if err != nil {
		return Usage{}, "", err
	}
	return Usage{Used: used, Limit: limit, Over: used > limit}, plan.Name, nil
}

// Allow reports whether the tenant may add one more of a resource. When it may
// not, Allow has already written the 403 (`plan_limit`) and the caller returns.
//
// ponytail: count-then-insert, so two simultaneous creates at limit-1 can both
// pass. Fine for a plan limit; take a per-tenant advisory lock if it must be exact.
func Allow(w http.ResponseWriter, r *http.Request, q *sqlc.Queries, tenantID pgtype.UUID, res Resource) bool {
	u, planName, err := Used(r.Context(), q, tenantID, res)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not check plan limit")
		return false
	}
	if u.Used < u.Limit {
		return true
	}
	details := map[string]any{
		"resource": string(res), "plan": planName, "limit": u.Limit, "used": u.Used,
	}
	msg := fmt.Sprintf("Your %s plan allows up to %d %s.", planName, u.Limit, res.noun(u.Limit))
	if next, err := q.CheapestPlanWithCapacity(r.Context(), sqlc.CheapestPlanWithCapacityParams{
		Resource: string(res), Needed: int32(u.Used), CurrentPlan: planName,
	}); err == nil {
		details["required_plan"] = next
		msg += fmt.Sprintf(" Upgrade to %s to add more.", next)
	}
	response.ErrorWithDetails(w, http.StatusForbidden, "plan_limit", msg, details)
	return false
}

// RequireFeature guards a route behind a plan feature. Requires
// tenantctx.Middleware (mounted globally), which loads the plan with the tenant.
func RequireFeature(q *sqlc.Queries, code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := tenantctx.FromContext(r.Context())
			if ok && t.HasEntitlement(code) {
				next.ServeHTTP(w, r)
				return
			}
			details := map[string]any{"feature": code, "plan": t.Plan}
			msg := fmt.Sprintf("%s is not included in your %s plan.", label(code), t.Plan)
			if need := requiredPlan(r, q, code, t.Plan); need != "" {
				details["required_plan"] = need
				msg += fmt.Sprintf(" Upgrade to %s to use it.", need)
			}
			response.ErrorWithDetails(w, http.StatusForbidden, "feature_not_in_plan", msg, details)
		})
	}
}

func requiredPlan(r *http.Request, q *sqlc.Queries, code, currentPlan string) string {
	name, err := q.CheapestPlanWithEntitlement(r.Context(), sqlc.CheapestPlanWithEntitlementParams{Code: code, CurrentPlan: currentPlan})
	if err != nil {
		return ""
	}
	return name
}

// Handler serves GET /tenant/entitlements: what the plan includes, how much of
// each limit is used, and the license's lifecycle state.
type Handler struct{ Q *sqlc.Queries }

type featureView struct {
	Feature
	Enabled      bool   `json:"enabled"`
	RequiredPlan string `json:"required_plan,omitempty"`
}

func (h Handler) Get(w http.ResponseWriter, r *http.Request) {
	t, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	tid := pgtype.UUID{Bytes: t.ID, Valid: true}

	features := make([]featureView, 0, len(Catalog))
	for _, f := range Catalog {
		v := featureView{Feature: f, Enabled: t.HasEntitlement(f.Code)}
		if !v.Enabled {
			v.RequiredPlan = requiredPlan(r, h.Q, f.Code, t.Plan)
		}
		features = append(features, v)
	}
	limits := map[string]Usage{}
	for _, res := range []Resource{Staff, Products} {
		u, _, err := Used(r.Context(), h.Q, tid, res)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "could not load usage")
			return
		}
		limits[string(res)] = u
	}

	license := map[string]any{"state": t.LicenseState}
	if t.LicenseExpiresAt != nil {
		license["expires_at"] = t.LicenseExpiresAt.UTC().Format(time.RFC3339)
	}
	if t.LicenseGraceEndsAt != nil {
		license["grace_ends_at"] = t.LicenseGraceEndsAt.UTC().Format(time.RFC3339)
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"plan": t.Plan, "features": features, "limits": limits, "license": license,
	})
}
