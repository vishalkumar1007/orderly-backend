package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
)

// Live payments through a Razorpay stand-in (PAYMENTS-PLAN.md P-C). The stand-in
// speaks just enough of the gateway's API; what is under test is Orderly's
// behaviour around it: amounts, signatures, replays, lost webhooks, expiry.

type rzPayment struct {
	ID, Status string
	Amount     int64
}

type rzRefund struct {
	PaymentID, Key string
	Amount         int64
}

type fakeRazorpay struct {
	// refundStatus, when non-zero, makes the refund call answer that status;
	// refundState is the gateway's refund status ("processed" by default).
	refundStatus   int
	refundState    string
	refunds        []rzRefund
	refundByKey    map[string]string
	t              *testing.T
	srv            *httptest.Server
	mu             sync.Mutex
	next           int
	orders         map[string]int64 // gateway order id -> amount (minor)
	currency       map[string]string
	payments       map[string][]rzPayment
	users          []string // basic-auth key ids seen
	createStatus   int      // when non-zero, order creation answers this status
	listStatus     int      // when non-zero, payment listing answers this status
	creates, lists int
}

func newFakeRazorpay(t *testing.T) *fakeRazorpay {
	f := &fakeRazorpay{t: t, orders: map[string]int64{}, currency: map[string]string{}, payments: map[string][]rzPayment{}, refundByKey: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	restore := configsvc.SetRazorpayBaseURLForTest(f.srv.URL)
	t.Cleanup(func() { restore(); f.srv.Close() })
	return f
}

func (f *fakeRazorpay) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, _, _ := r.BasicAuth()
	f.users = append(f.users, user)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/orders": // credential check
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
		f.creates++
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		var in struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.next++
		id := fmt.Sprintf("order_FAKE%03d", f.next)
		f.orders[id], f.currency[id] = in.Amount, in.Currency
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "amount": in.Amount})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/payments/") && strings.HasSuffix(r.URL.Path, "/refund"):
		payID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/payments/"), "/refund")
		key := r.Header.Get("X-Refund-Idempotency")
		var in struct {
			Amount int64 `json:"amount"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.refunds = append(f.refunds, rzRefund{PaymentID: payID, Key: key, Amount: in.Amount})
		if f.refundStatus != 0 {
			w.WriteHeader(f.refundStatus)
			return
		}
		// Same key, same refund: this is what makes a retry safe.
		id, seen := f.refundByKey[key]
		if !seen {
			id = fmt.Sprintf("rfnd_FAKE%03d", len(f.refundByKey)+1)
			f.refundByKey[key] = id
		}
		state := f.refundState
		if state == "" {
			state = "processed"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": state})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/orders/") && strings.HasSuffix(r.URL.Path, "/payments"):
		f.lists++
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/orders/"), "/payments")
		items := []map[string]any{}
		for _, p := range f.payments[id] {
			items = append(items, map[string]any{"id": p.ID, "status": p.Status, "amount": p.Amount})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRazorpay) addPayment(orderID, payID, status string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payments[orderID] = append(f.payments[orderID], rzPayment{payID, status, amount})
}

func (f *fakeRazorpay) counts() (creates, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.lists
}

func rzSignature(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func rzEventBody(event, orderID, payID string, amount int64, reason string) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": event,
		"payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
			"id": payID, "order_id": orderID, "amount": amount, "error_description": reason}}},
	})
	return b
}

// postRazorpay delivers a callback, signed with secret, to the shared API host.
func (e *testEnv) postRazorpay(secret, eventID string, body []byte) (int, map[string]any) {
	e.t.Helper()
	extra := map[string]string{"X-Razorpay-Signature": rzSignature(secret, body)}
	if eventID != "" {
		extra["X-Razorpay-Event-Id"] = eventID
	}
	return e.doOnHeaders(http.MethodPost, "/api/v1/webhooks/payments/razorpay", "", json.RawMessage(body), "api."+e.cfg.BaseDomain, extra)
}

type liveFixture struct {
	*shopFixture
	keyID, keySecret, webhookSecret string
}

// liveShop is a business with its own gateway keys, tested and switched to
// real payments: exactly the path a Super Admin takes.
func liveShop(t *testing.T, env *testEnv) *liveFixture {
	t.Helper()
	shop := newShop(t, env)
	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	lf := &liveFixture{shopFixture: shop, keyID: "rzp_test_" + sfx, keySecret: "KEYSECRET_" + sfx, webhookSecret: "WHSECRET_" + sfx}

	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/configurations/payments", map[string]any{
		"provider": "razorpay", "enabled": true,
		"config": map[string]any{"key_id": lf.keyID, "key_secret": lf.keySecret, "webhook_secret": lf.webhookSecret},
	})
	env.mustStatus(http.StatusOK, status, "owner saves gateway keys", body)
	status, body = shop.admin(http.MethodPost, "/api/v1/tenant/configurations/payments/test", nil)
	env.mustStatus(http.StatusOK, status, "owner tests gateway keys", body)
	if body["ok"] != true {
		t.Fatalf("connection test should pass against the stand-in: %v", body)
	}
	status, body = env.do(http.MethodPut, "/api/v1/admin/tenants/"+shop.tenantID.String()+"/payments", env.superAdmin(),
		map[string]any{"mode": "LIVE", "confirm": true})
	env.mustStatus(http.StatusOK, status, "Super Admin takes the business live", body)
	return lf
}

func (l *liveFixture) startOnline(t *testing.T, phone string) (orderNo int, status int, body map[string]any) {
	t.Helper()
	order := l.order(t, "Ravi Kumar", phone, "ONLINE")
	orderNo = number(t, order, "order_number")
	status, body = l.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": phone})
	return orderNo, status, body
}

type payRow struct {
	ID                                   uuid.UUID
	Status, Provider, Mode, GatewayOrder string
	Amount                               float64
	Attempts                             int
}

func (e *testEnv) paymentFor(tenantID uuid.UUID, orderNo int) payRow {
	e.t.Helper()
	var r payRow
	if err := e.server.pool.QueryRow(tctx(),
		`SELECT p.id, p.status, p.provider, p.mode, p.provider_order_id, p.amount::float8,
		        (SELECT COUNT(*)::int FROM payment_attempts a WHERE a.payment_id = p.id)
		 FROM payments p JOIN orders o ON o.id = p.order_id
		 WHERE p.tenant_id = $1 AND o.order_number = $2`, tenantID, orderNo).
		Scan(&r.ID, &r.Status, &r.Provider, &r.Mode, &r.GatewayOrder, &r.Amount, &r.Attempts); err != nil {
		e.t.Fatalf("payment for order %d: %v", orderNo, err)
	}
	return r
}

func TestLiveStartOpensAGatewayOrderForTheAmountInOurDatabase(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)

	orderNo, status, body := shop.startOnline(t, "9811122233")
	env.mustStatus(http.StatusOK, status, "start live payment", body)

	row := env.paymentFor(shop.tenantID, orderNo)
	if row.Provider != "razorpay" || row.Mode != "LIVE" || row.GatewayOrder == "" || row.Attempts != 1 || row.Status != "PENDING" {
		t.Fatalf("payment row = %+v", row)
	}
	if got := gw.orders[row.GatewayOrder]; got != int64(row.Amount*100+0.5) || gw.currency[row.GatewayOrder] != "INR" {
		t.Errorf("gateway order amount = %d %s, want the database amount %v INR", got, gw.currency[row.GatewayOrder], row.Amount)
	}
	for _, u := range gw.users {
		if u != shop.keyID {
			t.Errorf("gateway called with key %q, want this business's own %q", u, shop.keyID)
		}
	}

	checkout, _ := body["checkout"].(map[string]any)
	if checkout["gateway"] != "razorpay" || checkout["key_id"] != shop.keyID || checkout["order_id"] != row.GatewayOrder {
		t.Errorf("checkout = %v", checkout)
	}
	if _, has := body["payment_token"]; has {
		t.Error("a live payment must not hand the browser a token that could settle it")
	}
	if body["payment_mode"] != "LIVE" || body["is_test"] != false {
		t.Errorf("payment_mode=%v is_test=%v, want LIVE/false", body["payment_mode"], body["is_test"])
	}
	raw := str(t, body, "__raw")
	for _, secret := range []string{shop.keySecret, shop.webhookSecret} {
		if strings.Contains(raw, secret) {
			t.Errorf("the start response leaked %s", secret)
		}
	}
}

func TestStatusBeforeStartAlreadyShowsTheRightCheckout(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	live := liveShop(t, env)
	test := newShop(t, env)

	liveOrder := live.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	_, st := live.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", number(t, liveOrder, "order_number")), nil)
	if st["payment_mode"] != "LIVE" || st["is_test"] != false {
		t.Errorf("live business, before start: %v/%v, want LIVE/not test", st["payment_mode"], st["is_test"])
	}
	testOrder := test.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	_, st = test.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", number(t, testOrder, "order_number")), nil)
	if st["payment_mode"] != "SANDBOX" || st["is_test"] != true {
		t.Errorf("test business, before start: %v/%v, want SANDBOX/test", st["payment_mode"], st["is_test"])
	}
}

func TestLiveWebhookCapturesOnceAndRejectsForgeries(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	a, b := liveShop(t, env), liveShop(t, env)
	orderNo, status, body := a.startOnline(t, "9811122233")
	env.mustStatus(http.StatusOK, status, "start", body)
	row := env.paymentFor(a.tenantID, orderNo)
	minor := int64(row.Amount*100 + 0.5)
	captured := rzEventBody("payment.captured", row.GatewayOrder, "pay_REAL1", minor, "")

	// Forgeries first: none of them may move the payment.
	status, _ = env.postRazorpay("not-the-secret", "evt_x", captured)
	env.mustStatus(http.StatusUnauthorized, status, "wrong secret", nil)
	status, _ = env.postRazorpay(b.webhookSecret, "evt_x", captured)
	env.mustStatus(http.StatusUnauthorized, status, "another business's secret", nil)
	status, _ = env.rawWebhook("razorpay", captured, "")
	env.mustStatus(http.StatusUnauthorized, status, "no signature", nil)
	if got := env.paymentFor(a.tenantID, orderNo).Status; got != "PENDING" {
		t.Fatalf("payment = %s after forged callbacks, want PENDING", got)
	}

	// The real one.
	status, body = env.postRazorpay(a.webhookSecret, "evt_1", captured)
	env.mustStatus(http.StatusOK, status, "genuine capture", body)
	if body["status"] != "applied" {
		t.Errorf("first delivery = %v, want applied", body["status"])
	}
	if got := env.paymentFor(a.tenantID, orderNo).Status; got != "PAID" {
		t.Fatalf("payment = %s, want PAID", got)
	}
	var gwPayment string
	_ = env.server.pool.QueryRow(tctx(), `SELECT provider_payment_id FROM payments WHERE id = $1`, row.ID).Scan(&gwPayment)
	if gwPayment != "pay_REAL1" {
		t.Errorf("provider_payment_id = %q, want pay_REAL1", gwPayment)
	}

	// The gateway retries deliveries: the same event is a no-op.
	status, body = env.postRazorpay(a.webhookSecret, "evt_1", captured)
	env.mustStatus(http.StatusOK, status, "replayed delivery", body)
	if body["status"] != "duplicate" {
		t.Errorf("replay = %v, want duplicate", body["status"])
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND new_status = 'PAID'`, row.ID); n != 1 {
		t.Errorf("PAID audit rows = %d, want 1", n)
	}
	// A later failure notice cannot undo captured money.
	status, _ = env.postRazorpay(a.webhookSecret, "evt_2", rzEventBody("payment.failed", row.GatewayOrder, "pay_REAL1", minor, "late"))
	env.mustStatus(http.StatusOK, status, "late failure", nil)
	if got := env.paymentFor(a.tenantID, orderNo).Status; got != "PAID" {
		t.Errorf("payment = %s after a late failure, want PAID", got)
	}
	// And the shop's customer sees it.
	_, tracked := a.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	if tracked["payment_status"] != "PAID" || tracked["payable"] != false {
		t.Errorf("status after capture = %v/%v", tracked["payment_status"], tracked["payable"])
	}
}

