package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/payments"
)

// Payments framework (PAYMENTS-PLAN.md P-A): webhook path, audit trail, replay
// guard and staff-confirm restriction. These run with PAYMENTS_V2 on; the
// flag-off behaviour is covered by the pre-existing payment tests.

func newV2Env(t *testing.T) *testEnv {
	t.Helper()
	t.Setenv("PAYMENTS_V2", "true")
	return newTestEnv(t)
}

// sandboxFor signs callbacks exactly as the server's sandbox gateway does.
func (e *testEnv) sandboxFor() *payments.Sandbox {
	return payments.NewSandbox(e.cfg.JWTAccessSecret + ":payments-sandbox")
}

// postWebhook delivers a signed callback to the shared API host.
func (e *testEnv) postWebhook(provider string, sb *payments.Sandbox, ev payments.Event) (int, map[string]any) {
	e.t.Helper()
	body, hdr := sb.Sign(ev)
	return e.rawWebhook(provider, body, hdr.Get(payments.SignatureHeader))
}

func (e *testEnv) rawWebhook(provider string, body []byte, sig string) (int, map[string]any) {
	e.t.Helper()
	extra := map[string]string{}
	if sig != "" {
		extra[payments.SignatureHeader] = sig
	}
	return e.doOnHeaders(http.MethodPost, "/api/v1/webhooks/payments/"+provider, "",
		json.RawMessage(body), "api."+e.cfg.BaseDomain, extra)
}

// startedOnlineOrder places an online order and starts its payment, returning
// the order number, payment row id and gateway order id.
func startedOnlineOrder(t *testing.T, env *testEnv, shop *shopFixture) (orderNo int, paymentID uuid.UUID, gwOrder string, amountMinor int64) {
	t.Helper()
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo = number(t, order, "order_number")
	status, started := shop.guest(http.MethodPost,
		fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "start payment", started)
	var amount float64
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT id, provider_order_id, (amount*100)::bigint FROM payments WHERE tenant_id = $1`,
		shop.tenantID).Scan(&paymentID, &gwOrder, &amountMinor); err != nil {
		t.Fatalf("load payment: %v (%v)", err, amount)
	}
	return orderNo, paymentID, gwOrder, amountMinor
}

func (e *testEnv) paymentStatus(id uuid.UUID) (status, mode string, expires *time.Time) {
	e.t.Helper()
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT status, mode, expires_at FROM payments WHERE id = $1`, id).Scan(&status, &mode, &expires); err != nil {
		e.t.Fatalf("payment status: %v", err)
	}
	return
}

