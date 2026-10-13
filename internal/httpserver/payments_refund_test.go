package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Refunds (PAYMENTS-PLAN.md P-D).

// paidLive places an online order for a live business and has the gateway
// capture it, returning everything a refund test needs.
func paidLive(t *testing.T, env *testEnv, shop *liveFixture, phone string) (orderNo int, row payRow, gwPayment string) {
	t.Helper()
	orderNo, status, body := shop.startOnline(t, phone)
	env.mustStatus(http.StatusOK, status, "start live payment", body)
	row = env.paymentFor(shop.tenantID, orderNo)
	gwPayment = "pay_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	status, body = env.postRazorpay(shop.webhookSecret, "evt_cap_"+gwPayment,
		rzEventBody("payment.captured", row.GatewayOrder, gwPayment, int64(row.Amount*100+0.5), ""))
	env.mustStatus(http.StatusOK, status, "capture", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PAID" {
		t.Fatalf("setup: payment = %s, want PAID", got)
	}
	return orderNo, env.paymentFor(shop.tenantID, orderNo), gwPayment
}

func (e *testEnv) refundRows(paymentID uuid.UUID) (statuses []string) {
	e.t.Helper()
	rows, err := e.server.pool.Query(tctx(), `SELECT status FROM payment_refunds WHERE payment_id = $1 ORDER BY created_at`, paymentID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		statuses = append(statuses, s)
	}
	return statuses
}

func (e *testEnv) auditEvents(paymentID uuid.UUID) string {
	e.t.Helper()
	rows, err := e.server.pool.Query(tctx(), `SELECT event_type FROM payment_audit_log WHERE payment_id = $1 ORDER BY created_at`, paymentID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func refundURL(paymentID uuid.UUID) string {
	return "/api/v1/tenant/payments/" + paymentID.String() + "/refund"
}

func TestOwnerRefundReturnsTheMoneyOnceAndLeavesATrail(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, row, gwPay := paidLive(t, env, shop, "9811122233")

	status, body := shop.admin(http.MethodPost, refundURL(row.ID), map[string]any{"reason": "Customer changed their mind"})
	env.mustStatus(http.StatusOK, status, "refund", body)
	if body["payment_status"] != "REFUNDED" || body["done"] != true {
		t.Fatalf("refund response = %v", body)
	}
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED", got)
	}
	if len(gw.refunds) != 1 || gw.refunds[0].PaymentID != gwPay || gw.refunds[0].Amount != int64(row.Amount*100+0.5) || gw.refunds[0].Key == "" {
		t.Errorf("gateway refund calls = %+v, want one full refund of %s with an idempotency key", gw.refunds, gwPay)
	}
	if rows := env.refundRows(row.ID); len(rows) != 1 || rows[0] != "COMPLETED" {
		t.Errorf("refund rows = %v", rows)
	}
	if got := env.auditEvents(row.ID); !strings.Contains(got, "REFUNDING") || !strings.HasSuffix(got, "REFUNDED") {
		t.Errorf("audit trail = %s", got)
	}
	if env.count(`SELECT COUNT(*)::int FROM notifications WHERE tenant_id = $1 AND event_code = 'PAYMENT_REFUNDED'`, shop.tenantID) < 1 {
		t.Error("the owner should be told the refund happened")
	}

	// Pressing the button again (or a second tab) must not refund twice.
	status, body = shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusConflict, status, "second refund", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "refund_in_progress" {
		t.Errorf("error = %v, want refund_in_progress", body["error"])
	}
	if len(gw.refunds) != 1 {
		t.Errorf("gateway was asked %d times, want 1", len(gw.refunds))
	}

	// The staff view says so, for the order cards.
	_, orders := shop.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	if !strings.Contains(str(t, orders, "__raw"), `"refund_status":"COMPLETED"`) {
		t.Errorf("the order list does not show the refund: %s", str(t, orders, "__raw"))
	}
}

func TestRefundIsOnlyForPaidOnlineMoneyAndOnlyForTheOwner(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)

	// Unpaid online payment.
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	pending := env.paymentFor(shop.tenantID, orderNo)
	status, _ := shop.admin(http.MethodPost, refundURL(pending.ID), nil)
	env.mustStatus(http.StatusConflict, status, "refund of an unpaid payment", nil)

	// Cash never passed through us.
	cash := shop.order(t, "Asha Rao", "9876543210", "CASH")
	cashID := uuid.MustParse(env.paymentID(shop.tenantID, number(t, cash, "order_number")))
	status, _ = shop.admin(http.MethodPost, refundURL(cashID), nil)
	env.mustStatus(http.StatusConflict, status, "refund of cash", nil)

	// Unknown payment, and a payment that belongs to another business.
	status, _ = shop.admin(http.MethodPost, refundURL(uuid.New()), nil)
	env.mustStatus(http.StatusConflict, status, "refund of nothing", nil)
	other := newShop(t, env)
	otherOrder := other.order(t, "Asha Rao", "9876543210", "CASH")
	otherPay := uuid.MustParse(env.paymentID(other.tenantID, number(t, otherOrder, "order_number")))
	status, _ = shop.admin(http.MethodPost, refundURL(otherPay), nil)
	env.mustStatus(http.StatusConflict, status, "refund of another business's payment", nil)

	// Staff without owner rights cannot move money.
	_, paid, _ := paidLive(t, env, shop, "9811100000")
	status, _ = env.doOn(http.MethodPost, refundURL(paid.ID), shop.staffToken, nil, shop.host)
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("staff refund: status %d, want 401/403", status)
	}
	if got := env.paymentFor(shop.tenantID, number0(paid, env, shop)); got.Status != "PAID" {
		t.Errorf("payment = %s after a refused refund, want PAID", got.Status)
	}
}

