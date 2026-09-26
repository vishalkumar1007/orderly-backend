package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// auditActions returns the platform audit actions recorded for a service.
func auditActions(t *testing.T, env *testEnv, service string) []string {
	t.Helper()
	rows, err := env.server.pool.Query(context.Background(),
		`SELECT action FROM audit_logs
		 WHERE entity_type = 'platform_configuration'
		   AND action LIKE '%' || $1 || '%'
		 ORDER BY created_at DESC`, service)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		out = append(out, action)
	}
	return out
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

func TestConfigurationChangesAreAudited(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("STORAGE")
	token := env.superAdmin()
	before := len(auditActions(t, env, "STORAGE"))

	// Save.
	status, body := env.do(http.MethodPut, "/api/v1/admin/configurations/storage", token,
		map[string]any{"provider": "minio", "enabled": true, "config": map[string]any{
			"endpoint": "https://minio.test.local", "bucket": "media",
			"access_key": "AKIA", "secret_key": "AUDIT_CANARY_SECRET",
		}})
	env.mustStatus(http.StatusOK, status, "save", body)

	// Share with tenants.
	status, body = env.do(http.MethodPatch, "/api/v1/admin/configurations/storage/sharing", token,
		map[string]any{"allow_tenants": true})
	env.mustStatus(http.StatusOK, status, "share", body)

	// Disable.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE platform_configurations SET enabled = false WHERE service_type = 'STORAGE'`); err != nil {
		t.Fatal(err)
	}

	actions := auditActions(t, env, "STORAGE")
	if len(actions) <= before {
		t.Fatalf("no new audit rows: %v", actions)
	}
	for _, want := range []string{"Configuration Saved", "Access Granted to Tenants"} {
		if !containsAction(actions, want) {
			t.Errorf("missing audit action %q in %v", want, actions)
		}
	}
}

func TestAuditNeverContainsSecrets(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("AI")
	const canary = "AUDIT_SECRET_CANARY_XYZ"

	status, body := env.do(http.MethodPut, "/api/v1/admin/configurations/ai", env.superAdmin(),
		map[string]any{"provider": "openai", "enabled": true, "config": map[string]any{
			"base_url": "https://ai.test.local/v1", "default_model": "m", "api_key": canary,
		}})
	env.mustStatus(http.StatusOK, status, "save", body)

	var leaked int
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%' || $1 || '%' OR action LIKE '%' || $1 || '%'`,
		canary).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("%d audit rows contain the secret", leaked)
	}
}

func TestTenantSourceChangeIsAudited(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("AI")
	tenantID := env.createTenant("Audit Co", "audit-co-"+randSuffix())
	seedPlatform(t, env, "AI")
	sharePlatform(t, env, "AI", true)
	grantAccess(t, env, tenantID, "AI", true)
	token := env.tenantAdmin(tenantID)

	status, body := env.doAsTenant(http.MethodPut, "/api/v1/tenant/service-preferences/ai", token,
		map[string]any{"source": "PLATFORM"}, tenantID)
	env.mustStatus(http.StatusOK, status, "switch source", body)

	var actions int
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs
		 WHERE tenant_id = $1 AND action LIKE '%Source Changed%'`, tenantID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions == 0 {
		t.Error("a tenant switching configuration source must be audited")
	}
}
