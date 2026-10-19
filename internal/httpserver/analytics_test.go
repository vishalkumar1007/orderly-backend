package httpserver

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Analytics polish (Release V1 §3.7): trends by day/week/month, best sellers by
// volume and by revenue, and the fulfilment rate: plus proof the response the
// dashboard already depends on is unchanged.

type historicLine struct {
	product uuid.UUID
	name    string
	price   float64
	qty     int
}

var historicOrderNumber = map[uuid.UUID]int{}

// seedOrder writes a historical order straight to the database, `daysAgo`
// days before today at midday UTC (the database and the API both bucket in UTC).
func seedHistoricOrder(t *testing.T, env *testEnv, tenantID uuid.UUID, daysAgo int, status string, lines ...historicLine) {
	t.Helper()
	historicOrderNumber[tenantID]++
	var total float64
	for _, l := range lines {
		total += l.price * float64(l.qty)
	}
	var orderID uuid.UUID
	if err := env.server.pool.QueryRow(tctx(),
		`INSERT INTO orders (tenant_id, order_number, status, subtotal, total, customer_name, customer_phone, created_at)
		 VALUES ($1, $2, $3, $4, $4, 'Seed', '9000000000',
		         date_trunc('day', now() AT TIME ZONE 'utc') + interval '12 hours' - make_interval(days => $5))
		 RETURNING id`, tenantID, historicOrderNumber[tenantID], status, total, daysAgo).Scan(&orderID); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	for _, l := range lines {
		if _, err := env.server.pool.Exec(tctx(),
			`INSERT INTO order_items (tenant_id, order_id, product_id, product_name_snapshot, unit_price, quantity, subtotal)
			 VALUES ($1,$2,$3,$4,$5::numeric,$6::int,$5::numeric * $6::int)`, tenantID, orderID, l.product, l.name, l.price, l.qty); err != nil {
			t.Fatalf("seed order item: %v", err)
		}
	}
}

// analyticsShop seeds a shop with a history where volume and revenue disagree:
//
//	Chai          20 sold  ₹10   = ₹200    (volume leader)
//	Veg Momo      11 sold  ₹120  = ₹1,320
//	Truffle Pasta  4 sold  ₹400  = ₹1,600  (revenue leader)
//
// plus a cancelled order for 50 pastas that must count nowhere.
func analyticsShop(t *testing.T, env *testEnv) *shopFixture {
	t.Helper()
	shop := newShop(t, env)
	cat := uuid.MustParse(shop.categoryID)
	veg := uuid.MustParse(shop.productID)
	pasta := env.createProduct(shop.tenantID, cat, "Truffle Pasta", 400)
	chai := env.createProduct(shop.tenantID, cat, "Chai", 10)

	seedHistoricOrder(t, env, shop.tenantID, 1, "COMPLETED", historicLine{veg, "Veg Momo", 120, 10})
	seedHistoricOrder(t, env, shop.tenantID, 1, "PENDING", historicLine{veg, "Veg Momo", 120, 1})
	seedHistoricOrder(t, env, shop.tenantID, 3, "CANCELLED", historicLine{pasta, "Truffle Pasta", 400, 50})
	seedHistoricOrder(t, env, shop.tenantID, 9, "COMPLETED", historicLine{pasta, "Truffle Pasta", 400, 4})
	seedHistoricOrder(t, env, shop.tenantID, 15, "COMPLETED", historicLine{chai, "Chai", 10, 20})
	return shop
}

func (s *shopFixture) analytics(t *testing.T, query string) map[string]any {
	t.Helper()
	status, body := s.admin(http.MethodGet, "/api/v1/tenant/analytics"+query, nil)
	s.env.mustStatus(http.StatusOK, status, "analytics"+query, body)
	return body
}

