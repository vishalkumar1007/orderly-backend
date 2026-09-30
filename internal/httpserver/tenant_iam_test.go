package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/pkg/identity"
)

/*
Access control inside a business.

The product defines three roles and says access is permission-based rather than
"manager gets everything the owner has". These tests hold that line from the
outside: they call the API as each role and check what comes back, so a future
refactor of the permission table cannot quietly widen what a manager or a
member of staff can reach.
*/

// tenantUser mints a token for a business user in a given role.
func tenantUser(e *testEnv, role string, tenantID uuid.UUID) string {
	return e.mintToken(role, &tenantID)
}

func TestRolePermissionsMatchWhatTheAPIAllows(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Role Fence", "role-fence-"+randSuffix())
	host := env.hostFor(tenantID)

	// Every case is (route, method) against the three roles. The expectations
	// spell out the product's rule: staff run the shop, managers also manage
	// what it sells and who works there, owners also configure the business.
	cases := []struct {
		label   string
		method  string
		path    string
		body    any
		owner   int
		manager int
		staff   int
	}{
		{"the order board", http.MethodGet, "/api/v1/tenant/orders", nil,
			http.StatusOK, http.StatusOK, http.StatusOK},
		{"the menu", http.MethodGet, "/api/v1/tenant/products", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},
		{"customers", http.MethodGet, "/api/v1/tenant/customers", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},
		{"the staff list", http.MethodGet, "/api/v1/tenant/users", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},
		{"order history", http.MethodGet, "/api/v1/tenant/order-history", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},
		{"the audit log", http.MethodGet, "/api/v1/tenant/audit-logs", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},
		{"the dashboard", http.MethodGet, "/api/v1/tenant/dashboard", nil,
			http.StatusOK, http.StatusOK, http.StatusForbidden},

		// Configuration is the owner's alone.
		{"storefront configuration", http.MethodGet, "/api/v1/tenant/storefront", nil,
			http.StatusOK, http.StatusForbidden, http.StatusForbidden},
		{"payment settings", http.MethodGet, "/api/v1/tenant/payment-settings", nil,
			http.StatusOK, http.StatusForbidden, http.StatusForbidden},
		{"integrations", http.MethodGet, "/api/v1/tenant/configurations", nil,
			http.StatusOK, http.StatusForbidden, http.StatusForbidden},
		{"access control", http.MethodGet, "/api/v1/tenant/iam", nil,
			http.StatusOK, http.StatusForbidden, http.StatusForbidden},
	}

	tokens := map[string]string{
		identity.RoleTenantAdmin: tenantUser(env, identity.RoleTenantAdmin, tenantID),
		identity.RoleManager:     tenantUser(env, identity.RoleManager, tenantID),
		identity.RoleStaff:       tenantUser(env, identity.RoleStaff, tenantID),
	}

	for _, tc := range cases {
		expected := map[string]int{
			identity.RoleTenantAdmin: tc.owner,
			identity.RoleManager:     tc.manager,
			identity.RoleStaff:       tc.staff,
		}
		for role, want := range expected {
			status, body := env.doOn(tc.method, tc.path, tokens[role], tc.body, host)
			if status != want {
				t.Errorf("%s as %s: status = %d, want %d (body: %v)",
					tc.label, identity.RoleLabel(role), status, want, body)
			}
		}
	}
}

func TestStaffCannotWriteTheMenuOrTheStorefront(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Write Fence", "write-fence-"+randSuffix())
	host := env.hostFor(tenantID)
	staff := tenantUser(env, identity.RoleStaff, tenantID)
	manager := tenantUser(env, identity.RoleManager, tenantID)

	// A read that is refused would fail the first test; these are the writes
	// that would do real damage if the guard were only in the interface.
	status, body := env.doOn(http.MethodPost, "/api/v1/tenant/categories", staff,
		map[string]any{"name": "Should not exist"}, host)
	env.mustStatus(http.StatusForbidden, status, "staff creating a category", body)

	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/payment-settings", manager,
		map[string]any{"cash_enabled": false}, host)
	env.mustStatus(http.StatusForbidden, status, "a manager rewriting payment settings", body)

	// The manager may still do the job they were hired for.
	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/categories", manager,
		map[string]any{"name": "Manager category " + randSuffix()}, host)
	env.mustStatus(http.StatusCreated, status, "a manager creating a category", body)
}

