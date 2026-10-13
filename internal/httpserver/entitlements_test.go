package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// V1 S1–S5: plan limits, feature gates and the license lifecycle, end to end
// through the real router and database.

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func errDetail(body map[string]any, key string) any {
	e, _ := body["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d[key]
}

// tenantOnPlan creates a throwaway plan with the given limits and features and
// a tenant on it. The plan is removed after the tenant (cleanups run LIFO, and
// the plan's is registered first).
func (e *testEnv) tenantOnPlan(maxStaff, maxProducts int, entitlements []string) (tenantID, planID uuid.UUID) {
	// ₹1: cheaper than every real paid plan, so those remain valid "upgrade" targets.
	return e.tenantOnPricedPlan(1, maxStaff, maxProducts, entitlements)
}

func (e *testEnv) tenantOnPricedPlan(price float64, maxStaff, maxProducts int, entitlements []string) (tenantID, planID uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.server.pool.QueryRow(ctx,
		`INSERT INTO plans (name, price, max_staff, max_products, entitlements)
		 VALUES ($1, $5, $2, $3, $4) RETURNING id`,
		"QA-"+strings.ToUpper(randSuffix()), maxStaff, maxProducts, entitlements, price).Scan(&planID); err != nil {
		e.t.Fatalf("create test plan: %v", err)
	}
	e.t.Cleanup(func() { _, _ = e.server.pool.Exec(ctx, `DELETE FROM plans WHERE id = $1`, planID) })
	tenantID = e.createTenant("Plan Test", "plan-"+randSuffix())
	if _, err := e.server.pool.Exec(ctx, `UPDATE tenants SET plan_id = $1 WHERE id = $2`, planID, tenantID); err != nil {
		e.t.Fatalf("assign plan: %v", err)
	}
	return tenantID, planID
}

func (e *testEnv) newCategory(tenantID uuid.UUID) string {
	e.t.Helper()
	var id uuid.UUID
	if err := e.server.pool.QueryRow(context.Background(),
		`INSERT INTO categories (tenant_id, name, sort_order) VALUES ($1, 'Cat', 1) RETURNING id`,
		tenantID).Scan(&id); err != nil {
		e.t.Fatalf("create category: %v", err)
	}
	return id.String()
}

func TestPlanLimitBlocksProductCreation(t *testing.T) {
	env := newTestEnv(t)
	tenantID, _ := env.tenantOnPlan(5, 1, []string{"ANALYTICS"})
	cat := env.newCategory(tenantID)
	admin := env.tenantAdmin(tenantID)
	product := map[string]any{"name": "Momo", "category_id": cat, "price": 100}

	status, body := env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, product, tenantID)
	env.mustStatus(http.StatusCreated, status, "first product is within the plan", body)
	id, _ := body["id"].(string)

	status, body = env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, product, tenantID)
	env.mustStatus(http.StatusForbidden, status, "second product exceeds max_products", body)
	if got := errCode(body); got != "plan_limit" {
		t.Errorf("error code = %q, want plan_limit", got)
	}
	if got := errDetail(body, "resource"); got != "products" {
		t.Errorf("details.resource = %v, want products", got)
	}
	if got := errDetail(body, "limit"); got != float64(1) {
		t.Errorf("details.limit = %v, want 1", got)
	}
	if errDetail(body, "required_plan") == nil {
		t.Error("an upgrade prompt needs details.required_plan")
	}

	// Duplicating a product creates one too, so it must be gated the same way.
	status, body = env.doAs(http.MethodPost, "/api/v1/tenant/products/"+id+"/duplicate", admin, nil, tenantID)
	env.mustStatus(http.StatusForbidden, status, "duplicate exceeds max_products", body)
	if got := errCode(body); got != "plan_limit" {
		t.Errorf("duplicate error code = %q, want plan_limit", got)
	}
}

