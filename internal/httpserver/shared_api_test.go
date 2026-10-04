package httpserver

import (
	"net/http"
	"testing"
)

func TestSharedAPI_PublicThemeWithTenantSlugHeader(t *testing.T) {
	env := newTestEnv(t)
	slug := "shared-api-" + randSuffix()
	_ = env.createTenant("Shared API Shop", slug)

	status, body := env.doOnSharedAPI(http.MethodGet, "/api/v1/public/theme", "", nil, slug)
	env.mustStatus(http.StatusOK, status, "public theme via X-Tenant-Slug", body)
}

func TestSharedAPI_PublicThemeRequiresTenantContext(t *testing.T) {
	env := newTestEnv(t)
	status, body := env.doOn(http.MethodGet, "/api/v1/public/theme", "", nil, "api."+env.cfg.BaseDomain)
	if status == http.StatusOK {
		t.Fatalf("public theme on bare api host without slug should fail, body=%v", body["__raw"])
	}
}

func TestSharedAPI_TenantLoginAndSetupPassword(t *testing.T) {
	env := newTestEnv(t)
	slug := "login-slug-" + randSuffix()
	email := "owner-" + randSuffix() + "@test.local"

	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":           "Login Slug Shop",
		"slug":           slug,
		"business_type":  "FOOD_SHOP",
		"owner_name":     "Owner",
		"admin_name":     "Admin",
		"admin_email":    email,
		"email":          email,
		"terms_accepted": true,
	})
	env.mustStatus(http.StatusCreated, status, "create tenant for shared API login", body)

	invite, _ := body["invite_token"].(string)
	if invite == "" {
		t.Fatalf("invite_token missing: %#v", body)
	}

	status, body = env.doOn(http.MethodPost, "/api/v1/auth/setup-password", "", map[string]any{
		"token":    invite,
		"password": "SecurePass123!",
	}, "api."+env.cfg.BaseDomain)
	env.mustStatus(http.StatusOK, status, "setup-password on shared API host", body)

	status, body = env.doOnSharedAPI(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email":    email,
		"password": "SecurePass123!",
	}, slug)
	env.mustStatus(http.StatusOK, status, "tenant login via shared API + slug", body)
}

func TestSharedAPI_TenantLoginWithoutSlugFails(t *testing.T) {
	env := newTestEnv(t)
	status, body := env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email":    "x@example.com",
		"password": "whatever",
	}, "api."+env.cfg.BaseDomain)
	env.mustStatus(http.StatusBadRequest, status, "tenant login without slug", body)
}

func TestSharedAPI_AuthenticatedTenantRoute(t *testing.T) {
	env := newTestEnv(t)
	slug := "auth-slug-" + randSuffix()
	tenantID := env.createTenant("Auth Slug Shop", slug)
	token := env.tenantAdmin(tenantID)

	status, body := env.doOnSharedAPI(http.MethodGet, "/api/v1/tenant/storefront", token, nil, slug)
	env.mustStatus(http.StatusOK, status, "tenant route on shared API", body)
}

func TestSharedAPI_CrossTenantSlugRejected(t *testing.T) {
	env := newTestEnv(t)
	slugA := "iso-a-" + randSuffix()
	slugB := "iso-b-" + randSuffix()
	tenantA := env.createTenant("Iso A", slugA)
	_ = env.createTenant("Iso B", slugB)
	tokenA := env.tenantAdmin(tenantA)

	status, body := env.doOnSharedAPI(http.MethodGet, "/api/v1/tenant/storefront", tokenA, nil, slugB)
	env.mustStatus(http.StatusForbidden, status, "cross-tenant slug mismatch", body)
}

func TestSharedAPI_JWTFallbackWithoutSlug(t *testing.T) {
	env := newTestEnv(t)
	slug := "jwt-fb-" + randSuffix()
	tenantID := env.createTenant("JWT Fallback Shop", slug)
	token := env.tenantAdmin(tenantID)

	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/storefront", token, nil, "api."+env.cfg.BaseDomain)
	env.mustStatus(http.StatusOK, status, "JWT tenant fallback on shared API", body)
}

func TestSharedAPI_PlatformAdminUnaffected(t *testing.T) {
	env := newTestEnv(t)
	status, body := env.do(http.MethodGet, "/api/v1/admin/tenants", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "platform admin on api host", body)
}
