package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/secretbox"
)

// smtpBody is a valid SMTP save with a real secret in it.
func smtpBody(host, password string) map[string]any {
	return map[string]any{
		"provider": "smtp",
		"config": map[string]any{
			"host":       host,
			"port":       587,
			"encryption": "STARTTLS",
			"username":   "mailer",
			"password":   password,
			"from_name":  "Orderly",
			"from_email": "noreply@test.local",
		},
		"enabled": true,
	}
}

/* ------------------------------------------------------------------ *
 * Platform configuration CRUD
 * ------------------------------------------------------------------ */

func TestPlatformConfigurationLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	token := env.superAdmin()

	// Not configured yet: a 200 with an UNCONFIGURED view, so the UI can render
	// one shape for "never set up" and "set up but broken".
	status, body := env.do(http.MethodGet, "/api/v1/admin/configurations/smtp", token, nil)
	env.mustStatus(http.StatusOK, status, "get before save", body)
	cfg := mustView(t, body)
	if cfg["status"] != "UNCONFIGURED" {
		t.Errorf("initial status = %v, want UNCONFIGURED", cfg["status"])
	}
	if cfg["has_secret"] != false {
		t.Error("has_secret must be false before anything is stored")
	}

	// Save.
	status, body = env.do(http.MethodPut, "/api/v1/admin/configurations/smtp", token,
		smtpBody("smtp.test.local", "super-secret-password"))
	env.mustStatus(http.StatusOK, status, "save", body)
	cfg = mustView(t, body)
	if cfg["status"] != "ENABLED" {
		t.Errorf("status after save = %v, want ENABLED", cfg["status"])
	}
	if cfg["has_secret"] != true {
		t.Error("has_secret must be true once a password is stored")
	}

	// Stored row reflects the save.
	rowStatus, enabled, allow := env.platformRow("SMTP")
	if rowStatus != "ENABLED" || !enabled {
		t.Errorf("stored row = (%s, %v), want (ENABLED, true)", rowStatus, enabled)
	}
	if allow {
		t.Error("sharing must default to off")
	}
}

func TestSecretsAreNeverReturned(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	token := env.superAdmin()
	const secret = "PLAINTEXT_CANARY_VALUE"

	env.do(http.MethodPut, "/api/v1/admin/configurations/smtp", token, smtpBody("smtp.test.local", secret))

	for _, path := range []string{
		"/api/v1/admin/configurations",
		"/api/v1/admin/configurations/smtp",
		"/api/v1/admin/tenant-configurations",
	} {
		_, body := env.do(http.MethodGet, path, token, nil)
		raw, _ := body["__raw"].(string)
		if strings.Contains(raw, secret) {
			t.Errorf("%s leaked the secret in its response", path)
		}
		if strings.Contains(raw, "password\":\"") {
			t.Errorf("%s returned a password field", path)
		}
	}

	// The listing must still say a secret exists, so the UI can mask it.
	_, body := env.do(http.MethodGet, "/api/v1/admin/configurations/smtp", token, nil)
	cfg := mustView(t, body)
	if cfg["has_secret"] != true {
		t.Error("has_secret should be true so the UI can render a masked field")
	}
}