func points(t *testing.T, body map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, want an array (a null would break the chart)", key, body[key])
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

func sumPoints(pts []map[string]any) (orders, revenue float64) {
	for _, p := range pts {
		orders += p["order_count"].(float64)
		revenue += p["revenue"].(float64)
	}
	return
}

func names(pts []map[string]any) string {
	var out []string
	for _, p := range pts {
		out = append(out, fmt.Sprint(p["name"]))
	}
	return fmt.Sprint(out)
}

func TestAnalyticsKeepsEveryFieldTheDashboardReads(t *testing.T) {
	env := newTestEnv(t)
	shop := analyticsShop(t, env)
	body := shop.analytics(t, "")
	for _, key := range []string{
		"window", "days", "orders_today", "revenue_today", "orders_7d", "revenue_7d", "orders_30d", "revenue_30d",
		"orders_prev_30d", "revenue_prev_30d", "pending_orders", "preparing_orders", "ready_orders", "completed_today",
		"avg_order_value_30d", "products_available", "products_unavailable", "median_prep_minutes", "avg_prep_minutes",
		"avg_accept_minutes", "completion_rate", "cancellation_rate", "cancelled_today", "orders_by_day",
		"orders_by_hour", "top_products", "top_categories", "payment_mix", "status_breakdown",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("analytics response lost %q: the dashboard reads it", key)
		}
	}
	if body["window"] != "30d" || body["granularity"] != "day" {
		t.Errorf("defaults = window %v granularity %v, want 30d / day", body["window"], body["granularity"])
	}
	if got := len(points(t, body, "orders_by_day")); got != 30 {
		t.Errorf("orders_by_day has %d points, want one per day (30)", got)
	}
}

func TestOrderTrendsByDayWeekAndMonth(t *testing.T) {
	env := newTestEnv(t)
	shop := analyticsShop(t, env)

	day := shop.analytics(t, "?window=30d")
	byDay := points(t, day, "orders_by_day")
	wantOrders, wantRevenue := sumPoints(byDay)
	if wantOrders != 5 || wantRevenue != 3120 {
		t.Fatalf("daily series holds %v orders / ₹%v, want 5 / ₹3120 (cancelled revenue excluded, cancelled orders counted)", wantOrders, wantRevenue)
	}
	period := points(t, day, "orders_by_period")
	if len(period) != len(byDay) {
		t.Errorf("day granularity: %d period points vs %d daily points", len(period), len(byDay))
	}

	for _, g := range []string{"week", "month"} {
		body := shop.analytics(t, "?window=30d&granularity="+g)
		if body["granularity"] != g {
			t.Errorf("granularity = %v, want %s", body["granularity"], g)
		}
		pts := points(t, body, "orders_by_period")
		orders, revenue := sumPoints(pts)
		if orders != wantOrders || revenue != wantRevenue {
			t.Errorf("%s buckets hold %v orders / ₹%v, want exactly the daily totals %v / ₹%v", g, orders, revenue, wantOrders, wantRevenue)
		}
		if len(points(t, body, "orders_by_day")) != 30 {
			t.Errorf("%s: orders_by_day must stay one point per day", g)
		}
		for _, p := range pts {
			d, err := time.Parse("2006-01-02", p["day"].(string))
			if err != nil {
				t.Fatalf("%s bucket start %v is not a date", g, p["day"])
			}
			if g == "week" && d.Weekday() != time.Monday {
				t.Errorf("week bucket %s starts on %s, want Monday", p["day"], d.Weekday())
			}
			if g == "month" && d.Day() != 1 {
				t.Errorf("month bucket %s does not start on the 1st", p["day"])
			}
		}
		if g == "week" && (len(pts) < 4 || len(pts) > 6) {
			t.Errorf("30 days should span 4–6 weeks, got %d buckets", len(pts))
		}
		if g == "month" && (len(pts) < 1 || len(pts) > 2) {
			t.Errorf("30 days should span 1–2 months, got %d buckets", len(pts))
		}
	}

	// A nonsense granularity must not fail the dashboard.
	bad := shop.analytics(t, "?granularity=banana&window=bogus")
	if bad["granularity"] != "day" || bad["window"] != "30d" {
		t.Errorf("bad params fell back to %v / %v, want day / 30d", bad["granularity"], bad["window"])
	}
}

