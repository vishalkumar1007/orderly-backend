package httpserver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
)

// Payments mode + gateway configuration (PAYMENTS-PLAN.md P-B).

const (
	payKeySecret     = "PAYMODE_CANARY_KEY_SECRET"
	payWebhookSecret = "PAYMODE_CANARY_WEBHOOK_SECRET"
)

// keepPaymentModesClean puts the platform default back as the developer had it:
// these tests flip a global setting on a shared development database.
func keepPaymentModesClean(t *testing.T, env *testEnv) {
	t.Helper()
	var prior *string
	var mode string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT mode FROM payment_settings WHERE tenant_id IS NULL`).Scan(&mode); err == nil {
		prior = &mode
	}
	t.Cleanup(func() {
		_, _ = env.server.pool.Exec(tctx(), `DELETE FROM payment_settings WHERE tenant_id IS NULL`)
		if prior != nil {
			_, _ = env.server.pool.Exec(tctx(), `INSERT INTO payment_settings (tenant_id, mode) VALUES (NULL, $1)`, *prior)
		}
	})
}

// fakeGateway stands in for the real gateway during the connection test.
func fakeGateway(t *testing.T, status *int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(*status) }))
	restore := configsvc.SetRazorpayBaseURLForTest(srv.URL)
	t.Cleanup(func() { restore(); srv.Close() })
}

func platformPaymentsBody(keyID string) map[string]any {
	return map[string]any{"provider": "razorpay", "enabled": true, "config": map[string]any{
		"key_id": keyID, "key_secret": payKeySecret, "webhook_secret": payWebhookSecret,
	}}
}

func checkOK(t *testing.T, readiness map[string]any, key string) bool {
	t.Helper()
	checks, _ := readiness["checks"].([]any)
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["key"] == key {
			return m["ok"] == true
		}
	}
	t.Fatalf("readiness has no %q check: %v", key, readiness)
	return false
}

func TestPaymentModeDefaultsToSandbox(t *testing.T) {
	env := newTestEnv(t)
	keepPaymentModesClean(t, env)
	env.resetService("PAYMENTS")
	if _, err := env.server.pool.Exec(tctx(), `DELETE FROM payment_settings WHERE tenant_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	status, body := env.do(http.MethodGet, "/api/v1/admin/payments/settings", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "platform settings", body)
	if body["mode"] != "SANDBOX" {
		t.Errorf("mode = %v, want SANDBOX when never configured", body["mode"])
	}
	ready, _ := body["readiness"].(map[string]any)
	if ready["ready"] != false || checkOK(t, ready, "credentials") {
		t.Errorf("nothing is configured, so nothing can be ready: %v", ready)
	}
	if body["live_available"] != true {
		t.Errorf("live_available = %v, want true", body["live_available"])
	}
}

func TestPaymentCredentialsAreValidatedAndNeverReturned(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("PAYMENTS")
	token := env.superAdmin()

	status, _ := env.do(http.MethodPut, "/api/v1/admin/configurations/payments", token,
		platformPaymentsBody("not-a-razorpay-key"))
	env.mustStatus(http.StatusBadRequest, status, "malformed key id", nil)

	status, body := env.do(http.MethodPut, "/api/v1/admin/configurations/payments", token,
		platformPaymentsBody("rzp_test_abc123"))
	env.mustStatus(http.StatusOK, status, "save credentials", body)
	for _, secret := range []string{payKeySecret, payWebhookSecret} {
		if strings.Contains(str(t, body, "__raw"), secret) {
			t.Errorf("save response leaked %s", secret)
		}
	}
	status, body = env.do(http.MethodGet, "/api/v1/admin/configurations/payments", token, nil)
	env.mustStatus(http.StatusOK, status, "read credentials", body)
	if strings.Contains(str(t, body, "__raw"), payKeySecret) || strings.Contains(str(t, body, "__raw"), payWebhookSecret) {
		t.Error("configuration view leaked a secret")
	}
	if body["has_secret"] != true {
		t.Errorf("has_secret = %v, want true", body["has_secret"])
	}
	// The secrets are sealed in the database, never stored in clear.
	var raw string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT secret_config::text || config::text FROM platform_configurations WHERE service_type = 'PAYMENTS'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, payKeySecret) || strings.Contains(raw, payWebhookSecret) {
		t.Error("secrets are stored unsealed")
	}
}

