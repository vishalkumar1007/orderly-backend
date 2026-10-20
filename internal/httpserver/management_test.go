package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Shop "Management": menu fixes, customer profile, staff monitoring and
// attendance, end to end through the real router and database.

func TestMenuPartialPatchKeepsDescriptionAndCategoryIsScoped(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	path := "/api/v1/tenant/products/" + shop.productID

	status, body := shop.admin(http.MethodPatch, path, map[string]any{"description": "Steamed, with chutney"})
	env.mustStatus(http.StatusOK, status, "set description", body)
	status, body = shop.admin(http.MethodPatch, path, map[string]any{"is_available": false})
	env.mustStatus(http.StatusOK, status, "toggle availability", body)
	if body["description"] != "Steamed, with chutney" {
		t.Fatalf("a partial PATCH wiped the description: %v", body["description"])
	}

	other := newShop(t, env)
	status, body = shop.admin(http.MethodPatch, path, map[string]any{"category_id": other.categoryID})
	env.mustStatus(http.StatusBadRequest, status, "move into another shop's category", body)

	status, body = shop.admin(http.MethodPatch, "/api/v1/tenant/products/"+uuid.NewString(), map[string]any{"name": "Ghost"})
	env.mustStatus(http.StatusNotFound, status, "update a missing product", body)
}

func TestDeletingAnOrderedProductArchivesIt(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	shop.order(t, "Asha Rao", "9876543210", "CASH")

	status, body := shop.admin(http.MethodDelete, "/api/v1/tenant/products/"+shop.productID, nil)
	env.mustStatus(http.StatusOK, status, "delete ordered product", body)
	if body["status"] != "archived" {
		t.Fatalf("an ordered product should be archived, got %v", body["status"])
	}
	_, body = shop.admin(http.MethodGet, "/api/v1/tenant/products", nil)
	if products, _ := body["products"].([]any); len(products) != 0 {
		t.Fatalf("archived product still listed: %v", products)
	}

	// A never-ordered product is really deleted.
	catID := uuid.MustParse(shop.categoryID)
	fresh := env.createProduct(shop.tenantID, catID, "Fresh", 50)
	status, body = shop.admin(http.MethodDelete, "/api/v1/tenant/products/"+fresh.String(), nil)
	env.mustStatus(http.StatusOK, status, "delete fresh product", body)
	if body["status"] != "deleted" {
		t.Fatalf("an unordered product should be deleted, got %v", body["status"])
	}
}

func TestBulkProductsAndReorderStayInTheShop(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	catID := uuid.MustParse(shop.categoryID)
	second := env.createProduct(shop.tenantID, catID, "Fried Momo", 140)
	other := newShop(t, env)

	ids := []string{shop.productID, second.String(), other.productID}
	status, body := shop.admin(http.MethodPost, "/api/v1/tenant/products/bulk", map[string]any{"ids": ids, "action": "unavailable"})
	env.mustStatus(http.StatusOK, status, "bulk sold out", body)
	if n := number(t, body, "updated"); n != 2 {
		t.Fatalf("bulk touched %d products, want 2 (the foreign id must be skipped)", n)
	}
	var available bool
	if err := env.server.pool.QueryRow(tctx(), `SELECT is_available FROM products WHERE id = $1`, other.productID).Scan(&available); err != nil || !available {
		t.Fatalf("another shop's product was changed: available=%v err=%v", available, err)
	}

	status, body = shop.admin(http.MethodPost, "/api/v1/tenant/products/bulk", map[string]any{"ids": ids[:1], "action": "move", "category_id": other.categoryID})
	env.mustStatus(http.StatusBadRequest, status, "bulk move into a foreign category", body)

	status, body = shop.admin(http.MethodPost, "/api/v1/tenant/products/reorder", map[string]any{"ids": []string{second.String(), shop.productID}})
	env.mustStatus(http.StatusOK, status, "reorder", body)
	var first string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT id::text FROM products WHERE tenant_id = $1 ORDER BY sort_order, name LIMIT 1`, shop.tenantID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != second.String() {
		t.Fatalf("reorder did not apply: first is %s", first)
	}
}

func TestCustomerListSearchPagingAndActivity(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	env.verifyOTP(t, shop, "9876500001")
	shop.order(t, "Meera", "9876500001", "CASH")
	shop.order(t, "Guest Ravi", "9876500002", "CASH")

	status, body := shop.admin(http.MethodGet, "/api/v1/tenant/customers", nil)
	env.mustStatus(http.StatusOK, status, "list customers", body)
	counts, _ := body["counts"].(map[string]any)
	if counts["registered"] != 1.0 || counts["guest"] != 1.0 {
		t.Fatalf("counts = %v, want 1 registered and 1 guest", counts)
	}

	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/customers?q=ravi", nil)
	env.mustStatus(http.StatusOK, status, "search customers", body)
	list, _ := body["customers"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "Guest Ravi" {
		t.Fatalf("search for ravi returned %v", list)
	}

	_, body = shop.admin(http.MethodGet, "/api/v1/tenant/customers?limit=1", nil)
	if more, _ := body["has_more"].(bool); !more {
		t.Fatalf("limit=1 over two customers should report has_more: %v", body)
	}

	var customerID string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT id::text FROM customers WHERE tenant_id = $1 AND phone LIKE '%9876500001'`, shop.tenantID).Scan(&customerID); err != nil {
		t.Fatalf("registered customer: %v", err)
	}
	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/customers/"+customerID, nil)
	env.mustStatus(http.StatusOK, status, "customer profile", body)
	cust, _ := body["customer"].(map[string]any)
	if _, ok := cust["email"]; !ok {
		t.Fatalf("profile is missing email: %v", cust)
	}
	stats, _ := body["stats"].(map[string]any)
	if stats["order_count"] != 1.0 {
		t.Fatalf("stats = %v, want one order", stats)
	}

	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/customers/"+customerID+"/activity", nil)
	env.mustStatus(http.StatusOK, status, "customer activity", body)
	kinds := map[string]bool{}
	events, _ := body["events"].([]any)
	for _, e := range events {
		kinds[e.(map[string]any)["kind"].(string)] = true
	}
	if !kinds["login"] || !kinds["order"] {
		t.Fatalf("activity should include a login and an order, got %v", kinds)
	}
}

