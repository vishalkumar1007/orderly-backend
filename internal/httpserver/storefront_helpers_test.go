package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Helpers used by the storefront test suite. They talk to the database directly
// so a test can set up a state the API deliberately offers no shortcut for —
// taking a product off sale, marking a shop closed, switching acceptance to
// automatic — and then assert how the HTTP surface responds.

func tctx() context.Context { return context.Background() }

// publishTenant makes a tenant's storefront publicly reachable.
func (e *testEnv) publishTenant(tenantID uuid.UUID) {
	e.t.Helper()
	if _, err := e.server.pool.Exec(tctx(),
		`UPDATE tenants SET is_published = TRUE WHERE id = $1`, tenantID); err != nil {
		e.t.Fatalf("publish tenant: %v", err)
	}
}

func (e *testEnv) setPublished(tenantID uuid.UUID, published bool) {
	e.t.Helper()
	if _, err := e.server.pool.Exec(tctx(),
		`UPDATE tenants SET is_published = $2 WHERE id = $1`, tenantID, published); err != nil {
		e.t.Fatalf("set published: %v", err)
	}
}

func (e *testEnv) setStoreStatus(tenantID uuid.UUID, status string) {
	e.t.Helper()
	if _, err := e.server.pool.Exec(tctx(),
		`UPDATE tenants SET store_status = $2 WHERE id = $1`, tenantID, status); err != nil {
		e.t.Fatalf("set store status: %v", err)
	}
}

func (e *testEnv) createProduct(tenantID, categoryID uuid.UUID, name string, price float64) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	err := e.server.pool.QueryRow(tctx(),
		`INSERT INTO products (tenant_id, category_id, name, description, price, is_available)
		 VALUES ($1, $2, $3, 'Steamed to order', $4, TRUE) RETURNING id`,
		tenantID, categoryID, name, price).Scan(&id)
	if err != nil {
		e.t.Fatalf("create product: %v", err)
	}
	return id
}

func (e *testEnv) setProductAvailable(productID string, available bool) {
	e.t.Helper()
	if _, err := e.server.pool.Exec(tctx(),
		`UPDATE products SET is_available = $2 WHERE id = $1`, productID, available); err != nil {
		e.t.Fatalf("set product availability: %v", err)
	}
}

// setProductAddons replaces a product's add-on catalogue.
func (e *testEnv) setProductAddons(productID string, addons any) {
	e.t.Helper()
	raw, err := json.Marshal(addons)
	if err != nil {
		e.t.Fatalf("encode addons: %v", err)
	}
	if _, err := e.server.pool.Exec(tctx(),
		`UPDATE products SET addons = $2::jsonb WHERE id = $1`, productID, raw); err != nil {
		e.t.Fatalf("set addons: %v", err)
	}
}

// setCosting writes the tenant's tax percentage and packaging fee.
func (e *testEnv) setCosting(tenantID uuid.UUID, taxPercent, packagingFee float64) {
	e.t.Helper()
	if _, err := e.server.pool.Exec(tctx(),
		`INSERT INTO tenant_storefront_settings (tenant_id, tax_percent, packaging_fee)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id) DO UPDATE SET tax_percent = $2, packaging_fee = $3`,
		tenantID, taxPercent, packagingFee); err != nil {
		e.t.Fatalf("set costing: %v", err)
	}
}

// setOpeningHours writes the weekly schedule document.
func (e *testEnv) setOpeningHours(tenantID uuid.UUID, doc map[string]any) {
	e.t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		e.t.Fatalf("encode hours: %v", err)
	}
	if _, err := e.server.pool.Exec(tctx(),
		`INSERT INTO tenant_storefront_settings (tenant_id, opening_hours)
		 VALUES ($1, $2::jsonb)
		 ON CONFLICT (tenant_id) DO UPDATE SET opening_hours = $2::jsonb`,
		tenantID, raw); err != nil {
		e.t.Fatalf("set opening hours: %v", err)
	}
}

func (e *testEnv) orderID(tenantID uuid.UUID, orderNumber int) string {
	e.t.Helper()
	var id uuid.UUID
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT id FROM orders WHERE tenant_id = $1 AND order_number = $2`,
		tenantID, orderNumber).Scan(&id); err != nil {
		e.t.Fatalf("resolve order %d: %v", orderNumber, err)
	}
	return id.String()
}

func (e *testEnv) paymentID(tenantID uuid.UUID, orderNumber int) string {
	e.t.Helper()
	var id uuid.UUID
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT p.id FROM payments p
		 JOIN orders o ON o.id = p.order_id
		 WHERE p.tenant_id = $1 AND o.order_number = $2`,
		tenantID, orderNumber).Scan(&id); err != nil {
		e.t.Fatalf("resolve payment for order %d: %v", orderNumber, err)
	}
	return id.String()
}

func (e *testEnv) countOrders(tenantID uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT COUNT(*)::int FROM orders WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		e.t.Fatalf("count orders: %v", err)
	}
	return n
}

// historyLength counts the recorded status changes for an order.
func (e *testEnv) historyLength(orderID string) int {
	e.t.Helper()
	var n int
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT COUNT(*)::int FROM order_status_history WHERE order_id = $1`, orderID).Scan(&n); err != nil {
		e.t.Fatalf("count status history: %v", err)
	}
	return n
}

func (e *testEnv) countPayments(tenantID uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT COUNT(*)::int FROM payments WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
		e.t.Fatalf("count payments: %v", err)
	}
	return n
}

// otpFromResponse reads the development OTP out of a send-otp response. The
// code is only echoed outside production, which is what makes this testable
// without an SMS provider. The stored digest is one-way, so a test that needs
// the code again must keep it from the response rather than re-reading it.
func (e *testEnv) otpFromResponse(t *testing.T, body map[string]any) string {
	t.Helper()
	otp, _ := body["otp"].(map[string]any)
	code, _ := otp["dev_code"].(string)
	if code == "" {
		t.Fatalf("no dev_code in the send-otp response: %v", body["__raw"])
	}
	return code
}

// verifyOTP signs a customer in and returns their access token, reading the
// code from the send-otp response.
func (e *testEnv) verifyOTP(t *testing.T, shop *shopFixture, number string) string {
	t.Helper()
	host := shop.host
	status, body := e.doOn(http.MethodPost, "/api/v1/auth/customer/send-otp", "",
		map[string]any{"phone": number}, host)
	if status != http.StatusOK {
		t.Fatalf("send otp: status = %d, body %v", status, body["__raw"])
	}
	code := e.otpFromResponse(t, body)

	status, body = e.doOn(http.MethodPost, "/api/v1/auth/customer/verify-otp", "",
		map[string]any{"phone": number, "code": code}, host)
	if status != http.StatusOK {
		t.Fatalf("verify otp: status = %d, body %v", status, body["__raw"])
	}
	tokens, _ := body["tokens"].(map[string]any)
	token, _ := tokens["access_token"].(string)
	if token == "" {
		t.Fatalf("no access token: %v", body["__raw"])
	}
	return token
}
