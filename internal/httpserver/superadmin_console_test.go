package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/pkg/identity"
)

/*
The Super Admin console's own API surface.

These cover the endpoints the console leans on that nothing else exercises:
system health, the commercial terms on a plan, the business-type configuration
applied when a business is created, platform branding, and the flat user route
the IAM screen addresses people by.

They run against the developer's database like the rest of the integration
suite, and clean up after themselves: plans and the platform settings row are
real operator data, so each test restores what it found.
*/

/* ------------------------------------------------------------------ *
 * System health
 * ------------------------------------------------------------------ */

func TestSystemHealthReportsEveryComponent(t *testing.T) {
	env := newTestEnv(t)

	status, body := env.do(http.MethodGet, "/api/v1/admin/system-health", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "system health", body)

	components, ok := body["components"].([]any)
	if !ok || len(components) == 0 {
		t.Fatalf("no components in the response: %v", body)
	}

	valid := map[string]bool{"HEALTHY": true, "WARNING": true, "ERROR": true, "UNAVAILABLE": true}
	seen := map[string]string{}
	for _, raw := range components {
		component, _ := raw.(map[string]any)
		id, _ := component["id"].(string)
		state, _ := component["status"].(string)
		if !valid[state] {
			t.Errorf("component %q has status %q, which the console cannot render", id, state)
		}
		if detail, _ := component["detail"].(string); strings.TrimSpace(detail) == "" {
			t.Errorf("component %q has no detail; a status with no explanation is not actionable", id)
		}
		seen[id] = state
	}

	// The five the monitoring screen always expects to find.
	for _, id := range []string{"api", "database", "smtp", "storage", "ai"} {
		if _, ok := seen[id]; !ok {
			t.Errorf("no %q component reported", id)
		}
	}

	// The database is being queried by this very test, so anything but a pass
	// means the probe itself is wrong.
	if seen["database"] != "HEALTHY" {
		t.Errorf("database reported %q while serving this request", seen["database"])
	}

	// The overall status must not be rosier than its worst component.
	overall, _ := body["status"].(string)
	if seen["database"] == "HEALTHY" && overall == "ERROR" {
		t.Errorf("overall status %q does not follow from the components", overall)
	}
}

/* ------------------------------------------------------------------ *
 * Plans: the commercial terms
 * ------------------------------------------------------------------ */

// createTestPlan posts a plan and removes it again afterwards. Plans are a
// shared catalogue on a developer's database, so a test that leaves one behind
// changes what onboarding offers.
func createTestPlan(t *testing.T, env *testEnv, payload map[string]any) map[string]any {
	t.Helper()
	status, body := env.do(http.MethodPost, "/api/v1/admin/plans", env.superAdmin(), payload)
	env.mustStatus(http.StatusCreated, status, "create plan", body)

	id, _ := body["id"].(string)
	t.Cleanup(func() {
		if id == "" {
			return
		}
		if _, err := env.server.pool.Exec(context.Background(),
			`DELETE FROM plans WHERE id = $1 AND NOT EXISTS (
				SELECT 1 FROM tenants WHERE plan_id = plans.id)`, id); err != nil {
			t.Errorf("plan cleanup: %v", err)
		}
	})
	return body
}