func TestPlanLimitBlocksStaffInvitesAndReEnabling(t *testing.T) {
	env := newTestEnv(t)
	// The owner holds a seat too, so 3 seats = owner + two invites.
	tenantID, _ := env.tenantOnPlan(3, 50, []string{"ANALYTICS"})
	admin := env.tenantAdmin(tenantID)
	invite := func(label string) (int, map[string]any) {
		return env.doAs(http.MethodPost, "/api/v1/tenant/users", admin, map[string]any{
			"name": label, "email": label + "-" + randSuffix() + "@test.local", "role": "STAFF",
		}, tenantID)
	}

	status, first := invite("a")
	env.mustStatus(http.StatusCreated, status, "seat 2 (owner holds seat 1)", first)
	status, body := invite("b")
	env.mustStatus(http.StatusCreated, status, "seat 3", body)
	status, body = invite("c")
	env.mustStatus(http.StatusForbidden, status, "seat 4 exceeds max_staff", body)
	if got := errCode(body); got != "plan_limit" {
		t.Errorf("error code = %q, want plan_limit", got)
	}

	// Disabling a user frees the seat...
	userA, _ := first["user"].(map[string]any)
	idA, _ := userA["id"].(string)
	status, body = env.doAs(http.MethodPatch, "/api/v1/tenant/users/"+idA, admin,
		map[string]any{"status": "DISABLED"}, tenantID)
	env.mustStatus(http.StatusOK, status, "disable a user", body)
	status, body = invite("d")
	env.mustStatus(http.StatusCreated, status, "freed seat is reusable", body)

	// ...and re-enabling takes one again, so it cannot be used to dodge the cap.
	status, body = env.doAs(http.MethodPatch, "/api/v1/tenant/users/"+idA, admin,
		map[string]any{"status": "ACTIVE"}, tenantID)
	env.mustStatus(http.StatusForbidden, status, "re-enable beyond max_staff", body)
	if got := errCode(body); got != "plan_limit" {
		t.Errorf("re-enable error code = %q, want plan_limit", got)
	}
}

func TestFeatureGateFollowsThePlan(t *testing.T) {
	env := newTestEnv(t)
	tenantID, planID := env.tenantOnPlan(5, 50, []string{})
	admin := env.tenantAdmin(tenantID)

	status, body := env.doAs(http.MethodGet, "/api/v1/tenant/analytics", admin, nil, tenantID)
	env.mustStatus(http.StatusForbidden, status, "analytics without the feature", body)
	if got := errCode(body); got != "feature_not_in_plan" {
		t.Errorf("error code = %q, want feature_not_in_plan", got)
	}
	if got := errDetail(body, "feature"); got != "ANALYTICS" {
		t.Errorf("details.feature = %v, want ANALYTICS", got)
	}
	if errDetail(body, "required_plan") == nil {
		t.Error("an upgrade prompt needs details.required_plan")
	}

	// The basic dashboard is not gated; only the deeper analytics are.
	status, body = env.doAs(http.MethodGet, "/api/v1/tenant/dashboard", admin, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "dashboard stays available", body)

	// Upgrading the plan takes effect on the next request; nothing is cached.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE plans SET entitlements = ARRAY['ANALYTICS'] WHERE id = $1`, planID); err != nil {
		t.Fatal(err)
	}
	status, body = env.doAs(http.MethodGet, "/api/v1/tenant/analytics", admin, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "analytics once the plan includes it", body)
}

func TestEntitlementsEndpointReportsPlanUsageAndLicense(t *testing.T) {
	env := newTestEnv(t)
	tenantID, _ := env.tenantOnPlan(3, 7, []string{"ANALYTICS"})
	cat := env.newCategory(tenantID)
	env.createProduct(tenantID, uuid.MustParse(cat), "Momo", 10)
	staff := env.mintToken("STAFF", &tenantID) // every member can read their plan

	status, body := env.doAs(http.MethodGet, "/api/v1/tenant/entitlements", staff, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "entitlements", body)

	limits, _ := body["limits"].(map[string]any)
	products, _ := limits["products"].(map[string]any)
	if products["used"] != float64(1) || products["limit"] != float64(7) || products["over"] != false {
		t.Errorf("products usage = %v, want used 1 of 7", products)
	}
	staffUse, _ := limits["staff"].(map[string]any)
	if staffUse["limit"] != float64(3) {
		t.Errorf("staff limit = %v, want 3", staffUse["limit"])
	}

	features, _ := body["features"].([]any)
	enabled := map[string]bool{}
	for _, f := range features {
		m, _ := f.(map[string]any)
		enabled[fmt.Sprint(m["code"])], _ = m["enabled"].(bool)
	}
	if !enabled["ANALYTICS"] || enabled["INVENTORY"] {
		t.Errorf("feature flags = %v, want ANALYTICS on and INVENTORY off", enabled)
	}
	license, _ := body["license"].(map[string]any)
	if license["state"] != "ACTIVE" {
		t.Errorf("license state = %v, want ACTIVE", license["state"])
	}
}

