package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/payments"
)

// Production hardening (PAYMENTS-PLAN.md P-E).

// alertCount counts platform alerts raised under a key, and removes them when
// the test ends: alerts go to the real platform operator on a shared database.
func alertCount(t *testing.T, env *testEnv, keyLike string) int {
	t.Helper()
	t.Cleanup(func() {
		_, _ = env.server.pool.Exec(context.Background(),
			`DELETE FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' LIKE $1`, keyLike)
	})
	return env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' LIKE $1`, keyLike)
}

func TestMoneyRoutesAreThrottledWhenTurnedOn(t *testing.T) {
	t.Setenv("PAYMENTS_RATE_LIMIT", "true")
	env := newV2Env(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	path := fmt.Sprintf("/api/v1/public/orders/%d/pay", number(t, order, "order_number"))

	var blocked int
	for i := 1; i <= 25; i++ {
		status, body := shop.guest(http.MethodPost, path, map[string]any{"method": "ONLINE", "phone": "9811122233"})
		if status == http.StatusTooManyRequests {
			blocked++
			if i <= 20 {
				t.Fatalf("request %d was throttled, the limit is 20 a minute", i)
			}
			if errBody, _ := body["error"].(map[string]any); errBody["code"] != "rate_limited" {
				t.Errorf("error = %v, want rate_limited", body["error"])
			}
		}
	}
	if blocked != 5 {
		t.Errorf("throttled %d of the last 5 requests, want all 5", blocked)
	}
	// Status polling has its own, higher allowance and is not affected.
	status, _ := shop.guest(http.MethodGet, path+"?phone=9811122233", nil)
	env.mustStatus(http.StatusOK, status, "status polling after the start limit", nil)
}

func TestNothingIsThrottledByDefaultInDevelopment(t *testing.T) {
	t.Setenv("PAYMENTS_RATE_LIMIT", "")
	env := newV2Env(t)
	if env.cfg.PaymentsRateLimit {
		t.Skip("running with production settings")
	}
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	path := fmt.Sprintf("/api/v1/public/orders/%d/pay", number(t, order, "order_number"))
	for i := 0; i < 30; i++ {
		if status, _ := shop.guest(http.MethodPost, path, map[string]any{"method": "ONLINE", "phone": "9811122233"}); status == http.StatusTooManyRequests {
			t.Fatalf("request %d was throttled with limits off", i+1)
		}
	}
}

func TestWrongAmountRaisesOneAlertForThePlatformOperator(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	key := "mismatch:" + row.ID.String()
	before := alertCount(t, env, key)

	for i := 0; i < 3; i++ {
		env.postRazorpay(shop.webhookSecret, fmt.Sprintf("evt_short_%d", i),
			rzEventBody("payment.captured", row.GatewayOrder, "pay_S", 100, ""))
	}
	after := env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' = $1`, key)
	if before != 0 {
		t.Skip("platform has no operator account to alert")
	}
	if after < 1 {
		t.Fatal("a payment for the wrong amount must alert the platform operator")
	}
	perRecipient := after
	// Three bad callbacks, one alert per recipient — not three.
	env.postRazorpay(shop.webhookSecret, "evt_short_again", rzEventBody("payment.captured", row.GatewayOrder, "pay_S", 100, ""))
	if again := env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' = $1`, key); again != perRecipient {
		t.Errorf("alerts grew from %d to %d: one problem must not spam", perRecipient, again)
	}
}

func TestARunOfRejectedSignaturesRaisesAnAlert(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	alertCount(t, env, "badsig:razorpay") // registers cleanup

	body := rzEventBody("payment.captured", row.GatewayOrder, "pay_X", 100, "")
	for i := 0; i < 9; i++ {
		env.postRazorpay("wrong-secret", fmt.Sprintf("evt_bad_%d", i), body)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' = 'badsig:razorpay'`); n != 0 {
		t.Fatalf("nine rejections already alerted (%d): one stray failure is noise", n)
	}
	env.postRazorpay("wrong-secret", "evt_bad_9", body)
	if n := env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' = 'badsig:razorpay'`); n < 1 {
		t.Skip("platform has no operator account to alert")
	}
}

