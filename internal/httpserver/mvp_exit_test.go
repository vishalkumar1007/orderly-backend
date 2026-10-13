package httpserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// MVP exit checks (Release V1 §3.1): the Food happy path through the public
// API only, tenant isolation on the staff API, and store status at checkout.
// These are the local, repeatable half of the production tester guide.

// TestFoodHappyPathFromOnboardingToCompleted walks the master happy path with
// no SQL shortcuts: Super Admin onboards a business, the owner sets a password
// and signs in, builds a menu, publishes and opens the shop, a guest orders,
// and staff take the order PENDING → ... → COMPLETED while the guest tracks it.
func TestFoodHappyPathFromOnboardingToCompleted(t *testing.T) {
	env := newTestEnv(t)
	slug := "happy-" + randSuffix()
	email := "owner-" + randSuffix() + "@test.local"

	// 1. Super Admin onboards a Food business.
	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name": "Happy Path Momo", "slug": slug, "business_type": "RESTAURANT",
		"owner_name": "Owner", "admin_name": "Owner", "admin_email": email, "email": email,
		"terms_accepted": true,
	})
	env.mustStatus(http.StatusCreated, status, "onboard a business", body)
	tenantID := env.tenantID(slug)
	env.createdTenants = append(env.createdTenants, tenantID)
	t.Cleanup(env.removeTestTenants)
	invite, _ := body["invite_token"].(string)
	if invite == "" {
		t.Fatalf("onboarding returned no invite token: %v", body["__raw"])
	}

	// 2. The owner sets a password and signs in on their own host.
	status, body = env.doOn(http.MethodPost, "/api/v1/auth/setup-password", "",
		map[string]any{"token": invite, "password": "SecurePass123!"}, "api."+env.cfg.BaseDomain)
	env.mustStatus(http.StatusOK, status, "owner sets password", body)
	status, body = env.doOnSharedAPI(http.MethodPost, "/api/v1/auth/tenant/login", "",
		map[string]any{"email": email, "password": "SecurePass123!"}, slug)
	env.mustStatus(http.StatusOK, status, "owner signs in", body)
	tokens, _ := body["tokens"].(map[string]any)
	owner, _ := tokens["access_token"].(string)
	if owner == "" {
		t.Fatalf("login returned no access token: %v", body["__raw"])
	}
	shop := func(method, path string, payload any) (int, map[string]any) {
		return env.doOnSharedAPI(method, path, owner, payload, slug)
	}

	// 3. The owner builds a menu, opens the shop and publishes it.
	status, body = shop(http.MethodPost, "/api/v1/tenant/categories", map[string]any{"name": "Momo"})
	env.mustStatus(http.StatusCreated, status, "create category", body)
	categoryID, _ := body["id"].(string)
	status, body = shop(http.MethodPost, "/api/v1/tenant/products",
		map[string]any{"name": "Veg Momo", "category_id": categoryID, "price": 120})
	env.mustStatus(http.StatusCreated, status, "create product", body)
	productID, _ := body["id"].(string)
	status, body = shop(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{"store_status": "OPEN"})
	env.mustStatus(http.StatusOK, status, "set the store OPEN", body)
	status, body = shop(http.MethodPost, "/api/v1/tenant/publish", nil)
	env.mustStatus(http.StatusOK, status, "publish the storefront", body)

	// 4. A guest browses and checks out for pickup.
	guest := func(method, path string, payload any) (int, map[string]any) {
		return env.doOnSharedAPI(method, path, "", payload, slug)
	}
	status, body = guest(http.MethodGet, "/api/v1/public/menu", nil)
	env.mustStatus(http.StatusOK, status, "guest sees the menu", body)
	status, body = guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name": "Asha Rao", "customer_phone": "9876543210", "payment_method": "CASH",
		"client_token": "happy-" + randSuffix(),
		"items":        []map[string]any{{"product_id": productID, "quantity": 2}},
	})
	env.mustStatus(http.StatusCreated, status, "guest places an order", body)
	orderNumber := number(t, body, "order_number")
	if got := str(t, body, "status"); got != "PENDING" {
		t.Fatalf("new order status = %q, want PENDING", got)
	}
	track := func() string {
		t.Helper()
		status, body := guest(http.MethodGet,
			fmt.Sprintf("/api/v1/public/orders/%d?phone=9876543210", orderNumber), nil)
		env.mustStatus(http.StatusOK, status, "guest tracks the order", body)
		return str(t, body, "status")
	}
	if got := track(); got != "PENDING" {
		t.Errorf("tracked status = %q, want PENDING", got)
	}

	// 5. Staff work the order through the kitchen; the guest sees each step.
	orderID := env.orderID(tenantID, orderNumber)
	for _, step := range []struct{ action, want string }{
		{"accept", "ACCEPTED"}, {"prepare", "PREPARING"}, {"ready", "READY"}, {"complete", "COMPLETED"},
	} {
		status, body = shop(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/"+step.action, nil)
		env.mustStatus(http.StatusOK, status, step.action, body)
		if got := track(); got != step.want {
			t.Errorf("after %s the guest sees %q, want %q", step.action, got, step.want)
		}
	}
}