func TestGoingLiveIsGatedOnReadiness(t *testing.T) {
	env := newTestEnv(t)
	keepPaymentModesClean(t, env)
	env.resetService("PAYMENTS")
	token := env.superAdmin()
	gatewayStatus := http.StatusOK
	fakeGateway(t, &gatewayStatus)

	// Nothing configured: refused, with the checklist explaining why.
	status, body := env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusConflict, status, "live with no gateway", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "gateway_not_ready" {
		t.Errorf("error = %v, want gateway_not_ready", body["error"])
	}

	// Credentials saved but never tested: still refused.
	env.do(http.MethodPut, "/api/v1/admin/configurations/payments", token, platformPaymentsBody("rzp_test_abc123"))
	status, body = env.do(http.MethodGet, "/api/v1/admin/payments/settings", token, nil)
	env.mustStatus(http.StatusOK, status, "readiness after save", body)
	ready, _ := body["readiness"].(map[string]any)
	if !checkOK(t, ready, "credentials") || !checkOK(t, ready, "webhook") || checkOK(t, ready, "tested") || ready["ready"] != false {
		t.Errorf("after save: want credentials+webhook ok, tested not: %v", ready)
	}
	status, _ = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusConflict, status, "live before the connection test", nil)

	// A failing connection test keeps it blocked.
	gatewayStatus = http.StatusUnauthorized
	status, body = env.do(http.MethodPost, "/api/v1/admin/configurations/payments/test", token, nil)
	env.mustStatus(http.StatusOK, status, "failing test", body)
	if body["ok"] != false {
		t.Errorf("a rejected credential must not pass: %v", body)
	}
	status, _ = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusConflict, status, "live after a failed test", nil)

	// Passing test: ready, but still needs explicit confirmation.
	gatewayStatus = http.StatusOK
	status, body = env.do(http.MethodPost, "/api/v1/admin/configurations/payments/test", token, nil)
	env.mustStatus(http.StatusOK, status, "passing test", body)
	if body["ok"] != true {
		t.Fatalf("test should pass: %v", body)
	}
	status, body = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "LIVE"})
	env.mustStatus(http.StatusBadRequest, status, "live without confirmation", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "confirmation_required" {
		t.Errorf("error = %v, want confirmation_required", body["error"])
	}
	status, body = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusOK, status, "go live", body)
	if body["mode"] != "LIVE" {
		t.Errorf("mode = %v, want LIVE", body["mode"])
	}
	if !containsAction(auditActions(t, env, "PAYMENTS"), "mode set to LIVE") {
		t.Error("going live was not audited")
	}

	// Editing the credentials invalidates the earlier test.
	env.do(http.MethodPut, "/api/v1/admin/configurations/payments", token, platformPaymentsBody("rzp_test_zzz999"))
	status, body = env.do(http.MethodGet, "/api/v1/admin/payments/settings", token, nil)
	env.mustStatus(http.StatusOK, status, "readiness after an edit", body)
	if r, _ := body["readiness"].(map[string]any); checkOK(t, r, "tested") {
		t.Error("changed credentials must need a fresh connection test")
	}

	// Back to sandbox is always allowed and needs no confirmation.
	status, body = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "SANDBOX"})
	env.mustStatus(http.StatusOK, status, "back to sandbox", body)
	if body["mode"] != "SANDBOX" {
		t.Errorf("mode = %v, want SANDBOX", body["mode"])
	}

	status, _ = env.do(http.MethodPut, "/api/v1/admin/payments/settings", token, map[string]any{"mode": "BANANA"})
	env.mustStatus(http.StatusBadRequest, status, "unknown mode", nil)
}

