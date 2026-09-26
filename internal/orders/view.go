package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/phone"
)

// OrderView is the read model shared by the public tracking endpoint, the
// customer order history and the tenant admin screens.
type OrderView struct {
	ID            string
	OrderNumber   int32
	Reference     string
	Status        string
	OrderType     string
	Subtotal      float64
	Tax           float64
	Discount      float64
	Total         float64
	CustomerName  string
	CustomerPhone string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ReadyAt       *time.Time
	Items         []ItemView
	Payment       *PaymentView
	History       []HistoryEntry
	Store         StoreRef
}

// StoreRef carries the pickup details a customer needs on the tracking screen.
type StoreRef struct {
	Name    string
	Address string
	Phone   string
}

// ItemView is one order line with its add-ons resolved.
type ItemView struct {
	ID        string
	Name      string
	Quantity  int
	UnitPrice float64
	Subtotal  float64
	Addons    []LineAddon
	Notes     string
}

// PaymentView is the payment row as the customer is allowed to see it. There
// is no provider secret, gateway credential or raw callback body in here.
type PaymentView struct {
	ID            string
	Method        string
	Status        string
	Amount        float64
	FailureReason string
	PaidAt        *time.Time
	Attempts      int
}

// IsOnline reports whether the money is collected through the payment step
// rather than handed over at the counter.
func (p *PaymentView) IsOnline() bool {
	return p != nil && p.Method == storefront.MethodOnline
}

// Settled reports whether the money has actually arrived.
func (p *PaymentView) Settled() bool {
	return p != nil && p.Status == PaymentPaid
}

// AwaitingCash reports whether the customer still owes money that the shop
// collects in person.
func (p *PaymentView) AwaitingCash() bool {
	return p != nil && !p.Settled() && p.Method == storefront.MethodCash
}

// HistoryEntry is one recorded state change.
type HistoryEntry struct {
	From  string
	To    string
	Actor string
	At    time.Time
}

// Totals renders the money breakdown.
func (o OrderView) Totals() Totals {
	return Totals{
		Subtotal:  o.Subtotal,
		Tax:       o.Tax,
		Packaging: money(o.Subtotal + o.Tax - o.Total + o.Discount),
		Discount:  o.Discount,
		Total:     o.Total,
	}
}

// ItemCount is the total number of dishes, for "3 items" summaries.
func (o OrderView) ItemCount() int {
	n := 0
	for _, i := range o.Items {
		n += i.Quantity
	}
	return n
}

// NeedsOnlinePayment reports whether the customer has to settle an online
// payment before the order can move on. Cash is deliberately excluded: it is
// collected at the counter, so a cash order goes straight to confirmation
// rather than being sent to a payment screen it does not need.
func (o OrderView) NeedsOnlinePayment() bool {
	return o.Payment.IsOnline() && !o.Payment.Settled()
}

// AwaitingCash reports whether the shop still needs to take money in person.
func (o OrderView) AwaitingCash() bool {
	return o.Payment.AwaitingCash() && IsActive(o.Status)
}

// Summary is the compact shape used in list views.
func (o OrderView) Summary() map[string]any {
	items := make([]map[string]any, 0, len(o.Items))
	for _, i := range o.Items {
		items = append(items, map[string]any{
			"name":     i.Name,
			"quantity": i.Quantity,
		})
	}
	out := map[string]any{
		"order_number":  o.OrderNumber,
		"reference":     o.Reference,
		"status":        o.Status,
		"status_label":  StatusLabel(o.Status),
		"total":         o.Total,
		"item_count":    o.ItemCount(),
		"items":         items,
		"created_at":    o.CreatedAt.Format(time.RFC3339),
		"is_active":     IsActive(o.Status),
		"can_cancel":    IsCancellable(o.Status),
		"tracking_path": fmt.Sprintf("/order/%d", o.OrderNumber),
	}
	if o.Payment != nil {
		out["payment_status"] = o.Payment.Status
		out["payment_method"] = o.Payment.Method
	}
	if o.ReadyAt != nil {
		out["ready_at"] = o.ReadyAt.Format(time.RFC3339)
	}
	return out
}