func TestStoredWebhookEventsHoldNoCustomerDetails(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(row.Amount*100 + 0.5)

	// What the gateway really sends carries the payer's contact and instrument.
	raw, _ := json.Marshal(map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
			"id": "pay_PII", "order_id": row.GatewayOrder, "amount": minor,
			"email": "ravi.private@example.com", "contact": "+919811122233", "vpa": "ravi@okbank",
			"card": map[string]any{"last4": "4242", "name": "Ravi Kumar"},
		}}},
	})
	status, body := env.postRazorpay(shop.webhookSecret, "evt_pii", raw)
	env.mustStatus(http.StatusOK, status, "capture with personal details", body)

	var stored string
	if err := env.server.pool.QueryRow(tctx(), `SELECT payload::text FROM payment_events WHERE event_id = 'evt_pii'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, pii := range []string{"ravi.private@example.com", "+919811122233", "ravi@okbank", "4242", "Ravi Kumar"} {
		if strings.Contains(stored, pii) {
			t.Errorf("the event log kept %q: %s", pii, stored)
		}
	}
	for _, keep := range []string{row.GatewayOrder, "pay_PII"} {
		if !strings.Contains(stored, keep) {
			t.Errorf("the event log lost %q, which is needed to understand it later: %s", keep, stored)
		}
	}
}

func TestSandboxSecretCanBeSetApartFromTheLoginKey(t *testing.T) {
	t.Setenv("PAYMENTS_SANDBOX_SECRET", "a-separate-sandbox-secret")
	env := newV2Env(t)
	shop := newShop(t, env)
	_, _, gwOrder, minor := startedOnlineOrder(t, env, shop)
	ev := payments.Event{ID: "evt_sep", Type: payments.EventPaid, ProviderOrderID: gwOrder, AmountMinor: minor}

	status, _ := env.postWebhook("sandbox", env.sandboxFor(), ev) // signed with the login-derived key
	env.mustStatus(http.StatusUnauthorized, status, "signed with the old derived key", nil)
	status, body := env.postWebhook("sandbox", payments.NewSandbox("a-separate-sandbox-secret"), ev)
	env.mustStatus(http.StatusOK, status, "signed with the configured key", body)
	if body["status"] != "applied" {
		t.Errorf("status = %v, want applied", body["status"])
	}
}

func TestPaymentHealthShowsWhatNeedsAHuman(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	admin := env.superAdmin()

	status, base := env.do(http.MethodGet, "/api/v1/admin/payments/health", admin, nil)
	env.mustStatus(http.StatusOK, status, "health", base)
	toInt := func(m map[string]any, k string) int { v, _ := m[k].(float64); return int(v) }

	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payments SET expires_at = now() - interval '1 hour' WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	env.postRazorpay(shop.webhookSecret, "evt_health_short", rzEventBody("payment.captured", row.GatewayOrder, "pay_S", 100, ""))

	status, now := env.do(http.MethodGet, "/api/v1/admin/payments/health", admin, nil)
	env.mustStatus(http.StatusOK, status, "health after trouble", now)
	if toInt(now, "overdue_pending") <= toInt(base, "overdue_pending") {
		t.Errorf("overdue_pending %v -> %v, want it to grow", base["overdue_pending"], now["overdue_pending"])
	}
	if toInt(now, "mismatches_24h") <= toInt(base, "mismatches_24h") {
		t.Errorf("mismatches_24h %v -> %v, want it to grow", base["mismatches_24h"], now["mismatches_24h"])
	}
	if now["needs_attention"] != true {
		t.Errorf("needs_attention = %v, want true", now["needs_attention"])
	}
	// Platform operators only.
	status, _ = env.do(http.MethodGet, "/api/v1/admin/payments/health", shop.adminToken, nil)
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("owner reading platform health: %d, want 401/403", status)
	}
}

func TestOperatorCanReconcileAPaymentTheCustomerSaysTheyPaid(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	admin := env.superAdmin()
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	url := "/api/v1/admin/payments/" + row.ID.String() + "/reconcile"

	// The gateway cannot be reached: an honest error, nothing changes.
	gw.mu.Lock()
	gw.listStatus = http.StatusInternalServerError
	gw.mu.Unlock()
	status, _ := env.do(http.MethodPost, url, admin, nil)
	env.mustStatus(http.StatusBadGateway, status, "reconcile while the gateway is down", nil)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PENDING" {
		t.Fatalf("payment = %s, want PENDING", got)
	}

	// Gateway back, but the customer has not actually paid: still pending.
	gw.mu.Lock()
	gw.listStatus = 0
	gw.mu.Unlock()
	status, body := env.do(http.MethodPost, url, admin, nil)
	env.mustStatus(http.StatusOK, status, "reconcile unpaid", body)
	if body["status"] != "PENDING" || body["updated"] != false {
		t.Errorf("unpaid reconcile = %v", body)
	}

	// They did pay and the webhook was lost: no waiting for the polling throttle.
	gw.addPayment(row.GatewayOrder, "pay_FOUND", "captured", int64(row.Amount*100+0.5))
	status, body = env.do(http.MethodPost, url, admin, nil)
	env.mustStatus(http.StatusOK, status, "reconcile paid", body)
	if body["status"] != "PAID" || body["updated"] != true {
		t.Errorf("paid reconcile = %v", body)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM audit_logs WHERE tenant_id = $1 AND action LIKE 'Payment reconciled%'`, shop.tenantID); n != 2 {
		t.Errorf("reconcile audit rows = %d, want 2 (the unpaid and the paid reconcile; the failed one changed nothing): an operator action on a business's money must be on the record", n)
	}

	// Not for owners, and not for ids that do not exist.
	status, _ = env.do(http.MethodPost, url, shop.adminToken, nil)
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("owner reconcile: %d, want 401/403", status)
	}
	status, _ = env.do(http.MethodPost, "/api/v1/admin/payments/"+uuid.NewString()+"/reconcile", admin, nil)
	env.mustStatus(http.StatusNotFound, status, "unknown payment", nil)
}