func TestBusinessOverrideBeatsThePlatformDefault(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	env.resetService("PAYMENTS")
	token := env.superAdmin()
	gatewayStatus := http.StatusOK
	fakeGateway(t, &gatewayStatus)
	a, b := newShop(t, env), newShop(t, env)
	path := func(id uuid.UUID) string { return "/api/v1/admin/tenants/" + id.String() + "/payments" }

	status, body := env.do(http.MethodGet, path(a.tenantID), token, nil)
	env.mustStatus(http.StatusOK, status, "tenant mode", body)
	if body["effective_mode"] != "SANDBOX" || body["mode_source"] != "default" {
		t.Errorf("fresh business: %v/%v, want SANDBOX/default", body["effective_mode"], body["mode_source"])
	}

	// A business cannot go live without its own ready gateway.
	status, _ = env.do(http.MethodPut, path(a.tenantID), token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusConflict, status, "business live without gateway", nil)

	// The owner saves its own keys; Super Admin then tests and flips it.
	status, body = a.admin(http.MethodPut, "/api/v1/tenant/configurations/payments", platformPaymentsBody("rzp_test_biza01"))
	env.mustStatus(http.StatusOK, status, "owner saves gateway keys", body)
	status, body = a.admin(http.MethodPost, "/api/v1/tenant/configurations/payments/test", nil)
	env.mustStatus(http.StatusOK, status, "owner tests gateway keys", body)
	status, body = env.do(http.MethodPut, path(a.tenantID), token, map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusOK, status, "business goes live", body)
	if body["effective_mode"] != "LIVE" || body["mode_source"] != "business" {
		t.Errorf("effective = %v/%v, want LIVE/business", body["effective_mode"], body["mode_source"])
	}

	// Business A going live changes nothing for business B.
	status, body = env.do(http.MethodGet, path(b.tenantID), token, nil)
	env.mustStatus(http.StatusOK, status, "other business", body)
	if body["effective_mode"] != "SANDBOX" {
		t.Errorf("business B = %v, want SANDBOX", body["effective_mode"])
	}

	// The owner sees (read-only) the mode its customers pay in.
	status, body = a.admin(http.MethodGet, "/api/v1/tenant/payments/mode", nil)
	env.mustStatus(http.StatusOK, status, "owner view", body)
	if body["mode"] != "LIVE" || body["is_test"] != false {
		t.Errorf("owner view = %v/%v, want LIVE/not test", body["mode"], body["is_test"])
	}
	if body["online_ready"] != true {
		t.Errorf("online_ready = %v: the business passed every check, so online payment is ready", body["online_ready"])
	}
	status, _ = a.admin(http.MethodPut, "/api/v1/tenant/payments/mode", map[string]any{"mode": "SANDBOX"})
	if status != http.StatusMethodNotAllowed && status != http.StatusNotFound {
		t.Errorf("an owner must not be able to change the mode, got %d", status)
	}

	// INHERIT clears the override.
	status, body = env.do(http.MethodPut, path(a.tenantID), token, map[string]any{"mode": "INHERIT"})
	env.mustStatus(http.StatusOK, status, "clear override", body)
	if body["effective_mode"] != "SANDBOX" || body["override"] != nil {
		t.Errorf("after INHERIT: %v / %v", body["effective_mode"], body["override"])
	}
}

func TestLivePaymentsNeverFallBackToTheSandbox(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	shop := newShop(t, env)
	// Put the business in LIVE directly: the point is what checkout does when a
	// business is live but no live gateway can take the payment.
	if _, err := env.server.pool.Exec(tctx(),
		`INSERT INTO payment_settings (tenant_id, mode) VALUES ($1, 'LIVE')`, shop.tenantID); err != nil {
		t.Fatal(err)
	}
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")

	status, body := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusServiceUnavailable, status, "start payment in live without a gateway", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "gateway_unavailable" {
		t.Errorf("error = %v, want gateway_unavailable", body["error"])
	}
	var paid int
	_ = env.server.pool.QueryRow(tctx(), `SELECT COUNT(*)::int FROM payments WHERE tenant_id = $1 AND status = 'PAID'`, shop.tenantID).Scan(&paid)
	if paid != 0 {
		t.Fatal("a live business must never get a sandbox-paid order")
	}

	// Cash still works for that business: the order is not stranded.
	cash := shop.order(t, "Asha Rao", "9876543210", "CASH")
	if got := str(t, cash, "next_step"); got != "confirmation" {
		t.Errorf("cash next_step = %q, want confirmation", got)
	}

	// Switch the business back to sandbox: payments flow, labelled as test.
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payment_settings SET mode = 'SANDBOX' WHERE tenant_id = $1`, shop.tenantID); err != nil {
		t.Fatal(err)
	}
	status, body = shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "start payment in sandbox", body)
	if body["payment_mode"] != "SANDBOX" || body["is_test"] != true {
		t.Errorf("sandbox payment = %v/%v, want SANDBOX/test", body["payment_mode"], body["is_test"])
	}
}

func TestPaymentModeEndpointsAreRoleGated(t *testing.T) {
	env := newTestEnv(t)
	keepPaymentModesClean(t, env)
	shop := newShop(t, env)
	path := "/api/v1/admin/tenants/" + shop.tenantID.String() + "/payments"

	for name, call := range map[string]func() (int, map[string]any){
		"anonymous read": func() (int, map[string]any) {
			return env.do(http.MethodGet, "/api/v1/admin/payments/settings", "", nil)
		},
		"owner read": func() (int, map[string]any) {
			return env.do(http.MethodGet, "/api/v1/admin/payments/settings", shop.adminToken, nil)
		},
		"owner platform": func() (int, map[string]any) {
			return env.do(http.MethodPut, "/api/v1/admin/payments/settings", shop.adminToken, map[string]any{"mode": "SANDBOX"})
		},
		"owner own": func() (int, map[string]any) {
			return env.do(http.MethodPut, path, shop.adminToken, map[string]any{"mode": "SANDBOX"})
		},
		"staff read mode": func() (int, map[string]any) {
			return env.doOn(http.MethodGet, "/api/v1/tenant/payments/mode", shop.staffToken, nil, shop.host)
		},
	} {
		status, _ := call()
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s: status %d, want 401/403", name, status)
		}
	}
}
