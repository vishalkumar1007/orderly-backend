package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Inventory, Food first (Release V1 §3.6). Every behaviour the console and the
// storefront rely on, through the real router and database.

// newStockShop is a published shop on a plan that includes inventory.
// The plan is created first so its cleanup runs after the tenant's.
func newStockShop(t *testing.T, env *testEnv, features ...string) *shopFixture {
	t.Helper()
	ctx := context.Background()
	var planID uuid.UUID
	if err := env.server.pool.QueryRow(ctx,
		`INSERT INTO plans (name, price, max_staff, max_products, entitlements)
		 VALUES ($1, 100000, 20, 500, $2) RETURNING id`,
		"QA-"+strings.ToUpper(randSuffix()), features).Scan(&planID); err != nil {
		t.Fatalf("create test plan: %v", err)
	}
	t.Cleanup(func() { _, _ = env.server.pool.Exec(ctx, `DELETE FROM plans WHERE id = $1`, planID) })
	shop := newShop(t, env)
	if _, err := env.server.pool.Exec(ctx, `UPDATE tenants SET plan_id = $1 WHERE id = $2`, planID, shop.tenantID); err != nil {
		t.Fatal(err)
	}
	return shop
}

func stockShop(t *testing.T, env *testEnv) *shopFixture {
	return newStockShop(t, env, "ANALYTICS", "INVENTORY")
}

func (s *shopFixture) track(t *testing.T, productID string, body map[string]any) map[string]any {
	t.Helper()
	status, resp := s.admin(http.MethodPut, "/api/v1/tenant/inventory/"+productID, body)
	s.env.mustStatus(http.StatusOK, status, "track product", resp)
	return resp
}

func (s *shopFixture) adjust(productID, reason string, qty int, note string) (int, map[string]any) {
	return s.admin(http.MethodPost, "/api/v1/tenant/inventory/"+productID+"/adjust",
		map[string]any{"reason": reason, "quantity": qty, "note": note})
}

// stock reads a product's tracked quantity straight from the database.
func (s *shopFixture) stock(t *testing.T, productID string) int {
	t.Helper()
	var n int
	if err := s.env.server.pool.QueryRow(tctx(),
		`SELECT quantity FROM inventory_items WHERE product_id = $1`, productID).Scan(&n); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return n
}

func (s *shopFixture) productVisible(t *testing.T, productID string) (available, autoDisabled bool) {
	t.Helper()
	if err := s.env.server.pool.QueryRow(tctx(),
		`SELECT p.is_available, COALESCE(i.auto_disabled, FALSE)
		 FROM products p LEFT JOIN inventory_items i ON i.product_id = p.id WHERE p.id = $1`,
		productID).Scan(&available, &autoDisabled); err != nil {
		t.Fatal(err)
	}
	return
}

// placeQty places a guest order for qty of the shop's product.
func (s *shopFixture) placeQty(t *testing.T, qty int) (int, map[string]any) {
	t.Helper()
	return s.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		// A fresh phone each time: identical back-to-back orders are (rightly)
		// refused as duplicates, which is not what these tests are about.
		"customer_name": "Asha Rao", "customer_phone": "98" + randDigits(8), "payment_method": "CASH",
		"client_token": "stock-" + randSuffix(),
		"items":        []map[string]any{{"product_id": s.productID, "quantity": qty}},
	})
}

func (s *shopFixture) orderIDOf(t *testing.T, created map[string]any) string {
	t.Helper()
	return s.env.orderID(s.tenantID, number(t, created, "order_number"))
}

func (s *shopFixture) act(orderID, action string) (int, map[string]any) {
	return s.admin(http.MethodPost, "/api/v1/tenant/orders/"+orderID+"/"+action, nil)
}

