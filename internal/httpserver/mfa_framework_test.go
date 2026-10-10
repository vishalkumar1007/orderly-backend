package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/internal/auth"
)

/*
Tests for the per-tenant/platform MFA policy framework, on top of the
already-tested permission gate (mfa_test.go). Each test here proves one of
the three real authentication bypasses found while building this is actually
closed, or one piece of the policy/enrollment machinery that replaced the
single on/off switch.
*/

// setTenantMfaPolicy is a small helper so every test below states its policy
// the same way, through the real HTTP endpoint a Tenant Admin would use.
func setTenantMfaPolicy(t *testing.T, env *testEnv, tenantID uuid.UUID, mode string, methods []string, graceDays int) {
	t.Helper()
	status, body := env.do(http.MethodPut, "/api/v1/tenant/mfa-policy", env.tenantAdmin(tenantID), map[string]any{
		"mode": mode, "allowed_methods": methods, "enforce_scope": "ALL_ADMINS", "grace_period_days": graceDays,
	})
	env.mustStatus(http.StatusOK, status, "set tenant mfa policy", body)
}

func allowAndPolicyTenant(t *testing.T, env *testEnv, name, slug, mode string, methods []string, graceDays int) uuid.UUID {
	t.Helper()
	tenantID := env.createTenant(name, slug)
	status, body := env.do(http.MethodPatch, "/api/v1/admin/tenants/"+tenantID.String(), env.superAdmin(), map[string]any{
		"mfa_allowed": true,
	})
	env.mustStatus(http.StatusOK, status, "allow mfa for tenant", body)
	setTenantMfaPolicy(t, env, tenantID, mode, methods, graceDays)
	return tenantID
}

// createRealTenantAdmin inserts a tenant admin with a real bcrypt password —
// mintToken's fixture users have no usable password, so exercising an actual
// login requires a real row, same pattern mfa_test.go already established.
func createRealTenantAdmin(t *testing.T, env *testEnv, tenantID uuid.UUID, password string) (email string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	email = "mfa-admin-" + randSuffix() + "@test.local"
	if _, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status)
		 VALUES ($1, 'MFA Admin', $2, $3, 'TENANT_ADMIN', 'ACTIVE')`,
		tenantID, email, string(hash)); err != nil {
		t.Fatalf("create tenant admin: %v", err)
	}
	return email
}

// TestAdminLoginRejectsAccountPendingPasswordSetup closes the third bypass
// found while building this: authenticateUser skips the password check
// entirely when must_set_password is true (there is no usable password
// yet), and AdminLogin had no must_set_password check of its own — unlike
// TenantLogin, which already guarded it. That combination meant any
// invited-but-not-yet-activated console account could be logged into with
// any password string at all.
func TestAdminLoginRejectsAccountPendingPasswordSetup(t *testing.T) {
	env := newTestEnv(t)

	var email string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT email FROM users WHERE role = 'SUPER_ADMIN' LIMIT 1`).Scan(&email); err != nil {
		t.Fatalf("find the platform's super admin: %v", err)
	}
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE users SET must_set_password = true WHERE email = $1`, email); err != nil {
		t.Fatalf("set must_set_password: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.server.pool.Exec(context.Background(),
			`UPDATE users SET must_set_password = false WHERE email = $1`, email)
	})

	status, body := env.do(http.MethodPost, "/api/v1/auth/admin/login", "", map[string]any{
		"email": email, "password": "literally anything — the bug let this through",
	})
	if status == http.StatusOK {
		t.Fatalf("admin login must not succeed while must_set_password is true, got 200: %v", body)
	}
	if _, hasTokens := body["tokens"]; hasTokens {
		t.Error("a real token pair must never be issued while must_set_password is true")
	}
}

// TestSetupPasswordHonorsRequiredMFAAndCompletesEnrollment closes the first
// bypass: SetupPassword (first login after an invite) used to call
// issueTokens directly with no MFA check at all, so a business that requires
// MFA could be bypassed entirely by completing an invite instead of using a
// normal login. It also exercises the forced-enrollment flow end to end.
func TestSetupPasswordHonorsRequiredMFAAndCompletesEnrollment(t *testing.T) {
	env := newTestEnv(t)
	tenantID := allowAndPolicyTenant(t, env, "Required MFA Shop", "required-mfa-"+randSuffix(),
		"REQUIRED", []string{"TOTP"}, 0) // zero grace: enforced immediately

	rawToken := "invite-" + randSuffix()
	email := "invited-" + randSuffix() + "@test.local"
	hash, _ := bcrypt.GenerateFromPassword([]byte(uuid.NewString()), bcrypt.DefaultCost)
	if _, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status, must_set_password, invite_token_hash)
		 VALUES ($1, 'Invited Admin', $2, $3, 'TENANT_ADMIN', 'ACTIVE', true, $4)`,
		tenantID, email, string(hash), auth.HashInviteToken(rawToken)); err != nil {
		t.Fatalf("create invited user: %v", err)
	}

	status, body := env.do(http.MethodPost, "/api/v1/auth/setup-password", "", map[string]any{
		"token": rawToken, "password": "a brand new password",
	})
	env.mustStatus(http.StatusOK, status, "setup password", body)
	if _, hasTokens := body["tokens"]; hasTokens {
		t.Fatalf("a real session must not be issued before required enrollment completes: %v", body)
	}
	if required, _ := body["mfa_enroll_required"].(bool); !required {
		t.Fatalf("expected mfa_enroll_required: true for a brand-new user under REQUIRED policy, got: %v", body)
	}
	enrollToken := str(t, body, "enrollment_token")
	if enrollToken == "" {
		t.Fatalf("no enrollment_token in mfa_enroll_required response: %v", body)
	}

	status, body = env.do(http.MethodPost, "/api/v1/auth/mfa/enroll/start", "", map[string]any{
		"enrollment_token": enrollToken,
	})
	env.mustStatus(http.StatusOK, status, "enroll start", body)
	secret := str(t, body, "secret")
	setupToken := str(t, body, "setup_token")
	if secret == "" || setupToken == "" {
		t.Fatalf("enroll start missing secret/setup_token: %v", body)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}
	status, body = env.do(http.MethodPost, "/api/v1/auth/mfa/enroll/confirm", "", map[string]any{
		"enrollment_token": enrollToken, "setup_token": setupToken, "code": code,
	})
	env.mustStatus(http.StatusOK, status, "enroll confirm", body)
	tokens, _ := body["tokens"].(map[string]any)
	if tokens["access_token"] == "" || tokens["access_token"] == nil {
		t.Fatalf("enroll confirm did not complete the login: %v", body)
	}
}