func TestLiveWebhookNeverCapturesTheWrongAmountOrAnUnknownOrder(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(row.Amount*100 + 0.5)

	status, body := env.postRazorpay(shop.webhookSecret, "evt_short", rzEventBody("payment.captured", row.GatewayOrder, "pay_S", minor-100, ""))
	env.mustStatus(http.StatusOK, status, "short payment", body)
	if body["status"] != "rejected" || env.paymentFor(shop.tenantID, orderNo).Status != "PENDING" {
		t.Errorf("short payment: %v / %s, want rejected / PENDING", body["status"], env.paymentFor(shop.tenantID, orderNo).Status)
	}

	status, body = env.postRazorpay(shop.webhookSecret, "evt_ghost", rzEventBody("payment.captured", "order_NOT_OURS", "pay_G", minor, ""))
	env.mustStatus(http.StatusOK, status, "unknown order", body)
	if body["status"] != "ignored" {
		t.Errorf("unknown order = %v, want ignored", body["status"])
	}

	// Event kinds that change nothing are acknowledged, not applied.
	status, body = env.postRazorpay(shop.webhookSecret, "evt_other", rzEventBody("payment.authorized", row.GatewayOrder, "pay_S", minor, ""))
	env.mustStatus(http.StatusOK, status, "irrelevant event", body)
	if body["status"] != "ignored" {
		t.Errorf("irrelevant event = %v, want ignored", body["status"])
	}
	// A refund made in the gateway's own dashboard is not one we recorded.
	status, body = env.postRazorpay(shop.webhookSecret, "evt_dash_refund", rzEventBody("refund.processed", row.GatewayOrder, "pay_S", minor, ""))
	env.mustStatus(http.StatusOK, status, "refund we did not start", body)
	if body["status"] != "ignored" {
		t.Errorf("a refund we did not start = %v, want ignored", body["status"])
	}
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}
}