func (s *shopFixture) movementReasons(t *testing.T, productID string) []string {
	t.Helper()
	rows, err := s.env.server.pool.Query(tctx(),
		`SELECT m.reason FROM inventory_movements m JOIN inventory_items i ON i.id = m.item_id
		 WHERE i.product_id = $1 ORDER BY m.created_at, m.id`, productID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	return out
}

func TestInventoryIsGatedByPlan(t *testing.T) {
	env := newTestEnv(t)
	basic := newStockShop(t, env, "ANALYTICS") // no INVENTORY
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tenant/inventory"},
		{http.MethodPut, "/api/v1/tenant/inventory/" + basic.productID},
		{http.MethodPost, "/api/v1/tenant/inventory/" + basic.productID + "/adjust"},
		{http.MethodGet, "/api/v1/tenant/inventory/" + basic.productID + "/movements"},
	} {
		status, body := basic.admin(c.method, c.path, map[string]any{})
		if status != http.StatusForbidden || errCode(body) != "feature_not_in_plan" {
			t.Errorf("%s %s on a plan without inventory = %d %s, want 403 feature_not_in_plan",
				c.method, c.path, status, errCode(body))
		}
		if errDetail(body, "feature") != "INVENTORY" {
			t.Errorf("%s %s: details.feature = %v, want INVENTORY", c.method, c.path, errDetail(body, "feature"))
		}
	}
	full := stockShop(t, env)
	status, body := full.admin(http.MethodGet, "/api/v1/tenant/inventory", nil)
	env.mustStatus(http.StatusOK, status, "inventory on a plan that includes it", body)

	// Staff without the menu permission cannot reach it even on a good plan.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/inventory", full.staffToken, nil, full.host)
	env.mustStatus(http.StatusForbidden, status, "staff without the menu permission", body)
}

func TestTrackAdjustAndLedger(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)

	// Untracked until the owner says otherwise.
	_, body := shop.admin(http.MethodGet, "/api/v1/tenant/inventory", nil)
	items, _ := body["items"].([]any)
	first, _ := items[0].(map[string]any)
	if first["tracked"] != false || first["status"] != "UNTRACKED" {
		t.Fatalf("a new product should be UNTRACKED, got %v", first)
	}

	resp := shop.track(t, shop.productID, map[string]any{"quantity": 10, "unit": "plates", "reorder_threshold": 3})
	if resp["quantity"] != float64(10) || resp["unit"] != "plates" || resp["status"] != "OK" {
		t.Errorf("tracked item = %v, want 10 plates OK", resp)
	}

	status, body := shop.adjust(shop.productID, "ADD", 5, "delivery")
	env.mustStatus(http.StatusOK, status, "add stock", body)
	if body["quantity"] != float64(15) {
		t.Errorf("quantity after ADD = %v, want 15", body["quantity"])
	}
	status, body = shop.adjust(shop.productID, "WASTE", 20, "")
	env.mustStatus(http.StatusConflict, status, "waste more than is in stock", body)
	if errCode(body) != "insufficient_stock" || shop.stock(t, shop.productID) != 15 {
		t.Errorf("an over-removal must be refused and change nothing (code %q, stock %d)", errCode(body), shop.stock(t, shop.productID))
	}
	status, body = shop.adjust(shop.productID, "WASTE", 2, "dropped tray")
	env.mustStatus(http.StatusOK, status, "waste", body)
	status, body = shop.adjust(shop.productID, "CORRECTION", 13, "")
	env.mustStatus(http.StatusBadRequest, status, "a correction to the same number", body)
	if errCode(body) != "no_change" {
		t.Errorf("code = %q, want no_change", errCode(body))
	}
	status, body = shop.adjust(shop.productID, "CORRECTION", 20, "stock take")
	env.mustStatus(http.StatusOK, status, "correction", body)

	if got := fmt.Sprint(shop.movementReasons(t, shop.productID)); got != "[INITIAL ADD WASTE CORRECTION]" {
		t.Errorf("ledger reasons = %s, want [INITIAL ADD WASTE CORRECTION]", got)
	}
	status, body = shop.admin(http.MethodGet, "/api/v1/tenant/inventory/"+shop.productID+"/movements", nil)
	env.mustStatus(http.StatusOK, status, "movements", body)
	moves, _ := body["movements"].([]any)
	newest, _ := moves[0].(map[string]any)
	if newest["reason"] != "CORRECTION" || newest["delta"] != float64(7) || newest["quantity_after"] != float64(20) || newest["note"] != "stock take" {
		t.Errorf("newest movement = %v, want CORRECTION +7 → 20 'stock take'", newest)
	}
	if newest["actor"] == "" {
		t.Error("every movement records who made it")
	}

	// Validation.
	for label, payload := range map[string]map[string]any{
		"unknown reason": {"reason": "STEAL", "quantity": 1},
		"zero quantity":  {"reason": "ADD", "quantity": 0},
		"negative total": {"reason": "CORRECTION", "quantity": -1},
		"absurd amount":  {"reason": "ADD", "quantity": 99999999},
	} {
		status, body := shop.admin(http.MethodPost, "/api/v1/tenant/inventory/"+shop.productID+"/adjust", payload)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%v)", label, status, body["__raw"])
		}
	}
	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/inventory/"+shop.productID, map[string]any{"reorder_threshold": -4})
	env.mustStatus(http.StatusBadRequest, status, "negative reorder level", body)
}