func TestBestSellersByVolumeAndByRevenue(t *testing.T) {
	env := newTestEnv(t)
	shop := analyticsShop(t, env)
	body := shop.analytics(t, "")

	if got := names(points(t, body, "top_products")); got != "[Chai Veg Momo Truffle Pasta]" {
		t.Errorf("by volume = %s, want [Chai Veg Momo Truffle Pasta]", got)
	}
	byRevenue := points(t, body, "top_products_by_revenue")
	if got := names(byRevenue); got != "[Truffle Pasta Veg Momo Chai]" {
		t.Errorf("by revenue = %s, want [Truffle Pasta Veg Momo Chai]", got)
	}
	// The cancelled order for 50 pastas must not leak into either list.
	pasta := byRevenue[0]
	if pasta["units"] != float64(4) || pasta["revenue"] != float64(1600) {
		t.Errorf("Truffle Pasta = %v units / ₹%v, want 4 / ₹1600 (cancelled order excluded)", pasta["units"], pasta["revenue"])
	}
	veg := byRevenue[1]
	if veg["units"] != float64(11) || veg["revenue"] != float64(1320) {
		t.Errorf("Veg Momo = %v units / ₹%v, want 11 / ₹1320 (an in-progress order still counts)", veg["units"], veg["revenue"])
	}
}

func TestFulfilmentRateIsCompletedOverFinished(t *testing.T) {
	env := newTestEnv(t)
	shop := analyticsShop(t, env)

	body := shop.analytics(t, "?window=30d")
	// 3 completed, 1 cancelled, 1 still pending.
	if body["completed_orders"] != float64(3) || body["cancelled_orders"] != float64(1) {
		t.Errorf("completed/cancelled = %v/%v, want 3/1", body["completed_orders"], body["cancelled_orders"])
	}
	if body["fulfilment_rate"] != float64(75) {
		t.Errorf("fulfilment_rate = %v, want 75 (3 of the 4 finished orders)", body["fulfilment_rate"])
	}
	// The older figure keeps its meaning: completed ÷ ALL orders, pending included.
	if body["completion_rate"] != float64(60) || body["cancellation_rate"] != float64(20) {
		t.Errorf("completion/cancellation = %v/%v, want 60/20 (unchanged semantics)", body["completion_rate"], body["cancellation_rate"])
	}

	// 7-day window sees 1 completed + 1 cancelled (+1 pending).
	week := shop.analytics(t, "?window=7d")
	if week["fulfilment_rate"] != float64(50) {
		t.Errorf("7d fulfilment_rate = %v, want 50", week["fulfilment_rate"])
	}
}

func TestAnalyticsForAShopWithNoHistory(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	body := shop.analytics(t, "?granularity=week")
	if body["fulfilment_rate"] != nil {
		t.Errorf("fulfilment_rate = %v with nothing finished, want null", body["fulfilment_rate"])
	}
	for _, key := range []string{"top_products", "top_products_by_revenue", "orders_by_period", "orders_by_day"} {
		points(t, body, key) // fails if null
	}
	if n := len(points(t, body, "top_products_by_revenue")); n != 0 {
		t.Errorf("an empty shop lists %d best sellers", n)
	}
}

func TestAnalyticsNeverMixesShops(t *testing.T) {
	env := newTestEnv(t)
	busy := analyticsShop(t, env)
	quiet := newShop(t, env)
	seedHistoricOrder(t, env, quiet.tenantID, 1, "COMPLETED", historicLine{uuid.MustParse(quiet.productID), "Veg Momo", 120, 2})

	body := quiet.analytics(t, "?window=30d&granularity=month")
	orders, revenue := sumPoints(points(t, body, "orders_by_period"))
	if orders != 1 || revenue != 240 {
		t.Errorf("the quiet shop sees %v orders / ₹%v, want 1 / ₹240: nothing from the busy shop", orders, revenue)
	}
	if body["fulfilment_rate"] != float64(100) || body["completed_orders"] != float64(1) {
		t.Errorf("quiet shop fulfilment = %v (%v completed), want 100", body["fulfilment_rate"], body["completed_orders"])
	}
	if got := names(points(t, body, "top_products_by_revenue")); got != "[Veg Momo]" {
		t.Errorf("quiet shop best sellers = %s, want [Veg Momo]", got)
	}
	_ = busy
}