func TestSecretIsEncryptedAtRest(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	const secret = "AT_REST_CANARY_12345"

	env.do(http.MethodPut, "/api/v1/admin/configurations/smtp", env.superAdmin(), smtpBody("smtp.test.local", secret))

	var sealed string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT secret_config->>'password' FROM platform_configurations WHERE service_type = 'SMTP'`).Scan(&sealed); err != nil {
		t.Fatalf("read sealed secret: %v", err)
	}
	if strings.Contains(sealed, secret) {
		t.Error("secret is stored in plaintext")
	}
	if !strings.HasPrefix(sealed, "v1:") {
		t.Errorf("sealed value is not a versioned envelope: %q", sealed)
	}

	// And it must decrypt back to the original with the configured key.
	box := secretbox.New(env.cfg.ConfigEncryptionKey)
	plain, err := box.OpenString(sealed, configsvc.PlatformContextID(configsvc.ServiceSMTP))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain != secret {
		t.Errorf("round trip = %q, want %q", plain, secret)
	}
}

func TestMaskedSecretPreservesStoredValue(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	token := env.superAdmin()
	const secret = "PRESERVE_ME"

	env.do(http.MethodPut, "/api/v1/admin/configurations/smtp", token, smtpBody("smtp.test.local", secret))

	// A form resubmit with the sentinel must not blank the stored secret.
	body := smtpBody("smtp.changed.local", configsvc.MaskSentinel)
	status, resp := env.do(http.MethodPut, "/api/v1/admin/configurations/smtp", token, body)
	env.mustStatus(http.StatusOK, status, "save with mask", resp)

	var sealed string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT secret_config->>'password' FROM platform_configurations WHERE service_type = 'SMTP'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	box := secretbox.New(env.cfg.ConfigEncryptionKey)
	plain, err := box.OpenString(sealed, configsvc.PlatformContextID(configsvc.ServiceSMTP))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain != secret {
		t.Errorf("masked save changed the secret to %q, want it preserved as %q", plain, secret)
	}

	// The non-secret field should have taken the new value.
	cfg := mustView(t, resp)
	inner := mustMapAt(t, cfg, "config")
	if inner["host"] != "smtp.changed.local" {
		t.Errorf("host = %v, want the submitted smtp.changed.local", inner["host"])
	}
}

/* ------------------------------------------------------------------ *
 * Validation
 * ------------------------------------------------------------------ */

func TestValidationRejectsBadInput(t *testing.T) {
	env := newTestEnv(t)
	token := env.superAdmin()

	cases := []struct {
		name    string
		service string
		body    map[string]any
	}{
		{"smtp without host", "smtp", map[string]any{"config": map[string]any{
			"from_email": "a@b.co", "password": "x"}}},
		{"smtp without from_email", "smtp", map[string]any{"config": map[string]any{
			"host": "smtp.test.local", "password": "x"}}},
		{"smtp with bad port", "smtp", map[string]any{"config": map[string]any{
			"host": "smtp.test.local", "port": 99999, "from_email": "a@b.co", "password": "x"}}},
		{"smtp with bad encryption", "smtp", map[string]any{"config": map[string]any{
			"host": "smtp.test.local", "encryption": "rot13", "from_email": "a@b.co", "password": "x"}}},
		{"storage without bucket", "storage", map[string]any{"config": map[string]any{
			"provider": "s3", "access_key": "AKIA", "secret_key": "s"}}},
		{"storage with unknown provider", "storage", map[string]any{"config": map[string]any{
			"provider": "dropbox", "bucket": "b", "access_key": "k", "secret_key": "s"}}},
		{"storage with bad endpoint", "storage", map[string]any{"config": map[string]any{
			"provider": "minio", "endpoint": "ftp://x", "bucket": "b", "access_key": "k", "secret_key": "s"}}},
		{"ai with unknown provider", "ai", map[string]any{"config": map[string]any{
			"provider": "llama", "api_key": "k"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.resetService(strings.ToUpper(tc.service))
			status, body := env.do(http.MethodPut, "/api/v1/admin/configurations/"+tc.service, token, tc.body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body %v)", status, body["__raw"])
			}
		})
	}
}

func TestUnknownServiceRejected(t *testing.T) {
	env := newTestEnv(t)
	token := env.superAdmin()
	for _, svc := range []string{"email", "sms", "whatsapp", "SMTPP", ""} {
		status, _ := env.do(http.MethodGet, "/api/v1/admin/configurations/"+svc, token, nil)
		if status == http.StatusOK {
			t.Errorf("service %q was accepted, want rejected", svc)
		}
	}
}

/* ------------------------------------------------------------------ *
 * Tenant isolation and access control
 * ------------------------------------------------------------------ */

func TestTenantCannotReachPlatformEndpoints(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Isolation Co", "isolation-co-"+randSuffix())
	token := env.tenantAdmin(tenantID)

	for _, path := range []string{
		"/api/v1/admin/configurations",
		"/api/v1/admin/configurations/smtp",
		"/api/v1/admin/tenant-configurations",
	} {
		status, _ := env.do(http.MethodGet, path, token, nil)
		if status != http.StatusForbidden && status != http.StatusUnauthorized {
			t.Errorf("%s as tenant admin = %d, want 401/403", path, status)
		}
	}
}

// TestTenantCannotAddressAnotherTenant proves the tenant id is taken from the
// token and the host, not from anything the caller controls. Every attack here
// is a real one: forging a path, forging a body field, and replaying another
// tenant's host.
func TestTenantCannotAddressAnotherTenant(t *testing.T) {
	env := newTestEnv(t)
	tenantA := env.createTenant("Tenant A", "tenant-a-"+randSuffix())
	tenantB := env.createTenant("Tenant B", "tenant-b-"+randSuffix())
	env.resetService("SMTP")

	// B stores a secret.
	status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/configurations/smtp",
		env.tenantAdmin(tenantB), smtpBody("smtp.b.local", "TENANT_B_SECRET"), tenantB)
	env.mustStatus(http.StatusOK, status, "B saves", body)

	// Attack 1: A's token, B's host. The host/tenant mismatch must reject it.
	status, body = env.doAs(http.MethodGet, "/api/v1/tenant/configurations/smtp",
		env.tenantAdmin(tenantA), nil, tenantB)
	if status != http.StatusForbidden {
		t.Errorf("A reading via B's host = %d, want 403 (%v)", status, body["__raw"])
	}

	// Attack 2: A's token and host, but a forged tenant_id in the body. The
	// handler ignores the body entirely, so this only writes A's own row.
	status, body = env.doAsTenant(http.MethodPut, "/api/v1/tenant/configurations/smtp",
		env.tenantAdmin(tenantA), map[string]any{
			"tenant_id": tenantB.String(),
			"provider":  "smtp",
			"config": map[string]any{
				"host": "smtp.a.local", "port": 587, "encryption": "STARTTLS",
				"username": "a", "password": "TENANT_A_SECRET", "from_email": "a@a.co",
			},
			"enabled": true,
		}, tenantA)
	env.mustStatus(http.StatusOK, status, "A saves with a forged tenant_id", body)

	// The forged id must not have created or touched B's row.
	var bHost, aHost string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT config->>'host' FROM tenant_configurations WHERE tenant_id = $1 AND service_type = 'SMTP'`,
		tenantB).Scan(&bHost); err != nil {
		t.Fatal(err)
	}
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT config->>'host' FROM tenant_configurations WHERE tenant_id = $1 AND service_type = 'SMTP'`,
		tenantA).Scan(&aHost); err != nil {
		t.Fatal(err)
	}
	if bHost != "smtp.b.local" {
		t.Errorf("B's configuration was modified by A: host = %q", bHost)
	}
	if aHost != "smtp.a.local" {
		t.Errorf("A's own save landed wrong: host = %q", aHost)
	}

	// Attack 3: A tries the admin tenant-access endpoint.
	status, _ = env.doAsTenant(http.MethodPut,
		"/api/v1/admin/tenants/"+tenantB.String()+"/configurations/smtp/access",
		env.tenantAdmin(tenantA), map[string]any{"allow_platform": true}, tenantA)
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("A granting itself access = %d, want 401/403", status)
	}
	var grants int
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_service_access WHERE tenant_id = $1 AND service_type = 'SMTP' AND allow_platform`,
		tenantB).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Error("A was able to grant itself platform access")
	}

	// A's view must only ever describe A.
	_, resp := env.doAsTenant(http.MethodGet, "/api/v1/tenant/configurations/smtp",
		env.tenantAdmin(tenantA), nil, tenantA)
	raw, _ := resp["__raw"].(string)
	if strings.Contains(raw, "TENANT_B_SECRET") || strings.Contains(raw, "smtp.b.local") {
		t.Errorf("A's view leaked B's configuration: %s", raw)
	}
}

/* ---------- helpers ---------- */

func mustView(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	// Single-service endpoints return the view at the top level; the list
	// endpoint wraps it in an array.
	if _, ok := body["service"]; ok {
		return body
	}
	t.Fatalf("response is not a configuration view: %v", body["__raw"])
	return nil
}

func mustMapAt(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in %v", key, m)
	}
	inner, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want object", key, v)
	}
	return inner
}

func randSuffix() string {
	return strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}
