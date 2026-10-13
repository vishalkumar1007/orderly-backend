package httpserver

import (
	"fmt"
	"net/http"
	"testing"
)

// Characterisation of the public payment contract. The payments framework is
// being refactored (PAYMENTS-PLAN.md P-0..P-A); these keys are what the
// storefront reads today, so they may be added to but never removed or renamed.

func requireKeys(t *testing.T, where string, body map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := body[k]; !ok {
			t.Errorf("%s: response lost key %q (body %v)", where, k, body["__raw"])
		}
	}
}

func TestPaymentResponseContractIsStable(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	orderNo := number(t, order, "order_number")
	base := fmt.Sprintf("/api/v1/public/orders/%d/pay", orderNo)

	status, started := shop.guest(http.MethodPost, base, map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "start", started)
	requireKeys(t, "start", started, "order_number", "order_status", "order_status_label", "method",
		"payment_status", "amount", "currency", "store_name", "payment_methods", "can_pay_online",
		"payable", "created_at", "payment_token")
	if started["payment_status"] != "PENDING" || started["payable"] != true {
		t.Errorf("start: want PENDING/payable, got %v/%v", started["payment_status"], started["payable"])
	}
	token := str(t, started, "payment_token")

	// The token is only echoed while pending.
	status, polled := shop.guest(http.MethodGet, base+"?phone=9811122233", nil)
	env.mustStatus(http.StatusOK, status, "status while pending", polled)
	if polled["payment_token"] != token {
		t.Errorf("pending status should echo the intent token")
	}

	status, done := shop.guest(http.MethodPost, base+"/confirm",
		map[string]any{"outcome": "SUCCESS", "token": token, "phone": "9811122233"})
	env.mustStatus(http.StatusOK, status, "confirm", done)
	requireKeys(t, "confirm", done, "order_number", "order_status", "payment_status", "amount",
		"outcome", "message", "can_retry")
	if done["outcome"] != "SUCCESS" || done["can_retry"] != false {
		t.Errorf("confirm: outcome=%v can_retry=%v", done["outcome"], done["can_retry"])
	}

	status, after := shop.guest(http.MethodGet, base+"?phone=9811122233", nil)
	env.mustStatus(http.StatusOK, status, "status after paid", after)
	if _, leaked := after["payment_token"]; leaked {
		t.Error("a captured payment must not echo the intent token")
	}
	if after["payable"] != false {
		t.Errorf("paid order should not be payable again, got %v", after["payable"])
	}

	// Starting again after capture is refused, not duplicated.
	status, _ = shop.guest(http.MethodPost, base, map[string]any{"method": "ONLINE", "phone": "9811122233"})
	env.mustStatus(http.StatusConflict, status, "restart after paid", nil)
	if got := env.countPayments(shop.tenantID); got != 1 {
		t.Errorf("payment rows = %d, want 1", got)
	}
}

func TestPaymentRoutesNeedOwnershipProof(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	order := shop.order(t, "Ravi Kumar", "9811122233", "ONLINE")
	base := fmt.Sprintf("/api/v1/public/orders/%d/pay", number(t, order, "order_number"))

	status, _ := shop.guest(http.MethodPost, base, map[string]any{"method": "ONLINE"})
	env.mustStatus(http.StatusBadRequest, status, "start without phone", nil)
	status, _ = shop.guest(http.MethodPost, base, map[string]any{"method": "ONLINE", "phone": "9000000000"})
	env.mustStatus(http.StatusNotFound, status, "start with someone else's phone", nil)
	status, _ = shop.guest(http.MethodGet, base+"?phone=9000000000", nil)
	env.mustStatus(http.StatusNotFound, status, "status with someone else's phone", nil)
}
