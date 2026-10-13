package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rzSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func capturedBody(orderID, payID string, amount int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{"payment": map[string]any{"entity": map[string]any{
			"id": payID, "order_id": orderID, "amount": amount, "status": "captured"}}},
	})
	return b
}

func TestRazorpayInitiateSendsOurAmountAndOnlyPublicValuesBack(t *testing.T) {
	var gotBody map[string]any
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/orders" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"order_ABC123","amount":12500}`))
	}))
	defer srv.Close()

	gw := NewRazorpay(srv.URL, "rzp_test_key", "THE_SECRET", "THE_WEBHOOK_SECRET")
	intent, err := gw.Initiate(context.Background(), IntentInput{PaymentID: "11111111-2222-3333-4444-555555555555", AmountMinor: 12500, Currency: "INR"})
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if user != "rzp_test_key" || pass != "THE_SECRET" {
		t.Errorf("basic auth = %q/%q", user, pass)
	}
	if gotBody["amount"] != float64(12500) || gotBody["currency"] != "INR" {
		t.Errorf("gateway order = %v, want 12500 INR", gotBody)
	}
	if intent.ProviderOrderID != "order_ABC123" || intent.ClientToken != "" {
		t.Errorf("intent = %+v: a live gateway issues no browser token", intent)
	}
	raw, _ := json.Marshal(intent.Checkout)
	for _, secret := range []string{"THE_SECRET", "THE_WEBHOOK_SECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("checkout payload leaks %s: %s", secret, raw)
		}
	}
	if intent.Checkout["key_id"] != "rzp_test_key" || intent.Checkout["order_id"] != "order_ABC123" || intent.Checkout["amount_minor"] != int64(12500) {
		t.Errorf("checkout = %v", intent.Checkout)
	}
	if intent.ExpiresAt.IsZero() {
		t.Error("a live intent must expire")
	}
}

func TestRazorpayGatewayFailuresAreErrorsNotSuccess(t *testing.T) {
	status := http.StatusInternalServerError
	body := `{"error":{"description":"boom SECRET_ECHO"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	gw := NewRazorpay(srv.URL, "rzp_test_key", "s", "w")

	for _, st := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusBadRequest} {
		status = st
		_, err := gw.Initiate(context.Background(), IntentInput{AmountMinor: 100, Currency: "INR"})
		if !errors.Is(err, ErrGateway) {
			t.Errorf("status %d: err = %v, want ErrGateway", st, err)
		} else if strings.Contains(err.Error(), "SECRET_ECHO") {
			t.Errorf("status %d: the gateway's response body leaked into the error", st)
		}
	}
	status = http.StatusOK
	body = `{}`
	if _, err := gw.Initiate(context.Background(), IntentInput{AmountMinor: 100, Currency: "INR"}); err == nil {
		t.Error("a response with no order id must not count as an intent")
	}
	body = `not json`
	if _, err := gw.Initiate(context.Background(), IntentInput{AmountMinor: 100, Currency: "INR"}); !errors.Is(err, ErrGateway) {
		t.Errorf("garbled response: err = %v", err)
	}
	srv.Close()
	if _, err := gw.Initiate(context.Background(), IntentInput{AmountMinor: 100, Currency: "INR"}); !errors.Is(err, ErrGateway) {
		t.Errorf("unreachable gateway: err = %v", err)
	}
}

func TestRazorpayWebhookSignatureRules(t *testing.T) {
	gw := NewRazorpay("http://unused", "k", "s", "whsec")
	body := capturedBody("order_1", "pay_1", 5000)
	hdr := func(sig string) http.Header {
		return http.Header{"X-Razorpay-Signature": {sig}, "X-Razorpay-Event-Id": {"evt_77"}}
	}

	ev, err := gw.VerifyWebhook(hdr(rzSign("whsec", body)), body)
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if ev.Type != EventPaid || ev.ProviderOrderID != "order_1" || ev.ProviderPaymentID != "pay_1" || ev.AmountMinor != 5000 || ev.ID != "evt_77" {
		t.Errorf("event = %+v", ev)
	}

	cases := map[string]struct {
		h    http.Header
		body []byte
		gw   *Razorpay
	}{
		"missing header":     {http.Header{}, body, gw},
		"wrong secret":       {hdr(rzSign("other", body)), body, gw},
		"tampered amount":    {hdr(rzSign("whsec", body)), capturedBody("order_1", "pay_1", 1), gw},
		"signature of empty": {hdr(rzSign("whsec", nil)), body, gw},
		"no webhook secret":  {hdr(rzSign("", body)), body, NewRazorpay("http://unused", "k", "s", "")},
	}
	for name, c := range cases {
		if _, err := c.gw.VerifyWebhook(c.h, c.body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestRazorpayEventMapping(t *testing.T) {
	gw := NewRazorpay("http://unused", "k", "s", "whsec")
	mk := func(event, body string) Event {
		raw := []byte(body)
		ev, err := gw.VerifyWebhook(http.Header{"X-Razorpay-Signature": {rzSign("whsec", raw)}}, raw)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		return ev
	}
	failed := mk("failed", `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_9","order_id":"order_9","amount":100,"error_description":"Card declined"}}}}`)
	if failed.Type != EventFailed || failed.Reason != "Card declined" {
		t.Errorf("failed = %+v", failed)
	}
	orderPaid := mk("order.paid", `{"event":"order.paid","payload":{"order":{"entity":{"id":"order_9"}},"payment":{"entity":{"id":"pay_9","order_id":"order_9","amount":100}}}}`)
	if orderPaid.Type != EventPaid || orderPaid.ProviderOrderID != "order_9" {
		t.Errorf("order.paid = %+v", orderPaid)
	}
	other := mk("authorized", `{"event":"payment.authorized","payload":{"payment":{"entity":{"id":"pay_9","order_id":"order_9"}}}}`)
	if other.Type != "" {
		t.Errorf("an event that changes nothing must have no type, got %q", other.Type)
	}
	// With no event-id header the id is derived from the body, so a retry of
	// the same delivery still dedupes.
	a, b := mk("a", `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"p","order_id":"o"}}}}`), mk("b", `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"p","order_id":"o"}}}}`)
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("derived ids = %q / %q, want equal and non-empty", a.ID, b.ID)
	}
}

