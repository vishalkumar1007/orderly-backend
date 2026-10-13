package configsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaymentsConfigValidation(t *testing.T) {
	good := PaymentsConfig{Provider: ProviderRazorpay, KeyID: "rzp_test_abc123"}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if good.KeyKind() != "test" {
		t.Errorf("KeyKind = %q, want test", good.KeyKind())
	}
	if (PaymentsConfig{KeyID: "rzp_live_abc123"}).KeyKind() != "live" {
		t.Error("live key not recognised")
	}
	for name, c := range map[string]PaymentsConfig{
		"empty":        {},
		"wrong prefix": {KeyID: "sk_live_abc123"},
		"prefix only":  {KeyID: "rzp_test_"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
}

func TestPaymentsSecretsAreSealedFields(t *testing.T) {
	for _, k := range []string{"key_secret", "webhook_secret"} {
		if !IsSecretField(ServicePayments, k) {
			t.Errorf("%s must be a secret field", k)
		}
	}
	if IsSecretField(ServicePayments, "key_id") {
		t.Error("key_id is public by design")
	}
}

func TestCheckRazorpay(t *testing.T) {
	var gotUser, gotPass string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		if r.Method != http.MethodGet {
			t.Errorf("credential check must be read-only, got %s", r.Method)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()

	if err := CheckRazorpay(context.Background(), srv.URL, "rzp_test_k", "s3cret"); err != nil {
		t.Fatalf("ok case: %v", err)
	}
	if gotUser != "rzp_test_k" || gotPass != "s3cret" {
		t.Errorf("basic auth = %q/%q", gotUser, gotPass)
	}

	status = http.StatusUnauthorized
	if err := CheckRazorpay(context.Background(), srv.URL, "rzp_test_k", "bad"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("401 should read as rejected credentials, got %v", err)
	}
	status = http.StatusInternalServerError
	if err := CheckRazorpay(context.Background(), srv.URL, "rzp_test_k", "s3cret"); err == nil {
		t.Error("a 500 must not count as verified")
	}
	if err := CheckRazorpay(context.Background(), srv.URL, "", ""); err == nil {
		t.Error("missing credentials must fail before any request")
	}
	srv.Close()
	if err := CheckRazorpay(context.Background(), srv.URL, "rzp_test_k", "s3cret"); err == nil {
		t.Error("an unreachable gateway must not count as verified")
	}
}