func TestStaffLoginHistorySessionsAndManagerSignIn(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	const password = "correct horse battery staple"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	email := "manager-" + randSuffix() + "@test.local"
	var managerID string
	if err := env.server.pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, name, email, password_hash, role, status)
		 VALUES ($1, 'Floor Manager', $2, $3, 'MANAGER', 'ACTIVE') RETURNING id::text`,
		shop.tenantID, email, string(hash)).Scan(&managerID); err != nil {
		t.Fatalf("create manager: %v", err)
	}

	status, body := env.doOn(http.MethodPost, "/api/v1/auth/tenant/login", "",
		map[string]any{"email": email, "password": password}, shop.host)
	env.mustStatus(http.StatusOK, status, "manager login", body)
	tokens, _ := body["tokens"].(map[string]any)
	refresh, _ := tokens["refresh_token"].(string)

	base := "/api/v1/tenant/users/" + managerID
	status, body = shop.admin(http.MethodGet, base+"/logins", nil)
	env.mustStatus(http.StatusOK, status, "login history", body)
	if logins, _ := body["logins"].([]any); len(logins) == 0 || logins[0].(map[string]any)["result"] != "SUCCESS" {
		t.Fatalf("login history = %v", body["logins"])
	}
	status, body = shop.admin(http.MethodGet, base+"/sessions", nil)
	env.mustStatus(http.StatusOK, status, "sessions", body)
	if sessions, _ := body["sessions"].([]any); len(sessions) != 1 {
		t.Fatalf("sessions = %v, want one", body["sessions"])
	}
	status, body = shop.admin(http.MethodGet, base+"/stats", nil)
	env.mustStatus(http.StatusOK, status, "stats", body)
	if body["last_login_at"] == nil {
		t.Fatalf("last_login_at not set after sign-in: %v", body)
	}

	status, body = shop.admin(http.MethodDelete, base+"/sessions", nil)
	env.mustStatus(http.StatusOK, status, "sign out everywhere", body)
	status, _ = env.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	if status == http.StatusOK {
		t.Fatalf("refresh still works after sign out everywhere")
	}

	// Another shop's owner cannot read this manager's history.
	other := newShop(t, env)
	status, _ = other.admin(http.MethodGet, base+"/logins", nil)
	if status != http.StatusNotFound {
		t.Fatalf("foreign owner read login history: status %d", status)
	}
}

func TestOrdersHandledCountsPerStaffMember(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	created := shop.order(t, "Asha Rao", "9876543210", "CASH")
	orderID := shop.orderIDOf(t, created)
	status, body := shop.act(orderID, "accept")
	env.mustStatus(http.StatusOK, status, "accept", body)

	var adminID string
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT actor_user_id::text FROM order_status_history WHERE order_id = $1 AND to_status = 'ACCEPTED'`, orderID).Scan(&adminID); err != nil {
		t.Fatalf("accept was not attributed to the staff member: %v", err)
	}
	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/users/"+adminID+"/stats", nil)
	env.mustStatus(http.StatusOK, status, "stats", body)
	if body["orders_handled"] != 1.0 {
		t.Fatalf("orders_handled = %v, want 1", body["orders_handled"])
	}

	// The batched order list still carries items and the payment.
	_, body = shop.admin(http.MethodGet, "/api/v1/tenant/orders", nil)
	orders, _ := body["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("orders = %v", orders)
	}
	o := orders[0].(map[string]any)
	if items, _ := o["items"].([]any); len(items) != 1 {
		t.Fatalf("order list lost its items: %v", o)
	}
}