func TestAdjustingAnUntrackedOrUnknownProduct(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	status, body := shop.adjust(shop.productID, "ADD", 1, "")
	env.mustStatus(http.StatusNotFound, status, "adjust an untracked product", body)
	if errCode(body) != "not_tracked" {
		t.Errorf("code = %q, want not_tracked", errCode(body))
	}
	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/inventory/"+uuid.NewString(), map[string]any{"quantity": 1})
	env.mustStatus(http.StatusNotFound, status, "track a product that does not exist", body)
	status, body = shop.admin(http.MethodPut, "/api/v1/tenant/inventory/not-a-uuid", map[string]any{})
	env.mustStatus(http.StatusBadRequest, status, "track with a bad id", body)
}

func TestInventoryNeverCrossesTenants(t *testing.T) {
	env := newTestEnv(t)
	a, b := stockShop(t, env), stockShop(t, env)
	a.track(t, a.productID, map[string]any{"quantity": 5})

	status, body := b.admin(http.MethodPut, "/api/v1/tenant/inventory/"+a.productID, map[string]any{"quantity": 99})
	env.mustStatus(http.StatusNotFound, status, "B tracks A's product", body)
	status, body = b.adjust(a.productID, "ADD", 1, "")
	env.mustStatus(http.StatusNotFound, status, "B adjusts A's product", body)
	status, body = b.admin(http.MethodGet, "/api/v1/tenant/inventory/"+a.productID+"/movements", nil)
	env.mustStatus(http.StatusNotFound, status, "B reads A's ledger", body)
	_, body = b.admin(http.MethodGet, "/api/v1/tenant/inventory", nil)
	if raw := str(t, body, "__raw"); strings.Contains(raw, a.productID) {
		t.Errorf("B's inventory lists A's product: %s", raw)
	}
	if a.stock(t, a.productID) != 5 {
		t.Error("A's stock changed")
	}
}

