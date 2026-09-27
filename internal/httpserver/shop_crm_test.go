package httpserver

import (
	"net/http"
	"testing"

	"github.com/orderly/orderly-backend/pkg/identity"
)

// Shop CRM (Customers + Staff) is owner-only and must resolve under the tenant
// host. These routes were easy to ship without a smoke test and then look
// "broken" when an old binary was still listening.
func TestShopListCustomersAndUsers(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)

	status, body := env.doAs(http.MethodGet, "/api/v1/tenant/customers", shop.adminToken, nil, shop.tenantID)
	if status != http.StatusOK {
		t.Fatalf("list customers: status %d body %#v", status, body)
	}
	if _, ok := body["customers"]; !ok {
		t.Fatalf("list customers: missing customers key: %#v", body)
	}
	if _, ok := body["counts"]; !ok {
		t.Fatalf("list customers: missing counts key: %#v", body)
	}

	status, body = env.doAs(http.MethodGet, "/api/v1/tenant/users", shop.adminToken, nil, shop.tenantID)
	if status != http.StatusOK {
		t.Fatalf("list users: status %d body %#v", status, body)
	}
	users, ok := body["users"].([]any)
	if !ok {
		t.Fatalf("list users: missing users array: %#v", body)
	}
	if len(users) == 0 {
		t.Fatalf("list users: expected at least the admin row")
	}

	// Staff must not manage CRM.
	staff := env.mintToken(identity.RoleStaff, &shop.tenantID)
	status, _ = env.doAs(http.MethodGet, "/api/v1/tenant/customers", staff, nil, shop.tenantID)
	if status != http.StatusForbidden {
		t.Fatalf("staff list customers: want 403 got %d", status)
	}
	status, _ = env.doAs(http.MethodGet, "/api/v1/tenant/users", staff, nil, shop.tenantID)
	if status != http.StatusForbidden {
		t.Fatalf("staff list users: want 403 got %d", status)
	}
}
