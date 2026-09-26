package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/pkg/database"
	"github.com/orderly/orderly-backend/pkg/identity"
)

// testEnv is a live server backed by the real database. Every test that needs
// HTTP goes through here, so the routing, middleware and handler wiring is
// exercised rather than assumed.
type testEnv struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	cfg    config.Config

	// createdTenants are removed on cleanup so a test run does not leave a
	// growing pile of tenants on the developer's database.
	createdTenants []uuid.UUID

	// backupName prefixes this environment's configuration snapshots.
	backupName string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	// Load the project .env first: DATABASE_URL normally lives there, and the
	// skip check below must see it.
	loadDotEnv(t)
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL is not set; skipping integration test")
	}
	if os.Getenv("CONFIG_ENCRYPTION_KEY") == "" {
		t.Skip("CONFIG_ENCRYPTION_KEY is not set; skipping integration test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// The real key from .env is used, so tests read what the running app wrote.
	if cfg.ConfigEncryptionKey == "" {
		t.Skip("CONFIG_ENCRYPTION_KEY is empty in config; skipping integration test")
	}

	// Errors are surfaced to the test log so a silently swallowed failure
	// during development is visible.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pool, err := database.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	t.Cleanup(pool.Close)
	srv := New(log, pool, cfg)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	env := &testEnv{t: t, server: srv, http: ts, cfg: cfg}
	env.snapshotConfigTables()
	return env
}

// configTables are the rows the configuration tests rewrite. They hold real,
// operator-entered settings on a development database, so they are copied aside
// before the suite runs and restored afterwards. Running the tests must never be
// the reason a developer's SMTP password disappears.
//
// The backups are ordinary tables rather than TEMP ones because the pool spreads
// work across connections, and a TEMP table would only be visible to whichever
// connection created it.
var configTables = []string{
	"tenant_service_preferences",
	"tenant_service_access",
	"tenant_configurations",
	"platform_configurations",
}

// backupSeq makes each environment's backup table names unique, so two
// environments in the same test (or two tests in one run) never overwrite each
// other's snapshot.
var backupSeq atomic.Int64

// snapshotConfigTables copies the configuration tables for later restoration.
func (e *testEnv) snapshotConfigTables() {
	e.t.Helper()
	ctx := context.Background()
	e.backupName = fmt.Sprintf("orderly_cfg_backup_%d_%d", os.Getpid(), backupSeq.Add(1))
	for i, table := range configTables {
		backup := fmt.Sprintf("%s_%d", e.backupName, i)
		if _, err := e.server.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", backup)); err != nil {
			e.t.Fatalf("drop stale backup %s: %v", backup, err)
		}
		if _, err := e.server.pool.Exec(ctx,
			fmt.Sprintf("CREATE TABLE %s AS SELECT * FROM %s", backup, table)); err != nil {
			e.t.Fatalf("snapshot %s: %v", table, err)
		}
	}
	e.t.Cleanup(e.restoreConfigTables)
}

// restoreConfigTables puts the snapshotted rows back and drops the backups. It
// is a named method rather than an inline closure so the harness test can invoke
// it directly and prove the restore actually works.
func (e *testEnv) restoreConfigTables() {
	ctx := context.Background()
	for i, table := range configTables {
		backup := fmt.Sprintf("%s_%d", e.backupName, i)

		// A restore can legitimately run twice: the harness test invokes it
		// directly to prove it works, and the registered cleanup runs afterwards.
		// Once the backup is gone there is nothing to put back, and clearing the
		// table again would destroy data — so stop.
		var exists bool
		if err := e.server.pool.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, backup).Scan(&exists); err != nil {
			e.t.Errorf("could not check backup %s: %v", backup, err)
			return
		}
		if !exists {
			return
		}

		if _, err := e.server.pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s", table)); err != nil {
			e.t.Errorf("could not clear %s: %v", table, err)
			continue
		}
		if _, err := e.server.pool.Exec(ctx,
			fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", table, backup)); err != nil {
			e.t.Errorf("could not restore %s: %v", table, err)
		}
		if _, err := e.server.pool.Exec(ctx,
			fmt.Sprintf("DROP TABLE IF EXISTS %s", backup)); err != nil {
			e.t.Errorf("could not drop backup %s: %v", backup, err)
		}
	}
}

func loadDotEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{".env", "../.env", "../../.env"} {
		if _, err := os.Stat(name); err == nil {
			loadDotEnvFile(t, name)
			return
		}
	}
}

func loadDotEnvFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, value, ok := bytes.Cut(line, []byte("="))
		if !ok {
			continue
		}
		k := string(bytes.TrimSpace(key))
		if k == "" {
			continue
		}
		if _, present := os.LookupEnv(k); present {
			continue
		}
		_ = os.Setenv(k, string(bytes.TrimSpace(value)))
	}
}

// mintToken builds a valid access token for a role, using the same signing
// secret the server uses. This keeps the tests independent of any stored
// account's password.
//
// The subject must be a real users row: audit_logs has a foreign key on
// user_id, so a synthetic subject would make every audited change fail to
// record — which is exactly the kind of thing these tests should catch rather
// than paper over.
func (e *testEnv) mintToken(role string, tenantID *uuid.UUID) string {
	e.t.Helper()
	now := time.Now()
	email := fmt.Sprintf("test-%s-%s@test.local", role, randSuffix())
	var id uuid.UUID
	err := e.server.pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status)
		 VALUES ($1, $2, $3, 'not-a-real-hash', $4, 'ACTIVE')
		 RETURNING id`,
		tenantPtr(tenantID), "Test "+role, email, role).Scan(&id)
	if err != nil {
		e.t.Fatalf("create test user: %v", err)
	}
	e.t.Cleanup(func() {
		// audit_logs references the user, so clear its rows first.
		_, _ = e.server.pool.Exec(context.Background(),
			`DELETE FROM audit_logs WHERE user_id = $1`, id)
		_, _ = e.server.pool.Exec(context.Background(),
			`DELETE FROM refresh_tokens WHERE user_id = $1`, id)
		_, _ = e.server.pool.Exec(context.Background(),
			`DELETE FROM users WHERE id = $1`, id)
	})

	claims := auth.Claims{
		Role:  role,
		Email: email,
		Name:  "Test " + role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	if tenantID != nil {
		str := tenantID.String()
		claims.TenantID = &str
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(e.cfg.JWTAccessSecret))
	if err != nil {
		e.t.Fatalf("mint token: %v", err)
	}
	return signed
}

// tenantPtr maps an optional tenant id to a nullable column.
func tenantPtr(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return *id
}

// superAdmin mints a token for the platform's Super Admin.
//
// A partial unique index permits exactly one SUPER_ADMIN row, so the existing
// one is reused rather than creating a second — the tests must not depend on
// being able to add another.
func (e *testEnv) superAdmin() string {
	e.t.Helper()
	var id uuid.UUID
	err := e.server.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE role = 'SUPER_ADMIN' ORDER BY created_at ASC LIMIT 1`).Scan(&id)
	if err != nil {
		e.t.Fatalf("no super admin exists to authenticate as: %v", err)
	}
	return e.mintTokenAs(id, identity.RoleSuperAdmin, nil)
}

// mintTokenAs builds a token for an existing user row.
func (e *testEnv) mintTokenAs(id uuid.UUID, role string, tenantID *uuid.UUID) string {
	e.t.Helper()
	now := time.Now()
	claims := auth.Claims{
		Role:  role,
		Email: "super-admin@test.local",
		Name:  "Super Admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	if tenantID != nil {
		str := tenantID.String()
		claims.TenantID = &str
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(e.cfg.JWTAccessSecret))
	if err != nil {
		e.t.Fatalf("mint token: %v", err)
	}
	return signed
}

func (e *testEnv) tenantAdmin(tenantID uuid.UUID) string {
	return e.mintToken(identity.RoleTenantAdmin, &tenantID)
}

// do issues a request against the API host.
func (e *testEnv) do(method, path, token string, body any) (int, map[string]any) {
	return e.doOn(method, path, token, body, "api.localhost")
}

// doAs issues a request against a tenant's subdomain. The tenant routes are
// guarded by MatchHostTenant, so a tenant call must arrive on that tenant's
// host or the middleware rejects it — which is exactly the isolation guarantee
// being tested.
func (e *testEnv) doAs(method, path, token string, body any, tenantID uuid.UUID) (int, map[string]any) {
	return e.doOn(method, path, token, body, e.hostFor(tenantID))
}

// doAsTenant issues a tenant-scoped request, taking the tenant id last so the
// call sites read naturally.
func (e *testEnv) doAsTenant(method, path, token string, body any, tenantID uuid.UUID) (int, map[string]any) {
	return e.doAs(method, path, token, body, tenantID)
}

