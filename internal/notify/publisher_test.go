package notify

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
)

func TestFilterByPermission(t *testing.T) {
	admin := uuid.New()
	staff := uuid.New()
	disabledStaff := uuid.New()
	manager := uuid.New()

	rows := []sqlc.ListTenantUsersRow{
		{ID: pgtype.UUID{Bytes: admin, Valid: true}, Role: identity.RoleTenantAdmin, Status: "ACTIVE", Email: "admin@example.com"},
		{ID: pgtype.UUID{Bytes: staff, Valid: true}, Role: identity.RoleStaff, Status: "ACTIVE", Email: "staff@example.com"},
		{ID: pgtype.UUID{Bytes: disabledStaff, Valid: true}, Role: identity.RoleStaff, Status: "DISABLED", Email: "disabled@example.com"},
		{ID: pgtype.UUID{Bytes: manager, Valid: true}, Role: identity.RoleManager, Status: "ACTIVE", Email: "manager@example.com"},
	}

	got := filterByPermission(rows, identity.PermSelling)

	want := map[uuid.UUID]bool{}
	for _, r := range got {
		want[r.UserID] = true
		if r.Email == "" {
			t.Errorf("recipient %v is missing its email", r.UserID)
		}
	}

	if !want[admin] {
		t.Error("expected an active admin to receive a selling notification")
	}
	if !want[staff] {
		t.Error("expected an active staff member to receive a selling notification")
	}
	if want[disabledStaff] {
		t.Error("a disabled user must never be a recipient")
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected duplicate recipients: %v", got)
	}
}

func TestWithinWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) }
	us := func(h int) int64 { return int64(h) * 3600 * 1_000_000 }
	cases := []struct {
		start, end int64
		now        time.Time
		want       bool
	}{
		{us(22), us(7), at(23, 0), true},  // crosses midnight, evening side
		{us(22), us(7), at(3, 0), true},   // crosses midnight, morning side
		{us(22), us(7), at(12, 0), false}, // outside
		{us(13), us(15), at(14, 0), true}, // same-day window
		{us(13), us(15), at(15, 0), false},
	}
	for i, c := range cases {
		if got := withinWindow(c.start, c.end, c.now); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestIdempotencyKeyBucketsEventsWithoutEntity(t *testing.T) {
	now := time.Date(2026, 1, 1, 10, 0, 5, 0, time.UTC)
	a := idempotencyKey("SECURITY_LOGIN_FAILED", nil, "EMAIL", "a@x.io", map[string]any{"user_email": "u"}, now)
	same := idempotencyKey("SECURITY_LOGIN_FAILED", nil, "EMAIL", "a@x.io", map[string]any{"user_email": "u"}, now.Add(20*time.Second))
	later := idempotencyKey("SECURITY_LOGIN_FAILED", nil, "EMAIL", "a@x.io", map[string]any{"user_email": "u"}, now.Add(2*time.Minute))
	if a != same {
		t.Error("a same-minute retry must collapse onto one delivery")
	}
	if a == later {
		t.Error("a repeat in a later minute must not be swallowed")
	}
	o1 := idempotencyKey("ORDER_READY", nil, "EMAIL", "a@x.io", map[string]any{"order_id": "1"}, now)
	o2 := idempotencyKey("ORDER_READY", nil, "EMAIL", "a@x.io", map[string]any{"order_id": "1"}, now.Add(time.Hour))
	if o1 != o2 {
		t.Error("an entity event must stay stable across time")
	}
}
