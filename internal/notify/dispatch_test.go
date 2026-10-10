package notify

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/database"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// TestBackoffFor is a pure unit test — no database needed.
func TestBackoffFor(t *testing.T) {
	if backoffFor(0) != backoffSchedule[0] {
		t.Errorf("attempt 0 should use the first backoff step")
	}
	if backoffFor(2) != backoffSchedule[2] {
		t.Errorf("attempt 2 should use the third backoff step")
	}
	if got := backoffFor(99); got != backoffSchedule[len(backoffSchedule)-1] {
		t.Errorf("an attempt past the schedule should cap at the last step, got %v", got)
	}
}

// loadDotEnv mirrors internal/httpserver's test helper: DATABASE_URL and
// CONFIG_ENCRYPTION_KEY normally come from the project .env, and an
// already-set (even empty) env var is left alone — that is what lets `make
// check` force these tests to skip by exporting them as empty.
func loadDotEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{".env", "../.env", "../../.env"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 || line[0] == '#' {
				continue
			}
			parts := bytes.SplitN(line, []byte("="), 2)
			if len(parts) != 2 {
				continue
			}
			key := string(bytes.TrimSpace(parts[0]))
			if _, set := os.LookupEnv(key); set {
				continue
			}
			_ = os.Setenv(key, string(bytes.TrimSpace(parts[1])))
		}
		return
	}
}

// testPool connects to the real database, skipping when none is configured
// (the same convention every other integration test in this codebase uses).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	loadDotEnv(t)
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL is not set; skipping integration test")
	}
	pool, err := database.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testTenant inserts a minimal tenant row, cleaned up (cascading to every
// row this test writes against it) when the test ends.
func testTenant(t *testing.T, q *sqlc.Queries, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name, slug, business_type, owner_name, email, status)
		 VALUES ($1, $2, 'RESTAURANT', 'Owner', 'owner@test.local', 'ACTIVE')
		 RETURNING id`, "Notify Test "+slug, slug).Scan(&id)
	if err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

// TestRulePrecedence verifies the Platform Defaults -> Business Configuration
// override: a tenant that writes its own rule for an (event, channel,
// audience) sees its own row, every other tenant still sees the platform
// default — proving both the override and the tenant isolation in the same
// query path a cross-tenant read would otherwise leak through.
func TestRulePrecedence(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()

	tenantA := testTenant(t, q, pool, "notify-rules-a-"+uuid.NewString()[:8])
	tenantB := testTenant(t, q, pool, "notify-rules-b-"+uuid.NewString()[:8])

	// The platform default for this (event, channel, audience) ships disabled
	// — see the seed in 20260929240000_notification_rules_engine.sql.
	const event, channel, policy = "APPOINTMENT_CREATED", "SMS", "CUSTOMER"

	before, err := q.ListEffectiveRulesForEvent(ctx, sqlc.ListEffectiveRulesForEventParams{
		EventCode: event, TenantID: pgutil.UUID(tenantA),
	})
	if err != nil {
		t.Fatalf("list effective rules: %v", err)
	}
	if enabledFor(before, channel, policy) {
		t.Fatalf("expected the platform default to start disabled")
	}

	if _, err := q.UpsertTenantNotificationRule(ctx, sqlc.UpsertTenantNotificationRuleParams{
		TenantID: pgutil.UUID(tenantA), EventCode: event, Channel: channel,
		Enabled: true, RecipientPolicy: policy, Priority: "NORMAL",
	}); err != nil {
		t.Fatalf("upsert tenant rule: %v", err)
	}

	afterA, err := q.ListEffectiveRulesForEvent(ctx, sqlc.ListEffectiveRulesForEventParams{
		EventCode: event, TenantID: pgutil.UUID(tenantA),
	})
	if err != nil {
		t.Fatalf("list effective rules for A: %v", err)
	}
	if !enabledFor(afterA, channel, policy) {
		t.Error("tenant A's own override should make the rule enabled for tenant A")
	}

	afterB, err := q.ListEffectiveRulesForEvent(ctx, sqlc.ListEffectiveRulesForEventParams{
		EventCode: event, TenantID: pgutil.UUID(tenantB),
	})
	if err != nil {
		t.Fatalf("list effective rules for B: %v", err)
	}
	if enabledFor(afterB, channel, policy) {
		t.Error("tenant A's override must never leak into tenant B's effective rules")
	}
}

func enabledFor(rules []sqlc.NotificationRule, channel, policy string) bool {
	for _, r := range rules {
		if r.Channel == channel && r.RecipientPolicy == policy {
			return r.Enabled
		}
	}
	return false
}

// TestEnqueueIdempotent verifies that dispatching the same event twice (a
// retried webhook, a duplicate publish) produces exactly one delivery row,
// never two.
func TestEnqueueIdempotent(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	ctx := context.Background()

	tenant := testTenant(t, q, pool, "notify-idem-"+uuid.NewString()[:8])
	key := uuid.NewString()

	for i := 0; i < 2; i++ {
		_, err := q.EnqueueNotificationDelivery(ctx, sqlc.EnqueueNotificationDeliveryParams{
			TenantID:       pgutil.UUID(tenant),
			EventCode:      "ORDER_READY",
			Channel:        "EMAIL",
			Recipient:      "customer@example.com",
			IdempotencyKey: key,
		})
		// The second call hits ON CONFLICT DO NOTHING and returns no row —
		// that is success (an already-enqueued delivery), not an error.
		if err != nil && i == 0 {
			t.Fatalf("first enqueue failed: %v", err)
		}
	}

	rows, err := q.ListDeliveriesForTenant(ctx, sqlc.ListDeliveriesForTenantParams{
		TenantID: pgutil.UUID(tenant), Limit: 10, Offset: 0,
	})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one delivery row for a duplicate dispatch, got %d", len(rows))
	}
}

// TestDispatchIsBestEffort proves the core reliability contract: Dispatch
// never panics or blocks on a missing/invalid event, which is what lets
// every call site invoke it without checking an error.
func TestDispatchIsBestEffort(t *testing.T) {
	pool := testPool(t)
	q := sqlc.New(pool)
	tenant := testTenant(t, q, pool, "notify-noop-"+uuid.NewString()[:8])

	done := make(chan struct{})
	go func() {
		defer close(done)
		Dispatch(context.Background(), Deps{Q: q}, &tenant, "NOT_A_REAL_EVENT", "t", "b", nil, Contact{})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Dispatch did not return for an unknown event code")
	}
}
