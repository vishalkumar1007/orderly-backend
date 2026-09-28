package menu

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// analyticsWindows are the ranges a tenant may ask for. The list is closed so a
// caller cannot request an unbounded scan of the orders table.
var analyticsWindows = map[string]int{
	"7d":  7,
	"14d": 14,
	"30d": 30,
	"90d": 90,
}

// AnalyticsResponse is the tenant dashboard's data payload.
type AnalyticsResponse struct {
	Window string `json:"window"`
	Days   int    `json:"days"`

	OrdersToday  int64   `json:"orders_today"`
	RevenueToday float64 `json:"revenue_today"`
	Orders7d     int64   `json:"orders_7d"`
	Revenue7d    float64 `json:"revenue_7d"`
	Orders30d    int64   `json:"orders_30d"`
	Revenue30d   float64 `json:"revenue_30d"`

	// Previous 30 days, for the trend indicator.
	OrdersPrev30d  int64   `json:"orders_prev_30d"`
	RevenuePrev30d float64 `json:"revenue_prev_30d"`

	PendingOrders   int64 `json:"pending_orders"`
	PreparingOrders int64 `json:"preparing_orders"`
	ReadyOrders     int64 `json:"ready_orders"`
	CompletedToday  int64 `json:"completed_today"`

	AvgOrderValue30d float64 `json:"avg_order_value_30d"`

	ProductsAvailable   int64 `json:"products_available"`
	ProductsUnavailable int64 `json:"products_unavailable"`

	// Operational quality over the window. Median prep time leads because one
	// ticket left open all afternoon should not move the number an owner
	// manages against.
	MedianPrepMinutes float64 `json:"median_prep_minutes"`
	AvgPrepMinutes    float64 `json:"avg_prep_minutes"`
	AvgAcceptMinutes  float64 `json:"avg_accept_minutes"`
	CompletionRate    float64 `json:"completion_rate"`
	CancellationRate  float64 `json:"cancellation_rate"`
	CancelledToday    int64   `json:"cancelled_today"`

	OrdersByDay     []DayPoint      `json:"orders_by_day"`
	OrdersByHour    []HourPoint     `json:"orders_by_hour"`
	TopProducts     []ProductPoint  `json:"top_products"`
	TopCategories   []CategoryPoint `json:"top_categories"`
	PaymentMix      []PaymentPoint  `json:"payment_mix"`
	StatusBreakdown []StatusPoint   `json:"status_breakdown"`
}

// CategoryPoint is a line on the best-selling categories list.
type CategoryPoint struct {
	Name    string  `json:"name"`
	Units   int64   `json:"units"`
	Revenue float64 `json:"revenue"`
}

// PaymentPoint is one way customers paid, and what it was worth.
type PaymentPoint struct {
	Method     string  `json:"method"`
	Status     string  `json:"status"`
	OrderCount int64   `json:"order_count"`
	Revenue    float64 `json:"revenue"`
	/// Share of orders in the window, 0–100, so the bar needs no second pass.
	Share float64 `json:"share"`
}

// DayPoint is one calendar day in the selected window.
type DayPoint struct {
	Day        string  `json:"day"`
	OrderCount int64   `json:"order_count"`
	Revenue    float64 `json:"revenue"`
}

// HourPoint is one hour of the day, averaged across the window.
type HourPoint struct {
	Hour       int     `json:"hour"`
	OrderCount float64 `json:"order_count"`
}

// ProductPoint is a line on the best-sellers list.
type ProductPoint struct {
	Name    string  `json:"name"`
	Units   int64   `json:"units"`
	Revenue float64 `json:"revenue"`
}

// StatusPoint is a slice of the order-status breakdown.
type StatusPoint struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

