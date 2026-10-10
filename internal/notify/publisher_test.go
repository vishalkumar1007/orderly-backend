package notify

import (
	"testing"

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