func TestLiveRetryInTheSameCheckoutAfterADeclinedCard(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(row.Amount*100 + 0.5)

	status, _ := env.postRazorpay(shop.webhookSecret, "evt_f", rzEventBody("payment.failed", row.GatewayOrder, "pay_F", minor, "Card declined"))
	env.mustStatus(http.StatusOK, status, "declined card", nil)
	_, st := shop.guest(http.MethodGet, fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo), nil)
	if st["payment_status"] != "FAILED" || st["failure_reason"] != "Card declined" {
		t.Fatalf("after a decline: %v / %v", st["payment_status"], st["failure_reason"])
	}
	// The widget lets the customer try another card on the same gateway order.
	status, _ = env.postRazorpay(shop.webhookSecret, "evt_c", rzEventBody("payment.captured", row.GatewayOrder, "pay_OK", minor, ""))
	env.mustStatus(http.StatusOK, status, "second card works", nil)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PAID" {
		t.Errorf("payment = %s, want PAID", got)
	}
}

func TestLateCaptureOfAnEarlierAttemptStillPaysTheOrder(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	first := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(first.Amount*100 + 0.5)

	// The customer abandons the widget and starts again: a second gateway order.
	status, body := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "second attempt", body)
	second := env.paymentFor(shop.tenantID, orderNo)
	if second.GatewayOrder == first.GatewayOrder || second.Attempts != 2 {
		t.Fatalf("second attempt: order %q (first %q), attempts %d", second.GatewayOrder, first.GatewayOrder, second.Attempts)
	}
	if n, _ := gw.counts(); n != 2 {
		t.Errorf("gateway orders created = %d, want 2", n)
	}
	if env.countPayments(shop.tenantID) != 1 {
		t.Error("a retry must reuse the one payment row")
	}

	// The first window turns out to have been paid after all.
	status, body = env.postRazorpay(shop.webhookSecret, "evt_late", rzEventBody("payment.captured", first.GatewayOrder, "pay_LATE", minor, ""))
	env.mustStatus(http.StatusOK, status, "late capture of attempt one", body)
	if body["status"] != "applied" || env.paymentFor(shop.tenantID, orderNo).Status != "PAID" {
		t.Fatalf("late capture: %v / %s, want applied / PAID: the customer's money moved", body["status"], env.paymentFor(shop.tenantID, orderNo).Status)
	}

	// If the second attempt is then also captured, the customer paid twice:
	// the order stays paid once and the double charge is flagged, not hidden.
	status, _ = env.postRazorpay(shop.webhookSecret, "evt_dup", rzEventBody("payment.captured", second.GatewayOrder, "pay_DUP", minor, ""))
	env.mustStatus(http.StatusOK, status, "second capture", nil)
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND event_type = 'DUPLICATE_PAYMENT'`, first.ID); n != 1 {
		t.Errorf("DUPLICATE_PAYMENT audit rows = %d, want 1", n)
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND event_type = 'PAID'`, first.ID); n != 1 {
		t.Errorf("PAID transitions = %d, want exactly 1", n)
	}
}

