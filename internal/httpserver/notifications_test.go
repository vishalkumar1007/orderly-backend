package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

/*
The in-app notification pipeline (internal/notify), end to end through the
real HTTP handlers — not the bare-SQL fixtures other tests use, since those
skip the provisioning a notification event hooks into.

Runs against the developer's database like the rest of this suite. The
platform event test publishes to the real super admin account found in the
database (same account env.superAdmin() always authenticates as elsewhere in
this file) — it deletes the one notification row it created, leaving that
account exactly as it found it.
*/

func TestCreateTenantPublishesOnboardedNotification(t *testing.T) {
	env := newTestEnv(t)
	slug := "notif-biz-" + randSuffix()

	var superAdminID uuid.UUID
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE role = 'SUPER_ADMIN' ORDER BY created_at ASC LIMIT 1`).
		Scan(&superAdminID); err != nil {
		t.Fatalf("no super admin exists to receive the notification: %v", err)
	}

	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":           "Notify Test Business",
		"slug":           slug,
		"business_type":  "RESTAURANT",
		"owner_name":     "Owner",
		"admin_name":     "Admin",
		"admin_email":    "admin-" + randSuffix() + "@test.local",
		"email":          "owner@test.local",
		"terms_accepted": true,
	})
	env.mustStatus(http.StatusCreated, status, "create a business", body)

	id := env.tenantID(slug)
	env.createdTenants = append(env.createdTenants, id)
	t.Cleanup(env.removeTestTenants)

	var notificationID uuid.UUID
	var title string
	t.Cleanup(func() {
		if notificationID != uuid.Nil {
			_, _ = env.server.pool.Exec(context.Background(),
				`DELETE FROM notifications WHERE id = $1`, notificationID)
		}
	})

	err := env.server.pool.QueryRow(context.Background(), `
		SELECT id, title FROM notifications
		WHERE user_id = $1 AND type = 'BUSINESS_ONBOARDED' AND (data->>'tenant_id') = $2`,
		superAdminID, id.String()).Scan(&notificationID, &title)
	if err != nil {
		t.Fatalf("expected a BUSINESS_ONBOARDED notification for the super admin: %v", err)
	}
	if title == "" {
		t.Error("notification title is empty")
	}
}