// TestRefreshRejectsStaleTokenOnceMFABecomesRequired closes the second
// bypass: Refresh never checked whether MFA had since become required, so a
// refresh token minted while it was optional kept silently rotating valid
// access tokens for its full lifetime regardless of a later policy change.
func TestRefreshRejectsStaleTokenOnceMFABecomesRequired(t *testing.T) {
	env := newTestEnv(t)
	tenantID := allowAndPolicyTenant(t, env, "Policy Flip Shop", "policy-flip-"+randSuffix(),
		"OPTIONAL", []string{"TOTP"}, 0)

	const password = "correct horse battery staple"
	email := createRealTenantAdmin(t, env, tenantID, password)

	tenantHost := env.hostFor(tenantID)
	status, body := env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "login while mfa is optional", body)
	tokens, _ := body["tokens"].(map[string]any)
	refreshToken, _ := tokens["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("no refresh token issued while mfa was optional: %v", body)
	}

	// The business now requires MFA, with no grace window at all.
	setTenantMfaPolicy(t, env, tenantID, "REQUIRED", []string{"TOTP"}, 0)

	status, body = env.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refresh_token": refreshToken,
	})
	if status == http.StatusOK {
		t.Fatalf("a refresh token minted before MFA became required must not keep working, got 200: %v", body)
	}

	// A fresh login now correctly routes into forced enrollment instead.
	status, body = env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, tenantHost)
	env.mustStatus(http.StatusOK, status, "login after the policy flip", body)
	if required, _ := body["mfa_enroll_required"].(bool); !required {
		t.Fatalf("expected mfa_enroll_required after the policy flip, got: %v", body)
	}
}