func TestIamReportsRolesPermissionsAndSessions(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("IAM Shop", "iam-shop-"+randSuffix())
	host := env.hostFor(tenantID)

	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)
	tenantUser(env, identity.RoleManager, tenantID)
	tenantUser(env, identity.RoleStaff, tenantID)

	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/iam", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read IAM", body)

	roles, _ := body["roles"].([]any)
	if len(roles) != 3 {
		t.Fatalf("roles = %d, want owner, manager and staff", len(roles))
	}
	permissions, _ := body["permissions"].([]any)
	if len(permissions) == 0 {
		t.Fatal("no permission catalogue returned; the IAM matrix would be empty")
	}

	users, _ := body["users"].([]any)
	if len(users) < 3 {
		t.Fatalf("users = %d, want at least the three just created", len(users))
	}

	// Owners sort first, and each row carries what that person can reach.
	first, _ := users[0].(map[string]any)
	if first["role"] != identity.RoleTenantAdmin {
		t.Errorf("first row is %v, want the owner", first["role"])
	}
	for _, raw := range users {
		row, _ := raw.(map[string]any)
		held, _ := row["permissions"].([]any)
		if len(held) == 0 {
			t.Errorf("user %v has no permissions listed", row["email"])
		}
		if _, ok := row["active_sessions"]; !ok {
			t.Errorf("user %v has no session count", row["email"])
		}
		if row["role"] == identity.RoleStaff && len(held) != 2 {
			t.Errorf("staff hold %d permissions, want exactly selling and kitchen", len(held))
		}
	}

	summary, _ := body["summary"].(map[string]any)
	if summary["managers"] != float64(1) {
		t.Errorf("summary managers = %v, want 1", summary["managers"])
	}
}

func TestIdentityCarriesItsPermissions(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Identity", "identity-"+randSuffix())
	host := env.hostFor(tenantID)

	// The console draws its navigation from this, so a role with no
	// permissions on /me would render an empty shell.
	for _, role := range []string{identity.RoleTenantAdmin, identity.RoleManager, identity.RoleStaff} {
		token := tenantUser(env, role, tenantID)
		status, body := env.doOn(http.MethodGet, "/api/v1/auth/me", token, nil, host)
		env.mustStatus(http.StatusOK, status, "read identity as "+role, body)

		user, ok := body["user"].(map[string]any)
		if !ok {
			user = body
		}
		permissions, _ := user["permissions"].([]any)
		if len(permissions) == 0 {
			t.Errorf("%s has no permissions on /me", role)
		}
		if user["role_label"] == nil || user["role_label"] == "" {
			t.Errorf("%s has no role_label on /me", role)
		}
	}
}

func TestManagerRoleCanBeAssignedAndIsIsolated(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Assign", "assign-"+randSuffix())
	otherID := env.createTenant("Other", "other-"+randSuffix())
	host := env.hostFor(tenantID)
	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)

	status, body := env.doOn(http.MethodPost, "/api/v1/tenant/users", owner, map[string]any{
		"name":  "New Manager",
		"email": "manager-" + randSuffix() + "@test.local",
		"role":  identity.RoleManager,
	}, host)
	env.mustStatus(http.StatusCreated, status, "invite a manager", body)

	user, _ := body["user"].(map[string]any)
	if user["role"] != identity.RoleManager {
		t.Errorf("role = %v, want MANAGER", user["role"])
	}

	// A manager's token is still confined to its own business: the host check
	// applies to every role equally.
	manager := tenantUser(env, identity.RoleManager, tenantID)
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/products", manager, nil, env.hostFor(otherID))
	if status != http.StatusForbidden {
		t.Errorf("a manager reached another business: status = %d, want 403 (body: %v)", status, body)
	}

	// And an unknown role is refused rather than stored.
	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/users", owner, map[string]any{
		"name":  "Nope",
		"email": "nope-" + randSuffix() + "@test.local",
		"role":  "SUPER_ADMIN",
	}, host)
	env.mustStatus(http.StatusBadRequest, status, "refuse to mint a platform owner inside a business", body)
}

