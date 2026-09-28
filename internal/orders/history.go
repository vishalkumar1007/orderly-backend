package orders

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
Order history and the business activity feed.

These are the *record* of the shop, as opposed to the Selling board, which is
its present tense. The distinction matters operationally: the board is a queue
somebody is working, and it must never grow filters and date pickers that slow
the person clearing it down. The history is where questions get answered.

The activity feed here is business history — orders moving through the
workflow. Who changed a price or a permission is a different question with
different retention needs, and it lives in the audit log.
*/

// historyPageSize bounds a page. The console pages rather than scrolls, so an
// operator cannot accidentally ask for the whole year on a phone.
const (
	historyPageSize    = 25
	historyMaxPageSize = 100
	activityMaxRows    = 200
)

// OrderHistory returns the filtered, paginated record of this shop's orders.
func (h *Handler) OrderHistory(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	ctx := r.Context()
	query := r.URL.Query()

	limit := clampInt(intParam(query.Get("limit"), historyPageSize), 1, historyMaxPageSize)
	offset := clampInt(intParam(query.Get("offset"), 0), 0, 1_000_000)

	status, err := historyStatus(query.Get("status"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	source := optionalUpper(query.Get("source"), []string{"GUEST", "CUSTOMER", "STAFF"})
	from, err := optionalDate(query.Get("from"), false)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "from must be a date, e.g. 2026-09-01")
		return
	}
	to, err := optionalDate(query.Get("to"), true)
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "to must be a date, e.g. 2026-09-30")
		return
	}
	search := optionalText(strings.TrimSpace(query.Get("q")))

	rows, err := h.q.ListOrderHistory(ctx, sqlc.ListOrderHistoryParams{
		TenantID:  pgutil.UUID(tenantID),
		Status:    status,
		Source:    source,
		FromDate:  from,
		ToDate:    to,
		Search:    search,
		RowLimit:  int32(limit),
		RowOffset: int32(offset),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load order history")
		return
	}

	summary, err := h.q.SummarizeOrderHistory(ctx, sqlc.SummarizeOrderHistoryParams{
		TenantID: pgutil.UUID(tenantID),
		Status:   status,
		Source:   source,
		FromDate: from,
		ToDate:   to,
		Search:   search,
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not summarise order history")
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":             pgutil.UUIDString(row.ID),
			"order_number":   row.OrderNumber,
			"status":         row.Status,
			"source":         row.Source,
			"customer_name":  row.CustomerName,
			"customer_phone": row.CustomerPhone,
			"item_count":     row.ItemCount,
			"total":          pgutil.NumericToFloat(row.Total),
			"payment_status": row.PaymentStatus,
			"payment_method": row.PaymentMethod,
			"cancel_reason":  row.CancelReason,
			"created_at":     row.CreatedAt.Time.Format(time.RFC3339),
			"completed_at":   timeOrNil(row.CompletedAt),
			"cancelled_at":   timeOrNil(row.CancelledAt),
			// Elapsed minutes from acceptance to ready, which is the number an
			// operator actually manages. Nil until an order reaches READY.
			"prep_minutes": prepMinutes(row.AcceptedAt, row.ReadyAt),
		})
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"orders": out,
		"page": map[string]any{
			"limit":  limit,
			"offset": offset,
			"total":  summary.TotalOrders,
		},
		"summary": map[string]any{
			"total_orders":  summary.TotalOrders,
			"total_revenue": summary.TotalRevenue,
			"completed":     summary.Completed,
			"cancelled":     summary.Cancelled,
		},
	})
}

// ActivityFeed returns recent business events: orders moving through the
// workflow, newest first.
func (h *Handler) ActivityFeed(w http.ResponseWriter, r *http.Request) {
	tenantID := mustTenant(r)
	query := r.URL.Query()

	limit := clampInt(intParam(query.Get("limit"), 100), 1, activityMaxRows)
	status, err := historyStatus(query.Get("status"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	rows, err := h.q.ListTenantActivity(r.Context(), sqlc.ListTenantActivityParams{
		TenantID: pgutil.UUID(tenantID),
		ToStatus: status,
		RowLimit: int32(limit),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load activity")
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":            pgutil.UUIDString(row.ID),
			"order_id":      pgutil.UUIDString(row.OrderID),
			"order_number":  row.OrderNumber,
			"from_status":   row.FromStatus,
			"to_status":     row.ToStatus,
			"actor":         row.Actor,
			"customer_name": row.CustomerName,
			"total":         pgutil.NumericToFloat(row.Total),
			"source":        row.Source,
			"at":            row.CreatedAt.Time.Format(time.RFC3339),
		})
	}

	response.JSON(w, http.StatusOK, map[string]any{"events": out})
}

/* ---------- parameter helpers ---------- */

// historyStatus validates a status filter against the closed set of order
// states. An unknown value is refused rather than silently ignored, because a
// filter that quietly does nothing reads as "there are no such orders".
func historyStatus(raw string) (pgtype.Text, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" {
		return pgtype.Text{}, nil
	}
	switch value {
	case StatusPending, StatusAccepted, StatusPreparing, StatusReady, StatusCompleted, StatusCancelled:
		return pgtype.Text{String: value, Valid: true}, nil
	}
	return pgtype.Text{}, errUnknownStatus
}

var errUnknownStatus = &statusError{"status must be one of PENDING, ACCEPTED, PREPARING, READY, COMPLETED or CANCELLED"}

type statusError struct{ msg string }

func (e *statusError) Error() string { return e.msg }

func optionalUpper(raw string, allowed []string) pgtype.Text {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" {
		return pgtype.Text{}
	}
	for _, a := range allowed {
		if a == value {
			return pgtype.Text{String: value, Valid: true}
		}
	}
	return pgtype.Text{}
}

func optionalText(value string) pgtype.Text {
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

// optionalDate parses a YYYY-MM-DD filter. `endOfDay` shifts the boundary to
// the following midnight so a range of 1st–1st includes that whole day; the
// query compares with `<` against it.
func optionalDate(raw string, endOfDay bool) (pgtype.Timestamptz, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return pgtype.Timestamptz{}, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return pgtype.Timestamptz{}, err
	}
	if endOfDay {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return pgtype.Timestamptz{Time: parsed, Valid: true}, nil
}

func intParam(raw string, fallback int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func timeOrNil(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format(time.RFC3339)
}

// prepMinutes is how long the kitchen had the order, or nil when it never got
// that far. Rounded to the minute: seconds are noise at this scale.
func prepMinutes(accepted, ready pgtype.Timestamptz) any {
	if !accepted.Valid || !ready.Valid {
		return nil
	}
	minutes := int(ready.Time.Sub(accepted.Time).Round(time.Minute).Minutes())
	if minutes < 0 {
		return nil
	}
	return minutes
}
