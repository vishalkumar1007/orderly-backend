package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// sourcePref reads the tenant's elected source straight from the database, so
// the assertions do not depend on the API echoing its own write back.
func sourcePref(t *testing.T, env *testEnv, tenantID uuid.UUID, service string) string {
	t.Helper()
	var source string
	err := env.server.pool.QueryRow(context.Background(),
		`SELECT source FROM tenant_service_preferences WHERE tenant_id = $1 AND service_type = $2`,
		tenantID, service).Scan(&source)
	if err != nil {
		t.Fatalf("read preference: %v", err)
	}
	return source
}

func setSource(t *testing.T, env *testEnv, tenantID uuid.UUID, service, source string) {
	t.Helper()
	_, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO tenant_service_preferences (tenant_id, service_type, source)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, service_type) DO UPDATE SET source = EXCLUDED.source`,
		tenantID, service, source)
	if err != nil {
		t.Fatalf("set preference: %v", err)
	}
}

func grantAccess(t *testing.T, env *testEnv, tenantID uuid.UUID, service string, allow bool) {
	t.Helper()
	_, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO tenant_service_access (tenant_id, service_type, allow_platform)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, service_type) DO UPDATE SET allow_platform = EXCLUDED.allow_platform`,
		tenantID, service, allow)
	if err != nil {
		t.Fatalf("grant access: %v", err)
	}
}

func sharePlatform(t *testing.T, env *testEnv, service string, allow bool) {
	t.Helper()
	_, err := env.server.pool.Exec(context.Background(),
		`UPDATE platform_configurations SET allow_tenants = $2 WHERE service_type = $1`, service, allow)
	if err != nil {
		t.Fatalf("share platform: %v", err)
	}
}

// seedPlatform stores an enabled platform configuration for a service.
func seedPlatform(t *testing.T, env *testEnv, service string) {
	t.Helper()
	var body map[string]any
	switch service {
	case "SMTP":
		body = smtpBody("smtp.platform.local", "platform-secret")
	case "STORAGE":
		body = map[string]any{"provider": "minio", "enabled": true, "config": map[string]any{
			"endpoint": "https://minio.test.local", "region": "us-east-1",
			"bucket": "media", "access_key": "AKIA", "secret_key": "platform-secret"}}
	case "AI":
		body = map[string]any{"provider": "openai", "enabled": true, "config": map[string]any{
			"base_url": "https://api.openai.test.local/v1", "default_model": "gpt-test",
			"api_key": "platform-secret"}}
	}
	status, resp := env.do(http.MethodPut, "/api/v1/admin/configurations/"+lower(service), env.superAdmin(), body)
	env.mustStatus(http.StatusOK, status, "seed platform "+service, resp)
}

func lower(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		out = append(out, r)
	}
	return string(out)
}

/* ------------------------------------------------------------------ *
 * Resolution precedence
 * ------------------------------------------------------------------ */

func TestTenantDefaultsToOrganization(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Defaulting Co", "default-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	sharePlatform(t, env, "SMTP", true)
	grantAccess(t, env, tenantID, "SMTP", true)

	// No preference row at all: the tenant must not silently inherit the
	// platform, even with access granted.
	_, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective",
		env.tenantAdmin(tenantID), nil, tenantID)
	if body["available"] != false {
		t.Errorf("available = %v, want false before the tenant configures anything", body["available"])
	}
}

func TestPlatformRequiresBothSwitches(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Two Switch Co", "two-switch-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	setSource(t, env, tenantID, "SMTP", "PLATFORM")
	token := env.tenantAdmin(tenantID)

	// Neither switch on.
	_, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if body["available"] != false {
		t.Fatalf("available = %v, want false with both switches off", body["available"])
	}

	// Global on, per-tenant grant missing.
	sharePlatform(t, env, "SMTP", true)
	_, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if body["available"] != false {
		t.Fatalf("available = %v, want false without the per-tenant grant", body["available"])
	}

	// Both on: now it resolves.
	grantAccess(t, env, tenantID, "SMTP", true)
	_, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if body["available"] != true {
		t.Fatalf("available = %v, want true once both switches are on (%v)", body["available"], body["__raw"])
	}
	if body["source"] != "PLATFORM" {
		t.Errorf("source = %v, want PLATFORM", body["source"])
	}
}