func TestStatusReconcilesALostWebhook(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)
	minor := int64(row.Amount*100 + 0.5)
	statusURL := fmt.Sprintf("/api/v1/public/orders/%d/pay?phone=9811122233", orderNo)

	// Not paid yet: the poll asks the gateway and the answer is "nothing".
	_, st := shop.guest(http.MethodGet, statusURL, nil)
	if st["payment_status"] != "PENDING" {
		t.Fatalf("before payment: %v", st["payment_status"])
	}
	if _, lists := gw.counts(); lists != 1 {
		t.Errorf("gateway lookups = %d, want 1", lists)
	}
	// Polling again straight away must not hammer the gateway.
	shop.guest(http.MethodGet, statusURL, nil)
	if _, lists := gw.counts(); lists != 1 {
		t.Errorf("an immediate re-poll reached the gateway again (%d lookups)", lists)
	}

	// The customer pays but the webhook never arrives. Clear the throttle the
	// way five idle seconds would, then poll.
	gw.addPayment(row.GatewayOrder, "pay_LOST", "captured", minor)
	env.server.paymentSvc.ResetRefreshThrottle()
	_, st = shop.guest(http.MethodGet, statusURL, nil)
	if st["payment_status"] != "PAID" {
		t.Fatalf("a captured payment with a lost webhook: status = %v, want PAID", st["payment_status"])
	}
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND actor_type = 'provider' AND event_type = 'PAID'`, row.ID); n != 1 {
		t.Errorf("provider PAID audit rows = %d, want 1", n)
	}
	// The webhook then turning up late is a harmless no-op.
	status, body := env.postRazorpay(shop.webhookSecret, "evt_after", rzEventBody("payment.captured", row.GatewayOrder, "pay_LOST", minor, ""))
	env.mustStatus(http.StatusOK, status, "late webhook", body)
	if n := env.count(`SELECT COUNT(*)::int FROM payment_audit_log WHERE payment_id = $1 AND event_type = 'PAID'`, row.ID); n != 1 {
		t.Errorf("PAID transitions = %d after the late webhook, want 1", n)
	}
}

func TestSweeperExpiresOnlyWhatWasNotPaid(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	unpaidNo, _, _ := shop.startOnline(t, "9811122233")
	paidNo, _, _ := shop.startOnline(t, "9811122244")
	unpaid, paid := env.paymentFor(shop.tenantID, unpaidNo), env.paymentFor(shop.tenantID, paidNo)
	gw.addPayment(paid.GatewayOrder, "pay_SWEPT", "captured", int64(paid.Amount*100+0.5))

	// Nothing of ours is due yet. (The sweep is global, so judge our own rows:
	// other tests may have left expired payments of their own behind.)
	if _, err := env.server.paymentSvc.SweepExpired(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	if env.paymentFor(shop.tenantID, unpaidNo).Status != "PENDING" || env.paymentFor(shop.tenantID, paidNo).Status != "PENDING" {
		t.Fatal("a payment inside its window must not be expired")
	}
	if _, err := env.server.pool.Exec(tctx(),
		`UPDATE payments SET expires_at = now() - interval '1 minute' WHERE id = ANY($1)`, []uuid.UUID{unpaid.ID, paid.ID}); err != nil {
		t.Fatal(err)
	}

	// If the gateway cannot be asked, nothing is expired: the customer may have paid.
	gw.mu.Lock()
	gw.listStatus = http.StatusInternalServerError
	gw.mu.Unlock()
	env.server.paymentSvc.ResetRefreshThrottle()
	_, _ = env.server.paymentSvc.SweepExpired(context.Background(), 500)
	if env.paymentFor(shop.tenantID, paidNo).Status != "PENDING" || env.paymentFor(shop.tenantID, unpaidNo).Status != "PENDING" {
		t.Fatal("payments must stay pending while the gateway cannot confirm them")
	}

	gw.mu.Lock()
	gw.listStatus = 0
	gw.mu.Unlock()
	env.server.paymentSvc.ResetRefreshThrottle()
	if _, err := env.server.paymentSvc.SweepExpired(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	if got := env.paymentFor(shop.tenantID, unpaidNo).Status; got != "EXPIRED" {
		t.Errorf("unpaid = %s, want EXPIRED", got)
	}
	if got := env.paymentFor(shop.tenantID, paidNo).Status; got != "PAID" {
		t.Errorf("paid-but-webhook-lost = %s, want PAID (not expired)", got)
	}

	// An expired payment can simply be started again.
	status, body := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", unpaidNo),
		map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "restart after expiry", body)
	if got := env.paymentFor(shop.tenantID, unpaidNo).Status; got != "PENDING" {
		t.Errorf("restarted = %s, want PENDING", got)
	}
}

func TestACustomerCannotSettleALivePaymentFromTheBrowser(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)

	for name, payload := range map[string]map[string]any{
		"no token":   {"outcome": "SUCCESS", "phone": "9811122233"},
		"made-up":    {"outcome": "SUCCESS", "token": "anything", "phone": "9811122233"},
		"gateway id": {"outcome": "SUCCESS", "token": row.GatewayOrder, "phone": "9811122233"},
	} {
		status, _ := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo), payload)
		if status != http.StatusConflict {
			t.Errorf("%s: status %d, want 409", name, status)
		}
	}
	// Nor can staff mark an online payment as received.
	status, _ := shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+row.ID.String()+"/confirm", nil)
	env.mustStatus(http.StatusConflict, status, "staff confirm of a live online payment", nil)
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}
}

func TestGatewayDownAtCheckoutKeepsTheOrderAndOffersCash(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	gw := newFakeRazorpay(t)
	shop := liveShop(t, env)
	gw.mu.Lock()
	gw.createStatus = http.StatusInternalServerError
	gw.mu.Unlock()

	orderNo, status, body := shop.startOnline(t, "9811122233")
	env.mustStatus(http.StatusBadGateway, status, "gateway down", body)
	if errBody, _ := body["error"].(map[string]any); errBody["code"] != "gateway_unavailable" {
		t.Errorf("error = %v, want gateway_unavailable", body["error"])
	}
	row := env.paymentFor(shop.tenantID, orderNo)
	if row.Status != "PENDING" || row.GatewayOrder != "" || row.Attempts != 0 {
		t.Errorf("a failed gateway call must leave the payment untouched, got %+v", row)
	}

	// Cash still works for that very order, and touches no gateway.
	creates, _ := gw.counts()
	status, body = shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo),
		map[string]any{"method": "CASH", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "switch to cash", body)
	if after, _ := gw.counts(); after != creates+0 && after != creates {
		t.Errorf("a cash payment reached the gateway (%d -> %d)", creates, after)
	}
	if _, has := body["payment_token"]; has {
		t.Error("cash must not hand the browser a token")
	}
	row = env.paymentFor(shop.tenantID, orderNo)
	if row.Status != "PENDING" || row.GatewayOrder != "" {
		t.Errorf("cash payment row = %+v", row)
	}
	status, _ = shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay/confirm", orderNo),
		map[string]any{"outcome": "SUCCESS", "phone": "9811122233", "token": "x"})
	env.mustStatus(http.StatusConflict, status, "customer tries to settle cash", nil)
	status, _ = shop.admin(http.MethodPost, "/api/v1/tenant/payments/"+row.ID.String()+"/confirm", nil)
	env.mustStatus(http.StatusOK, status, "staff confirm cash", nil)
}

func TestLiveWebhookStillWorksWithTheFlagOff(t *testing.T) {
	// A live payment in flight must be able to finish even if PAYMENTS_V2 is
	// turned off while the customer is paying.
	t.Setenv("PAYMENTS_V2", "")
	env := newTestEnv(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")
	row := env.paymentFor(shop.tenantID, orderNo)
	if _, err := env.server.pool.Exec(tctx(),
		`UPDATE payments SET provider = 'razorpay', provider_order_id = 'order_INFLIGHT', mode = 'LIVE' WHERE id = $1`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.server.pool.Exec(tctx(),
		`INSERT INTO payment_attempts (tenant_id, payment_id, provider, provider_order_id) VALUES ($1, $2, 'razorpay', 'order_INFLIGHT')`, shop.tenantID, row.ID); err != nil {
		t.Fatal(err)
	}
	status, body := env.postRazorpay(shop.webhookSecret, "evt_flagoff",
		rzEventBody("payment.captured", "order_INFLIGHT", "pay_FO", int64(row.Amount*100+0.5), ""))
	env.mustStatus(http.StatusOK, status, "webhook with the flag off", body)
	if env.paymentFor(shop.tenantID, orderNo).Status != "PAID" {
		t.Errorf("payment = %s, want PAID", env.paymentFor(shop.tenantID, orderNo).Status)
	}
}

func TestBrokenCredentialsNeverFallBackToSandboxOrDropMoney(t *testing.T) {
	env := newV2Env(t)
	keepPaymentModesClean(t, env)
	newFakeRazorpay(t)
	shop := liveShop(t, env)
	orderNo, _, _ := shop.startOnline(t, "9811122233")
	row := env.paymentFor(shop.tenantID, orderNo)

	// The business's gateway configuration gets disabled mid-flight.
	if _, err := env.server.pool.Exec(tctx(),
		`UPDATE tenant_configurations SET enabled = false WHERE tenant_id = $1 AND service_type = 'PAYMENTS'`, shop.tenantID); err != nil {
		t.Fatal(err)
	}
	// New checkouts are refused outright: never quietly paid in the sandbox.
	order := shop.order(t, "Asha Rao", "9876543210", "ONLINE")
	status, body := shop.guest(http.MethodPost, fmt.Sprintf("/api/v1/public/orders/%d/pay", number(t, order, "order_number")),
		map[string]any{"method": "ONLINE", "phone": "9876543210"})
	env.mustStatus(http.StatusServiceUnavailable, status, "checkout with broken credentials", body)

	// The in-flight payment's callback cannot be verified right now: a 5xx so
	// the gateway retries later, not a 200 that would make it give up.
	status, _ = env.postRazorpay(shop.webhookSecret, "evt_retry",
		rzEventBody("payment.captured", row.GatewayOrder, "pay_R", int64(row.Amount*100+0.5), ""))
	if status < 500 {
		t.Errorf("callback while credentials are down: status %d, want 5xx so it is retried", status)
	}
	if got := env.paymentFor(shop.tenantID, orderNo).Status; got != "PENDING" {
		t.Errorf("payment = %s, want PENDING", got)
	}
}
