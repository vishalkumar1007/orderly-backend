package httpserver

import (
	"net/http"
	"testing"
)

// The business profile under Settings writes platform facts (tenants) through
// the storefront document, and the console rail reads the logo choice from
// /store-link. These pin the round trip and the two refusals that keep a
// profile link from becoming a javascript: anchor.
func TestBusinessProfileRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Profile Shop", "profile-shop-"+randSuffix())
	owner := env.tenantAdmin(tenantID)

	status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/storefront", owner, map[string]any{
		"logo_url":             "https://cdn.example.com/logo.png",
		"email":                "hello@profile.example",
		"legal_name":           "Profile Shop Private Limited",
		"website":              "https://profile.example",
		"tax_id":               "29ABCDE1234F1Z5",
		"currency":             "inr",
		"timezone":             "Asia/Kolkata",
		"social_links":         map[string]string{"Instagram": "https://instagram.com/profileshop", "x": ""},
		"show_logo_in_console": true,
	}, tenantID)
	env.mustStatus(http.StatusOK, status, "save business profile", body)

	status, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/storefront", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "read business profile", body)
	store, _ := body["store"].(map[string]any)
	want := map[string]any{
		"logo_url":             "https://cdn.example.com/logo.png",
		"email":                "hello@profile.example",
		"legal_name":           "Profile Shop Private Limited",
		"website":              "https://profile.example",
		"tax_id":               "29ABCDE1234F1Z5",
		"currency":             "INR",
		"timezone":             "Asia/Kolkata",
		"show_logo_in_console": true,
	}
	for k, v := range want {
		if store[k] != v {
			t.Errorf("store.%s = %v, want %v", k, store[k], v)
		}
	}
	links, _ := store["social_links"].(map[string]any)
	if links["instagram"] != "https://instagram.com/profileshop" || len(links) != 1 {
		t.Errorf("social_links = %v, want only the lower-cased instagram entry", links)
	}

	// The rail reads the logo and the choice from the lighter store-link call.
	status, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/store-link", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "store link", body)
	if body["logo_url"] != "https://cdn.example.com/logo.png" || body["show_logo_in_console"] != true {
		t.Errorf("store-link does not carry the rail logo: %v", body)
	}

	for label, payload := range map[string]map[string]any{
		"javascript website": {"website": "javascript:alert(1)"},
		"relative social":    {"social_links": map[string]string{"facebook": "/profileshop"}},
		"bad timezone":       {"timezone": "Mars/Olympus"},
		"bad currency":       {"currency": "RUPEES"},
	} {
		status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/storefront", owner, payload, tenantID)
		if status < 400 || status >= 500 {
			t.Errorf("%s: status = %d, want a 4xx refusal (body: %v)", label, status, body)
		}
	}
}