func TestOrderHistoryFiltersAndSummarises(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("History", "history-"+randSuffix())
	host := env.hostFor(tenantID)
	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)

	// Two orders written directly: the history screen reads the record, and
	// this test is about the query rather than the ordering flow.
	seedOrder(t, env, tenantID, 1001, "COMPLETED", "Asha", "9876500001", 250)
	seedOrder(t, env, tenantID, 1002, "CANCELLED", "Bilal", "9876500002", 90)

	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/order-history", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read order history", body)

	summary, _ := body["summary"].(map[string]any)
	if summary["total_orders"] != float64(2) {
		t.Errorf("total_orders = %v, want 2", summary["total_orders"])
	}
	if summary["completed"] != float64(1) || summary["cancelled"] != float64(1) {
		t.Errorf("summary miscounts statuses: %v", summary)
	}
	// Revenue excludes the cancelled order, which is the number an operator
	// would otherwise have to work out by hand.
	if revenue, _ := summary["total_revenue"].(string); revenue != "250" && revenue != "250.00" {
		t.Errorf("total_revenue = %q, want the completed order only", revenue)
	}

	// Filtering by status narrows both the rows and the summary.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/order-history?status=CANCELLED", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "filter by status", body)
	orders, _ := body["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("orders = %d, want only the cancelled one", len(orders))
	}
	row, _ := orders[0].(map[string]any)
	if row["order_number"] != float64(1002) {
		t.Errorf("wrong order returned: %v", row["order_number"])
	}

	// Searching by phone finds the other one.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/order-history?q=9876500001", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "search by phone", body)
	orders, _ = body["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("search returned %d rows, want 1", len(orders))
	}

	// An unknown status is refused rather than silently returning everything,
	// which would read as "there are no cancelled orders".
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/order-history?status=SHIPPED", owner, nil, host)
	env.mustStatus(http.StatusBadRequest, status, "reject an unknown status filter", body)
}

// seedOrder inserts one order for the history tests.
func seedOrder(t *testing.T, env *testEnv, tenantID uuid.UUID, number int, status, name, phone string, total float64) {
	t.Helper()
	_, err := env.server.pool.Exec(context.Background(), `
		INSERT INTO orders (tenant_id, order_number, status, customer_name, customer_phone, total, subtotal)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		tenantID, number, status, name, phone, total)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

func TestSetupChecklistReflectsRealState(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Checklist", "checklist-"+randSuffix())
	host := env.hostFor(tenantID)
	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)

	// A brand new business has done nothing. The earlier version of this
	// endpoint reported business_info and payment as complete regardless,
	// which told an owner they had finished screens they had never opened.
	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/setup", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the checklist", body)

	steps, _ := body["steps"].(map[string]any)
	if steps == nil {
		t.Fatal("no steps in the response")
	}
	for _, key := range []string{"business_info", "menu", "payment", "hours", "storefront", "staff", "launch"} {
		if _, ok := steps[key]; !ok {
			t.Errorf("step %q is missing from the checklist", key)
		}
	}
	if steps["menu"] != false {
		t.Error("a business with no products reports a complete menu")
	}
	if steps["launch"] != false {
		t.Error("an unpublished business reports itself launched")
	}

	required, _ := body["required"].([]any)
	if len(required) == 0 {
		t.Error("no required steps reported, so the console cannot gate publishing")
	}

	// Fill in the business details and the step flips. This is the whole point
	// of deriving the checklist: it has to respond to what actually changed.
	if _, err := env.server.pool.Exec(context.Background(),
		`UPDATE tenants SET phone = '9876500000', address = '1 Test Lane' WHERE id = $1`,
		tenantID); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/setup", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "re-read the checklist", body)
	steps, _ = body["steps"].(map[string]any)
	if steps["business_info"] != true {
		t.Error("business details were filled in but the step did not complete")
	}

	// Virgin hours must stay incomplete; always-open after an explicit save completes.
	if steps["hours"] == true {
		t.Error("a brand-new shop must not report hours as complete")
	}
	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/storefront/hours", owner, map[string]any{
		"always_open": true,
		"timezone":    "Asia/Kolkata",
		"schedule":    map[string]any{},
	}, host)
	env.mustStatus(http.StatusOK, status, "save always-open hours", body)
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/setup", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "checklist after hours", body)
	steps, _ = body["steps"].(map[string]any)
	if steps["hours"] != true {
		t.Error("explicit always-open hours should complete the hours step")
	}
}

func TestSetupBusinessInfoFromStorefrontIdentity(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("SF Checklist", "sf-check-"+randSuffix())
	host := env.hostFor(tenantID)
	owner := tenantUser(env, identity.RoleTenantAdmin, tenantID)

	status, body := env.doOn(http.MethodPut, "/api/v1/tenant/storefront", owner, map[string]any{
		"phone":   "9876500001",
		"address": "2 Storefront Lane",
	}, host)
	env.mustStatus(http.StatusOK, status, "save storefront identity", body)

	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/setup", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read checklist", body)
	steps, _ := body["steps"].(map[string]any)
	if steps["business_info"] != true {
		t.Error("storefront phone+address should complete business_info")
	}
}