func TestAttendanceGatingPunchesGeofenceAndEdits(t *testing.T) {
	env := newTestEnv(t)

	// Not in the plan: every attendance route is refused.
	plain := newShop(t, env)
	status, body := plain.admin(http.MethodGet, "/api/v1/tenant/attendance/me", nil)
	env.mustStatus(http.StatusForbidden, status, "attendance without the plan feature", body)

	shop := newStockShop(t, env, "ATTENDANCE")
	staff := shop.staffToken
	punch := func(token, action string, req map[string]any) (int, map[string]any) {
		return env.doOn(http.MethodPost, "/api/v1/tenant/attendance/"+action, token, req, shop.host)
	}

	// In the plan but switched off by the shop.
	status, body = punch(staff, "punch-in", nil)
	if status != http.StatusConflict || errCode(body) != "attendance_disabled" {
		t.Fatalf("punch while disabled: %d %v", status, body)
	}

	// Staff cannot change settings or read the timesheet.
	status, _ = env.doOn(http.MethodPut, "/api/v1/tenant/attendance/settings", staff, map[string]any{"enabled": true}, shop.host)
	if status != http.StatusForbidden {
		t.Fatalf("staff changed attendance settings: %d", status)
	}
	status, _ = env.doOn(http.MethodGet, "/api/v1/tenant/attendance", staff, nil, shop.host)
	if status != http.StatusForbidden {
		t.Fatalf("staff read the timesheet: %d", status)
	}

	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/attendance/settings", map[string]any{"enabled": true})
	env.mustStatus(http.StatusOK, status, "enable attendance", body)

	status, body = punch(staff, "punch-in", map[string]any{"note": "morning"})
	env.mustStatus(http.StatusOK, status, "punch in", body)
	status, body = punch(staff, "punch-in", nil)
	if status != http.StatusConflict || errCode(body) != "already_punched_in" {
		t.Fatalf("second punch in: %d %v", status, body)
	}
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/attendance/me", staff, nil, shop.host)
	env.mustStatus(http.StatusOK, status, "me while on shift", body)
	if body["open"] == nil {
		t.Fatalf("open shift missing: %v", body)
	}
	status, body = punch(staff, "punch-out", nil)
	env.mustStatus(http.StatusOK, status, "punch out", body)
	punchID, _ := body["id"].(string)
	status, body = punch(staff, "punch-out", nil)
	if status != http.StatusConflict || errCode(body) != "not_punched_in" {
		t.Fatalf("punch out twice: %d %v", status, body)
	}

	// Location rule: 100 m around the shop.
	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/attendance/settings", map[string]any{
		"enabled": true, "require_location": true, "geo_lat": 12.9716, "geo_lng": 77.5946, "geo_radius_m": 100,
	})
	env.mustStatus(http.StatusOK, status, "set location rule", body)
	status, body = punch(staff, "punch-in", nil)
	if status != http.StatusBadRequest || errCode(body) != "location_required" {
		t.Fatalf("punch without location: %d %v", status, body)
	}
	status, body = punch(staff, "punch-in", map[string]any{"lat": 12.9816, "lng": 77.5946})
	if status != http.StatusForbidden || errCode(body) != "outside_geofence" {
		t.Fatalf("punch 1 km away: %d %v", status, body)
	}
	status, body = punch(staff, "punch-in", map[string]any{"lat": 12.9717, "lng": 77.5946})
	env.mustStatus(http.StatusOK, status, "punch inside the radius", body)

	// The owner sees both punches and corrects the first; the edit is audited.
	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/attendance", nil)
	env.mustStatus(http.StatusOK, status, "timesheet", body)
	if punches, _ := body["punches"].([]any); len(punches) != 2 {
		t.Fatalf("timesheet = %v, want two punches", punches)
	}
	status, body = shop.admin(http.MethodPatch, "/api/v1/tenant/attendance/"+punchID, map[string]any{
		"clock_in_at": "2026-01-05T09:00:00Z", "clock_out_at": "2026-01-05T17:30:00Z", "note": "fixed",
	})
	env.mustStatus(http.StatusOK, status, "edit punch", body)
	var audited int
	if err := env.server.pool.QueryRow(tctx(),
		`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'attendance.edited'`, shop.tenantID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("edit was not audited: %d %v", audited, err)
	}
	status, body = shop.admin(http.MethodPatch, "/api/v1/tenant/attendance/"+punchID, map[string]any{
		"clock_in_at": "2026-01-05T18:00:00Z", "clock_out_at": "2026-01-05T09:00:00Z",
	})
	env.mustStatus(http.StatusBadRequest, status, "punch out before punch in", body)

}