// D2: stock leaves on acceptance, not placement, and comes back on cancellation.
func TestStockFollowsTheOrder(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 5})

	status, created := shop.placeQty(t, 2)
	env.mustStatus(http.StatusCreated, status, "place an order for 2", created)
	id := shop.orderIDOf(t, created)
	if got := shop.stock(t, shop.productID); got != 5 {
		t.Errorf("stock after placement = %d, want 5 (stock leaves on acceptance)", got)
	}

	status, body := shop.act(id, "accept")
	env.mustStatus(http.StatusOK, status, "accept", body)
	if got := shop.stock(t, shop.productID); got != 3 {
		t.Errorf("stock after acceptance = %d, want 3", got)
	}
	_, hist := shop.admin(http.MethodGet, "/api/v1/tenant/inventory/"+shop.productID+"/movements", nil)
	moves, _ := hist["movements"].([]any)
	latest, _ := moves[0].(map[string]any)
	if latest["reason"] != "ORDER" || latest["delta"] != float64(-2) || latest["order_number"] != created["order_number"] {
		t.Errorf("ledger = %v, want ORDER -2 tied to order #%v", latest, created["order_number"])
	}

	// Cancel from ACCEPTED gives the stock back, exactly once, however many taps.
	got := race(6, func(int) int { s, _ := shop.act(id, "cancel"); return s })
	wantOneWinner(t, "6 × cancel", got, 6)
	if got := shop.stock(t, shop.productID); got != 5 {
		t.Errorf("stock after cancel = %d, want 5 (restored once)", got)
	}
	if r := fmt.Sprint(shop.movementReasons(t, shop.productID)); r != "[INITIAL ORDER CANCEL]" {
		t.Errorf("ledger = %s, want [INITIAL ORDER CANCEL]", r)
	}

	// A PENDING order never took stock, so cancelling it returns nothing.
	_, created = shop.placeQty(t, 1)
	id = shop.orderIDOf(t, created)
	if status, body := shop.act(id, "cancel"); status != http.StatusOK {
		t.Fatalf("cancel pending: %d %v", status, body["__raw"])
	}
	if got := shop.stock(t, shop.productID); got != 5 {
		t.Errorf("stock after cancelling a PENDING order = %d, want 5", got)
	}
	if r := fmt.Sprint(shop.movementReasons(t, shop.productID)); r != "[INITIAL ORDER CANCEL]" {
		t.Errorf("a PENDING cancel wrote a ledger row: %s", r)
	}
}

func TestCartAskingForMoreThanIsInStockIsRefused(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 2})

	status, body := shop.placeQty(t, 3)
	env.mustStatus(http.StatusConflict, status, "3 of 2", body)
	if errCode(body) != "out_of_stock" {
		t.Errorf("code = %q, want out_of_stock", errCode(body))
	}
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "Only 2 left") {
		t.Errorf("message = %q, want it to say 'Only 2 left'", msg)
	}
	if env.countOrders(shop.tenantID) != 0 {
		t.Error("a refused cart created an order")
	}
	status, body = shop.placeQty(t, 2)
	env.mustStatus(http.StatusCreated, status, "exactly what is in stock", body)
}

// I5: zero stock hides the product; restocking shows it again.
func TestSoldOutProductDisappearsAndReturns(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 2})

	_, created := shop.placeQty(t, 2)
	if status, body := shop.act(shop.orderIDOf(t, created), "accept"); status != http.StatusOK {
		t.Fatalf("accept: %d %v", status, body["__raw"])
	}
	avail, auto := shop.productVisible(t, shop.productID)
	if avail || !auto {
		t.Errorf("after selling out: available=%v auto_disabled=%v, want hidden by stock", avail, auto)
	}
	_, menu := shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	if strings.Contains(str(t, menu, "__raw"), shop.productName) {
		t.Error("a sold-out product is still on the public menu")
	}
	status, _ := shop.placeQty(t, 1)
	if status == http.StatusCreated {
		t.Error("ordered a sold-out product")
	}

	status, body := shop.adjust(shop.productID, "ADD", 4, "restock")
	env.mustStatus(http.StatusOK, status, "restock", body)
	avail, auto = shop.productVisible(t, shop.productID)
	if !avail || auto {
		t.Errorf("after restocking: available=%v auto_disabled=%v, want visible", avail, auto)
	}
	_, menu = shop.guest(http.MethodGet, "/api/v1/public/menu", nil)
	if !strings.Contains(str(t, menu, "__raw"), shop.productName) {
		t.Error("a restocked product did not come back on the public menu")
	}
}

