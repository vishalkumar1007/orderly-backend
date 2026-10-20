package httpserver

import (
	"net/http"
	"testing"
)

// Customers can only sign in with the methods the owner switched on. Tests
// run with the sign-in test fallback, so both channels count as set up here;
// the "not set up" side is covered in internal/signin.
func TestCustomerSignInFollowsTheOwnersMethods(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	methods := func() []any {
		status, body := shop.guest(http.MethodGet, "/api/v1/public/store", nil)
		env.mustStatus(http.StatusOK, status, "store config", body)
		ordering, _ := body["ordering"].(map[string]any)
		list, _ := ordering["customer_login_methods"].([]any)
		return list
	}
	if got := methods(); len(got) != 2 {
		t.Fatalf("a new shop offers %v, want phone and email", got)
	}

	// The admin screen shows whether each channel is set up.
	status, admin := shop.admin(http.MethodGet, "/api/v1/tenant/storefront", nil)
	env.mustStatus(http.StatusOK, status, "admin config", admin)
	if _, ok := admin["sign_in_channels"].(map[string]any); !ok {
		t.Errorf("admin config has no sign_in_channels: %v", admin["__raw"])
	}

	// Email only.
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{"customer_login_methods": []string{"EMAIL"}})
	env.mustStatus(http.StatusOK, status, "choose email only", body)
	if got := methods(); len(got) != 1 || got[0] != "EMAIL" {
		t.Fatalf("after choosing email only the shop offers %v", got)
	}

	status, body = shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"phone": "9876543210"})
	env.mustStatus(http.StatusForbidden, status, "phone code when only email is offered", body)

	status, body = shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"email": "not-an-email"})
	env.mustStatus(http.StatusBadRequest, status, "a malformed email", body)

	status, sent := shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"email": "Asha@Example.com"})
	env.mustStatus(http.StatusOK, status, "email code", sent)
	if sent["email"] != "a***@example.com" {
		t.Errorf("email is not masked as expected: %v", sent["email"])
	}
	code := env.otpFromResponse(t, sent)

	status, verified := shop.guest(http.MethodPost, "/api/v1/auth/customer/verify-otp", map[string]any{"email": "asha@example.com", "code": code})
	env.mustStatus(http.StatusOK, status, "verify email code", verified)
	customer, _ := verified["customer"].(map[string]any)
	if customer["email"] != "asha@example.com" {
		t.Errorf("signed-in customer email = %v", customer["email"])
	}
	tokens, _ := verified["tokens"].(map[string]any)
	token, _ := tokens["access_token"].(string)
	status, body = shop.guestAuth(http.MethodGet, "/api/v1/customer/profile", token, nil)
	env.mustStatus(http.StatusOK, status, "email customer profile", body)

	// Nothing switched on: sign-in is gone, and Required cannot lock guests out.
	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/storefront", map[string]any{
		"customer_login_methods": []string{}, "customer_login_mode": "required",
	})
	env.mustStatus(http.StatusOK, status, "switch every method off", body)
	if got := methods(); len(got) != 0 {
		t.Fatalf("with no methods the shop still offers %v", got)
	}
	status, body = shop.guest(http.MethodPost, "/api/v1/auth/customer/send-otp", map[string]any{"email": "asha@example.com"})
	env.mustStatus(http.StatusForbidden, status, "sign-in with every method off", body)
	order := shop.order(t, "Guest", "9876500000", "CASH")
	if order["order_number"] == nil {
		t.Errorf("a required-sign-in shop with no way to sign in refused a guest order: %v", order["__raw"])
	}
}