func TestRevokingGlobalRevokesTenant(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("STORAGE")
	tenantID := env.createTenant("Revoke Co", "revoke-co-"+randSuffix())
	seedPlatform(t, env, "STORAGE")
	sharePlatform(t, env, "STORAGE", true)
	grantAccess(t, env, tenantID, "STORAGE", true)
	setSource(t, env, tenantID, "STORAGE", "PLATFORM")
	token := env.tenantAdmin(tenantID)

	_, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/storage/effective", token, nil, tenantID)
	if body["available"] != true {
		t.Fatalf("setup failed: %v", body["__raw"])
	}

	// Super Admin turns sharing off globally.
	sharePlatform(t, env, "STORAGE", false)
	_, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/storage/effective", token, nil, tenantID)
	if body["available"] != false {
		t.Errorf("available = %v, want false after global sharing was revoked", body["available"])
	}
	// The tenant's stored preference is untouched: we do not rewrite its choice.
	if got := sourcePref(t, env, tenantID, "STORAGE"); got != "PLATFORM" {
		t.Errorf("stored preference = %q, want it left as PLATFORM (no silent rewrite)", got)
	}
}

func TestDisabledPlatformIsUnavailableWithNoFallback(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Fallback Co", "fallback-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	sharePlatform(t, env, "SMTP", true)
	grantAccess(t, env, tenantID, "SMTP", true)
	setSource(t, env, tenantID, "SMTP", "PLATFORM")
	token := env.tenantAdmin(tenantID)

	// The tenant also has a perfectly good configuration of its own.
	tenantSaveStatus, tenantSaveBody := env.doAsTenant(http.MethodPut, "/api/v1/tenant/configurations/smtp", token,
		smtpBody("smtp.own.local", "own-secret"), tenantID)
	env.mustStatus(http.StatusOK, tenantSaveStatus, "tenant save", tenantSaveBody)

	// Platform disabled underneath it.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE platform_configurations SET enabled = false WHERE service_type = 'SMTP'`); err != nil {
		t.Fatal(err)
	}

	_, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if body["available"] != false {
		t.Fatalf("available = %v, want false — the resolver must not fall back", body["available"])
	}
	if body["source"] != "PLATFORM" {
		t.Errorf("source = %v, want PLATFORM so the UI can explain the outage", body["source"])
	}
	reason, _ := body["reason"].(string)
	if reason == "" {
		t.Error("an unavailable platform must come with a reason")
	}
	hint, _ := body["hint"].(string)
	if hint == "" {
		t.Error("an unavailable platform must come with tenant-facing guidance")
	}
}

func TestSwitchingSourceRoundTrips(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Switching Co", "switch-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	sharePlatform(t, env, "SMTP", true)
	grantAccess(t, env, tenantID, "SMTP", true)
	token := env.tenantAdmin(tenantID)

	// Organization first.
	status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
		map[string]any{"source": "ORGANIZATION"}, tenantID)
	env.mustStatus(http.StatusOK, status, "select organization", body)
	if got := sourcePref(t, env, tenantID, "SMTP"); got != "ORGANIZATION" {
		t.Errorf("preference = %q, want ORGANIZATION", got)
	}

	// Then platform.
	status, body = env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
		map[string]any{"source": "PLATFORM"}, tenantID)
	env.mustStatus(http.StatusOK, status, "select platform", body)
	if got := sourcePref(t, env, tenantID, "SMTP"); got != "PLATFORM" {
		t.Errorf("preference = %q, want PLATFORM", got)
	}

	// And back again.
	status, body = env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
		map[string]any{"source": "ORGANIZATION"}, tenantID)
	env.mustStatus(http.StatusOK, status, "select organization again", body)
	if got := sourcePref(t, env, tenantID, "SMTP"); got != "ORGANIZATION" {
		t.Errorf("preference = %q, want ORGANIZATION", got)
	}
}

