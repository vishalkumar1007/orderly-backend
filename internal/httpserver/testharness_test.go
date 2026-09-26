package httpserver

import (
	"context"
	"net/http"
	"testing"
)

// seedOperatorStorageRow writes a row that stands in for something an operator
// configured by hand. It is called before a test environment is created, so the
// environment's snapshot includes it and the restore has something to bring back.
func seedOperatorStorageRow(t *testing.T, env *testEnv) {
	t.Helper()
	if _, err := env.server.pool.Exec(context.Background(), `
		INSERT INTO platform_configurations
			(service_type, provider, config, secret_config, status, enabled, allow_tenants)
		VALUES ('STORAGE','minio',
			'{"bucket":"operator-media","access_key":"AKIAOPERATOR","provider":"minio"}'::jsonb,
			'{"secret_key":"v1:OPERATOR_SENTINEL"}'::jsonb,
			'ENABLED', true, true)
		ON CONFLICT (service_type) DO UPDATE SET
			provider = EXCLUDED.provider,
			config = EXCLUDED.config,
			secret_config = EXCLUDED.secret_config,
			status = EXCLUDED.status,
			enabled = true,
			allow_tenants = true`); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}
}

// TestHarnessRestoresOperatorData guards the test harness itself.
//
// The configuration tests rewrite real tables on the developer's database. If
// the harness ever stops restoring them, running `go test ./...` silently
// destroys an operator's SMTP credentials — a failure mode that stays invisible
// until someone notices their mail has stopped working.
func TestHarnessRestoresOperatorData(t *testing.T) {
	// Phase 1: put an operator row in place using a throwaway environment.
	seeder := newTestEnv(t)
	seedOperatorStorageRow(t, seeder)
	// The seeder's own restore puts back the pre-seed state, so put the row
	// back afterwards — both for phase two and for the end of the test.
	seedOperatorStorageRow(t, seeder)
	t.Cleanup(func() { seedOperatorStorageRow(t, seeder) })

	// Phase 2: a fresh environment now snapshots that row, then a test
	// deliberately destroys it.
	env := newTestEnv(t)
	ctx := context.Background()

	var seeded int
	if err := env.server.pool.QueryRow(ctx,
		`SELECT count(*) FROM platform_configurations WHERE service_type = 'STORAGE'`).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded != 1 {
		t.Fatalf("setup: expected the operator row to exist before the snapshot, found %d", seeded)
	}

	env.resetService("STORAGE")
	status, body := env.do(http.MethodPut, "/api/v1/admin/configurations/storage", env.superAdmin(),
		map[string]any{"provider": "s3", "enabled": true, "config": map[string]any{
			"bucket": "test-bucket", "access_key": "AKIATEST", "secret_key": "TEST_SECRET",
		}})
	env.mustStatus(http.StatusOK, status, "test save", body)

	var midBucket string
	if err := env.server.pool.QueryRow(ctx,
		`SELECT config->>'bucket' FROM platform_configurations WHERE service_type = 'STORAGE'`).
		Scan(&midBucket); err != nil {
		t.Fatalf("the test's own write did not land: %v", err)
	}
	if midBucket != "test-bucket" {
		t.Fatalf("setup did not take effect (bucket = %q)", midBucket)
	}

	// Run the restore the way cleanup would, then confirm the operator's row is
	// back exactly as it was — values, secret and flags.
	env.restoreConfigTables()

	var bucket, secret, provider string
	var enabled, allow bool
	if err := env.server.pool.QueryRow(ctx, `
		SELECT config->>'bucket', secret_config->>'secret_key', provider, enabled, allow_tenants
		FROM platform_configurations WHERE service_type = 'STORAGE'`).
		Scan(&bucket, &secret, &provider, &enabled, &allow); err != nil {
		t.Fatalf("operator row was not restored: %v", err)
	}
	if bucket != "operator-media" {
		t.Errorf("bucket = %q, want the operator's original value", bucket)
	}
	if secret != "v1:OPERATOR_SENTINEL" {
		t.Errorf("secret was not restored intact: got %q", secret)
	}
	if provider != "minio" {
		t.Errorf("provider = %q, want minio", provider)
	}
	if !enabled || !allow {
		t.Error("enabled / allow_tenants were not restored")
	}
}

// TestHarnessRemovesTestTenants covers the other half of the pollution problem:
// the suite must not accumulate tenants on the database.
func TestHarnessRemovesTestTenants(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	const probe = "SELECT count(*) FROM tenants WHERE email = 'owner@test.local'"

	var before int
	if err := env.server.pool.QueryRow(ctx, probe).Scan(&before); err != nil {
		t.Fatal(err)
	}

	env.createTenant("Pollution Probe", "pollution-probe-"+randSuffix())

	var during int
	if err := env.server.pool.QueryRow(ctx, probe).Scan(&during); err != nil {
		t.Fatal(err)
	}
	if during != before+1 {
		t.Fatalf("tenant was not created: before=%d during=%d", before, during)
	}

	// The same call cleanup makes.
	env.removeTestTenants()

	var after int
	if err := env.server.pool.QueryRow(ctx, probe).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("test tenants were left behind: before=%d after=%d", before, after)
	}
}