// A product the owner hid by hand stays hidden through restocks.
func TestRestockDoesNotOverrideAnOwnersChoice(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 1})
	patch := func(available bool) {
		t.Helper()
		status, body := shop.admin(http.MethodPatch, "/api/v1/tenant/products/"+shop.productID, map[string]any{"is_available": available})
		env.mustStatus(http.StatusOK, status, "set availability", body)
	}

	patch(false)                                // the owner hides it (e.g. kitchen is out of an ingredient)
	shop.adjust(shop.productID, "WASTE", 1, "") // stock hits zero while already hidden
	shop.adjust(shop.productID, "ADD", 5, "")
	if avail, _ := shop.productVisible(t, shop.productID); avail {
		t.Error("a restock re-showed a product the owner had hidden")
	}

	// Stock hides it, the owner shows it by hand, then hides it by hand: a later
	// restock must not undo the owner's last word.
	patch(true)
	shop.adjust(shop.productID, "CORRECTION", 0, "")
	if avail, auto := shop.productVisible(t, shop.productID); avail || !auto {
		t.Fatalf("zero stock should hide it (available=%v auto=%v)", avail, auto)
	}
	patch(true)
	patch(false)
	shop.adjust(shop.productID, "ADD", 3, "")
	if avail, _ := shop.productVisible(t, shop.productID); avail {
		t.Error("the owner's manual hide was overridden by a restock")
	}
}

// Two orders for the last unit: both pass the checkout pre-check, only one can be accepted.
func TestTheLastUnitCannotBeSoldTwice(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 1})

	var ids []string
	for i := 0; i < 2; i++ {
		status, created := shop.placeQty(t, 1)
		env.mustStatus(http.StatusCreated, status, fmt.Sprintf("order %d", i+1), created)
		ids = append(ids, shop.orderIDOf(t, created))
	}
	var codes codeList
	got := race(2, func(i int) int {
		status, body := shop.act(ids[i], "accept")
		if status == http.StatusConflict {
			codes.add(errCode(body))
		}
		return status
	})
	wantOneWinner(t, "accept both orders for the last unit", got, 2)
	if len(codes.list) != 1 || codes.list[0] != "insufficient_stock" {
		t.Errorf("the loser's error codes = %v, want [insufficient_stock]", codes.list)
	}
	if shop.stock(t, shop.productID) != 0 {
		t.Errorf("stock = %d, want 0 (never negative)", shop.stock(t, shop.productID))
	}
	var pending int
	_ = env.server.pool.QueryRow(tctx(), `SELECT count(*) FROM orders WHERE tenant_id = $1 AND status = 'PENDING'`, shop.tenantID).Scan(&pending)
	if pending != 1 {
		t.Errorf("%d orders still PENDING, want 1 (the refused one waits for staff)", pending)
	}
}

func TestAutoAcceptDeductsAtPlacementAndNeverOversells(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow", map[string]any{"acceptance_mode": "AUTO"})
	env.mustStatus(http.StatusOK, status, "automatic acceptance", body)
	shop.track(t, shop.productID, map[string]any{"quantity": 3})

	status, created := shop.placeQty(t, 2)
	env.mustStatus(http.StatusCreated, status, "auto-accepted order", created)
	if str(t, created, "status") != "ACCEPTED" || shop.stock(t, shop.productID) != 1 {
		t.Errorf("status %q stock %d, want ACCEPTED and 1 left", str(t, created, "status"), shop.stock(t, shop.productID))
	}
	// Six people grab the last unit at once: exactly one order exists for it.
	got := race(6, func(int) int { s, _ := shop.placeQty(t, 1); return s })
	// The losers are refused cleanly: 409 (stock ran out inside the transaction)
	// or 400 (the product was already hidden by the sale that won). Never a 5xx.
	if got[http.StatusCreated] != 1 || got[http.StatusCreated]+got[http.StatusConflict]+got[http.StatusBadRequest] != 6 {
		t.Errorf("racing for the last unit: %v, want one 201 and five clean refusals", got)
	}
	if shop.stock(t, shop.productID) != 0 {
		t.Errorf("stock = %d, want 0", shop.stock(t, shop.productID))
	}
	if n := env.countOrders(shop.tenantID); n != 2 {
		t.Errorf("%d orders exist, want 2 (a refused order leaves nothing behind)", n)
	}
}