func TestOverLimitAfterDowngradeKeepsRowsButBlocksNewOnes(t *testing.T) {
	env := newTestEnv(t)
	tenantID, planID := env.tenantOnPlan(5, 3, []string{"ANALYTICS"})
	cat := uuid.MustParse(env.newCategory(tenantID))
	for i := range 3 {
		env.createProduct(tenantID, cat, fmt.Sprintf("P%d", i), 10)
	}
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE plans SET max_products = 1 WHERE id = $1`, planID); err != nil {
		t.Fatal(err)
	}
	admin := env.tenantAdmin(tenantID)

	status, body := env.doAs(http.MethodGet, "/api/v1/tenant/entitlements", admin, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "entitlements", body)
	limits, _ := body["limits"].(map[string]any)
	products, _ := limits["products"].(map[string]any)
	if products["over"] != true {
		t.Errorf("a tenant above its limit should report over = true, got %v", products)
	}
	status, body = env.doAs(http.MethodPost, "/api/v1/tenant/products", admin,
		map[string]any{"name": "New", "category_id": cat.String(), "price": 1}, tenantID)
	env.mustStatus(http.StatusForbidden, status, "new product while over the limit", body)
}

func TestLicenseLifecycleGraceExpiryAndSuspension(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("License Test", "lic-"+randSuffix())
	admin := env.tenantAdmin(tenantID)
	setLicense := func(status, expires string) {
		t.Helper()
		if _, err := env.server.pool.Exec(context.Background(),
			`UPDATE licenses SET status = $2, expires_at = `+expires+` WHERE tenant_id = $1`,
			tenantID, status); err != nil {
			t.Fatal(err)
		}
	}
	get := func() (int, map[string]any) {
		return env.doAs(http.MethodGet, "/api/v1/tenant/entitlements", admin, nil, tenantID)
	}

	// Past expires_at but inside the template's grace period: still working,
	// and the owner can be told.
	setLicense("ACTIVE", "now() - interval '1 day'")
	status, body := get()
	env.mustStatus(http.StatusOK, status, "inside the grace period", body)
	license, _ := body["license"].(map[string]any)
	if license["state"] != "GRACE" || license["grace_ends_at"] == nil {
		t.Errorf("license = %v, want state GRACE with grace_ends_at", license)
	}

	// Past the grace period: blocked, and the reason says it expired.
	setLicense("ACTIVE", "now() - interval '30 days'")
	status, body = get()
	env.mustStatus(http.StatusForbidden, status, "after the grace period", body)
	if errCode(body) != "license_inactive" || errDetail(body, "state") != "EXPIRED" {
		t.Errorf("error = %v, want license_inactive / EXPIRED", body["__raw"])
	}

	setLicense("SUSPENDED", "NULL")
	status, body = get()
	env.mustStatus(http.StatusForbidden, status, "suspended license", body)
	if errDetail(body, "state") != "SUSPENDED" {
		t.Errorf("details.state = %v, want SUSPENDED", errDetail(body, "state"))
	}

	// Renewal takes effect immediately.
	setLicense("ACTIVE", "now() + interval '30 days'")
	status, body = get()
	env.mustStatus(http.StatusOK, status, "renewed license", body)
	license, _ = body["license"].(map[string]any)
	if license["state"] != "ACTIVE" {
		t.Errorf("license state = %v, want ACTIVE", license["state"])
	}
}

func TestSuperAdminEditsPlanEntitlements(t *testing.T) {
	env := newTestEnv(t)
	name := "QA-" + strings.ToUpper(randSuffix())
	t.Cleanup(func() {
		_, _ = env.server.pool.Exec(context.Background(), `DELETE FROM plans WHERE name = $1`, name)
	})
	sa := env.superAdmin()

	status, body := env.do(http.MethodPost, "/api/v1/admin/plans", sa, map[string]any{
		"name": name, "price": 1, "entitlements": []string{"inventory", "analytics", "ANALYTICS"},
	})
	env.mustStatus(http.StatusCreated, status, "create plan with entitlements", body)
	got := fmt.Sprint(body["entitlements"])
	if got != "[ANALYTICS INVENTORY]" {
		t.Errorf("entitlements = %s, want normalised [ANALYTICS INVENTORY]", got)
	}
	id, _ := body["id"].(string)

	status, body = env.do(http.MethodPost, "/api/v1/admin/plans", sa, map[string]any{
		"name": name + "X", "entitlements": []string{"TELEPORTATION"},
	})
	env.mustStatus(http.StatusBadRequest, status, "unknown entitlement is refused", body)

	// A partial write that does not mention entitlements must not wipe them.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/plans/"+id, sa, map[string]any{"description": "d"})
	env.mustStatus(http.StatusOK, status, "partial update", body)
	if got := fmt.Sprint(body["entitlements"]); got != "[ANALYTICS INVENTORY]" {
		t.Errorf("partial update changed entitlements to %s", got)
	}
	status, body = env.do(http.MethodPatch, "/api/v1/admin/plans/"+id, sa,
		map[string]any{"entitlements": []string{"ANALYTICS"}})
	env.mustStatus(http.StatusOK, status, "update entitlements", body)
	if got := fmt.Sprint(body["entitlements"]); got != "[ANALYTICS]" {
		t.Errorf("entitlements = %s, want [ANALYTICS]", got)
	}

	status, body = env.do(http.MethodGet, "/api/v1/admin/plans", sa, nil)
	env.mustStatus(http.StatusOK, status, "list plans", body)
	if cat, _ := body["entitlement_catalog"].([]any); len(cat) == 0 {
		t.Error("the plan editor needs entitlement_catalog to render its checkboxes")
	}
}

// "Upgrade" must mean a plan that costs more. A business already on the most
// expensive plan has nothing to upgrade to, so the prompt says so instead of
// naming a cheaper plan that merely has a bigger limit.
func TestUpgradeSuggestionNeverPointsAtACheaperPlan(t *testing.T) {
	env := newTestEnv(t)
	tenantID, _ := env.tenantOnPricedPlan(9_000_000, 3, 1, []string{})
	cat := env.newCategory(tenantID)
	admin := env.tenantAdmin(tenantID)
	product := map[string]any{"name": "Momo", "category_id": cat, "price": 100}
	env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, product, tenantID)

	status, body := env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, product, tenantID)
	env.mustStatus(http.StatusForbidden, status, "over the product limit on the priciest plan", body)
	if got := errDetail(body, "required_plan"); got != nil {
		t.Errorf("suggested %v as an upgrade from a ₹9,000,000 plan — every real plan is cheaper", got)
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); strings.Contains(msg, "Upgrade to") {
		t.Errorf("message %q tells the owner to upgrade to nothing", msg)
	}

	status, body = env.doAs(http.MethodGet, "/api/v1/tenant/analytics", admin, nil, tenantID)
	env.mustStatus(http.StatusForbidden, status, "feature missing on the priciest plan", body)
	if got := errDetail(body, "required_plan"); got != nil {
		t.Errorf("feature gate suggested %v from the most expensive plan", got)
	}
}

func TestLimitMessagesUseTheRightSingularAndPlural(t *testing.T) {
	env := newTestEnv(t)
	tenantID, _ := env.tenantOnPlan(1, 1, []string{"ANALYTICS"}) // 1 seat: the owner holds it
	cat := env.newCategory(tenantID)
	admin := env.tenantAdmin(tenantID)
	env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, map[string]any{"name": "A", "category_id": cat, "price": 1}, tenantID)

	msg := func(body map[string]any) string { m, _ := body["error"].(map[string]any)["message"].(string); return m }
	_, body := env.doAs(http.MethodPost, "/api/v1/tenant/products", admin, map[string]any{"name": "B", "category_id": cat, "price": 1}, tenantID)
	if m := msg(body); !strings.Contains(m, "up to 1 product.") || strings.Contains(m, "1 products") {
		t.Errorf("product message = %q, want \"up to 1 product.\"", m)
	}
	_, body = env.doAs(http.MethodPost, "/api/v1/tenant/users", admin, map[string]any{"name": "S", "email": "s-" + randSuffix() + "@test.local", "role": "STAFF"}, tenantID)
	if m := msg(body); !strings.Contains(m, "up to 1 staff member.") || strings.Contains(m, "1 staff members") {
		t.Errorf("staff message = %q, want \"up to 1 staff member.\"", m)
	}

	tenant5, _ := env.tenantOnPlan(5, 5, []string{"ANALYTICS"})
	cat5 := env.newCategory(tenant5)
	for i := 0; i < 5; i++ {
		env.createProduct(tenant5, uuid.MustParse(cat5), fmt.Sprintf("P%d", i), 1)
	}
	_, body = env.doAs(http.MethodPost, "/api/v1/tenant/products", env.tenantAdmin(tenant5), map[string]any{"name": "X", "category_id": cat5, "price": 1}, tenant5)
	if m := msg(body); !strings.Contains(m, "up to 5 products.") {
		t.Errorf("plural message = %q, want \"up to 5 products.\"", m)
	}
}