// Analytics serves the tenant dashboard's charts and headline figures.
//
// GET /api/v1/tenant/analytics?window=30d
func (h *Handler) Analytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant := pgutil.UUID(tenantID(r))

	window := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("window")))
	days, ok := analyticsWindows[window]
	if !ok {
		// Default rather than error: the dashboard should never fail to render
		// because of a stale or hand-edited query string.
		window, days = "30d", analyticsWindows["30d"]
	}
	// The queries take the window as a Postgres interval so the filter can use
	// the column index; a plain integer would defeat that.
	span := pgtype.Interval{Microseconds: int64(days) * int64(24*time.Hour/time.Microsecond)}

	summary, err := h.q.TenantPeriodSummary(ctx, tenant)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load analytics summary")
		return
	}

	byDay, err := h.q.TenantOrdersByDayRange(ctx, sqlc.TenantOrdersByDayRangeParams{
		TenantID: tenant,
		Days:     span,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load daily orders")
		return
	}

	byHour, err := h.q.TenantOrdersByHour(ctx, sqlc.TenantOrdersByHourParams{
		TenantID: tenant,
		Days:     span,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load hourly orders")
		return
	}

	top, err := h.q.TenantTopProducts(ctx, sqlc.TenantTopProductsParams{
		TenantID: tenant,
		Days:     span,
		RowLimit: 6,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load top products")
		return
	}

	status, err := h.q.TenantOrderStatusBreakdown(ctx, tenant)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load status breakdown")
		return
	}

	operations, err := h.q.TenantOperationsSummary(ctx, sqlc.TenantOperationsSummaryParams{
		TenantID: tenant,
		Days:     span,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load operations summary")
		return
	}

	payments, err := h.q.TenantPaymentBreakdown(ctx, sqlc.TenantPaymentBreakdownParams{
		TenantID: tenant,
		Days:     span,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load payment breakdown")
		return
	}

	categories, err := h.q.TenantTopCategories(ctx, sqlc.TenantTopCategoriesParams{
		TenantID: tenant,
		Days:     span,
		RowLimit: 6,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load top categories")
		return
	}

	out := AnalyticsResponse{
		Window:              window,
		Days:                days,
		OrdersToday:         summary.OrdersToday,
		RevenueToday:        parseMoney(summary.RevenueToday),
		Orders7d:            summary.Orders7d,
		Revenue7d:           parseMoney(summary.Revenue7d),
		Orders30d:           summary.Orders30d,
		Revenue30d:          parseMoney(summary.Revenue30d),
		OrdersPrev30d:       summary.OrdersPrev30d,
		RevenuePrev30d:      parseMoney(summary.RevenuePrev30d),
		PendingOrders:       summary.PendingOrders,
		PreparingOrders:     summary.PreparingOrders,
		ReadyOrders:         summary.ReadyOrders,
		CompletedToday:      summary.CompletedToday,
		AvgOrderValue30d:    parseMoney(summary.AvgOrderValue30d),
		ProductsAvailable:   summary.ProductsAvailable,
		ProductsUnavailable: summary.ProductsUnavailable,

		MedianPrepMinutes: round1(operations.MedianPrepMinutes),
		AvgPrepMinutes:    round1(operations.AvgPrepMinutes),
		AvgAcceptMinutes:  round1(operations.AvgAcceptMinutes),
		CompletionRate:    rate(operations.Completed, operations.Total),
		CancellationRate:  rate(operations.Cancelled, operations.Total),
		CancelledToday:    operations.CancelledToday,

		OrdersByDay:     fillDayGaps(byDay, days),
		OrdersByHour:    averageByHour(byHour, days),
		TopProducts:     topProducts(top),
		TopCategories:   topCategories(categories),
		PaymentMix:      paymentMix(payments),
		StatusBreakdown: statusBreakdown(status),
	}

	response.JSON(w, http.StatusOK, out)
}

// topCategories maps the category rows onto the response shape.
func topCategories(rows []sqlc.TenantTopCategoriesRow) []CategoryPoint {
	out := make([]CategoryPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, CategoryPoint{
			Name:    r.Name,
			Units:   r.Units,
			Revenue: parseMoney(r.Revenue),
		})
	}
	return out
}

// paymentMix maps the payment rows and works out each one's share, so the
// dashboard renders a bar without recomputing a total it already has.
func paymentMix(rows []sqlc.TenantPaymentBreakdownRow) []PaymentPoint {
	var total int64
	for _, r := range rows {
		total += r.OrderCount
	}
	out := make([]PaymentPoint, 0, len(rows))
	for _, r := range rows {
		share := 0.0
		if total > 0 {
			share = round1(float64(r.OrderCount) / float64(total) * 100)
		}
		out = append(out, PaymentPoint{
			Method:     r.Method,
			Status:     r.Status,
			OrderCount: r.OrderCount,
			Revenue:    parseMoney(r.Revenue),
			Share:      share,
		})
	}
	return out
}

// rate is a percentage of a total, guarding the empty case.
func rate(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return round1(float64(part) / float64(total) * 100)
}

// round1 keeps one decimal place. Minutes and percentages are read, not
// calculated with, so more precision is noise on the screen.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// fillDayGaps returns one point per day in the window.
//
// The SQL only returns days that have orders, which makes a quiet week look
// like a gap. A chart with a continuous time axis needs every day present so a
// zero reads as "no orders that day" rather than "no data".
func fillDayGaps(rows []sqlc.TenantOrdersByDayRangeRow, days int) []DayPoint {
	byDay := make(map[string]DayPoint, len(rows))
	for _, r := range rows {
		key := r.Day.Time.Format("2006-01-02")
		byDay[key] = DayPoint{
			Day:        key,
			OrderCount: r.OrderCount,
			Revenue:    parseMoney(r.Revenue),
		}
	}

	out := make([]DayPoint, 0, days)
	// Anchor on UTC midnight so the slice is stable regardless of the hour.
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start := today.AddDate(0, 0, -(days - 1))

	for i := 0; i < days; i++ {
		key := start.AddDate(0, 0, i).Format("2006-01-02")
		if p, ok := byDay[key]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, DayPoint{Day: key})
	}
	return out
}

// averageByHour returns all 24 hours with a per-day average, so the shape of a
// trading day is comparable across windows of different lengths.
func averageByHour(rows []sqlc.TenantOrdersByHourRow, days int) []HourPoint {
	counts := make(map[int]float64, len(rows))
	for _, r := range rows {
		counts[int(r.HourOfDay)] = float64(r.OrderCount)
	}
	if days < 1 {
		days = 1
	}
	out := make([]HourPoint, 0, 24)
	for h := 0; h < 24; h++ {
		out = append(out, HourPoint{Hour: h, OrderCount: counts[h] / float64(days)})
	}
	return out
}

func topProducts(rows []sqlc.TenantTopProductsRow) []ProductPoint {
	out := make([]ProductPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, ProductPoint{
			Name:    r.Name,
			Units:   r.Units,
			Revenue: parseMoney(r.Revenue),
		})
	}
	return out
}

func statusBreakdown(rows []sqlc.TenantOrderStatusBreakdownRow) []StatusPoint {
	out := make([]StatusPoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, StatusPoint{Status: r.Status, Count: r.Count})
	}
	return out
}

// parseMoney converts the numeric text Postgres returns into a float, tolerating
// a NULL or malformed value rather than failing the whole response.
func parseMoney(v string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return n
}