// number0 re-reads a payment row by id (the helper above keys on order number).
func number0(p payRow, env *testEnv, shop *liveFixture) int {
	var n int
	_ = env.server.pool.QueryRow(tctx(), `SELECT o.order_number FROM orders o JOIN payments p ON p.order_id = o.id WHERE p.id = $1`, p.ID).Scan(&n)
	return n
}

func TestRefundThatSettlesLaterIsFinishedByTheGatewaysCallback(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, row, gwPay := paidLive(t, env, shop, "9811122233")
	gw.mu.Lock()
	gw.refundState = "pending"
	gw.mu.Unlock()

	status, body := shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusOK, status, "refund accepted", body)
	if body["done"] != false || body["payment_status"] != "REFUNDING" {
		t.Fatalf("a pending refund = %v, want REFUNDING / not done", body)
	}
	rfID := gw.refundByKey[gw.refunds[0].Key]

	// Forged and mis-signed callbacks do nothing.
	cb, _ := json.Marshal(map[string]any{"event": "refund.processed", "payload": map[string]any{"refund": map[string]any{"entity": map[string]any{"id": rfID, "payment_id": gwPay, "amount": gw.refunds[0].Amount}}}})
	status, _ = env.postRazorpay("wrong", "evt_rf_bad", cb)
	env.mustStatus(http.StatusUnauthorized, status, "forged refund callback", nil)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDING" {
		t.Fatalf("payment = %s after a forged callback", got)
	}

	status, body = env.postRazorpay(shop.webhookSecret, "evt_rf_ok", cb)
	env.mustStatus(http.StatusOK, status, "genuine refund callback", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED", got)
	}
	if rows := env.refundRows(row.ID); len(rows) != 1 || rows[0] != "COMPLETED" {
		t.Errorf("refund rows = %v", rows)
	}
	// A retried delivery changes nothing.
	status, body = env.postRazorpay(shop.webhookSecret, "evt_rf_ok", cb)
	env.mustStatus(http.StatusOK, status, "retried callback", body)
	if body["status"] != "duplicate" {
		t.Errorf("retry = %v, want duplicate", body["status"])
	}
}