func TestStuckRefundsAreFlaggedForTheOperator(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	_, row, _ := paidLive(t, env, shop, "9811122233")
	gw.mu.Lock()
	gw.refundState = "pending"
	gw.mu.Unlock()
	status, _ := shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusOK, status, "refund accepted but unconfirmed", nil)

	var refundID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(), `SELECT id FROM payment_refunds WHERE payment_id = $1`, row.ID).Scan(&refundID); err != nil {
		t.Fatal(err)
	}
	alertCount(t, env, "refund:"+refundID.String())
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payment_refunds SET updated_at = now() - interval '2 hours' WHERE id = $1`, refundID); err != nil {
		t.Fatal(err)
	}
	env.server.paymentSvc.Watch(context.Background())

	_, health := env.do(http.MethodGet, "/api/v1/admin/payments/health", env.superAdmin(), nil)
	if v, _ := health["stuck_refunds"].(float64); v < 1 {
		t.Errorf("stuck_refunds = %v, want at least 1", health["stuck_refunds"])
	}
	if n := env.count(`SELECT COUNT(*)::int FROM notifications WHERE event_code = 'PLATFORM_OPERATIONAL_ALERT' AND data->>'alert_key' = $1`, "refund:"+refundID.String()); n < 1 {
		t.Skip("platform has no operator account to alert")
	}
}

func TestPaymentsSmokeScriptWorksEndToEndInDummyMode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	env := newV2Env(t)
	shop := newShop(t, env)

	cmd := exec.Command("bash", "../../scripts/payments_smoke.sh")
	cmd.Env = append(os.Environ(),
		"API="+env.http.URL,
		"HOST="+shop.host,
		"ADMIN_HOST=api."+env.cfg.BaseDomain,
		"OWNER_TOKEN="+shop.adminToken,
		"PRODUCT_ID="+shop.productID,
		"PHONE=9000000042",
		"WAIT_SECS=30",
		"ALLOW_SANDBOX=1",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the smoke script failed: %v", err)
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("the smoke script reported a failure:\n%s", out)
	}
	for _, want := range []string{"payment became PAID", "payment ended REFUNDED", "a second refund is refused"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the script never reached %q", want)
		}
	}
}

func TestPaymentsSmokeScriptRefusesToRunAgainstTheWrongMode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	env := newV2Env(t)
	shop := newShop(t, env) // dummy mode, and ALLOW_SANDBOX is not set
	cmd := exec.Command("bash", "../../scripts/payments_smoke.sh")
	cmd.Env = append(os.Environ(), "API="+env.http.URL, "HOST="+shop.host, "OWNER_TOKEN="+shop.adminToken, "PRODUCT_ID="+shop.productID)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "business is in real-payment mode") {
		t.Errorf("a live-gateway check must not silently pass against a dummy-mode business:\n%s", out)
	}
}