func TestLowStockAlertFiresOnCrossingOnly(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 6, "reorder_threshold": 3})
	alerts := func() int {
		var n int
		if err := env.server.pool.QueryRow(tctx(),
			`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND event_code = 'INVENTORY_LOW'`, shop.tenantID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sell := func(qty int) {
		t.Helper()
		_, created := shop.placeQty(t, qty)
		if status, body := shop.act(shop.orderIDOf(t, created), "accept"); status != http.StatusOK {
			t.Fatalf("accept: %d %v", status, body["__raw"])
		}
	}

	sell(2) // 6 → 4, above the level
	if alerts() != 0 {
		t.Fatal("alerted while still above the reorder level")
	}
	sell(1) // 4 → 3: reaches the level
	if alerts() != 1 {
		t.Fatalf("alerts = %d after reaching the reorder level, want 1", alerts())
	}
	sell(1) // 3 → 2: already low, no repeat
	if alerts() != 1 {
		t.Errorf("alerts = %d after selling further below the level, want still 1", alerts())
	}
	shop.adjust(shop.productID, "ADD", 10, "")
	sell(10) // 12 → 2: crosses again after a restock
	if alerts() != 2 {
		t.Errorf("alerts = %d after crossing again, want 2", alerts())
	}
	// A manual write-off that drops below the level alerts too.
	shop.adjust(shop.productID, "ADD", 10, "")
	shop.adjust(shop.productID, "WASTE", 10, "spoiled")
	if alerts() != 3 {
		t.Errorf("alerts = %d after a write-off across the level, want 3", alerts())
	}
}

func TestPausedTrackingStopsBlockingAndHidingOrders(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 1})
	shop.adjust(shop.productID, "WASTE", 1, "") // sold out: hidden by stock
	if avail, _ := shop.productVisible(t, shop.productID); avail {
		t.Fatal("setup: expected the product hidden")
	}

	resp := shop.track(t, shop.productID, map[string]any{"track": false})
	if resp["status"] != "PAUSED" {
		t.Errorf("status = %v, want PAUSED", resp["status"])
	}
	if avail, auto := shop.productVisible(t, shop.productID); !avail || auto {
		t.Errorf("pausing should hand the product back (available=%v auto=%v)", avail, auto)
	}
	status, body := shop.placeQty(t, 5)
	env.mustStatus(http.StatusCreated, status, "ordering while tracking is paused", body)
	if shop.stock(t, shop.productID) != 0 {
		t.Error("a paused item still moved stock")
	}

	// Resuming at zero hides it again.
	resp = shop.track(t, shop.productID, map[string]any{"track": true})
	if avail, auto := shop.productVisible(t, shop.productID); avail || !auto {
		t.Errorf("resuming at zero should hide it (available=%v auto=%v) %v", avail, auto, resp["status"])
	}
}

func TestTrackingAtZeroHidesTheProductUntilStocked(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 0})
	if avail, auto := shop.productVisible(t, shop.productID); avail || !auto {
		t.Errorf("tracked with no stock: available=%v auto=%v, want hidden by stock", avail, auto)
	}
	shop.adjust(shop.productID, "ADD", 1, "")
	if avail, _ := shop.productVisible(t, shop.productID); !avail {
		t.Error("first stock should show the product")
	}
}