func TestPlanCarriesItsCommercialTerms(t *testing.T) {
	env := newTestEnv(t)
	name := "TESTPLAN" + strings.ToUpper(randSuffix())

	created := createTestPlan(t, env, map[string]any{
		"name":           name,
		"description":    "Written by the console test",
		"price":          799,
		"max_staff":      7,
		"max_products":   70,
		"billing_period": "yearly",
		"trial_days":     21,
		"features":       []string{"Unlimited orders", "Priority support"},
		"business_types": []string{"grocery", "CAFE"},
	})

	if got := created["billing_period"]; got != "yearly" {
		t.Errorf("billing_period = %v, want yearly", got)
	}
	if got := created["trial_days"]; got != float64(21) {
		t.Errorf("trial_days = %v, want 21", got)
	}
	features, _ := created["features"].([]any)
	if len(features) != 2 {
		t.Errorf("features = %v, want both entries", created["features"])
	}
	// Business types are normalised the same way tenant types are, so a
	// lower-case entry from a form still matches the catalogue.
	types, _ := created["business_types"].([]any)
	if len(types) != 2 || types[0] != "GROCERY" || types[1] != "CAFE" {
		t.Errorf("business_types = %v, want normalised upper-case codes", created["business_types"])
	}

	// The listing carries the terms and the usage count the console shows.
	status, body := env.do(http.MethodGet, "/api/v1/admin/plans", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "list plans", body)
	plans, _ := body["plans"].([]any)
	var listed map[string]any
	for _, raw := range plans {
		row, _ := raw.(map[string]any)
		if row["name"] == name {
			listed = row
		}
	}
	if listed == nil {
		t.Fatalf("the new plan is missing from the listing")
	}
	if listed["billing_period"] != "yearly" || listed["trial_days"] != float64(21) {
		t.Errorf("the listing lost the terms: %v", listed)
	}
	if _, ok := listed["tenant_count"]; !ok {
		t.Error("the listing has no tenant_count, so the console cannot warn before withdrawing a plan")
	}
}

func TestPlanUpdateMergesTermsRatherThanReplacingThem(t *testing.T) {
	env := newTestEnv(t)
	name := "TESTPLAN" + strings.ToUpper(randSuffix())

	created := createTestPlan(t, env, map[string]any{
		"name":           name,
		"price":          499,
		"billing_period": "monthly",
		"trial_days":     14,
		"features":       []string{"One"},
	})
	id, _ := created["id"].(string)

	// A form that edited only the feature list must not blank the rest — this
	// is the failure the merge exists to prevent.
	status, body := env.do(http.MethodPatch, "/api/v1/admin/plans/"+id, env.superAdmin(),
		map[string]any{"features": []string{"One", "Two"}})
	env.mustStatus(http.StatusOK, status, "patch features", body)

	if body["billing_period"] != "monthly" {
		t.Errorf("billing_period = %v after a features-only patch, want monthly", body["billing_period"])
	}
	if body["trial_days"] != float64(14) {
		t.Errorf("trial_days = %v after a features-only patch, want 14", body["trial_days"])
	}
	if features, _ := body["features"].([]any); len(features) != 2 {
		t.Errorf("features = %v, want both entries", body["features"])
	}

	// An unknown billing period is refused rather than stored.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/plans/"+id, env.superAdmin(),
		map[string]any{"billing_period": "fortnightly"})
	env.mustStatus(http.StatusBadRequest, status, "reject an unknown billing period", body)

	// Withdrawing a plan is a status change, not a deletion.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/plans/"+id, env.superAdmin(),
		map[string]any{"is_active": false})
	env.mustStatus(http.StatusOK, status, "withdraw plan", body)
	if body["is_active"] != false {
		t.Errorf("is_active = %v after withdrawing", body["is_active"])
	}
}

/* ------------------------------------------------------------------ *
 * Business-type configuration at creation
 * ------------------------------------------------------------------ */