func TestRefundTheGatewayFailsPutsTheMoneyBackWhereItWas(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, row, gwPay := paidLive(t, env, shop, "9811122233")

	// The gateway says no outright.
	gw.mu.Lock()
	gw.refundStatus = http.StatusBadRequest
	gw.mu.Unlock()
	status, body := shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusBadGateway, status, "refused refund", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "refund_rejected" {
		t.Errorf("error = %v, want refund_rejected", body["error"])
	}
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PAID" {
		t.Errorf("payment = %s, want PAID (nothing was returned)", got)
	}
	if rows := env.refundRows(row.ID); len(rows) != 1 || rows[0] != "FAILED" {
		t.Errorf("refund rows = %v, want one FAILED", rows)
	}
	if env.count(`SELECT COUNT(*)::int FROM notifications WHERE tenant_id = $1 AND event_code = 'PAYMENT_REFUND_FAILED'`, shop.tenantID) < 1 {
		t.Error("a failed refund must reach the owner")
	}

	// It can be tried again once the gateway agrees: a failed refund does not block a new one.
	gw.mu.Lock()
	gw.refundStatus = 0
	gw.mu.Unlock()
	status, body = shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusOK, status, "retry", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED", got)
	}

	// An asynchronous failure notice behaves the same way.
	orderNo2, row2, gwPay2 := paidLive(t, env, shop, "9811122244")
	gw.mu.Lock()
	gw.refundState = "pending"
	gw.mu.Unlock()
	_, _ = shop.admin(http.MethodPost, refundURL(row2.ID), nil)
	rf := gw.refundByKey[gw.refunds[len(gw.refunds)-1].Key]
	cb, _ := json.Marshal(map[string]any{"event": "refund.failed", "payload": map[string]any{"refund": map[string]any{"entity": map[string]any{"id": rf, "payment_id": gwPay2}}}})
	status, _ = env.postRazorpay(shop.webhookSecret, "evt_rf_fail", cb)
	env.mustStatus(http.StatusOK, status, "refund.failed callback", nil)
	if got := env.paymentFor(shop.tenantID, orderNo2).Status; got != "PAID" {
		t.Errorf("payment = %s after refund.failed, want PAID", got)
	}
	_ = gwPay
}