// After a downgrade the owner can no longer manage stock, so stock rules must
// stop applying: orders flow, nothing is hidden or deducted, but stock already
// taken by an accepted order is still returned on cancellation.
func TestDowngradedTenantStopsStockRules(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	var planID uuid.UUID
	shop := stockShop(t, env)
	if err := env.server.pool.QueryRow(ctx, `SELECT plan_id FROM tenants WHERE id = $1`, shop.tenantID).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	shop.track(t, shop.productID, map[string]any{"quantity": 3})
	_, created := shop.placeQty(t, 1)
	accepted := shop.orderIDOf(t, created)
	shop.act(accepted, "accept") // took 1 → 2 left

	if _, err := env.server.pool.Exec(ctx, `UPDATE plans SET entitlements = ARRAY['ANALYTICS'] WHERE id = $1`, planID); err != nil {
		t.Fatal(err)
	}
	status, body := shop.placeQty(t, 9) // more than the 2 on record
	env.mustStatus(http.StatusCreated, status, "order beyond recorded stock after a downgrade", body)
	_, over := shop.placeQty(t, 1)
	if st, b := shop.act(shop.orderIDOf(t, over), "accept"); st != http.StatusOK {
		t.Fatalf("accept after downgrade: %d %v", st, b["__raw"])
	}
	if shop.stock(t, shop.productID) != 2 {
		t.Errorf("stock = %d, want 2 (no deduction after a downgrade)", shop.stock(t, shop.productID))
	}
	if st, b := shop.act(accepted, "cancel"); st != http.StatusOK {
		t.Fatalf("cancel: %d %v", st, b["__raw"])
	}
	if shop.stock(t, shop.productID) != 3 {
		t.Errorf("stock = %d, want 3 (stock an order took is returned even after a downgrade)", shop.stock(t, shop.productID))
	}
}

func TestUntrackedProductsAreUnaffected(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	for i := 0; i < 3; i++ {
		status, body := shop.placeQty(t, 4)
		env.mustStatus(http.StatusCreated, status, "untracked product order", body)
	}
	var n int
	_ = env.server.pool.QueryRow(tctx(), `SELECT count(*) FROM inventory_items WHERE tenant_id = $1`, shop.tenantID).Scan(&n)
	if n != 0 {
		t.Errorf("%d inventory rows exist for a shop that tracks nothing", n)
	}
}

// codeList collects error codes from racing goroutines.
type codeList struct {
	mu   sync.Mutex
	list []string
}

func (c *codeList) add(code string) {
	c.mu.Lock()
	c.list = append(c.list, code)
	c.mu.Unlock()
}

func randDigits(n int) string {
	id := uuid.New()
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteByte('0' + id[i%16]%10)
	}
	return b.String()
}

// A payment landing for an order the shop cannot fill must not fail the payment:
// the order just waits PENDING for staff.
func TestAutoAcceptAfterPaymentWithoutStockLeavesTheOrderPending(t *testing.T) {
	env := newTestEnv(t)
	shop := stockShop(t, env)
	shop.track(t, shop.productID, map[string]any{"quantity": 1})

	// Two orders placed while acceptance is manual (both pass the pre-check).
	_, first := shop.placeQty(t, 1)
	_, second := shop.placeQty(t, 1)
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow", map[string]any{"acceptance_mode": "AUTO"})
	env.mustStatus(http.StatusOK, status, "automatic acceptance", body)

	autoAccept := func(created map[string]any) (string, error) {
		row, err := env.server.orders.Queries().GetOrderByID(tctx(), sqlcGetOrder(shop, shop.orderIDOf(t, created)))
		if err != nil {
			t.Fatal(err)
		}
		view, err := env.server.orders.AutoAcceptIfNeeded(tctx(), shop.tenantID, row)
		return view.Status, err
	}
	if got, err := autoAccept(first); err != nil || got != "ACCEPTED" {
		t.Fatalf("first order: status %q err %v, want ACCEPTED", got, err)
	}
	got, err := autoAccept(second)
	if err != nil {
		t.Fatalf("a paid order without stock must not return an error: %v", err)
	}
	if got != "PENDING" {
		t.Errorf("second order status = %q, want PENDING (waits for staff)", got)
	}
	if shop.stock(t, shop.productID) != 0 {
		t.Errorf("stock = %d, want 0", shop.stock(t, shop.productID))
	}
}

func sqlcGetOrder(shop *shopFixture, orderID string) sqlc.GetOrderByIDParams {
	return sqlc.GetOrderByIDParams{ID: pgutil.UUID(uuid.MustParse(orderID)), TenantID: pgutil.UUID(shop.tenantID)}
}