// TestTenantIsolationOnTheStaffAPI proves one business's staff can neither see
// nor touch another's data, on the API and not just the storefront.
func TestTenantIsolationOnTheStaffAPI(t *testing.T) {
	env := newTestEnv(t)
	a := newShop(t, env)
	b := newShop(t, env)
	orderA := a.order(t, "Asha Rao", "9876543210", "CASH")
	orderAID := env.orderID(a.tenantID, number(t, orderA, "order_number"))

	// A's token is rejected on B's host, and B's on A's.
	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/products", a.adminToken, nil, b.host)
	env.mustStatus(http.StatusForbidden, status, "A's token on B's host", body)
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/products", b.adminToken, nil, a.host)
	env.mustStatus(http.StatusForbidden, status, "B's token on A's host", body)
	// The shared API host with the other tenant's slug is no way round it.
	status, body = env.doOnSharedAPI(http.MethodGet, "/api/v1/tenant/products", a.adminToken, nil, b.slug)
	env.mustStatus(http.StatusForbidden, status, "A's token with B's slug header", body)

	// On their own host, B's staff cannot read or change A's rows by id.
	status, body = b.admin(http.MethodGet, "/api/v1/tenant/products/"+a.productID, nil)
	env.mustStatus(http.StatusNotFound, status, "read A's product as B", body)
	status, body = b.admin(http.MethodPatch, "/api/v1/tenant/products/"+a.productID, map[string]any{"name": "hijacked"})
	env.mustStatus(http.StatusNotFound, status, "edit A's product as B", body)
	status, body = b.admin(http.MethodDelete, "/api/v1/tenant/products/"+a.productID, nil)
	if status == http.StatusOK || status == http.StatusNoContent {
		// A delete that "succeeds" is only acceptable if it touched nothing of A's.
		if n := env.countProducts(a.tenantID); n != 1 {
			t.Errorf("B deleted A's product (A has %d products left)", n)
		}
	}
	status, body = b.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderAID+"/accept", nil)
	env.mustStatus(http.StatusNotFound, status, "accept A's order as B", body)

	// B's order list and history never contain A's order.
	status, body = b.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	env.mustStatus(http.StatusOK, status, "B lists orders", body)
	if raw := str(t, body, "__raw"); containsAny(raw, orderAID, "Asha Rao") {
		t.Errorf("B's order list leaks A's order: %s", raw)
	}
	status, body = b.admin(http.MethodGet, "/api/v1/tenant/customers", nil)
	env.mustStatus(http.StatusOK, status, "B lists customers", body)
	if raw := str(t, body, "__raw"); containsAny(raw, "Asha Rao", "9876543210") {
		t.Errorf("B's customer list leaks A's customer: %s", raw)
	}
	// Suspending A must not touch B.
	if _, err := env.server.pool.Exec(tctx(), `UPDATE tenants SET status = 'SUSPENDED' WHERE id = $1`, a.tenantID); err != nil {
		t.Fatal(err)
	}
	status, body = b.admin(http.MethodGet, "/api/v1/tenant/products", nil)
	env.mustStatus(http.StatusOK, status, "B while A is suspended", body)
	status, body = a.admin(http.MethodGet, "/api/v1/tenant/products", nil)
	env.mustStatus(http.StatusForbidden, status, "suspended A", body)
}

// TestStoreStatusAtCheckout covers OPEN / BUSY / AWAY, set through the same
// console route the owner uses: BUSY still takes orders, AWAY pauses them, and
// browsing works either way.
func TestStoreStatusAtCheckout(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	place := func() int {
		status, _ := shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
			"customer_name": "Asha", "customer_phone": "9876543210", "payment_method": "CASH",
			"client_token": "status-" + randSuffix(),
			"items":        []map[string]any{{"product_id": shop.productID, "quantity": 1}},
		})
		return status
	}
	setStatus := func(s string) {
		t.Helper()
		status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{"store_status": s})
		env.mustStatus(http.StatusOK, status, "set store status "+s, body)
	}

	for _, c := range []struct {
		status string
		want   int
	}{
		{"OPEN", http.StatusCreated},
		{"BUSY", http.StatusCreated},
		{"AWAY", http.StatusConflict},
		{"OPEN", http.StatusCreated},
	} {
		setStatus(c.status)
		if got := place(); got != c.want {
			t.Errorf("ordering while %s: status = %d, want %d", c.status, got, c.want)
		}
		status, _ := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
		if status != http.StatusOK {
			t.Errorf("browsing while %s: status = %d, want 200", c.status, status)
		}
	}
	// The storefront config tells the UI whether ordering is on.
	setStatus("AWAY")
	_, body := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	ordering, _ := body["ordering"].(map[string]any)
	if ordering["enabled"] != false {
		t.Errorf("AWAY should report ordering.enabled = false, got %v", ordering["enabled"])
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func (e *testEnv) countProducts(tenantID uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT count(*) FROM products WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		e.t.Fatalf("count products: %v", err)
	}
	return n
}