func TestTenantCannotSelectUnavailablePlatform(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Denied Co", "denied-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	// Sharing off, so the tenant is not permitted.
	token := env.tenantAdmin(tenantID)

	status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
		map[string]any{"source": "PLATFORM"}, tenantID)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %v)", status, body["__raw"])
	}
	var count int
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_service_preferences WHERE tenant_id = $1 AND service_type = 'SMTP'`,
		tenantID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Error("a rejected preference must not be persisted")
	}
}

func TestUnknownSourceRejected(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Bad Source Co", "bad-source-"+randSuffix())
	token := env.tenantAdmin(tenantID)
	for _, src := range []string{"platform", "PLATFORM", "organization", "ORGANIZATION"} {
		status, _ := env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
			map[string]any{"source": src}, tenantID)
		if status != http.StatusOK && status != http.StatusConflict {
			t.Errorf("source %q = %d, want 200 or 409", src, status)
		}
	}
	for _, src := range []string{"SYSTEM", "GLOBAL", "", "tenant"} {
		status, _ := env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/smtp", token,
			map[string]any{"source": src}, tenantID)
		if status == http.StatusOK {
			t.Errorf("source %q was accepted, want rejected", src)
		}
	}
}

/* ------------------------------------------------------------------ *
 * Missing configuration
 * ------------------------------------------------------------------ */

func TestMissingPlatformConfiguration(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("AI")
	tenantID := env.createTenant("No AI Co", "no-ai-co-"+randSuffix())
	// Grant everything, but never configure the platform service.
	sharePlatformNothing(t, env)
	grantAccess(t, env, tenantID, "AI", true)
	setSource(t, env, tenantID, "AI", "PLATFORM")

	_, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/ai/effective",
		env.tenantAdmin(tenantID), nil, tenantID)
	if body["available"] != false {
		t.Errorf("available = %v, want false when no platform config exists", body["available"])
	}
	reason, _ := body["reason"].(string)
	if reason == "" {
		t.Error("a missing platform configuration must explain itself")
	}
}

// sharePlatformNothing is a no-op guard documenting that no platform row exists.
func sharePlatformNothing(t *testing.T, env *testEnv) {
	t.Helper()
	env.resetService("AI")
}

func TestDisabledOrganizationConfiguration(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Own Disabled Co", "own-off-"+randSuffix())
	token := env.tenantAdmin(tenantID)

	// Save with enabled=false.
	body := smtpBody("smtp.own.local", "own-secret")
	body["enabled"] = false
	status, resp := env.doAsTenant(http.MethodPut, "/api/v1/tenant/configurations/smtp", token, body, tenantID)
	env.mustStatus(http.StatusOK, status, "save disabled", resp)

	_, eff := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if eff["available"] != false {
		t.Errorf("available = %v, want false for a disabled own configuration", eff["available"])
	}
	if eff["source"] != "ORGANIZATION" {
		t.Errorf("source = %v, want ORGANIZATION", eff["source"])
	}
}

/* ------------------------------------------------------------------ *
 * Super Admin tenant control
 * ------------------------------------------------------------------ */

func TestSuperAdminControlsTenantAccess(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Managed Co", "managed-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	sharePlatform(t, env, "SMTP", true)
	admin := env.superAdmin()

	// Grant.
	status, body := env.do(http.MethodPut,
		"/api/v1/admin/tenants/"+tenantID.String()+"/configurations/smtp/access", admin,
		map[string]any{"allow_platform": true})
	env.mustStatus(http.StatusOK, status, "grant", body)

	var allow bool
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT allow_platform FROM tenant_service_access WHERE tenant_id = $1 AND service_type = 'SMTP'`,
		tenantID).Scan(&allow); err != nil {
		t.Fatal(err)
	}
	if !allow {
		t.Error("grant did not persist")
	}

	// Revoke while the tenant is pointed at the platform: the response must
	// warn that the tenant loses access, and must not silently rewrite it.
	setSource(t, env, tenantID, "SMTP", "PLATFORM")
	status, body = env.do(http.MethodPut,
		"/api/v1/admin/tenants/"+tenantID.String()+"/configurations/smtp/access", admin,
		map[string]any{"allow_platform": false})
	env.mustStatus(http.StatusOK, status, "revoke", body)
	if warning, _ := body["warning"].(string); warning == "" {
		t.Error("revoking access from a tenant using the platform must warn")
	}
	if got := sourcePref(t, env, tenantID, "SMTP"); got != "PLATFORM" {
		t.Errorf("preference = %q, want PLATFORM untouched", got)
	}
}

