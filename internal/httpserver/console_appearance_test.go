package httpserver

import (
	"net/http"
	"testing"

	"github.com/orderly/orderly-backend/pkg/identity"
)

/*
Console appearance.

The console's look has two layers and they answer to different people. The
business default is the owner's, shared by everyone who works there. The
personal layer is one person's own screen. These tests hold that boundary from
the outside, because the failure they guard against is silent: a preference
saved in the wrong place looks fine to the person who set it and changes the
console for everybody else.
*/

func TestPersonalAppearanceIsPerUserAndNeverTouchesTheBusinessDefault(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Appearance Shop", "appearance-shop-"+randSuffix())
	host := env.hostFor(tenantID)

	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)
	staff := tenantUser(env, identity.RoleStaff, tenantID)

	// Before anyone chooses anything, both people see the business default.
	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/me/appearance", staff, nil, host)
	env.mustStatus(http.StatusOK, status, "staff reads their appearance", body)
	if source, _ := body["source"].(string); source != "BUSINESS" {
		t.Fatalf("source = %q, want BUSINESS before anything is chosen", source)
	}
	if body["personal"] != nil {
		t.Fatalf("personal = %v, want null before anything is chosen", body["personal"])
	}
	businessBefore := themeMode(t, body["business"])

	// Staff may set their own. This is the ungated route on purpose: it is a
	// preference about their own screen, not a capability over the business.
	status, body = env.doOn(http.MethodPatch, "/api/v1/tenant/me/appearance", staff,
		map[string]any{"color_mode": "dark"}, host)
	env.mustStatus(http.StatusOK, status, "staff sets dark mode", body)
	if source, _ := body["source"].(string); source != "USER" {
		t.Fatalf("source = %q, want USER after a personal choice", source)
	}
	if mode := themeMode(t, body["theme"]); mode != "dark" {
		t.Fatalf("effective color_mode = %q, want dark", mode)
	}

	// The owner, on the same business, is unaffected. This is the assertion the
	// whole feature exists for.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/me/appearance", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "owner reads their appearance", body)
	if source, _ := body["source"].(string); source != "BUSINESS" {
		t.Fatalf("owner source = %q, want BUSINESS — another user's choice leaked", source)
	}
	if mode := themeMode(t, body["business"]); mode != businessBefore {
		t.Fatalf("business default color_mode = %q, want unchanged %q", mode, businessBefore)
	}

	// Resetting returns the person to the business default rather than freezing
	// a copy of it.
	status, body = env.doOn(http.MethodDelete, "/api/v1/tenant/me/appearance", staff, nil, host)
	env.mustStatus(http.StatusOK, status, "staff resets their appearance", body)
	if source, _ := body["source"].(string); source != "BUSINESS" {
		t.Fatalf("source after reset = %q, want BUSINESS", source)
	}
	if body["personal"] != nil {
		t.Fatalf("personal after reset = %v, want null", body["personal"])
	}
}

func TestPersonalAppearanceRejectsValuesItCannotPaint(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Appearance Guard", "appearance-guard-"+randSuffix())
	host := env.hostFor(tenantID)
	staff := tenantUser(env, identity.RoleStaff, tenantID)

	cases := []struct {
		label string
		body  map[string]any
	}{
		{"a colour mode that is not a mode", map[string]any{"color_mode": "sepia"}},
		{"a preset that does not exist", map[string]any{"preset_id": "not-a-real-preset"}},
		{"an accent that is not a hex colour", map[string]any{
			"overrides": map[string]string{"accent": "red; background:url(x)"},
		}},
	}
	for _, tc := range cases {
		status, body := env.doOn(http.MethodPatch, "/api/v1/tenant/me/appearance", staff, tc.body, host)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %v)", tc.label, status, body)
		}
	}
}

// themeMode reads color_mode out of a theme payload, failing loudly rather than
// letting a shape change register as a passing test.
func themeMode(t *testing.T, raw any) string {
	t.Helper()
	theme, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("theme payload = %v, want an object", raw)
	}
	mode, _ := theme["color_mode"].(string)
	if mode == "" {
		t.Fatalf("theme payload has no color_mode: %v", theme)
	}
	return mode
}

