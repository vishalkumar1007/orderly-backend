package httpserver

import (
	"net/http"
	"testing"
)

// "Your profile" in Settings lets a person fix their own name; nothing else
// about their identity is theirs to change.
func TestYourProfileNameUpdate(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Profile Person", "profile-person-"+randSuffix())
	owner := env.tenantAdmin(tenantID)

	// Anything beyond the name is refused outright, not silently applied.
	status, _ := env.doAsTenant(http.MethodPatch, "/api/v1/auth/me", owner, map[string]any{
		"name": "Asha Rao", "role": "SUPER_ADMIN",
	}, tenantID)
	if status != http.StatusBadRequest {
		t.Errorf("profile edit carrying a role: status = %d, want 400", status)
	}

	status, body := env.doAsTenant(http.MethodPatch, "/api/v1/auth/me", owner, map[string]any{
		"name": "  Asha Rao  ",
	}, tenantID)
	env.mustStatus(http.StatusOK, status, "update own name", body)
	if body["name"] != "Asha Rao" {
		t.Errorf("name = %v, want trimmed Asha Rao", body["name"])
	}
	if body["role"] != "TENANT_ADMIN" {
		t.Errorf("role changed to %v: a profile edit must never touch the role", body["role"])
	}

	status, body = env.doAsTenant(http.MethodGet, "/api/v1/auth/me", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "read identity", body)
	if u, _ := body["user"].(map[string]any); u["name"] != "Asha Rao" {
		t.Errorf("identity name = %v, want Asha Rao", u["name"])
	}

	status, _ = env.doAsTenant(http.MethodPatch, "/api/v1/auth/me", owner, map[string]any{"name": "A"}, tenantID)
	if status != http.StatusBadRequest {
		t.Errorf("one-letter name: status = %d, want 400", status)
	}
}