func TestPlatformSharingRequiresEnabled(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	seedPlatform(t, env, "SMTP")
	admin := env.superAdmin()

	// Turn it off, then try to share it.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE platform_configurations SET enabled = false WHERE service_type = 'SMTP'`); err != nil {
		t.Fatal(err)
	}
	status, body := env.do(http.MethodPatch, "/api/v1/admin/configurations/smtp/sharing", admin,
		map[string]any{"allow_tenants": true})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when sharing a disabled configuration (%v)", status, body["__raw"])
	}
}

func TestTenantOverviewCarriesNoConfigValues(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Overview Co", "overview-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	env.doAsTenant(http.MethodPut, "/api/v1/tenant/configurations/smtp", env.tenantAdmin(tenantID),
		smtpBody("smtp.overview.local", "OVERVIEW_CANARY"), tenantID)

	_, body := env.do(http.MethodGet,
		"/api/v1/admin/tenants/"+tenantID.String()+"/configurations", env.superAdmin(), nil)
	raw, _ := body["__raw"].(string)
	for _, leak := range []string{"OVERVIEW_CANARY", "smtp.overview.local", "platform-secret", "noreply@"} {
		if strings.Contains(raw, leak) {
			t.Errorf("tenant overview leaked %q", leak)
		}
	}
}

// TestDetailAndEffectiveEndpointsAgree guards a real inconsistency that shipped
// in the first draft: the detail endpoint reported the platform as "in use"
// while /effective reported it unavailable, so the tenant page would show no
// warning while the backend refused to send anything.
func TestDetailAndEffectiveEndpointsAgree(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	tenantID := env.createTenant("Agreement Co", "agree-co-"+randSuffix())
	seedPlatform(t, env, "SMTP")
	sharePlatform(t, env, "SMTP", true)
	grantAccess(t, env, tenantID, "SMTP", true)
	setSource(t, env, tenantID, "SMTP", "PLATFORM")
	token := env.tenantAdmin(tenantID)

	// Healthy: both endpoints must say it is in use.
	_, detail := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp", token, nil, tenantID)
	_, eff := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)
	if detail["in_use"] != true || eff["available"] != true {
		t.Fatalf("healthy state disagrees: in_use=%v available=%v", detail["in_use"], eff["available"])
	}

	// Now disable the platform underneath the tenant.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE platform_configurations SET enabled = false WHERE service_type = 'SMTP'`); err != nil {
		t.Fatal(err)
	}

	_, detail = env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp", token, nil, tenantID)
	_, eff = env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp/effective", token, nil, tenantID)

	if detail["in_use"] != false {
		t.Errorf("detail says in_use=%v, want false once the platform is disabled", detail["in_use"])
	}
	if eff["available"] != false {
		t.Errorf("effective says available=%v, want false", eff["available"])
	}
	// The tenant must be told why, on both endpoints, and must not be silently
	// moved onto its own configuration.
	for name, body := range map[string]map[string]any{"detail": detail, "effective": eff} {
		if reason, _ := body["reason"].(string); reason == "" {
			if r2, _ := body["unavailable_reason"].(string); r2 == "" {
				t.Errorf("%s endpoint gave no reason", name)
			}
		}
		if hint, _ := body["hint"].(string); hint == "" {
			t.Errorf("%s endpoint gave no tenant-facing hint", name)
		}
	}
	if detail["source"] != "PLATFORM" {
		t.Errorf("source = %v, want the stored preference left as PLATFORM", detail["source"])
	}
	opts := mustMapAt(t, detail, "options")
	if opts["platform_available"] != false {
		t.Errorf("platform_available = %v, want false when the platform row is disabled", opts["platform_available"])
	}
}