func TestPlatformAppearanceIsPerOperatorAndLayersOverTheConsoleDefault(t *testing.T) {
	env := newTestEnv(t)
	owner := env.superAdmin()

	/*
	 * There is exactly one SUPER_ADMIN by schema, so this test shares a
	 * long-lived row with whatever else has touched it — another test, or a
	 * developer clicking around the console against the same database. Start
	 * from a known state and leave one behind, rather than assuming a personal
	 * theme has never been set. Asserting on someone else's leftovers is how a
	 * suite passes alone and fails in a full run.
	 */
	clearPersonal := func() {
		status, body := env.do(http.MethodDelete, "/api/v1/admin/me/appearance", owner, nil)
		env.mustStatus(http.StatusOK, status, "clear the operator's personal theme", body)
	}
	clearPersonal()
	t.Cleanup(clearPersonal)

	// The console default is what an operator sees before choosing anything.
	status, body := env.do(http.MethodGet, "/api/v1/admin/me/appearance", owner, nil)
	env.mustStatus(http.StatusOK, status, "read console appearance", body)
	if source, _ := body["source"].(string); source != "BUSINESS" {
		t.Fatalf("source = %q, want BUSINESS before anything is chosen", source)
	}
	if _, ok := body["business"].(map[string]any); !ok {
		t.Fatalf("no console default returned: %v", body)
	}

	// A personal choice takes over, and the shared default is untouched.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/me/appearance", owner,
		map[string]any{"color_mode": "dark", "preset_id": "emerald"})
	env.mustStatus(http.StatusOK, status, "set a personal console theme", body)
	if source, _ := body["source"].(string); source != "USER" {
		t.Fatalf("source = %q, want USER after a personal choice", source)
	}
	theme, _ := body["theme"].(map[string]any)
	if id, _ := theme["preset_id"].(string); id != "emerald" {
		t.Fatalf("effective preset = %q, want emerald", id)
	}
	if mode := themeMode(t, body["business"]); mode == "dark" {
		t.Fatal("the console default followed a personal choice; it must not")
	}

	// And it is refused the same values a tenant's would be.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/me/appearance", owner,
		map[string]any{"preset_id": "not-a-real-preset"})
	if status != http.StatusBadRequest {
		t.Errorf("unknown preset: status = %d, want 400 (body: %v)", status, body)
	}

	status, body = env.do(http.MethodDelete, "/api/v1/admin/me/appearance", owner, nil)
	env.mustStatus(http.StatusOK, status, "reset console appearance", body)
	if source, _ := body["source"].(string); source != "BUSINESS" {
		t.Fatalf("source after reset = %q, want BUSINESS", source)
	}
}

// TestOnboardingSeedsTheBusinessConsoleTheme covers the promise onboarding
// makes: the look chosen for a new business is the *business's*, so its owner's
// console — the setup-password link and the first sign-in included — opens in
// it, not in the platform's stock accent.
func TestOnboardingSeedsTheBusinessConsoleTheme(t *testing.T) {
	env := newTestEnv(t)
	slug := "themed-" + randSuffix()

	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":             "Themed Business",
		"slug":             slug,
		"business_type":    "GROCERY",
		"owner_name":       "Owner",
		"admin_name":       "Admin",
		"admin_email":      "admin-" + randSuffix() + "@test.local",
		"email":            "owner@test.local",
		"theme_preset_id":  "emerald",
		"theme_color_mode": "light",
		"theme_overrides":  map[string]string{"accent": "#123456", "accent2": "#654321"},
		"terms_accepted":   true,
	})
	env.mustStatus(http.StatusCreated, status, "create a themed business", body)

	// The public theme route is what the sign-in and setup-password screens
	// read, and it renders with no token at all — which is the whole point.
	host := slug + ".localhost"
	status, theme := env.doOn(http.MethodGet, "/api/v1/public/theme", "", nil, host)
	env.mustStatus(http.StatusOK, status, "read the new shop's console theme unauthenticated", theme)

	if id, _ := theme["preset_id"].(string); id != "emerald" {
		t.Errorf("preset_id = %q, want emerald", id)
	}
	if mode, _ := theme["color_mode"].(string); mode != "light" {
		t.Errorf("color_mode = %q, want light", mode)
	}
	tokens, _ := theme["tokens"].(map[string]any)
	if accent, _ := tokens["accent"].(string); accent != "#123456" {
		t.Errorf("accent = %q, want the colour chosen at onboarding (#123456)", accent)
	}
}

// TestConsoleDefaultThemeCanBeResetToFactory covers the escape hatch: an
// operator who has made the console unreadable must be able to get back to a
// known-good theme without knowing what the original values were.
func TestConsoleDefaultThemeCanBeResetToFactory(t *testing.T) {
	env := newTestEnv(t)
	// This test rewrites the shared platform settings row. Put it back, or the
	// console theme every other test reads is whatever this one left behind.
	keepPlatformSettings(t, env)
	owner := env.superAdmin()

	status, body := env.do(http.MethodPatch, "/api/v1/admin/settings", owner, map[string]any{
		"branding": map[string]any{
			"preset_id":       "rose",
			"color_mode":      "dark",
			"primary_color":   "#abcdef",
			"secondary_color": "#fedcba",
		},
	})
	env.mustStatus(http.StatusOK, status, "set a console default", body)

	status, body = env.do(http.MethodPatch, "/api/v1/admin/settings", owner,
		map[string]any{"branding": map[string]any{"reset": true}})
	env.mustStatus(http.StatusOK, status, "reset the console default", body)

	branding, _ := body["branding"].(map[string]any)
	if id, _ := branding["preset_id"].(string); id != "indigo-violet" {
		t.Errorf("preset_id = %q, want the factory preset", id)
	}
	if mode, _ := branding["color_mode"].(string); mode != "system" {
		t.Errorf("color_mode = %q, want system", mode)
	}
	if c, _ := branding["primary_color"].(string); c != "" {
		t.Errorf("primary_color = %q, want cleared so the preset's own colour shows", c)
	}
	if secondary, _ := branding["secondary_color"].(string); secondary != "" {
		t.Errorf("secondary_color = %q, want cleared too", secondary)
	}

	// And the reset is visible to the console that paints from it.
	status, appearance := env.do(http.MethodGet, "/api/v1/admin/me/appearance", owner, nil)
	env.mustStatus(http.StatusOK, status, "read the appearance after reset", appearance)
	theme, _ := appearance["business"].(map[string]any)
	if id, _ := theme["preset_id"].(string); id != "indigo-violet" {
		t.Errorf("the painted default is %q, want the factory preset", id)
	}
}