func (e *testEnv) hostFor(tenantID uuid.UUID) string {
	e.t.Helper()
	var slug string
	if err := e.server.pool.QueryRow(context.Background(),
		`SELECT slug FROM tenants WHERE id = $1`, tenantID).Scan(&slug); err != nil {
		e.t.Fatalf("resolve tenant host: %v", err)
	}
	return slug + "." + e.cfg.BaseDomain
}

// doOn issues a request against a specific Host header.
func (e *testEnv) doOn(method, path, token string, body any, host string) (int, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.http.URL+path, reader)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Go's HTTP client takes the Host from req.Host; a "Host" entry in the
	// header map is ignored, which would leave the tenant resolver blind.
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	// Keep the raw body for leak assertions.
	out["__raw"] = string(raw)
	return resp.StatusCode, out
}

func (e *testEnv) mustStatus(want, got int, label string, body map[string]any) {
	e.t.Helper()
	if got != want {
		e.t.Fatalf("%s: status = %d, want %d (body: %v)", label, got, want, body["__raw"])
	}
}

// createTenant inserts a tenant directly so configuration tests do not depend on
// the onboarding flow.
func (e *testEnv) createTenant(name, slug string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	err := e.server.pool.QueryRow(context.Background(),
		`INSERT INTO tenants (name, slug, business_type, owner_name, email, status)
		 VALUES ($1, $2, 'restaurant', 'Owner', 'owner@test.local', 'ACTIVE')
		 RETURNING id`, name, slug).Scan(&id)
	if err != nil {
		e.t.Fatalf("create tenant: %v", err)
	}
	// Clean up so a test run does not leave a growing pile of tenants behind.
	e.createdTenants = append(e.createdTenants, id)
	e.t.Cleanup(e.removeTestTenants)
	return id
}

// removeTestTenants deletes every tenant this test created. Most child tables
// cascade, but several (orders, audit_logs, users) are ON DELETE RESTRICT, so
// they are cleared explicitly first.
func (e *testEnv) removeTestTenants() {
	ctx := context.Background()
	for _, id := range e.createdTenants {
		for _, stmt := range []string{
			`DELETE FROM audit_logs WHERE tenant_id = $1`,
			`DELETE FROM order_items WHERE tenant_id = $1`,
			`DELETE FROM payments WHERE tenant_id = $1`,
			`DELETE FROM orders WHERE tenant_id = $1`,
			`DELETE FROM subscriptions WHERE tenant_id = $1`,
			`DELETE FROM products WHERE tenant_id = $1`,
			`DELETE FROM categories WHERE tenant_id = $1`,
			`DELETE FROM customers WHERE tenant_id = $1`,
			`DELETE FROM order_counters WHERE tenant_id = $1`,
			`DELETE FROM refresh_tokens WHERE user_id IN (SELECT id FROM users WHERE tenant_id = $1)`,
			`DELETE FROM users WHERE tenant_id = $1`,
			`DELETE FROM tenants WHERE id = $1`,
		} {
			if _, err := e.server.pool.Exec(ctx, stmt, id); err != nil {
				e.t.Errorf("tenant cleanup (%s): %v", stmt, err)
			}
		}
	}
	e.createdTenants = nil
}

func (e *testEnv) tenantID(slug string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.server.pool.QueryRow(context.Background(),
		`SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id); err != nil {
		e.t.Fatalf("tenant %s: %v", slug, err)
	}
	return id
}

// resetService removes every configuration row so each test starts clean. The
// schema is shared, so isolation has to be explicit.
func (e *testEnv) resetService(service string) {
	e.t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM tenant_service_preferences WHERE service_type = $1`,
		`DELETE FROM tenant_service_access WHERE service_type = $1`,
		`DELETE FROM tenant_configurations WHERE service_type = $1`,
		`DELETE FROM platform_configurations WHERE service_type = $1`,
	} {
		if _, err := e.server.pool.Exec(ctx, stmt, service); err != nil {
			e.t.Fatalf("reset %s: %v", service, err)
		}
	}
}

func (e *testEnv) platformRow(service string) (status string, enabled bool, allow bool) {
	e.t.Helper()
	err := e.server.pool.QueryRow(context.Background(),
		`SELECT status, enabled, allow_tenants FROM platform_configurations WHERE service_type = $1`,
		service).Scan(&status, &enabled, &allow)
	if err != nil {
		e.t.Fatalf("read platform row %s: %v", service, err)
	}
	return status, enabled, allow
}