func TestOnboardingAppliesBusinessTypeConfiguration(t *testing.T) {
	env := newTestEnv(t)
	slug := "cfg-" + randSuffix()

	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":          "Configured Business",
		"slug":          slug,
		"business_type": "GROCERY",
		"owner_name":    "Owner",
		"admin_name":    "Admin",
		"admin_email":   "admin-" + randSuffix() + "@test.local",
		"email":         "owner@test.local",
		"configuration": map[string]any{
			"theme_preset":        "fresh",
			"product_layout":      "list",
			"hero_style":          "compact",
			"customer_login_mode": "required",
			"prep_time_minutes":   45,
			"ordering_enabled":    true,
			"payments": map[string]any{
				"online_payment_enabled": true,
				"cash_enabled":           false,
				"pay_at_pickup_enabled":  true,
				"default_payment_method": "ONLINE",
			},
			"workflow": map[string]any{
				"acceptance_mode":     "MANUAL",
				"payment_requirement": "AT_PICKUP",
				"ready_notification":  true,
				"auto_complete":       false,
			},
		},
	})
	env.mustStatus(http.StatusCreated, status, "create a configured business", body)

	id := env.tenantID(slug)
	env.createdTenants = append(env.createdTenants, id)
	t.Cleanup(env.removeTestTenants)

	var preset, layout, hero, loginMode string
	var prep int
	var payments, workflow []byte
	err := env.server.pool.QueryRow(context.Background(), `
		SELECT theme_preset, product_layout, hero_style, customer_login_mode,
		       prep_time_minutes, payments, workflow
		FROM tenant_storefront_settings WHERE tenant_id = $1`, id).
		Scan(&preset, &layout, &hero, &loginMode, &prep, &payments, &workflow)
	if err != nil {
		t.Fatalf("the business was created without a storefront row: %v", err)
	}

	if preset != "fresh" || layout != "list" || hero != "compact" {
		t.Errorf("theme was not applied: preset=%q layout=%q hero=%q", preset, layout, hero)
	}
	if loginMode != "required" {
		t.Errorf("customer_login_mode = %q, want required", loginMode)
	}
	if prep != 45 {
		t.Errorf("prep_time_minutes = %d, want 45", prep)
	}
	// Parsed rather than string-matched: the column is jsonb, so the database
	// decides the spacing and key order it returns.
	var paymentDoc struct {
		Online  bool   `json:"online_payment_enabled"`
		Cash    bool   `json:"cash_enabled"`
		Default string `json:"default_payment_method"`
	}
	if err := json.Unmarshal(payments, &paymentDoc); err != nil {
		t.Fatalf("payment document is not readable: %v", err)
	}
	if paymentDoc.Cash || !paymentDoc.Online || paymentDoc.Default != "ONLINE" {
		t.Errorf("payment document was not applied: %s", payments)
	}

	var workflowDoc struct {
		Acceptance string `json:"acceptance_mode"`
		Payment    string `json:"payment_requirement"`
		Ready      bool   `json:"ready_notification"`
	}
	if err := json.Unmarshal(workflow, &workflowDoc); err != nil {
		t.Fatalf("workflow document is not readable: %v", err)
	}
	if workflowDoc.Payment != "AT_PICKUP" || workflowDoc.Acceptance != "MANUAL" || !workflowDoc.Ready {
		t.Errorf("workflow document was not applied: %s", workflow)
	}
}