// Detail is the full customer-facing tracking payload.
func (o OrderView) Detail() map[string]any {
	items := make([]map[string]any, 0, len(o.Items))
	for _, i := range o.Items {
		row := map[string]any{
			"id":         i.ID,
			"name":       i.Name,
			"quantity":   i.Quantity,
			"unit_price": i.UnitPrice,
			"subtotal":   i.Subtotal,
		}
		if len(i.Addons) > 0 {
			addons := make([]map[string]any, 0, len(i.Addons))
			for _, a := range i.Addons {
				addons = append(addons, map[string]any{
					"name": a.Name, "price": a.Price, "quantity": a.Quantity,
				})
			}
			row["addons"] = addons
		}
		if i.Notes != "" {
			row["notes"] = i.Notes
		}
		items = append(items, row)
	}

	times := map[string]string{}
	for _, h := range o.History {
		if h.To != "" {
			times[h.To] = h.At.Format(time.RFC3339)
		}
	}

	out := map[string]any{
		"order_number":  o.OrderNumber,
		"reference":     o.Reference,
		"status":        o.Status,
		"status_label":  StatusLabel(o.Status),
		"order_type":    o.OrderType,
		"is_active":     IsActive(o.Status),
		"can_cancel":    IsCancellable(o.Status),
		"timeline":      Timeline(o.Status, times),
		"items":         items,
		"totals":        o.Totals(),
		"customer_name": o.CustomerName,
		"phone_masked":  MaskPhone(o.CustomerPhone),
		"store": map[string]any{
			"name":    o.Store.Name,
			"address": o.Store.Address,
			"phone":   o.Store.Phone,
		},
		"created_at":       o.CreatedAt.Format(time.RFC3339),
		"estimated_ready":  o.ReadyAt != nil,
		"payment_required": o.NeedsOnlinePayment(),
		"pay_at_pickup":    o.AwaitingCash(),
	}
	if o.ReadyAt != nil {
		out["estimated_ready_at"] = o.ReadyAt.Format(time.RFC3339)
	}
	if o.Payment != nil {
		payment := map[string]any{
			"method":   o.Payment.Method,
			"status":   o.Payment.Status,
			"amount":   o.Payment.Amount,
			"attempts": o.Payment.Attempts,
		}
		if o.Payment.PaidAt != nil {
			payment["paid_at"] = o.Payment.PaidAt.Format(time.RFC3339)
		}
		if o.Payment.FailureReason != "" {
			payment["failure_reason"] = o.Payment.FailureReason
		}
		out["payment"] = payment
	}
	return out
}