func (e *testEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.server.pool.QueryRow(tctx(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count: %v", err)
	}
	return n
}

func TestStartRecordsTheGatewayIntent(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	_, id, gwOrder, _ := startedOnlineOrder(t, env, shop)
	if gwOrder == "" {
		t.Fatal("start should record the gateway order id")
	}
	status, mode, expires := env.paymentStatus(id)
	if status != "PENDING" || mode != "SANDBOX" || expires == nil || !expires.After(time.Now()) {
		t.Errorf("status=%s mode=%s expires=%v", status, mode, expires)
	}
}

func TestSignedWebhookCapturesThePaymentOnce(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	orderNo, id, gwOrder, minor := startedOnlineOrder(t, env, shop)
	sb := env.sandboxFor()
	ev := payments.Event{ID: "evt_" + gwOrder + "_PAID", Type: payments.EventPaid, ProviderOrderID: gwOrder, AmountMinor: minor}

	status, body := env.postWebhook("sandbox", sb, ev)
	env.mustStatus(http.StatusOK, status, "webhook", body)
	if body["status"] != "applied" {
		t.Errorf("first delivery = %v, want applied", body["status"])
	}
	if got, _, _ := env.paymentStatus(id); got != "PAID" {
		t.Fatalf("payment = %s, want PAID", got)
	}

	// Replay: acknowledged, nothing changes, nothing is recorded twice.
	status, body = env.postWebhook("sandbox", sb, ev)
	env.mustStatus(http.StatusOK, status, "replayed webhook", body)
	if body["status"] != "duplicate" {
		t.Errorf("replay = %v, want duplicate", body["status"])
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_events WHERE payment_id = $1`, id); n != 1 {
		t.Errorf("payment_events rows = %d, want 1", n)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND new_status = 'PAID'`, id); n != 1 {
		t.Errorf("PAID audit rows = %d, want 1", n)
	}

	// A later failure callback can never undo captured money.
	fail := payments.Event{ID: "evt_other_FAILED", Type: payments.EventFailed, ProviderOrderID: gwOrder, AmountMinor: minor}
	status, body = env.postWebhook("sandbox", sb, fail)
	env.mustStatus(http.StatusOK, status, "late failure", body)
	if got, _, _ := env.paymentStatus(id); got != "PAID" {
		t.Errorf("payment = %s after a late failure, want PAID", got)
	}

	// The shop is told in-app, and the customer's order reflects it.
	if n := env.count(`SELECT COUNT(*)::int FROM notifications WHERE tenant_id = $1 AND event_code = 'PAYMENT_RECEIVED'`, shop.tenantID); n < 1 {
		t.Error("expected a PAYMENT_RECEIVED notification for the shop")
	}
	_, tracked := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d?phone=9811122233", orderNo), nil)
	if p, _ := tracked["payment"].(map[string]any); p == nil || p["status"] != "PAID" {
		t.Errorf("tracked payment = %v, want PAID", tracked["payment"])
	}
}

func TestWebhookRejectsWhatItCannotAuthenticate(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	_, id, gwOrder, minor := startedOnlineOrder(t, env, shop)
	sb := env.sandboxFor()
	ev := payments.Event{ID: "evt_x", Type: payments.EventPaid, ProviderOrderID: gwOrder, AmountMinor: minor}
	body, hdr := sb.Sign(ev)

	status, _ := env.rawWebhook("sandbox", body, "")
	env.mustStatus(http.StatusUnauthorized, status, "no signature", nil)
	status, _ = env.rawWebhook("sandbox", body, "t=1,v1=deadbeef")
	env.mustStatus(http.StatusUnauthorized, status, "wrong signature", nil)
	tampered := []byte(string(body[:len(body)-1]) + `,"amount_minor":1}`)
	status, _ = env.rawWebhook("sandbox", tampered, hdr.Get(payments.SignatureHeader))
	env.mustStatus(http.StatusUnauthorized, status, "tampered body", nil)
	wrongKey := payments.NewSandbox("not-the-secret")
	status, _ = env.postWebhook("sandbox", wrongKey, ev)
	env.mustStatus(http.StatusUnauthorized, status, "signed with the wrong key", nil)

	status, _ = env.postWebhook("stripe", sb, ev)
	env.mustStatus(http.StatusNotFound, status, "unknown provider", nil)

	if got, _, _ := env.paymentStatus(id); got != "PENDING" {
		t.Errorf("payment = %s after rejected callbacks, want PENDING", got)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_events WHERE payment_id = $1`, id); n != 0 {
		t.Errorf("rejected callbacks were recorded: %d rows", n)
	}
}

func TestWebhookNeverCapturesTheWrongAmount(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	_, id, gwOrder, minor := startedOnlineOrder(t, env, shop)

	status, body := env.postWebhook("sandbox", env.sandboxFor(), payments.Event{
		ID: "evt_short", Type: payments.EventPaid, ProviderOrderID: gwOrder, AmountMinor: minor - 1})
	env.mustStatus(http.StatusOK, status, "mismatched amount", body)
	if body["status"] != "rejected" {
		t.Errorf("status = %v, want rejected", body["status"])
	}
	if got, _, _ := env.paymentStatus(id); got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_events WHERE payment_id = $1 AND error = 'amount_mismatch'`, id); n != 1 {
		t.Errorf("amount_mismatch events = %d, want 1", n)
	}
}

func TestWebhookForAnUnknownPaymentIsAcknowledgedAndIgnored(t *testing.T) {
	env := newV2Env(t)
	status, body := env.postWebhook("sandbox", env.sandboxFor(), payments.Event{
		ID: "evt_ghost_" + uuid.NewString(), Type: payments.EventPaid, ProviderOrderID: "sbx_does_not_exist", AmountMinor: 100})
	env.mustStatus(http.StatusOK, status, "unknown payment", body)
	if body["status"] != "ignored" {
		t.Errorf("status = %v, want ignored", body["status"])
	}
}

func TestSandboxConfirmGoesThroughTheWebhookPath(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	orderNo, id, gwOrder, _ := startedOnlineOrder(t, env, shop)
	_, polled := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	token := str(t, polled, "payment_token")

	status, done := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "sandbox confirm", done)

	if got, _, _ := env.paymentStatus(id); got != "PAID" {
		t.Fatalf("payment = %s, want PAID", got)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_events WHERE provider = 'sandbox' AND event_id = $1`, "evt_"+gwOrder+"_PAID"); n != 1 {
		t.Errorf("sandbox confirm did not leave a webhook event record (%d)", n)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1`, id); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	// Double tap: same answer, still one event and one audit row.
	status, again := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "double tap", again)
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1`, id); n != 1 {
		t.Errorf("double tap wrote %d audit rows, want 1", n)
	}
}

func TestExpiredSandboxSessionCannotBeConfirmed(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	orderNo, id, _, _ := startedOnlineOrder(t, env, shop)
	_, polled := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	token := str(t, polled, "payment_token")
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payments SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	status, _ := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusConflict, status, "expired session", nil)
	if got, _, _ := env.paymentStatus(id); got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}
}

func TestPaymentStartedBeforeTheFlagStillSettles(t *testing.T) {
	// A payment opened while the flag was off has no gateway order id. Turning
	// the flag on mid-flight must not strand it.
	env := newV2Env(t)
	shop := newShop(t, env)
	orderNo, id, _, _ := startedOnlineOrder(t, env, shop)
	_, polled := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	token := str(t, polled, "payment_token")
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payments SET provider_order_id = '', expires_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	status, done := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "confirm legacy payment", done)
	if got, _, _ := env.paymentStatus(id); got != "PAID" {
		t.Errorf("payment = %s, want PAID", got)
	}
}

func TestStaffCannotConfirmAnOnlinePayment(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	orderNo, id, _, _ := startedOnlineOrder(t, env, shop)

	status, body := shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+env.paymentID(shop.tenantID, orderNo)+"/confirm", nil)
	env.mustStatus(http.StatusConflict, status, "staff confirm of an online payment", body)
	if got, _, _ := env.paymentStatus(id); got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}

	// Cash is still the counter's to confirm, and leaves an audit row naming staff.
	cash := shop.order(t, "Asha Rao", "9876543210", "CASH")
	cashID := env.paymentID(shop.tenantID, number(t, cash, "order_number"))
	status, body = shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+cashID+"/confirm", nil)
	env.mustStatus(http.StatusOK, status, "staff confirm of cash", body)
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND actor_type = 'staff'`, uuid.MustParse(cashID)); n != 1 {
		t.Errorf("staff audit rows = %d, want 1", n)
	}
	// Confirming twice keeps the answer this endpoint always gave.
	status, _ = shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+cashID+"/confirm", nil)
	env.mustStatus(http.StatusNotFound, status, "second cash confirm", nil)
}

func TestWebhookEndpointIsOffWithTheFlagOff(t *testing.T) {
	t.Setenv("PAYMENTS_V2", "")
	env := newTestEnv(t)
	sb := env.sandboxFor()
	status, _ := env.postWebhook("sandbox", sb, payments.Event{ID: "evt_off", Type: payments.EventPaid, ProviderOrderID: "sbx_1"})
	env.mustStatus(http.StatusNotFound, status, "sandbox webhook with the flag off", nil)
}