func TestInterruptedRefundIsResumedWithoutRefundingTwice(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, row, _ := paidLive(t, env, shop, "9811122233")

	// The gateway is down when the owner presses refund.
	gw.mu.Lock()
	gw.refundStatus = http.StatusInternalServerError
	gw.mu.Unlock()
	status, body := shop.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusAccepted, status, "refund while the gateway is down", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDING" {
		t.Fatalf("payment = %s, want REFUNDING (recorded, waiting to be sent)", got)
	}
	if rows := env.refundRows(row.ID); len(rows) != 1 || rows[0] != "PENDING" {
		t.Fatalf("refund rows = %v, want one PENDING", rows)
	}
	firstKey := gw.refunds[0].Key

	// The gateway recovers and the background job finishes the refund.
	gw.mu.Lock()
	gw.refundStatus = 0
	gw.mu.Unlock()
	if _, err := env.server.pool.Exec(tctx(), `UPDATE payment_refunds SET updated_at = now() - interval '5 minutes' WHERE payment_id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.server.paymentSvc.ResumeRefunds(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED after the job", got)
	}
	// Same key both times, so the gateway itself would have refunded once.
	if last := gw.refunds[len(gw.refunds)-1]; last.Key != firstKey {
		t.Errorf("retry used key %q, first used %q: a retry must reuse the key", last.Key, firstKey)
	}
	if n := len(gw.refundByKey); n != 1 {
		t.Errorf("the gateway minted %d refunds, want 1", n)
	}
}

func TestCancellingAPaidOrderRefundsItAndNeverBlocksTheCancel(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)

	// Normal case: paid online, then cancelled.
	orderNo, row, _ := paidLive(t, env, shop, "9811122233")
	status, body := shop.act(env.orderID(shop.tenantID, orderNo), "cancel")
	env.mustStatus(http.StatusOK, status, "cancel a paid order", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s after cancelling, want REFUNDED", got)
	}
	if len(gw.refunds) != 1 {
		t.Errorf("refund calls = %d, want 1", len(gw.refunds))
	}
	if !strings.Contains(env.auditEvents(row.ID), "REFUNDED") {
		t.Errorf("audit = %s", env.auditEvents(row.ID))
	}

	// The gateway is down: the order is still cancelled, the refund waits.
	orderNo2, row2, _ := paidLive(t, env, shop, "9811122244")
	gw.mu.Lock()
	gw.refundStatus = http.StatusServiceUnavailable
	gw.mu.Unlock()
	status, body = shop.act(env.orderID(shop.tenantID, orderNo2), "cancel")
	env.mustStatus(http.StatusOK, status, "cancel while the gateway is down", body)
	var orderStatus string
	_ = env.server.pool.QueryRow(tctx(), `SELECT status FROM orders WHERE tenant_id = $1 AND order_number = $2`, shop.tenantID, orderNo2).Scan(&orderStatus)
	if orderStatus != "CANCELLED" {
		t.Fatalf("order = %s, want CANCELLED even though the refund could not be sent", orderStatus)
	}
	if got := env.paymentFor(shop.tenantID, orderNo2).Status; got != "REFUNDING" {
		t.Errorf("payment = %s, want REFUNDING (queued for the job)", got)
	}
	if rows := env.refundRows(row2.ID); len(rows) != 1 || rows[0] != "PENDING" {
		t.Errorf("refund rows = %v", rows)
	}

	// Cash orders and unpaid orders have nothing to return.
	cash := shop.order(t, "Asha Rao", "9876543210", "CASH")
	cashNo := number(t, cash, "order_number")
	before := len(gw.refunds)
	status, _ = shop.act(env.orderID(shop.tenantID, cashNo), "cancel")
	env.mustStatus(http.StatusOK, status, "cancel a cash order", nil)
	if len(gw.refunds) != before {
		t.Error("cancelling a cash order must not call the gateway")
	}
}

func TestPaymentThatLandsAfterTheOrderWasCancelledIsReturned(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, status, body := shop.startOnline(t, "9811122233")
	env.mustStatus(http.StatusOK, status, "start", body)
	row := env.paymentFor(shop.tenantID, orderNo)

	// Cancelled while the customer still has the payment window open.
	status, _ = shop.act(env.orderID(shop.tenantID, orderNo), "cancel")
	env.mustStatus(http.StatusOK, status, "cancel before payment", nil)
	if len(gw.refunds) != 0 {
		t.Fatal("nothing was paid yet, so nothing to refund")
	}

	// They pay anyway.
	status, body = env.postRazorpay(shop.webhookSecret, "evt_late_pay",
		rzEventBody("payment.captured", row.GatewayOrder, "pay_TOOLATE", int64(row.Amount*100+0.5), ""))
	env.mustStatus(http.StatusOK, status, "late payment", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED: money must not be kept for a cancelled order", got)
	}
	if len(gw.refunds) != 1 || gw.refunds[0].PaymentID != "pay_TOOLATE" {
		t.Errorf("refund calls = %+v", gw.refunds)
	}
	if got := env.auditEvents(row.ID); !strings.Contains(got, "PAID") || !strings.Contains(got, "REFUNDED") {
		t.Errorf("audit = %s, want both the payment and its return recorded", got)
	}
}

func TestDoubleChargeIsReturnedAndTheOrderStaysPaid(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, status, body := shop.startOnline(t, "9811122233")
	env.mustStatus(http.StatusOK, status, "start", body)
	first := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(first.Amount*100 + 0.5)
	status, body = shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo), map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "second attempt", body)
	second := env.paymentFor(shop.tenantID, orderNo)

	env.postRazorpay(shop.webhookSecret, "evt_a", rzEventBody("payment.captured", first.GatewayOrder, "pay_FIRST", minor, ""))
	env.postRazorpay(shop.webhookSecret, "evt_b", rzEventBody("payment.captured", second.GatewayOrder, "pay_SECOND", minor, ""))

	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PAID" {
		t.Errorf("order payment = %s, want PAID (the first charge stands)", got)
	}
	if len(gw.refunds) != 1 || gw.refunds[0].PaymentID != "pay_SECOND" || gw.refunds[0].Amount != minor {
		t.Errorf("refunds = %+v, want exactly the second charge returned", gw.refunds)
	}
	if rows := env.refundRows(first.ID); len(rows) != 1 || rows[0] != "COMPLETED" {
		t.Errorf("refund rows = %v", rows)
	}
	if got := env.auditEvents(first.ID); !strings.Contains(got, "DUPLICATE_PAYMENT") || !strings.Contains(got, "DUPLICATE_REFUNDED") {
		t.Errorf("audit = %s", got)
	}
	// The owner can still refund the genuine payment later, separately.
	status, _ = shop.admin(http.MethodPost, refundURL(first.ID), nil)
	env.mustStatus(http.StatusOK, status, "refund of the first payment", nil)
	if len(gw.refunds) != 2 || gw.refunds[1].PaymentID != "pay_FIRST" {
		t.Errorf("refunds = %+v", gw.refunds)
	}
}

func TestSandboxPaymentsRefundWithoutAGateway(t *testing.T) {
	env := newV2Env(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")
	_, started := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo), map[string]any{"method": "ONLINE", "phone": "9811122233"})
	status, _ := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": str(t, started, "payment_token"), "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "sandbox payment", nil)
	id := uuid.MustParse(env.paymentID(shop.tenantID, orderNo))

	status, body := shop.admin(http.MethodPost, refundURL(id), nil)
	env.mustStatus(http.StatusOK, status, "sandbox refund", body)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "REFUNDED" {
		t.Errorf("payment = %s, want REFUNDED", got)
	}
}

// dashboardRevenue reads today's revenue; the API sends it as a decimal string.
func dashboardRevenue(t *testing.T, shop *shopFixture) float64 {
	t.Helper()
	_, d := shop.admin(http.MethodGet, "/api/v1/tenant/dashboard", nil)
	v, err := strconv.ParseFloat(fmt.Sprint(d["revenue_today"]), 64)
	if err != nil {
		t.Fatalf("revenue_today = %v", d["revenue_today"])
	}
	return v
}

func TestTestAndRefundedMoneyStaysOutOfRevenue(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := newShop(t, env)

	complete := func(orderNo int) {
		id := env.orderID(shop.tenantID, orderNo)
		for _, a := range []string{"accept", "prepare", "ready", "complete"} {
			if status, body := shop.act(id, a); status != http.StatusOK {
				t.Fatalf("order %d %s: %d %v", orderNo, a, status, body["__raw"])
			}
		}
	}
	revenue := func() float64 {
		return dashboardRevenue(t, shop)
	}

	// A completed cash order counts.
	cash := shop.order(t, "Asha Rao", "9876543210", "CASH")
	cashNo := number(t, cash, "order_number")
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+env.paymentID(shop.tenantID, cashNo)+"/confirm", nil)
	env.mustStatus(http.StatusOK, status, "cash received", nil)
	complete(cashNo)
	base := revenue()
	if base <= 0 {
		t.Fatalf("a completed cash order should count towards revenue, got %v", base)
	}

	// A completed order paid through the dummy gateway does not.
	online := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	onlineNo := number(t, online, "order_number")
	_, started := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", onlineNo), map[string]any{"method": "ONLINE", "phone": "9811122233"})
	shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", onlineNo),
		map[string]any{"outcome": "SUCCESS", "token": str(t, started, "payment_token"), "phone": "9811122233"})
	complete(onlineNo)
	if got := revenue(); got != base {
		t.Errorf("revenue = %v after a test payment, want it unchanged at %v", got, base)
	}
	// ...and the mix labels it as a test, not as real online sales.
	_, an := shop.admin(http.MethodGet, "/api/v1/tenant/analytics", nil)
	if !strings.Contains(str(t, an, "__raw"), "ONLINE_TEST") {
		t.Errorf("payment mix should label dummy payments: %s", str(t, an, "__raw"))
	}

	// A completed, then refunded, real payment stops counting.
	live := liveShop(t, env)
	orderNo, row, _ := paidLive(t, env, live, "9811100001")
	complete2 := func() {
		id := env.orderID(live.tenantID, orderNo)
		for _, a := range []string{"accept", "prepare", "ready", "complete"} {
			live.act(id, a)
		}
	}
	complete2()
	rev := func() float64 {
		return dashboardRevenue(t, live.shopFixture)
	}
	if rev() <= 0 {
		t.Fatalf("a completed real payment should count, got %v", rev())
	}
	status, _ = live.admin(http.MethodPost, refundURL(row.ID), nil)
	env.mustStatus(http.StatusOK, status, "refund of a completed order", nil)
	if got := rev(); got != 0 {
		t.Errorf("revenue = %v after a refund, want 0", got)
	}
}

func TestRefundsDoNotChangeAnythingWithTheFlagOff(t *testing.T) {
	t.Setenv("PAYMENTS_V2", "")
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")
	_, started := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo), map[string]any{"method": "ONLINE", "phone": "9811122233"})
	shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "token": str(t, started, "payment_token"), "phone": "9811122233"})

	// Cancelling a paid order with the flag off behaves as it always did.
	status, _ := shop.act(env.orderID(shop.tenantID, orderNo), "cancel")
	env.mustStatus(http.StatusOK, status, "cancel", nil)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PAID" {
		t.Errorf("payment = %s with the flag off, want it left as PAID", got)
	}
	// Legacy dummy payments are not relabelled as test either.
	_, orders := shop.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	if strings.Contains(str(t, orders, "__raw"), `"is_test":true`) {
		t.Error("with the flag off nothing should be labelled test")
	}
}