func TestOnboardingRejectsAnUnknownStorefrontValue(t *testing.T) {
	env := newTestEnv(t)
	slug := "cfg-bad-" + randSuffix()

	// A template with a typo must not create a shop the storefront cannot
	// render. The unknown value is dropped and the schema default stands.
	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":          "Fallback Business",
		"slug":          slug,
		"business_type": "CAFE",
		"owner_name":    "Owner",
		"admin_name":    "Admin",
		"admin_email":   "admin-" + randSuffix() + "@test.local",
		"email":         "owner@test.local",
		"configuration": map[string]any{
			"theme_preset":   "neon-disco",
			"product_layout": "grid",
		},
	})
	env.mustStatus(http.StatusCreated, status, "create with a bad preset", body)

	id := env.tenantID(slug)
	env.createdTenants = append(env.createdTenants, id)
	t.Cleanup(env.removeTestTenants)

	var preset, layout string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT theme_preset, product_layout FROM tenant_storefront_settings WHERE tenant_id = $1`, id).
		Scan(&preset, &layout); err != nil {
		t.Fatalf("no storefront row: %v", err)
	}
	if preset == "neon-disco" {
		t.Error("an unknown theme preset was stored")
	}
	if layout != "grid" {
		t.Errorf("a valid value beside an invalid one was dropped: layout = %q", layout)
	}
}

/* ------------------------------------------------------------------ *
 * Platform settings: branding and defaults
 * ------------------------------------------------------------------ */

// keepPlatformSettings restores the settings row after a test edits it.
func keepPlatformSettings(t *testing.T, env *testEnv) {
	t.Helper()
	ctx := context.Background()
	var before []byte
	_ = env.server.pool.QueryRow(ctx, `SELECT config FROM platform_settings LIMIT 1`).Scan(&before)
	t.Cleanup(func() {
		if before == nil {
			return
		}
		if _, err := env.server.pool.Exec(ctx,
			`UPDATE platform_settings SET config = $1`, before); err != nil {
			t.Errorf("settings restore: %v", err)
		}
	})
}

func TestPlatformBrandingRoundTripsAndValidates(t *testing.T) {
	env := newTestEnv(t)
	keepPlatformSettings(t, env)

	status, body := env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"general": map[string]any{"default_currency": "aed"},
		"branding": map[string]any{
			"primary_color":   "#123ABC",
			"secondary_color": "#ABCDEF",
			"preset_id":       "emerald",
			"color_mode":      "dark",
		},
	})
	env.mustStatus(http.StatusOK, status, "save the console theme", body)

	branding, _ := body["branding"].(map[string]any)
	if branding["primary_color"] != "#123ABC" {
		t.Errorf("primary_color = %v", branding["primary_color"])
	}
	if branding["preset_id"] != "emerald" {
		t.Errorf("preset_id = %v", branding["preset_id"])
	}
	if branding["color_mode"] != "dark" {
		t.Errorf("color_mode = %v", branding["color_mode"])
	}
	// The logo and favicon were removed: they were stored and returned and
	// nothing ever rendered either, which is a field that lies about working.
	if _, ok := branding["logo_url"]; ok {
		t.Error("logo_url is back in the payload; nothing renders it")
	}
	general, _ := body["general"].(map[string]any)
	if general["default_currency"] != "AED" {
		t.Errorf("default_currency = %v, want it upper-cased", general["default_currency"])
	}

	// A colour that is not a colour is refused, rather than silently dropped —
	// otherwise the console shows a saved value the platform never stored.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"branding": map[string]any{"primary_color": "indigo"},
	})
	env.mustStatus(http.StatusBadRequest, status, "reject a non-hex colour", body)

	// Likewise a preset that does not exist, which would leave every console
	// falling back to the built-in accent with no sign anything was wrong.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"branding": map[string]any{"preset_id": "not-a-real-preset"},
	})
	env.mustStatus(http.StatusBadRequest, status, "reject an unknown preset", body)
}

/* ------------------------------------------------------------------ *
 * IAM
 * ------------------------------------------------------------------ */

func TestConsoleAccessListsOnlyPlatformIdentities(t *testing.T) {
	env := newTestEnv(t)
	// A business with its own people. None of them may appear in console
	// access: the two are separate authentication contexts, and mixing them is
	// exactly what this endpoint must not do.
	tenantID := env.createTenant("Console Fence", "console-fence-"+randSuffix())
	env.mintToken(identity.RoleTenantAdmin, &tenantID)
	env.mintToken(identity.RoleStaff, &tenantID)

	status, body := env.do(http.MethodGet, "/api/v1/admin/users", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "list console access", body)

	users, _ := body["users"].([]any)
	if len(users) == 0 {
		t.Fatal("console access is empty; the owner must always appear")
	}
	sawOwner := false
	for _, raw := range users {
		row, _ := raw.(map[string]any)
		role, _ := row["role"].(string)
		if !identity.IsPlatformRole(role) {
			t.Errorf("a business identity leaked into console access: %v (%s)", row["email"], role)
		}
		if role == identity.RoleSuperAdmin {
			sawOwner = true
			if row["is_owner"] != true {
				t.Error("the owner row is not flagged as the owner")
			}
		}
		if _, ok := row["permissions"]; !ok {
			t.Errorf("console user %v has no permission list", row["email"])
		}
	}
	if !sawOwner {
		t.Error("the owner is missing from console access")
	}

	roles, _ := body["roles"].([]any)
	if len(roles) != 3 {
		t.Errorf("roles = %d, want owner, platform admin and support", len(roles))
	}
}

func TestConsoleInviteAndRoleRules(t *testing.T) {
	env := newTestEnv(t)
	email := "console-" + randSuffix() + "@test.local"

	status, body := env.do(http.MethodPost, "/api/v1/admin/users", env.superAdmin(), map[string]any{
		"name":  "Platform Colleague",
		"email": email,
		"role":  identity.RolePlatformAdmin,
	})
	env.mustStatus(http.StatusCreated, status, "invite a platform admin", body)

	user, _ := body["user"].(map[string]any)
	id, _ := user["id"].(string)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = env.server.pool.Exec(ctx, `DELETE FROM audit_logs WHERE user_id = $1`, id)
		_, _ = env.server.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE user_id = $1`, id)
		_, _ = env.server.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	})

	// An invitation, never a password: the account cannot be signed into until
	// the person sets one through the single-use link.
	if user["must_set_password"] != true {
		t.Error("an invited console user should have to set a password")
	}
	if user["status"] != "INVITED" {
		t.Errorf("status = %v, want INVITED", user["status"])
	}
	if setupURL, _ := body["setup_url"].(string); !strings.Contains(setupURL, "/superadmin/setup-password?token=") {
		t.Errorf("setup_url = %q, want a console setup link", body["setup_url"])
	}
	for _, key := range []string{"password", "password_hash", "invite_token"} {
		if _, leaked := user[key]; leaked {
			t.Errorf("the response leaked %q", key)
		}
	}

	// The owner role is not delegable: there is exactly one, created at setup.
	status, body = env.do(http.MethodPost, "/api/v1/admin/users", env.superAdmin(), map[string]any{
		"name":  "Second Owner",
		"email": "owner-" + randSuffix() + "@test.local",
		"role":  identity.RoleSuperAdmin,
	})
	env.mustStatus(http.StatusBadRequest, status, "refuse a second owner", body)

	// A business role is not a console role.
	status, body = env.do(http.MethodPost, "/api/v1/admin/users", env.superAdmin(), map[string]any{
		"name":  "Wrong Scope",
		"email": "wrong-" + randSuffix() + "@test.local",
		"role":  identity.RoleTenantAdmin,
	})
	env.mustStatus(http.StatusBadRequest, status, "refuse a business role on the console", body)

	// Demoting to support is allowed; role and permissions move together.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/users/"+id, env.superAdmin(),
		map[string]any{"role": identity.RoleSupport})
	env.mustStatus(http.StatusOK, status, "change a console role", body)
	if body["role"] != identity.RoleSupport {
		t.Errorf("role = %v, want SUPPORT", body["role"])
	}
	if held, _ := body["permissions"].([]any); len(held) == 0 {
		t.Error("support holds no permissions at all")
	}
}