func TestRazorpayCheckStatusReportsOnlyCapturedPayments(t *testing.T) {
	items := `[{"id":"pay_f","status":"failed","amount":5000}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orders/order_1/payments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":` + items + `}`))
	}))
	defer srv.Close()
	gw := NewRazorpay(srv.URL, "k", "s", "w")

	if ev, err := gw.CheckStatus(context.Background(), "order_1"); err != nil || ev != nil {
		t.Fatalf("only a failed attempt: ev=%v err=%v, want nothing (the customer may retry in the same checkout)", ev, err)
	}
	items = `[{"id":"pay_f","status":"failed","amount":5000},{"id":"pay_ok","status":"captured","amount":5000}]`
	ev, err := gw.CheckStatus(context.Background(), "order_1")
	if err != nil || ev == nil {
		t.Fatalf("captured: ev=%v err=%v", ev, err)
	}
	if ev.Type != EventPaid || ev.ProviderPaymentID != "pay_ok" || ev.AmountMinor != 5000 || ev.ProviderOrderID != "order_1" || ev.ID != "fetch_pay_ok_captured" {
		t.Errorf("event = %+v", ev)
	}
	items = `[{"id":"pay_a","status":"authorized","amount":5000}]`
	if ev, _ := gw.CheckStatus(context.Background(), "order_1"); ev != nil {
		t.Error("an authorized-only payment has not been captured: it must not count as paid")
	}
}

func TestRazorpayOrderRefComesOnlyFromTheBody(t *testing.T) {
	if got := razorpayOrderRef(capturedBody("order_X", "pay_X", 1)); got != "order_X" {
		t.Errorf("ref = %q", got)
	}
	for _, junk := range []string{"", "not json", `{"event":"payment.captured"}`} {
		if got := razorpayOrderRef([]byte(junk)); got != "" {
			t.Errorf("ref(%q) = %q, want empty", junk, got)
		}
	}
}