// TestMFAGracePeriodAllowsNormalLoginBeforeDeadline proves a grace period
// does what it says: a user created under a REQUIRED policy still gets a
// normal session until their window elapses, rather than being locked out
// the instant the policy takes effect.
func TestMFAGracePeriodAllowsNormalLoginBeforeDeadline(t *testing.T) {
	env := newTestEnv(t)
	tenantID := allowAndPolicyTenant(t, env, "Grace Period Shop", "grace-period-"+randSuffix(),
		"REQUIRED", []string{"TOTP"}, 7)

	const password = "correct horse battery staple"
	email := createRealTenantAdmin(t, env, tenantID, password)

	status, body := env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "", map[string]any{
		"email": email, "password": password,
	}, env.hostFor(tenantID))
	env.mustStatus(http.StatusOK, status, "login within the grace period", body)
	if _, required := body["mfa_enroll_required"]; required {
		t.Fatalf("a user within their grace period must not be forced into enrollment yet: %v", body)
	}
	tokens, _ := body["tokens"].(map[string]any)
	if tokens["access_token"] == "" || tokens["access_token"] == nil {
		t.Fatalf("expected a normal session within the grace period: %v", body)
	}
}

// TestEmailOTPUnavailableWithoutProvider proves a missing email
// configuration produces a clear, typed error rather than a silent
// fallback — no SMTP provider is configured anywhere in this test stack.
func TestEmailOTPUnavailableWithoutProvider(t *testing.T) {
	env := newTestEnv(t)
	tenantID := allowAndPolicyTenant(t, env, "Email OTP Shop", "email-otp-"+randSuffix(),
		"OPTIONAL", []string{"EMAIL_OTP"}, 7)

	status, body := env.do(http.MethodPost, "/api/v1/auth/me/mfa/email/send", env.tenantAdmin(tenantID), nil)
	if status == http.StatusOK {
		t.Fatalf("email otp must not appear to send with no configured provider, got 200: %v", body)
	}
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "email_otp_unavailable" {
		t.Fatalf("expected a typed email_otp_unavailable error, got: %v", body)
	}
}

// TestEmailOTPAttemptLimitAndExpiry exercises the rate-limit and expiry
// paths directly against the stored code row — mirroring
// internal/customers/otp.go's own test shape, since no provider is
// configured to drive a real send in this stack.
func TestEmailOTPAttemptLimitAndExpiry(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Email OTP Codes Shop", "email-otp-codes-"+randSuffix())
	token := env.tenantAdmin(tenantID)

	status, body := env.do(http.MethodGet, "/api/v1/auth/me", token, nil)
	env.mustStatus(http.StatusOK, status, "load fixture user", body)
	userID := str(t, body, "id")
	if userID == "" {
		t.Fatalf("could not resolve fixture user id: %v", body)
	}

	hashCode := func(code string) string {
		sum := sha256.Sum256([]byte(userID + ":" + code))
		return hex.EncodeToString(sum[:])
	}

	// Expired: inserted already past its expiry, the correct code must still fail.
	if _, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO mfa_email_otp_codes (user_id, code_hash, expires_at) VALUES ($1, $2, now() - interval '1 minute')`,
		userID, hashCode("111111")); err != nil {
		t.Fatalf("insert expired code: %v", err)
	}
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/email/confirm", token, map[string]any{"code": "111111"})
	if status == http.StatusOK {
		t.Fatalf("an expired code must not verify, got 200: %v", body)
	}

	// Attempt limit: five wrong guesses against a live code exhaust it, and a
	// sixth attempt with the right code must still fail.
	if _, err := env.server.pool.Exec(context.Background(),
		`DELETE FROM mfa_email_otp_codes WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clear codes: %v", err)
	}
	if _, err := env.server.pool.Exec(context.Background(),
		`INSERT INTO mfa_email_otp_codes (user_id, code_hash, expires_at) VALUES ($1, $2, now() + interval '10 minutes')`,
		userID, hashCode("222222")); err != nil {
		t.Fatalf("insert live code: %v", err)
	}
	for i := 0; i < 5; i++ {
		status, _ = env.do(http.MethodPost, "/api/v1/auth/me/mfa/email/confirm", token, map[string]any{"code": "000000"})
		if status == http.StatusOK {
			t.Fatalf("a wrong code must never verify (attempt %d)", i+1)
		}
	}
	status, body = env.do(http.MethodPost, "/api/v1/auth/me/mfa/email/confirm", token, map[string]any{"code": "222222"})
	if status == http.StatusOK {
		t.Fatalf("the correct code must be rejected once the attempt limit is exhausted, got 200: %v", body)
	}
}
