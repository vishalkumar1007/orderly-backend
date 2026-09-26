package httpserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/pkg/identity"
)

// shopFixture is a published tenant with a small menu, ready for storefront
// tests. Every test gets its own tenant so nothing leaks between them.
type shopFixture struct {
	env         *testEnv
	tenantID    uuid.UUID
	slug        string
	host        string
	adminToken  string
	staffToken  string
	categoryID  string
	productID   string
	productName string
	price       float64
}

func newShop(t *testing.T, env *testEnv) *shopFixture {
	t.Helper()
	slug := "shop-" + randSuffix()
	tenantID := env.createTenant("Test Kitchen", slug)
	env.publishTenant(tenantID)
	env.setStoreStatus(tenantID, "OPEN")

	var categoryID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO categories (tenant_id, name, sort_order) VALUES ($1, 'Momo', 1) RETURNING id`,
		tenantID).Scan(&categoryID); err != nil {
		t.Fatalf("create category: %v", err)
	}

	price := 120.0
	productID := env.createProduct(tenantID, categoryID, "Veg Momo", price)

	return &shopFixture{
		env:         env,
		tenantID:    tenantID,
		slug:        slug,
		host:        slug + "." + env.cfg.BaseDomain,
		adminToken:  env.tenantAdmin(tenantID),
		staffToken:  env.mintToken(identity.RoleStaff, &tenantID),
		categoryID:  categoryID.String(),
		productID:   productID.String(),
		productName: "Veg Momo",
		price:       price,
	}
}

// guest issues a public storefront request on this shop's host.
func (s *shopFixture) guest(method, path string, body any) (int, map[string]any) {
	return s.env.doOn(method, path, "", body, s.host)
}

// guestAuth issues a public request carrying a customer token.
func (s *shopFixture) guestAuth(method, path, token string, body any) (int, map[string]any) {
	return s.env.doOn(method, path, token, body, s.host)
}

// admin issues a tenant admin request on this shop's host.
func (s *shopFixture) admin(method, path string, body any) (int, map[string]any) {
	return s.env.doOn(method, path, s.adminToken, body, s.host)
}

// order places a guest order and returns the created payload.
func (s *shopFixture) order(t *testing.T, name, phoneNumber, method string) map[string]any {
	t.Helper()
	status, body := s.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  name,
		"customer_phone": phoneNumber,
		"payment_method": method,
		"client_token":   "test-" + randSuffix(),
		"items": []map[string]any{
			{"product_id": s.productID, "quantity": 1},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("place order: status = %d, body: %v", status, body["__raw"])
	}
	return body
}

func number(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("response is missing %q: %v", key, body["__raw"])
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		t.Fatalf("%q is %T, expected a number", key, v)
		return 0
	}
}

func str(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	v, ok := body[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// Public storefront
// ---------------------------------------------------------------------------

func TestPublicStoreServesConfiguration(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	env.mustStatus(http.StatusOK, status, "GET /public/store", body)

	store, _ := body["store"].(map[string]any)
	if got := store["slug"]; got != shop.slug {
		t.Errorf("store.slug = %v, want %s", got, shop.slug)
	}
	theme, _ := body["theme"].(map[string]any)
	vars, _ := theme["vars"].(map[string]any)
	if _, ok := vars["--sf-primary"]; !ok {
		t.Error("the theme must ship resolved CSS custom properties, not just names")
	}
	// The storefront must never be handed tenant-private configuration.
	raw := str(t, body, "__raw")
	for _, secret := range []string{"config_encryption", "jwt", "password", "provider_reference", "database_url"} {
		if strings.Contains(raw, secret) {
			t.Errorf("public payload leaks %q", secret)
		}
	}
}

func TestPublicStoreRequiresATenantHost(t *testing.T) {
	env := newTestEnv(t)
	// The API host is not a storefront host.
	status, _ := env.do(http.MethodGet, "/api/v1/public/store", "", nil)
	env.mustStatus(http.StatusBadRequest, status, "public store on the api host", nil)
}

func TestPublicStoreHidesUnpublishedShopsFromCustomers(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.setPublished(shop.tenantID, false)

	status, _ := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	env.mustStatus(http.StatusNotFound, status, "unpublished store", nil)

	// The owner can still see it, because the admin is authenticated for this
	// exact tenant.
	status, _ = shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	env.mustStatus(http.StatusOK, status, "owner reading own storefront", nil)
}

func TestPublicMenuAndProductDetail(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	env.mustStatus(http.StatusOK, status, "GET /public/menu", body)
	categories, _ := body["categories"].([]any)
	if len(categories) != 1 {
		t.Fatalf("expected 1 category, got %d", len(categories))
	}

	status, body = shop.guest(http.MethodGet, "/api/v1/public/products/"+shop.productID, nil)
	env.mustStatus(http.StatusOK, status, "GET /public/products/{id}", body)
	product, _ := body["product"].(map[string]any)
	if product["name"] != shop.productName {
		t.Errorf("product.name = %v, want %s", product["name"], shop.productName)
	}
	if _, ok := product["addons"]; !ok {
		t.Error("product detail should always include an addons array")
	}
}

func TestUnavailableProductIsHiddenFromTheStorefront(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.setProductAvailable(shop.productID, false)

	status, _ := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	env.mustStatus(http.StatusOK, status, "menu still loads", nil)

	_, body := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	products, _ := body["products"].([]any)
	if len(products) != 0 {
		t.Errorf("an unavailable product must not be listed, got %d", len(products))
	}

	status, _ = shop.guest(http.MethodGet, "/api/v1/public/products/"+shop.productID, nil)
	env.mustStatus(http.StatusNotFound, status, "unavailable product detail", nil)
}

func TestTenantIsolationAcrossTheStorefront(t *testing.T) {
	env := newTestEnv(t)
	first := newShop(t, env)
	second := newShop(t, env)

	// One shop must not be able to read another's product by id.
	status, _ := first.guest(http.MethodGet, "/api/v1/public/products/"+second.productID, nil)
	env.mustStatus(http.StatusNotFound, status, "cross-tenant product read", nil)

	// Nor place an order containing the other shop's product.
	status, _ = first.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items":          []map[string]any{{"product_id": second.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusBadRequest, status, "cross-tenant order", nil)
}

func TestGuestCheckoutAndServerSideTotals(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha Rao",
		"customer_phone": "9876543210",
		"payment_method": "CASH",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 2}},
	})
	env.mustStatus(http.StatusCreated, status, "guest checkout", body)

	totals, _ := body["totals"].(map[string]any)
	if got := totals["subtotal"]; got != shop.price*2 {
		t.Errorf("subtotal = %v, want %v", got, shop.price*2)
	}
	if got := totals["total"]; got != shop.price*2 {
		t.Errorf("total = %v, want %v", got, shop.price*2)
	}
	if str(t, body, "status") != "PENDING" {
		t.Errorf("a manual-acceptance shop should start at PENDING, got %v", body["status"])
	}
	if str(t, body, "next_step") != "confirmation" {
		t.Errorf("a cash order should skip the payment step, got %v", body["next_step"])
	}
	// The reference is the short code a customer reads at the counter.
	if ref := str(t, body, "reference"); !strings.HasSuffix(ref, "0001") && ref == "" {
		t.Errorf("reference = %q", ref)
	}
}

func TestCheckoutRejectsBadInput(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"no name", map[string]any{
			"customer_phone": "9876543210",
			"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
		}, http.StatusBadRequest},
		{"no phone", map[string]any{
			"customer_name": "Asha",
			"items":         []map[string]any{{"product_id": shop.productID, "quantity": 1}},
		}, http.StatusBadRequest},
		{"empty cart", map[string]any{
			"customer_name": "Asha", "customer_phone": "9876543210", "items": []any{},
		}, http.StatusBadRequest},
		{"zero quantity", map[string]any{
			"customer_name": "Asha", "customer_phone": "9876543210",
			"items": []map[string]any{{"product_id": shop.productID, "quantity": 0}},
		}, http.StatusBadRequest},
		{"bad email", map[string]any{
			"customer_name": "Asha", "customer_phone": "9876543210", "customer_email": "nope",
			"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}},
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := shop.guest(http.MethodPost, "/api/v1/public/orders", tc.body)
			if status != tc.want {
				t.Errorf("status = %d, want %d (body %v)", status, tc.want, body["__raw"])
			}
		})
	}
}

func TestCheckoutIgnoresClientSuppliedPrices(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// The client claims the product costs 1. The server must price it from the
	// catalogue, so the order total reflects the real price.
	status, body := shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items": []map[string]any{{
			"product_id": shop.productID, "quantity": 1,
			"price": 1, "unit_price": 1, "total": 1,
		}},
	})
	if status != http.StatusCreated {
		// Unknown fields are rejected outright, which is an equally safe outcome.
		env.mustStatus(http.StatusBadRequest, status, "price in the payload", body)
		return
	}
	totals, _ := body["totals"].(map[string]any)
	if totals["total"] != shop.price {
		t.Errorf("total = %v, want the catalogue price %v", totals["total"], shop.price)
	}
}

func TestDuplicateOrderPrevention(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	token := "idem-" + randSuffix()
	payload := map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"payment_method": "CASH",
		"client_token":   token,
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	}

	firstStatus, first := shop.guest(http.MethodPost, "/api/v1/public/orders", payload)
	env.mustStatus(http.StatusCreated, firstStatus, "first submit", first)

	// The same checkout resubmitted must not place a second order.
	status, second := shop.guest(http.MethodPost, "/api/v1/public/orders", payload)
	env.mustStatus(http.StatusOK, status, "resubmitted checkout", second)
	if number(t, first, "order_number") != number(t, second, "order_number") {
		t.Errorf("resubmit created a new order: %v then %v", first["order_number"], second["order_number"])
	}
	if second["duplicate"] != true {
		t.Error("a resubmitted checkout should be reported as a duplicate")
	}
	if got := env.countOrders(shop.tenantID); got != 1 {
		t.Errorf("expected exactly 1 order in the database, found %d", got)
	}
}

func TestConcurrentSubmitsCreateOneOrder(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	token := "race-" + randSuffix()
	payload := map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"payment_method": "CASH",
		"client_token":   token,
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	}

	// A double tap sends two requests at once. The unique index on
	// (tenant_id, client_token) is what makes this safe, not a read-then-write.
	type result struct {
		status int
		body   map[string]any
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			status, body := shop.guest(http.MethodPost, "/api/v1/public/orders", payload)
			results <- result{status, body}
		}()
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.status != http.StatusCreated && r.status != http.StatusOK {
			t.Errorf("concurrent submit status = %d (body %v)", r.status, r.body["__raw"])
		}
	}
	if got := env.countOrders(shop.tenantID); got != 1 {
		t.Errorf("concurrent submits created %d orders, want 1", got)
	}
}

func TestQuoteUsesServerPricing(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.setCosting(shop.tenantID, 5, 10)

	status, body := shop.guest(http.MethodPost, "/api/v1/public/quote", map[string]any{
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 2}},
	})
	env.mustStatus(http.StatusOK, status, "POST /public/quote", body)

	totals, _ := body["totals"].(map[string]any)
	// 240 + 5% tax (12) + 10 packaging.
	if got := totals["total"]; got != 262.0 {
		t.Errorf("total = %v, want 262", got)
	}
}

func TestQuoteRejectsAnUnavailableProduct(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.setProductAvailable(shop.productID, false)

	status, _ := shop.guest(http.MethodPost, "/api/v1/public/quote", map[string]any{
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusBadRequest, status, "quote with a sold-out item", nil)
}

// ---------------------------------------------------------------------------
// Order tracking
// ---------------------------------------------------------------------------

func TestGuestTrackingRequiresProofOfOwnership(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderNo := number(t, order, "order_number")

	// An order number on its own reveals nothing.
	status, _ := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d", orderNo), nil)
	env.mustStatus(http.StatusBadRequest, status, "tracking with no phone", nil)

	// A different phone does not match either.
	status, _ = shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d?phone=9999999999", orderNo), nil)
	env.mustStatus(http.StatusNotFound, status, "tracking with the wrong phone", nil)

	// The right phone works, and the customer's own number is masked.
	status, body := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d?phone=9876543210", orderNo), nil)
	env.mustStatus(http.StatusOK, status, "tracking with the right phone", body)
	if got := str(t, body, "phone_masked"); strings.Contains(got, "9876543210") {
		t.Errorf("phone_masked leaked the number: %q", got)
	}
	if _, ok := body["timeline"]; !ok {
		t.Error("tracking should include a timeline")
	}
}

func TestGuestLookupByPhone(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	older := shop.order(t, "Asha Rao", "9876543210", "CASH")
	newer := shop.order(t, "Asha Rao", "9876543210", "CASH")

	status, body := shop.guest(http.MethodGet, "/api/v1/public/orders/lookup?phone=9876543210", nil)
	env.mustStatus(http.StatusOK, status, "guest lookup", body)
	orders, _ := body["orders"].([]any)
	if len(orders) != 2 {
		t.Fatalf("expected 2 orders for that number, got %d", len(orders))
	}
	// Newest first, so a customer returning to the shop sees their latest order
	// at the top rather than having to hunt for it.
	if got := orders[0].(map[string]any)["order_number"]; got != newer["order_number"] {
		t.Errorf("expected the newest order first, got %v", got)
	}
	if got := orders[1].(map[string]any)["order_number"]; got != older["order_number"] {
		t.Errorf("expected the older order second, got %v", got)
	}
}

func TestClosedStoreBlocksOrderingButNotBrowsing(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.setStoreStatus(shop.tenantID, "CLOSED")

	// Browsing must keep working: a customer can still look at the menu.
	status, _ := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	env.mustStatus(http.StatusOK, status, "menu while closed", nil)

	status, _ = shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusConflict, status, "ordering while closed", nil)

	// The storefront config tells the UI to show a "Currently Closed" state.
	_, body := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	ordering, _ := body["ordering"].(map[string]any)
	if ordering["enabled"] != false {
		t.Error("the store payload should report ordering as unavailable")
	}
	if reason := fmt.Sprint(ordering["closed_reason"]); reason == "" {
		t.Error("a closed store should explain itself to the customer")
	}
}

func TestOpeningHoursBlockOrderingOutsideTheWindow(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	// A window that is definitely not now: one minute a day, at 00:00.
	env.setOpeningHours(shop.tenantID, map[string]any{
		"always_open": false,
		"timezone":    "UTC",
		"schedule":    map[string]any{"mon": []string{"00:00", "00:01"}},
	})

	status, body := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	env.mustStatus(http.StatusOK, status, "store config", body)
	ordering, _ := body["ordering"].(map[string]any)
	if ordering["enabled"] != false {
		t.Error("ordering should be blocked outside opening hours")
	}

	status, _ = shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusConflict, status, "ordering outside opening hours", nil)
}

func TestOrderingDisabledSwitch(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{
		"ordering_enabled": false,
		"closed_message":   "Kitchen closed for Diwali",
	})
	env.mustStatus(http.StatusOK, status, "turn ordering off", nil)

	status, _ = shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusConflict, status, "ordering with the switch off", nil)

	_, config := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	ordering, _ := config["ordering"].(map[string]any)
	if got := fmt.Sprint(ordering["closed_reason"]); !strings.Contains(got, "Diwali") {
		t.Errorf("the tenant's own closed message should be shown, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Phone login
// ---------------------------------------------------------------------------

func TestPhoneLoginRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp",
		map[string]any{"phone": "9876543210"})
	env.mustStatus(http.StatusOK, status, "send otp", body)
	code := env.otpFromResponse(t, body)

	status, body = shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
		map[string]any{"phone": "9876543210", "code": code})
	env.mustStatus(http.StatusOK, status, "verify otp", body)

	tokens, _ := body["tokens"].(map[string]any)
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("no access token issued: %v", body["__raw"])
	}
	customer, _ := body["customer"].(map[string]any)
	if got := fmt.Sprint(customer["phone"]); strings.Contains(got, "9876543210") {
		t.Errorf("the sign-in response leaked the full number: %q", got)
	}

	// The token works on the customer API.
	status, profile := shop.guestAuth(http.MethodGet, "/api/v1/customer/profile", access, nil)
	env.mustStatus(http.StatusOK, status, "customer profile", profile)
}

func TestPhoneLoginRejectsBadCodes(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	_, sent := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp",
		map[string]any{"phone": "9876543210"})
	code := env.otpFromResponse(t, sent)

	// A wrong code is rejected, and it costs an attempt.
	status, _ := shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
		map[string]any{"phone": "9876543210", "code": "000000"})
	if status != http.StatusBadRequest {
		t.Errorf("wrong code status = %d, want 400", status)
	}

	// One typo is not fatal: the real code still works afterwards.
	status, body := shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
		map[string]any{"phone": "9876543210", "code": code})
	env.mustStatus(http.StatusOK, status, "correct code after one typo", body)

	// A consumed code cannot be replayed. An attacker who saw the SMS must not
	// be able to reuse what they read.
	status, _ = shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
		map[string]any{"phone": "9876543210", "code": code})
	if status == http.StatusOK {
		t.Error("an OTP must not be usable twice")
	}
}

func TestPhoneLoginLocksOutAfterTooManyAttempts(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	_, sent := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp",
		map[string]any{"phone": "9876543210"})
	code := env.otpFromResponse(t, sent)

	// The attempt counter lives in the database, so reloading the page does not
	// reset it.
	var locked bool
	for i := 0; i < 6; i++ {
		status, _ := shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
			map[string]any{"phone": "9876543210", "code": "000000"})
		if status == http.StatusTooManyRequests {
			locked = true
			break
		}
	}
	if !locked {
		t.Fatal("repeated wrong codes should lock the number out")
	}

	// Even the correct code is refused once locked out, so guessing cannot
	// eventually land.
	status, _ := shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp",
		map[string]any{"phone": "9876543210", "code": code})
	if status == http.StatusOK {
		t.Error("a locked-out number must not be able to sign in")
	}
}

func TestPhoneLoginResendIsRateLimited(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"phone": "9876543210"})

	// Asking again immediately is refused, so a customer cannot be spammed or
	// burn through the hourly allowance by tapping repeatedly.
	status, _ := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp",
		map[string]any{"phone": "9876543210"})
	env.mustStatus(http.StatusTooManyRequests, status, "immediate resend", nil)
}

func TestPhoneLoginRejectsMalformedNumbers(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	for _, bad := range []string{"", "abc", "+", "12345"} {
		status, _ := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"phone": bad})
		if status != http.StatusBadRequest {
			t.Errorf("phone %q status = %d, want 400", bad, status)
		}
	}
}

func TestCustomerOrdersArePrivateToTheirOwner(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// Signing in sends its own code; asking twice in a row is rate limited, so
	// the test reads the code from that single response.
	token := env.verifyOTP(t, shop, "9876543210")

	// One customer's order, placed while signed in so it is linked to the
	// profile.
	firstStatus, first := shop.guestAuth(http.MethodPost, "/api/v1/public/orders", token, map[string]any{
		"customer_name":  "Asha Rao",
		"customer_phone": "9876543210",
		"payment_method": "CASH",
		"client_token":   "mine-" + randSuffix(),
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusCreated, firstStatus, "signed-in checkout", first)
	// Another customer's order on the same shop, placed as a guest.
	second := shop.order(t, "Ravi Kumar", "9811122233", "CASH")

	status, body := shop.guestAuth(http.MethodGet, "/api/v1/customer/orders", token, nil)
	env.mustStatus(http.StatusOK, status, "customer order list", body)
	orders, _ := body["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order for this customer, got %d", len(orders))
	}
	if got := orders[0].(map[string]any)["order_number"]; got != first["order_number"] {
		t.Errorf("expected order %v, got %v", first["order_number"], got)
	}

	// Reading the other customer's order by number must not work.
	status, _ = shop.guestAuth(http.MethodGet,
		fmt.Sprintf("/api/v1/customer/orders/%d", number(t, second, "order_number")), token, nil)
	env.mustStatus(http.StatusNotFound, status, "cross-customer order read", nil)
}

func TestCustomerOrdersAreLinkedToTheirProfile(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// An order placed while signed in is attached to the customer, which is what
	// makes order history work without asking for the phone again.
	token := env.verifyOTP(t, shop, "9876543210")

	status, body := shop.guestAuth(http.MethodPost, "/api/v1/public/orders", token, map[string]any{
		"customer_name":  "Asha Rao",
		"customer_phone": "9876543210",
		"payment_method": "CASH",
		"client_token":   "link-" + randSuffix(),
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusCreated, status, "signed-in checkout", body)
	orderNo := number(t, body, "order_number")

	// Tracking now works with no phone, because the token is the proof.
	status, tracked := shop.guestAuth(http.MethodGet,
		fmt.Sprintf("/api/v1/public/orders/%d", orderNo), token, nil)
	env.mustStatus(http.StatusOK, status, "signed-in tracking", tracked)
}

func TestCustomerLoginCanBeDisabled(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/storefront",
		map[string]any{"customer_login_enabled": false})
	env.mustStatus(http.StatusOK, status, "disable customer login", nil)

	status, _ = shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp",
		map[string]any{"phone": "9876543210"})
	env.mustStatus(http.StatusForbidden, status, "sign in while disabled", nil)

	// Guest ordering must still work — login is never required.
	status, body := shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusCreated, status, "guest checkout while login is off", body)
}

// ---------------------------------------------------------------------------
// Payment workflow
// ---------------------------------------------------------------------------

func TestOnlinePaymentSuccess(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")

	// An unpaid online order routes the customer to the payment step.
	if got := str(t, order, "next_step"); got != "pay" {
		t.Errorf("next_step = %q, want pay", got)
	}

	status, started := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "start payment", started)
	token := str(t, started, "payment_token")
	if token == "" {
		t.Fatalf("no payment token issued: %v", started["__raw"])
	}

	status, confirmed := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "confirm payment", confirmed)
	payment, _ := confirmed["payment"].(map[string]any)
	if payment == nil {
		payment, _ = confirmed["payment_status"].(map[string]any)
	}
	if got := confirmed["payment_status"]; got != "PAID" {
		t.Errorf("payment_status = %v, want PAID (body %v)", got, confirmed["__raw"])
	}
	// Exactly one payment row for the order, no matter how many calls were made.
	if got := env.countPayments(shop.tenantID); got != 1 {
		t.Errorf("expected 1 payment row, found %d", got)
	}
}

func TestPaymentFailureLeavesTheOrderIntact(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")

	_, started := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	token := str(t, started, "payment_token")

	status, failed := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "FAILURE", "reason": "Insufficient funds", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "failed payment", failed)
	if got := failed["payment_status"]; got != "FAILED" {
		t.Errorf("payment_status = %v, want FAILED", got)
	}
	if failed["can_retry"] != true {
		t.Error("a failed payment should be retryable")
	}
	// The order itself is untouched and still trackable.
	status, tracked := shop.guest(http.MethodGet,
		fmt.Sprintf("/api/v1/public/orders/%d?phone=9811122233", orderNo), nil)
	env.mustStatus(http.StatusOK, status, "tracking after failure", tracked)
	if got := str(t, tracked, "status"); got != "PENDING" {
		t.Errorf("order status = %q, want PENDING after a failed payment", got)
	}
}

func TestPaymentRetryDoesNotCreateASecondPayment(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")

	_, first := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	staleToken := str(t, first, "payment_token")

	shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "FAILURE", "token": staleToken, "phone": "9811122233"})

	// Retrying re-uses the same payment row with a fresh intent.
	_, second := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	freshToken := str(t, second, "payment_token")
	if freshToken == "" || freshToken == staleToken {
		t.Errorf("a retry should mint a new intent token, got %q after %q", freshToken, staleToken)
	}
	if got := env.countPayments(shop.tenantID); got != 1 {
		t.Errorf("retry created %d payment rows, want 1", got)
	}

	// The superseded token is dead, so a stale tab cannot capture the payment.
	status, _ := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": staleToken, "phone": "9811122233"})
	env.mustStatus(http.StatusConflict, status, "confirm with a stale token", nil)

	status, confirmed := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": freshToken, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "confirm after retry", confirmed)
	if got := confirmed["payment_status"]; got != "PAID" {
		t.Errorf("payment_status = %v, want PAID", got)
	}
	if got := env.countPayments(shop.tenantID); got != 1 {
		t.Errorf("expected 1 payment row after the retry succeeded, found %d", got)
	}
}

func TestCapturedPaymentCannotBeReversed(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")

	_, started := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	token := str(t, started, "payment_token")
	shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})

	// Gateways retry callbacks, and a customer can double-tap. A late failure
	// callback must not undo captured money.
	status, replay := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "FAILURE", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "duplicate failure callback", replay)

	_, statusBody := shop.guest(http.MethodGet,
		fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	if got := statusBody["payment_status"]; got != "PAID" {
		t.Errorf("payment_status = %v, want PAID — a captured payment must stay captured", got)
	}
}

func TestPaymentConfirmationRequiresTheIntentToken(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")
	shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})

	// Without the token, anyone who guesses an order number could mark it paid.
	status, _ := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "phone": "9811122233"})
	env.mustStatus(http.StatusConflict, status, "confirm without a token", nil)
}

func TestOnlyEnabledPaymentMethodsAreOffered(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/payment-settings", map[string]any{
		"online_payment_enabled": false,
		"cash_enabled":           true,
		"default_payment_method": "CASH",
	})
	env.mustStatus(http.StatusOK, status, "switch online payment off", nil)

	_, config := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	payments, _ := config["payments"].(map[string]any)
	methods, _ := payments["methods"].([]any)
	if len(methods) != 1 || methods[0] != "CASH" {
		t.Errorf("methods = %v, want [CASH]", methods)
	}

	// An order cannot be placed with a method the tenant has switched off.
	status, _ = shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name":  "Asha",
		"customer_phone": "9876543210",
		"payment_method": "ONLINE",
		"items":          []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusBadRequest, status, "ordering with a disabled method", nil)
}

func TestTurningOffEveryPaymentMethodIsRefused(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/payment-settings", map[string]any{
		"online_payment_enabled": false,
		"cash_enabled":           false,
	})
	// A shop with no payment method could never take an order, so the switch
	// that would do it is refused with an explanation.
	env.mustStatus(http.StatusBadRequest, status, "disable all payment methods", nil)
}

// ---------------------------------------------------------------------------
// Order workflow
// ---------------------------------------------------------------------------

func TestManualAcceptanceHappyPath(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := env.orderID(shop.tenantID, number(t, order, "order_number"))

	// Manual acceptance: the order waits.
	status, _ := shop.guest(http.MethodGet,
		fmt.Sprintf("/api/v1/public/orders/%d?phone=9876543210", number(t, order, "order_number")), nil)
	env.mustStatus(http.StatusOK, status, "tracking", nil)

	var final map[string]any
	for _, step := range []string{"accept", "prepare", "ready", "complete"} {
		status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/"+step, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, body %v", step, status, body["__raw"])
		}
		final = body
	}
	if got := str(t, final, "status"); got != "COMPLETED" {
		t.Errorf("final status = %q, want COMPLETED", got)
	}
	// Every step is recorded, so the customer timeline and the shop's audit
	// agree on how the order travelled.
	if got := env.historyLength(orderID); got != 5 {
		t.Errorf("status history has %d entries, want 5 (placed + four transitions)", got)
	}
}

func TestInvalidTransitionsAreRejected(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := env.orderID(shop.tenantID, number(t, order, "order_number"))

	// A pending order cannot jump straight to preparing, or to ready.
	for _, step := range []string{"prepare", "ready", "complete"} {
		status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/"+step, nil)
		env.mustStatus(http.StatusConflict, status, "premature "+step, nil)
	}
	// A completed order cannot be reopened.
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept", nil)
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/prepare", nil)
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/ready", nil)
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/complete", nil)
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept", nil)
	env.mustStatus(http.StatusConflict, status, "reopening a completed order", nil)
}

func TestCancellationOnlyBeforePreparation(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	first := shop.order(t, "Asha Rao", "9876543210", "CASH")
	firstID := env.orderID(shop.tenantID, number(t, first, "order_number"))
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+firstID+"/cancel", nil)
	env.mustStatus(http.StatusOK, status, "cancel a pending order", nil)

	second := shop.order(t, "Asha Rao", "9876543210", "CASH")
	secondID := env.orderID(shop.tenantID, number(t, second, "order_number"))
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+secondID+"/accept", nil)
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+secondID+"/prepare", nil)
	status, _ = shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+secondID+"/cancel", nil)
	env.mustStatus(http.StatusConflict, status, "cancel once preparing", nil)
}

func TestAutomaticAcceptanceSkipsTheStaffStep(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow",
		map[string]any{"acceptance_mode": "AUTO"})
	env.mustStatus(http.StatusOK, status, "set automatic acceptance", nil)

	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	if got := str(t, order, "status"); got != "ACCEPTED" {
		t.Errorf("status = %q, want ACCEPTED with automatic acceptance", got)
	}
}

func TestPaymentBeforePreparationGatesTheKitchen(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// An unpaid online order may be accepted, but not prepared.
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderID := env.orderID(shop.tenantID, number(t, order, "order_number"))
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept", nil)
	env.mustStatus(http.StatusOK, status, "accept an unpaid order", nil)

	status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/prepare", nil)
	env.mustStatus(http.StatusConflict, status, "preparing before payment", body)

	// Once the money lands, preparation unlocks.
	_, started := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", number(t, order, "order_number")),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", number(t, order, "order_number")),
		map[string]any{"outcome": "SUCCESS", "token": str(t, started, "payment_token"), "phone": "9811122233"})

	status, _ = shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/prepare", nil)
	env.mustStatus(http.StatusOK, status, "preparing after payment", nil)
}

func TestCashOrdersAreNotGatedByPayment(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	// Cash at the counter is settled at pickup, so the kitchen is never blocked.
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := env.orderID(shop.tenantID, number(t, order, "order_number"))
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept", nil)
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/prepare", nil)
	env.mustStatus(http.StatusOK, status, "preparing an unpaid cash order", nil)
}

func TestAutoCompleteClosesTheOrderAtReady(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow",
		map[string]any{"auto_complete": true})
	env.mustStatus(http.StatusOK, status, "enable auto complete", nil)

	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := env.orderID(shop.tenantID, number(t, order, "order_number"))
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept", nil)
	shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/prepare", nil)
	status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/ready", nil)
	env.mustStatus(http.StatusOK, status, "ready", body)
	if got := str(t, body, "status"); got != "COMPLETED" {
		t.Errorf("status = %q, want COMPLETED when auto complete is on", got)
	}
}

func TestStaffConfirmPaymentMarksCashPaid(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	paymentID := env.paymentID(shop.tenantID, number(t, order, "order_number"))

	status, body := shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+paymentID+"/confirm", nil)
	env.mustStatus(http.StatusOK, status, "confirm cash payment", body)
	if got := str(t, body, "status"); got != "PAID" {
		t.Errorf("status = %q, want PAID", got)
	}
}

func TestWorkflowRejectsUnknownSettings(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow",
		map[string]any{"acceptance_mode": "WHENEVER"})
	env.mustStatus(http.StatusBadRequest, status, "invented acceptance mode", nil)

	status, _ = shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow",
		map[string]any{"payment_requirement": "WHENEVER"})
	env.mustStatus(http.StatusBadRequest, status, "invented payment requirement", nil)
}

func TestTenantAdminCannotReachAnotherTenantsOrders(t *testing.T) {
	env := newTestEnv(t)
	first := newShop(t, env)
	second := newShop(t, env)

	order := first.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := env.orderID(first.tenantID, number(t, order, "order_number"))

	// The first shop's admin token, sent to the second shop's host. The host
	// guard rejects it before the order id is even looked at, so no amount of
	// guessing ids helps.
	status, _ := env.doOn(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/accept",
		first.adminToken, nil, second.host)
	env.mustStatus(http.StatusForbidden, status, "cross-tenant staff action", nil)

	status, _ = second.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	env.mustStatus(http.StatusOK, status, "own order list still works", nil)
	_, list := second.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	if orders, _ := list["orders"].([]any); len(orders) != 0 {
		t.Errorf("expected no orders from another tenant, got %d", len(orders))
	}
}

func TestStorefrontConfigIsNotPubliclyWritable(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// No token at all.
	status, _ := shop.guest(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{"name": "Hijacked"})
	env.mustStatus(http.StatusUnauthorized, status, "unauthenticated config write", nil)

	// A storefront customer token is not a staff token: signing in as a diner
	// must never grant access to the shop's configuration.
	customerToken := env.verifyOTP(t, shop, "9876543210")
	status, _ = shop.guestAuth(http.MethodPut, "/api/v1/tenant/storefront",
		customerToken, map[string]any{"name": "Hijacked"})
	env.mustStatus(http.StatusUnauthorized, status, "customer writing tenant config", nil)

	// And this shop's own token, sent to another shop's host, is refused by the
	// host guard.
	other := newShop(t, env)
	status, _ = env.doOn(http.MethodPut, "/api/v1/tenant/storefront",
		shop.adminToken, map[string]any{"name": "Hijacked"}, other.host)
	env.mustStatus(http.StatusForbidden, status, "cross-tenant config write", nil)

	// The original shop is untouched by all of that.
	_, config := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	store, _ := config["store"].(map[string]any)
	if store["name"] == "Hijacked" {
		t.Error("a rejected write still changed the store name")
	}
}

// ---------------------------------------------------------------------------
// Storefront configuration
// ---------------------------------------------------------------------------

func TestStorefrontConfigRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{
		"name":        "Momo Magic",
		"tagline":     "Steamed to order",
		"description": "The best momo in town",
		"phone":       "98765 43210",
		"address":     "12 MG Road",
	})
	env.mustStatus(http.StatusOK, status, "save identity", body)

	store, _ := body["store"].(map[string]any)
	if store["name"] != "Momo Magic" {
		t.Errorf("name = %v", store["name"])
	}

	// The change is visible to customers straight away.
	_, public := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	publicStore, _ := public["store"].(map[string]any)
	if publicStore["name"] != "Momo Magic" || publicStore["tagline"] != "Steamed to order" {
		t.Errorf("customers do not see the saved identity: %v", publicStore)
	}
}

func TestThemeSwitching(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	for _, preset := range []string{"classic", "modern", "street-food", "minimal", "fresh", "dark"} {
		status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/theme", map[string]any{
			"preset": preset,
			"mode":   "light",
		})
		env.mustStatus(http.StatusOK, status, "theme preset "+preset, body)
		theme, _ := body["theme"].(map[string]any)
		if theme["preset"] != preset {
			t.Errorf("preset = %v, want %s", theme["preset"], preset)
		}
		vars, _ := theme["vars"].(map[string]any)
		if vars["--sf-primary"] == nil {
			t.Errorf("preset %s did not produce tokens", preset)
		}

		// The public payload carries the same tokens, so the storefront and the
		// admin preview can never disagree.
		_, public := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
		publicTheme, _ := public["theme"].(map[string]any)
		publicVars, _ := publicTheme["vars"].(map[string]any)
		if publicVars["--sf-primary"] != vars["--sf-primary"] {
			t.Errorf("preset %s: public tokens differ from admin tokens", preset)
		}
	}
}

func TestThemeRejectsValuesOutsideTheCatalogue(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	// Arbitrary CSS is not accepted anywhere in the theme document.
	cases := []map[string]any{
		{"preset": "neon-gothic"},
		{"mode": "hotdog"},
		{"radius": "13px"},
		{"button": "brutalist"},
		{"card": "glassmorphic"},
		{"header": "floating"},
		{"hero": "holographic"},
		{"font": "comic-sans"},
	}
	for _, body := range cases {
		status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/theme", body)
		if status != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", body, status)
		}
	}
}

func TestThemeAcceptsColourOverrides(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/theme", map[string]any{
		"primary":   "#ff6b00",
		"secondary": "#7a3e00",
		"accent":    "#ffd166",
	})
	env.mustStatus(http.StatusOK, status, "colour overrides", body)
	theme, _ := body["theme"].(map[string]any)
	if theme["primary"] != "#ff6b00" {
		t.Errorf("primary = %v", theme["primary"])
	}
	// Contrast-aware ink is derived, so a light brand still gets readable text.
	vars, _ := theme["vars"].(map[string]any)
	if vars["--sf-primary-ink"] == "" {
		t.Error("a colour override should still produce a readable ink colour")
	}
}

func TestThemeIgnoresAMalformedColour(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/theme", map[string]any{
		"preset":  "modern",
		"primary": "javascript:alert(1)",
	})
	env.mustStatus(http.StatusOK, status, "malformed colour", body)
	theme, _ := body["theme"].(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(theme["primary"]), "#") {
		t.Errorf("a malformed colour should fall back to the preset, got %v", theme["primary"])
	}
}

func TestHomepageSectionConfiguration(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	_, body := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	homepage, _ := body["homepage"].(map[string]any)
	sections, _ := homepage["sections"].([]any)
	if len(sections) == 0 {
		t.Fatal("the default homepage should ship with sections")
	}

	// Reorder, disable one, and edit another.
	reordered := []map[string]any{
		{"id": "menu", "type": "MENU", "enabled": true, "content": map[string]string{"title": "Everything on the menu"}},
		{"id": "hero", "type": "HERO", "enabled": true, "content": map[string]string{"title": "Hungry already?"}},
		{"id": "hours", "type": "OPENING_HOURS", "enabled": false, "content": map[string]string{}},
		{"id": "menu-2", "type": "BUSINESS_INFO", "enabled": true, "content": map[string]string{}},
	}
	status, saved := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/homepage",
		map[string]any{"sections": reordered})
	env.mustStatus(http.StatusOK, status, "save homepage", saved)

	savedHome, _ := saved["homepage"].(map[string]any)
	savedSections, _ := savedHome["sections"].([]any)
	if len(savedSections) != 4 {
		t.Fatalf("expected 4 sections, got %d", len(savedSections))
	}
	if savedSections[0].(map[string]any)["type"] != "MENU" {
		t.Errorf("reordering was not kept: %v", savedSections[0])
	}
	// A duplicate id is made unique rather than silently collapsing.
	if savedSections[3].(map[string]any)["id"] == "menu" {
		t.Error("a duplicate section id should be disambiguated")
	}

	// Customers see exactly the layout that was saved.
	_, public := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
	publicHome, _ := public["homepage"].(map[string]any)
	publicSections, _ := publicHome["sections"].([]any)
	if len(publicSections) != 4 {
		t.Errorf("customers see %d sections, expected 4", len(publicSections))
	}
	if publicSections[0].(map[string]any)["content"].(map[string]any)["title"] != "Everything on the menu" {
		t.Errorf("the customer's copy of the heading is wrong: %v", publicSections[0])
	}
}

func TestHomepageDropsUnknownSectionTypes(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, saved := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/homepage",
		map[string]any{"sections": []map[string]any{
			{"id": "hero", "type": "HERO", "enabled": true, "content": map[string]string{"title": "Hi"}},
			{"id": "x", "type": "RAW_HTML", "enabled": true, "content": map[string]string{"html": "<script>"}},
		}})
	env.mustStatus(http.StatusOK, status, "save homepage with an unknown type", saved)
	homepage, _ := saved["homepage"].(map[string]any)
	sections, _ := homepage["sections"].([]any)
	if len(sections) != 1 {
		t.Errorf("an unknown section type must be dropped, got %d sections", len(sections))
	}
}

func TestOpeningHoursRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/hours", map[string]any{
		"always_open": false,
		"timezone":    "Asia/Kolkata",
		"schedule": map[string]any{
			"mon": []string{"09:00", "22:00"},
			"tue": []any{[]string{"09:00", "13:00"}, []string{"17:00", "22:00"}},
		},
	})
	env.mustStatus(http.StatusOK, status, "save hours", body)
	hours, _ := body["hours"].(map[string]any)
	if hours["always_open"] != false {
		t.Error("always_open should be false")
	}
	schedule, _ := hours["schedule"].(map[string]any)
	tue, _ := schedule["tue"].([]any)
	if len(tue) != 2 {
		t.Errorf("a split shift should keep both windows, got %v", schedule["tue"])
	}
}

func TestOpeningHoursRejectGarbage(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, _ := shop.admin(http.MethodPut, "/api/v1/tenant/storefront/hours", map[string]any{
		"schedule": map[string]any{"mon": []string{"9am", "5pm"}},
	})
	// The unparseable range is dropped rather than stored, and the store falls
	// back to always open so it never silently disappears.
	env.mustStatus(http.StatusOK, status, "garbage hours", nil)

	_, body := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	hours, _ := body["hours"].(map[string]any)
	if hours["always_open"] != true {
		t.Error("a store whose schedule is unusable should read as always open")
	}
}

func TestQRCodeEndpoint(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := shop.admin(http.MethodGet, "/api/v1/tenant/storefront/qr", nil)
	env.mustStatus(http.StatusOK, status, "qr code", body)

	url := str(t, body, "url")
	if !strings.Contains(url, shop.slug) {
		t.Errorf("url = %q, want it to contain %s", url, shop.slug)
	}
	qr, _ := body["qr"].(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(qr["png"]), "data:image/png;base64,") {
		t.Error("the QR should come back as a data URI ready for an <img>")
	}
	if !strings.HasPrefix(fmt.Sprint(qr["svg"]), "<svg") {
		t.Error("the QR should also come back as inline vector markup for print")
	}
}

func TestStoreLinkEndpoint(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, body := shop.admin(http.MethodGet, "/api/v1/tenant/store-link", nil)
	env.mustStatus(http.StatusOK, status, "store link", body)
	if !strings.Contains(str(t, body, "public_url"), shop.slug) {
		t.Errorf("public_url = %q", body["public_url"])
	}
}

func TestStorefrontCataloguesAreAvailableToAdmins(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	_, body := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	catalogues, _ := body["catalogues"].(map[string]any)
	if presets, _ := catalogues["presets"].([]any); len(presets) != 6 {
		t.Errorf("expected 6 theme presets in the catalogue, got %d", len(presets))
	}
	if types, _ := catalogues["section_types"].([]any); len(types) != 9 {
		t.Errorf("expected 9 section types, got %d", len(types))
	}
}

// TestStorefrontConfigIsOwnerOnly pins the staff/admin split on the storefront
// configuration.
//
// A staff account works the kitchen board and takes orders. It must not be able
// to rewrite the document that decides what those orders cost: tax, packaging
// fee, which payment methods are offered, when the shop accepts orders, or
// whether the storefront is published at all. Gating only the handler bodies
// would have been enough to pass a functional test and still left the write
// surface one refactor away from being open again, so this asserts the router.
func TestStorefrontConfigIsOwnerOnly(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	staff := func(method, path string) (int, map[string]any) {
		return env.doOn(method, path, shop.staffToken, map[string]any{}, shop.host)
	}

	reads := []string{
		"/api/v1/tenant/storefront",
		"/api/v1/tenant/storefront/preview",
		"/api/v1/tenant/storefront/qr",
		"/api/v1/tenant/payment-settings",
		"/api/v1/tenant/order-workflow",
	}
	for _, path := range reads {
		status, body := staff(http.MethodGet, path)
		if status != http.StatusForbidden {
			t.Errorf("staff GET %s: status = %d, want 403 (body: %v)", path, status, body["__raw"])
		}
	}

	writes := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/v1/tenant/storefront"},
		{http.MethodPut, "/api/v1/tenant/storefront/theme"},
		{http.MethodPut, "/api/v1/tenant/storefront/homepage"},
		{http.MethodPut, "/api/v1/tenant/storefront/hours"},
		{http.MethodPut, "/api/v1/tenant/payment-settings"},
		{http.MethodPut, "/api/v1/tenant/order-workflow"},
	}
	for _, w := range writes {
		status, body := staff(w.method, w.path)
		if status != http.StatusForbidden {
			t.Errorf("staff %s %s: status = %d, want 403 (body: %v)", w.method, w.path, status, body["__raw"])
		}
	}

	// The gate is scoped to the storefront configuration. Staff still get the
	// order board and the store link, so the change cannot have been a blanket
	// tightening that quietly breaks the kitchen.
	for _, path := range []string{"/api/v1/tenant/orders", "/api/v1/tenant/store-link"} {
		status, body := staff(http.MethodGet, path)
		env.mustStatus(http.StatusOK, status, "staff GET "+path, body)
	}

	// And the owner is not locked out of their own configuration.
	status, body := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	env.mustStatus(http.StatusOK, status, "owner reading config", body)
}