// MaskPhone hides all but the country code and the last four digits, matching
// what the sign-in screen shows so a customer recognises their own number.
func MaskPhone(number string) string {
	return phone.Mask(number)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Viewer reads orders for the storefront and customer surfaces.
type Viewer struct {
	q *sqlc.Queries
}

// NewViewer builds a read-only order view over a query set.
func NewViewer(q *sqlc.Queries) *Viewer { return &Viewer{q: q} }

// ByNumber loads an order by its human order number within one tenant.
func (v *Viewer) ByNumber(ctx context.Context, tenantID uuid.UUID, number int32) (OrderView, error) {
	row, err := v.q.GetOrderByTenantAndNumber(ctx, sqlc.GetOrderByTenantAndNumberParams{
		TenantID:    pgutil.UUID(tenantID),
		OrderNumber: number,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OrderView{}, ErrOrderNotFound
		}
		return OrderView{}, err
	}
	return v.hydrate(ctx, row, storefront.Storefront{Slug: "", Name: ""})
}

// ByNumberForCustomer loads one of a signed-in customer's own orders. A
// customer may only ever read their own history, so the customer id is part of
// the lookup rather than checked afterwards.
func (v *Viewer) ByNumberForCustomer(ctx context.Context, tenantID, customerID uuid.UUID, number int32) (OrderView, error) {
	row, err := v.q.GetOrderByTenantAndNumber(ctx, sqlc.GetOrderByTenantAndNumberParams{
		TenantID:    pgutil.UUID(tenantID),
		OrderNumber: number,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OrderView{}, ErrOrderNotFound
		}
		return OrderView{}, err
	}
	if !row.CustomerID.Valid || uuid.UUID(row.CustomerID.Bytes) != customerID {
		return OrderView{}, ErrOrderNotFound
	}
	return v.hydrate(ctx, row, storefront.Storefront{})
}

// ByNumberAndPhone loads an order for a guest, who proves ownership with the
// phone number used at checkout. An order number on its own reveals nothing.
func (v *Viewer) ByNumberAndPhone(ctx context.Context, tenantID uuid.UUID, number int32, phone string) (OrderView, error) {
	row, err := v.q.GetOrderByTenantNumberAndPhone(ctx, sqlc.GetOrderByTenantNumberAndPhoneParams{
		TenantID:      pgutil.UUID(tenantID),
		OrderNumber:   number,
		CustomerPhone: phone,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OrderView{}, ErrOrderNotFound
		}
		return OrderView{}, err
	}
	return v.hydrate(ctx, row, storefront.Storefront{})
}

// CustomerOrders lists a customer's orders, newest first.
func (v *Viewer) CustomerOrders(ctx context.Context, tenantID, customerID uuid.UUID, limit int32) ([]OrderView, error) {
	rows, err := v.q.ListCustomerOrders(ctx, sqlc.ListCustomerOrdersParams{
		TenantID:   pgutil.UUID(tenantID),
		CustomerID: pgutil.UUID(customerID),
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, 0, len(rows))
	for _, row := range rows {
		view, err := v.hydrate(ctx, row, storefront.Storefront{})
		if err != nil {
			continue
		}
		out = append(out, view)
	}
	return out, nil
}

// ErrOrderNotFound is returned when an order does not exist for this tenant, or
// the caller is not allowed to see it. The two are deliberately identical.
var ErrOrderNotFound = errors.New("order not found")

// hydrate fills an order row with its items, payment, history and store detail.
func (v *Viewer) hydrate(ctx context.Context, row sqlc.Order, sf storefront.Storefront) (OrderView, error) {
	items, _ := v.q.ListOrderItemsByOrder(ctx, row.ID)
	pay, err := v.q.GetPaymentByOrderID(ctx, sqlc.GetPaymentByOrderIDParams{
		OrderID:  row.ID,
		TenantID: row.TenantID,
	})
	history, _ := v.q.ListOrderStatusHistory(ctx, sqlc.ListOrderStatusHistoryParams{
		OrderID:  row.ID,
		TenantID: row.TenantID,
	})

	view := OrderView{
		ID:            pgutil.UUIDString(row.ID),
		OrderNumber:   row.OrderNumber,
		Status:        row.Status,
		OrderType:     row.OrderType,
		Subtotal:      pgutil.NumericToFloat(row.Subtotal),
		Tax:           pgutil.NumericToFloat(row.Tax),
		Discount:      pgutil.NumericToFloat(row.Discount),
		Total:         pgutil.NumericToFloat(row.Total),
		CustomerName:  row.CustomerName,
		CustomerPhone: row.CustomerPhone,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
		Items:         make([]ItemView, 0, len(items)),
		Store:         StoreRef{Name: sf.Name, Address: sf.Address, Phone: sf.Phone},
	}
	if row.EstimatedReadyAt.Valid {
		view.ReadyAt = &row.EstimatedReadyAt.Time
	}
	view.Reference = sf.OrderReference(row.OrderNumber)

	for _, it := range items {
		view.Items = append(view.Items, ItemView{
			ID:        pgutil.UUIDString(it.ID),
			Name:      it.ProductNameSnapshot,
			Quantity:  int(it.Quantity),
			UnitPrice: pgutil.NumericToFloat(it.UnitPrice),
			Subtotal:  pgutil.NumericToFloat(it.Subtotal),
			Addons:    parseLineAddons(it.Addons),
			Notes:     it.Notes,
		})
	}
	if err == nil && pay.ID.Valid {
		p := &PaymentView{
			ID:            pgutil.UUIDString(pay.ID),
			Method:        pay.Method,
			Status:        pay.Status,
			Amount:        pgutil.NumericToFloat(pay.Amount),
			FailureReason: pay.FailureReason,
			Attempts:      int(pay.AttemptCount),
		}
		if pay.PaidAt.Valid {
			t := pay.PaidAt.Time
			p.PaidAt = &t
		}
		view.Payment = p
	}
	for _, h := range history {
		view.History = append(view.History, HistoryEntry{
			From: h.FromStatus, To: h.ToStatus, Actor: h.Actor, At: h.CreatedAt.Time,
		})
	}
	// An order created before the status log existed still needs a first tick.
	if len(view.History) == 0 {
		view.History = append(view.History, HistoryEntry{To: row.Status, At: row.CreatedAt.Time})
	}
	return view, nil
}

// ParseOrderNumber converts the URL segment into an order number.
func ParseOrderNumber(raw string) (int32, error) {
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 || n > 9_999_999 {
		return 0, fmt.Errorf("invalid order number")
	}
	return int32(n), nil
}

func parseLineAddons(raw []byte) []LineAddon {
	if len(raw) == 0 {
		return nil
	}
	var docs []LineAddon
	if err := json.Unmarshal(raw, &docs); err != nil || len(docs) == 0 {
		return nil
	}
	return docs
}