func TestRazorpayRefundIsIdempotentAndMapsEveryAnswer(t *testing.T) {
	var gotKey, gotPath string
	var gotBody map[string]any
	reply, status := `{"id":"rfnd_1","status":"processed"}`, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("X-Refund-Idempotency"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	gw := NewRazorpay(srv.URL, "k", "s", "w")
	in := RefundInput{ProviderPaymentID: "pay_42", AmountMinor: 12500, Reason: "Order cancelled", Key: "11111111-2222-3333-4444-555555555555"}

	out, err := gw.Refund(context.Background(), in)
	if err != nil || out.ID != "rfnd_1" || out.Status != RefundDone {
		t.Fatalf("processed: %+v %v", out, err)
	}
	if gotPath != "/v1/payments/pay_42/refund" || gotKey != in.Key || gotBody["amount"] != float64(12500) {
		t.Errorf("request: path=%s key=%s body=%v", gotPath, gotKey, gotBody)
	}

	reply = `{"id":"rfnd_2","status":"pending"}`
	if out, err := gw.Refund(context.Background(), in); err != nil || out.Status != RefundPending {
		t.Errorf("pending: %+v %v, want accepted-and-settling", out, err)
	}
	reply = `{"id":"rfnd_3","status":"failed"}`
	if _, err := gw.Refund(context.Background(), in); !IsPermanentGatewayError(err) {
		t.Errorf("a failed refund is a definite no, got %v", err)
	}
	reply = `{}`
	if _, err := gw.Refund(context.Background(), in); err == nil {
		t.Error("an answer with no refund id is not a refund")
	}
}

func TestGatewayErrorsAreSplitIntoRetryableAndFinal(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest:          true, // refused: retrying cannot help
		http.StatusUnprocessableEntity: true,
		http.StatusUnauthorized:        true,
		http.StatusTooManyRequests:     false, // slow down, then retry
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
		http.StatusServiceUnavailable:  false,
	}
	for status, permanent := range cases {
		if got := IsPermanentGatewayError(&GatewayError{Status: status}); got != permanent {
			t.Errorf("status %d permanent = %v, want %v", status, got, permanent)
		}
	}
	if IsPermanentGatewayError(errors.New("connection reset")) {
		t.Error("a network error is never a final answer")
	}
	if IsPermanentGatewayError(nil) {
		t.Error("nil is not a gateway error")
	}
}

func TestRazorpayRefundWebhookEvents(t *testing.T) {
	gw := NewRazorpay("http://unused", "k", "s", "whsec")
	verify := func(body string) Event {
		raw := []byte(body)
		ev, err := gw.VerifyWebhook(http.Header{"X-Razorpay-Signature": {rzSign("whsec", raw)}}, raw)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		return ev
	}
	done := verify(`{"event":"refund.processed","payload":{"refund":{"entity":{"id":"rfnd_9","payment_id":"pay_9","amount":5000}}}}`)
	if done.Type != EventRefunded || done.RefundID != "rfnd_9" || done.ProviderPaymentID != "pay_9" || done.ProviderOrderID != "" {
		t.Errorf("refund.processed = %+v", done)
	}
	bad := verify(`{"event":"refund.failed","payload":{"refund":{"entity":{"id":"rfnd_9","payment_id":"pay_9","amount":5000}}}}`)
	if bad.Type != EventRefundFailed {
		t.Errorf("refund.failed = %+v", bad)
	}
	created := verify(`{"event":"refund.created","payload":{"refund":{"entity":{"id":"rfnd_9","payment_id":"pay_9"}}}}`)
	if created.Type != "" {
		t.Errorf("refund.created changes nothing, got %q", created.Type)
	}
	// Refund events name the payment, not an order.
	body := []byte(`{"event":"refund.processed","payload":{"refund":{"entity":{"id":"r","payment_id":"pay_X"}}}}`)
	if razorpayOrderRef(body) != "" || razorpayPaymentRef(body) != "pay_X" {
		t.Errorf("refs = %q / %q", razorpayOrderRef(body), razorpayPaymentRef(body))
	}
}

func TestSandboxRefundIsDeterministicFromTheKey(t *testing.T) {
	s := NewSandbox("x")
	a, _ := s.Refund(context.Background(), RefundInput{Key: "abc"})
	b, _ := s.Refund(context.Background(), RefundInput{Key: "abc"})
	if a != b || a.Status != RefundDone || a.ID != "sbx_rf_abc" {
		t.Errorf("sandbox refund = %+v / %+v", a, b)
	}
}
