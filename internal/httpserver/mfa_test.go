package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/pkg/identity"
)

/*
The MFA pipeline end to end: enroll through the real self-service endpoints,
then prove a login is actually gated by it — rejected without a code,
completed with one, and a recovery code works exactly once.

Uses a disposable tenant admin with a known password (mintToken's fixture
users have no real password hash, so login itself can't be exercised against
them) on a fresh test tenant, never the real accounts in the database. MFA
access is per-tenant (tenants.mfa_allowed), granted here the same way a super
admin would from that business's own Configuration tab — a PATCH on that one
tenant, not a platform-wide switch.
*/

func TestMFALoginFlowEndToEnd(t *testing.T) {
	env := newTestEnv(t)

	slug := "mfa-shop-" + randSuffix()
	tenantID := env.createTenant("MFA Test Shop", slug)
	tenantHost := slug + "." + env.cfg.BaseDomain

	const password = "correct horse battery staple"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	email := "mfa-admin-" + randSuffix() + "@test.local"
	if _, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status)
		 VALUES ($1, 'MFA Admin', $2, $3, 'TENANT_ADMIN', 'ACTIVE')`,
		tenantID, email, string(hash)); err != nil {
		t.Fatalf("create tenant admin: %v", err)
	}
	// Cascades via removeTestTenants' `DELETE FROM users WHERE tenant_id = $1`.

	// Grant this one business the right to use MFA — off by default.
	status, body := env.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantID.String(), env.superAdmin(), map[string]any{
		"mfa_allowed": true,
	})
	env.mustStatus(http.StatusOK, status, "allow MFA for this business", body)

	// Permission alone isn't enough — the business's own policy must also
	// move off DISABLED before anyone can enroll.
	status, body = env.do(http.MethodPut, "/api/v1/tenant/mfa-policy", env.tenantAdmin(tenantID), map[string]any{
		"mode": "OPTIONAL", "allowed_methods": []string{"TOTP"}, "enforce_scope": "ALL_ADMINS", "grace_period_days": 7,
	})
	env.mustStatus(http.StatusOK, status, "set tenant mfa policy to optional", body)

	// Baseline: password-only login still works before enrollment.
	status, body = env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "login before MFA enrollment", body)
	tokens, _ := body["tokens"].(map[string]any)
	accessToken, _ := tokens["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("no access token in pre-MFA login response: %v", body)
	}

	// Enroll.
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/setup", accessToken, nil)
	env.mustStatus(http.StatusOK, status, "mfa setup", body)
	secret := str(t, body, "secret")
	setupToken := str(t, body, "setup_token")
	if secret == "" || setupToken == "" {
		t.Fatalf("setup response missing secret/setup_token: %v", body)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/confirm", accessToken, map[string]any{
		"setup_token": setupToken, "code": code,
	})
	env.mustStatus(http.StatusOK, status, "mfa confirm", body)
	recoveryCodesRaw, _ := body["recovery_codes"].([]any)
	if len(recoveryCodesRaw) == 0 {
		t.Fatalf("confirm response had no recovery codes: %v", body)
	}
	recoveryCode, _ := recoveryCodesRaw[0].(string)

	// Password alone is no longer enough.
	status, body = env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "login after MFA enrollment", body)
	if required, _ := body["mfa_required"].(bool); !required {
		t.Fatalf("expected mfa_required: true once MFA is enabled, got: %v", body)
	}
	challengeToken := str(t, body, "challenge_token")
	if challengeToken == "" {
		t.Fatalf("no challenge_token in mfa_required response: %v", body)
	}
	if _, hasTokens := body["tokens"]; hasTokens {
		t.Error("a real token pair must not be issued before the second factor is verified")
	}

	// A wrong code is rejected, and does not consume anything.
	status, body = env.do(http.MethodPost, "/api/v1/auth/mfa/verify", "", map[string]any{
		"challenge_token": challengeToken, "code": "000000",
	})
	if status == http.StatusOK {
		t.Fatalf("a wrong TOTP code must not complete login: %v", body)
	}

	// The right code completes it.
	code, err = totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	status, body = env.do(http.MethodPost, "/api/v1/auth/mfa/verify", "", map[string]any{
		"challenge_token": challengeToken, "code": code,
	})
	env.mustStatus(http.StatusOK, status, "mfa verify with correct code", body)
	if tokens, _ := body["tokens"].(map[string]any); tokens["access_token"] == "" || tokens["access_token"] == nil {
		t.Fatalf("mfa verify did not return a real token pair: %v", body)
	}

	// A fresh login challenge, completed with a recovery code instead —
	// and the same code must not work a second time.
	status, body = env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "second login after MFA enrollment", body)
	challengeToken = str(t, body, "challenge_token")

	status, body = env.do(http.MethodPost, "/api/v1/auth/mfa/verify", "", map[string]any{
		"challenge_token": challengeToken, "recovery_code": recoveryCode,
	})
	env.mustStatus(http.StatusOK, status, "mfa verify with recovery code", body)

	status, body = env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "third login after MFA enrollment", body)
	challengeToken = str(t, body, "challenge_token")

	status, _ = env.do(http.MethodPost, "/api/v1/auth/mfa/verify", "", map[string]any{
		"challenge_token": challengeToken, "recovery_code": recoveryCode,
	})
	if status == http.StatusOK {
		t.Error("a recovery code must not be usable twice")
	}
}

// TestMFAAccessIsPerTenantNotGlobal is the point of the per-tenant model:
// granting one business the right to use MFA must not leak to another.
func TestMFAAccessIsPerTenantNotGlobal(t *testing.T) {
	env := newTestEnv(t)

	blocked := env.createTenant("MFA Blocked Shop", "mfa-blocked-"+randSuffix())
	allowed := env.createTenant("MFA Allowed Shop", "mfa-allowed-"+randSuffix())

	// New by default: neither tenant may enroll until granted.
	status, body := env.do(http.MethodPost, "/api/v1/auth/me/mfa/setup", env.tenantAdmin(blocked), nil)
	if status == http.StatusOK {
		t.Fatalf("expected a fresh tenant to be blocked by default, got 200: %v", body)
	}

	status, body = env.do(http.MethodPatch, "/api/v1/admin/tenants/"+allowed.String(), env.superAdmin(), map[string]any{
		"mfa_allowed": true,
	})
	env.mustStatus(http.StatusOK, status, "allow MFA for the second business", body)

	status, body = env.do(http.MethodPut, "/api/v1/tenant/mfa-policy", env.tenantAdmin(allowed), map[string]any{
		"mode": "OPTIONAL", "allowed_methods": []string{"TOTP"}, "enforce_scope": "ALL_ADMINS", "grace_period_days": 7,
	})
	env.mustStatus(http.StatusOK, status, "set mfa policy to optional for the allowed business", body)

	// The granted tenant can now enroll...
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/setup", env.tenantAdmin(allowed), nil)
	env.mustStatus(http.StatusOK, status, "mfa setup on the allowed tenant", body)

	// ...but the first tenant is still blocked — the grant did not leak.
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/setup", env.tenantAdmin(blocked), nil)
	if status == http.StatusOK {
		t.Fatalf("granting MFA to one business must not unlock it for another, got 200: %v", body)
	}

	// A console account needs no grant at all.
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/setup", env.mintToken(identity.RoleSupport, nil), nil)
	env.mustStatus(http.StatusOK, status, "mfa setup on a console account", body)
}