func TestConsoleOwnerCannotBeChanged(t *testing.T) {
	env := newTestEnv(t)

	var ownerID uuid.UUID
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE role = 'SUPER_ADMIN' LIMIT 1`).Scan(&ownerID); err != nil {
		t.Fatalf("no owner: %v", err)
	}

	// Disabling or demoting the owner would leave the console with nobody able
	// to restore access, so it is refused rather than merely discouraged.
	status, body := env.do(http.MethodPatch, "/api/v1/admin/users/"+ownerID.String(), env.superAdmin(),
		map[string]any{"status": "DISABLED"})
	env.mustStatus(http.StatusForbidden, status, "refuse to disable the owner", body)

	status, body = env.do(http.MethodPatch, "/api/v1/admin/users/"+ownerID.String(), env.superAdmin(),
		map[string]any{"role": identity.RoleSupport})
	env.mustStatus(http.StatusForbidden, status, "refuse to demote the owner", body)
}

func TestSupportRoleIsReadShaped(t *testing.T) {
	env := newTestEnv(t)
	support := env.mintToken(identity.RoleSupport, nil)
	platformAdmin := env.mintToken(identity.RolePlatformAdmin, nil)

	// Support can see the estate and the monitoring screens.
	for _, path := range []string{
		"/api/v1/admin/tenants",
		"/api/v1/admin/dashboard",
		"/api/v1/admin/audit-logs",
	} {
		status, body := env.do(http.MethodGet, path, support, nil)
		env.mustStatus(http.StatusOK, status, "support reading "+path, body)
	}

	// And cannot reach what it has no business changing.
	for _, path := range []string{
		"/api/v1/admin/plans",
		"/api/v1/admin/configurations",
		"/api/v1/admin/settings",
		"/api/v1/admin/users",
	} {
		status, body := env.do(http.MethodGet, path, support, nil)
		if status != http.StatusForbidden {
			t.Errorf("support reached %s: status = %d, want 403 (body: %v)", path, status, body)
		}
	}

	// A platform admin runs the platform but does not hand out console access.
	status, body := env.do(http.MethodGet, "/api/v1/admin/plans", platformAdmin, nil)
	env.mustStatus(http.StatusOK, status, "a platform admin reading plans", body)

	status, body = env.do(http.MethodGet, "/api/v1/admin/users", platformAdmin, nil)
	if status != http.StatusForbidden {
		t.Errorf("a platform admin reached console access: status = %d, want 403 (body: %v)", status, body)
	}
}

func TestPartialSettingsPatchLeavesSiblingsAlone(t *testing.T) {
	env := newTestEnv(t)
	keepPlatformSettings(t, env)

	// Establish a known state across every group.
	status, body := env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"general": map[string]any{
			"platform_name":    "Baseline Platform",
			"support_email":    "baseline@test.local",
			"default_currency": "INR",
			"timezone":         "Asia/Kolkata",
			"default_locale":   "en-IN",
		},
		"security": map[string]any{"password_min_length": 10},
		"branding": map[string]any{"primary_color": "#101010"},
	})
	env.mustStatus(http.StatusOK, status, "seed settings", body)

	// The settings screens save one section at a time, so a patch carrying only
	// the identity fields must not blank the defaults beside them. Without this
	// guarantee, renaming the platform would silently reset its currency.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"general": map[string]any{"platform_name": "Renamed Platform"},
	})
	env.mustStatus(http.StatusOK, status, "patch only the platform name", body)

	general, _ := body["general"].(map[string]any)
	if general["platform_name"] != "Renamed Platform" {
		t.Errorf("platform_name = %v, want the new name", general["platform_name"])
	}
	for field, want := range map[string]string{
		"support_email":    "baseline@test.local",
		"default_currency": "INR",
		"timezone":         "Asia/Kolkata",
		"default_locale":   "en-IN",
	} {
		if general[field] != want {
			t.Errorf("a name-only patch changed %s: got %v, want %q", field, general[field], want)
		}
	}

	// Other groups are untouched by a general-only patch.
	security, _ := body["security"].(map[string]any)
	if security["password_min_length"] != float64(10) {
		t.Errorf("password_min_length = %v after a general-only patch, want 10", security["password_min_length"])
	}
	branding, _ := body["branding"].(map[string]any)
	if branding["primary_color"] != "#101010" {
		t.Errorf("primary_color = %v after a general-only patch, want it preserved", branding["primary_color"])
	}

	// And the reverse: a security-only patch leaves the name alone.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/settings", env.superAdmin(), map[string]any{
		"security": map[string]any{"session_timeout_minutes": 45},
	})
	env.mustStatus(http.StatusOK, status, "patch only the session timeout", body)
	general, _ = body["general"].(map[string]any)
	if general["platform_name"] != "Renamed Platform" {
		t.Errorf("a security-only patch changed the platform name: %v", general["platform_name"])
	}
	security, _ = body["security"].(map[string]any)
	if security["session_timeout_minutes"] != float64(45) {
		t.Errorf("session_timeout_minutes = %v, want 45", security["session_timeout_minutes"])
	}
	if security["password_min_length"] != float64(10) {
		t.Errorf("a session patch reset password_min_length to %v", security["password_min_length"])
	}
}
