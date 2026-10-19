package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/inventory"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/payments"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Handler owns order creation, the order state machine and the tenant admin
// order screens.
type Handler struct {
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	store   *storefront.Loader
	catalog *storefront.Catalog
	log     *slog.Logger
	// payments records staff-confirmed payments; set by the server wiring.
	payments *payments.Service
}

// NewHandler wires the order handler.
func NewHandler(pool *pgxpool.Pool, log *slog.Logger) *Handler {
	q := sqlc.New(pool)
	return &Handler{
		pool:    pool,
		q:       q,
		store:   storefront.NewLoader(pool),
		catalog: storefront.NewCatalog(q),
		log:     log,
	}
}

// SetPayments gives the handler the payments service (wired after both exist,
// since payments is built from this handler's PayableSource).
func (h *Handler) SetPayments(p *payments.Service) { h.payments = p }

// Viewer exposes the read-only order surface to other packages.
func (h *Handler) Viewer() *Viewer { return &Viewer{q: h.q} }

// Queries exposes the query set for packages that need raw reads.
func (h *Handler) Queries() *sqlc.Queries { return h.q }

// StorefrontLoader exposes the tenant configuration loader.
func (h *Handler) StorefrontLoader() *storefront.Loader { return h.store }

// Catalog exposes the menu reader.
func (h *Handler) Catalog() *storefront.Catalog { return h.catalog }

// CreateRequest is the public checkout payload. It carries intent only: no
// prices, no totals, no tenant id.
type CreateRequest struct {
	CustomerName  string `json:"customer_name"`
	CustomerPhone string `json:"customer_phone"`
	CustomerEmail string `json:"customer_email"`
	Notes         string `json:"notes"`
	PaymentMethod string `json:"payment_method"`
	// ClientToken makes a repeated submit idempotent. The storefront generates
	// one per checkout session and reuses it on retry.
	ClientToken string          `json:"client_token"`
	Items       []RequestedLine `json:"items"`
}

// CreateResult is what the checkout step needs to route the customer next.
type CreateResult struct {
	View         OrderView
	Duplicate    bool
	Payment      *PaymentView
	Methods      []string
	CanPayOnline bool
	StoreName    string
}

// validationError is a customer-presentable rejection.
type validationError struct {
	Code    string
	Message string
	Status  int
}

func (e *validationError) Error() string { return e.Message }

func badRequest(code, format string, args ...any) *validationError {
	return &validationError{Code: code, Message: sprintf(format, args...), Status: 400}
}

// Create places a new order for a tenant.
//
// The tenant comes from the resolved request context, the signed-in customer
// (when a valid customer token is present) is attached automatically, and every
// price is recomputed from the database inside the same transaction that writes
// the order. A repeated submit carrying the same client_token returns the
// original order rather than creating a second one.
func (h *Handler) Create(ctx context.Context, tenantID uuid.UUID, sf *storefront.Storefront, req CreateRequest, principal *auth.CustomerPrincipal) (CreateResult, error) {
	return h.placeOrder(ctx, tenantID, sf, req, principal, false)
}

// CreateCounter places a walk-in / counter order for staff. It reuses the same
// pricing and cart rules as public checkout, but skips the storefront open gate
// so the counter can sell when the public site is closed.
func (h *Handler) CreateCounter(ctx context.Context, tenantID uuid.UUID, sf *storefront.Storefront, req CreateRequest) (CreateResult, error) {
	return h.placeOrder(ctx, tenantID, sf, req, nil, true)
}

func (h *Handler) placeOrder(ctx context.Context, tenantID uuid.UUID, sf *storefront.Storefront, req CreateRequest, principal *auth.CustomerPrincipal, bypassStoreClosed bool) (CreateResult, error) {
	name := trim(req.CustomerName)
	phone := normalisePhoneInput(req.CustomerPhone)

	if name == "" {
		return CreateResult{}, &validationError{Code: "name_required", Message: "Please tell us your name", Status: 400}
	}
	if utf8Len(name) < 2 || utf8Len(name) > 80 {
		return CreateResult{}, &validationError{Code: "invalid_name", Message: "Please enter your name", Status: 400}
	}
	if phone == "" {
		return CreateResult{}, &validationError{Code: "phone_required", Message: "Please enter your phone number", Status: 400}
	}
	email := trim(req.CustomerEmail)
	if email != "" && (utf8Len(email) > 254 || !looksLikeEmail(email)) {
		return CreateResult{}, &validationError{Code: "invalid_email", Message: "Please check your email address", Status: 400}
	}
	if !bypassStoreClosed && !sf.OrderingAllowed() {
		return CreateResult{}, &validationError{
			Code:    "store_closed",
			Message: sf.ClosedReason(),
			Status:  409,
		}
	}

	method := upperTrim(req.PaymentMethod)
	if method == "" {
		method = sf.Payments.DefaultMethod
	}
	if method != storefront.MethodOnline && method != storefront.MethodCash {
		return CreateResult{}, badRequest("invalid_payment_method", "Choose how you would like to pay")
	}
	if !sf.Payments.Allows(method) {
		return CreateResult{}, &validationError{
			Code:    "payment_method_unavailable",
			Message: "That payment method is not available right now",
			Status:  400,
		}
	}
	clientToken := trim(req.ClientToken)
	if utf8Len(clientToken) > 80 {
		return CreateResult{}, badRequest("invalid_token", "Could not place this order: please try again")
	}

	// A repeat submit short-circuits before any money is touched.
	if clientToken != "" {
		if existing, err := h.q.GetOrderByClientToken(ctx, sqlc.GetOrderByClientTokenParams{
			TenantID:    pgutil.UUID(tenantID),
			ClientToken: clientToken,
		}); err == nil {
			view, verr := h.hydrateWithStore(ctx, existing, sf)
			if verr == nil {
				return CreateResult{
					View:         view,
					Duplicate:    true,
					Payment:      view.Payment,
					Methods:      sf.Payments.Methods(),
					CanPayOnline: sf.Payments.PayOnline(),
					StoreName:    sf.Name,
				}, nil
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return CreateResult{}, err
		}
	}

	products, lines, perr := h.resolveCart(ctx, tenantID, req.Items)
	if perr != nil {
		return CreateResult{}, perr
	}
	priced, totals, costErr := PriceCart(products, lines, Costing{
		TaxPercent:   sf.TaxPercent,
		PackagingFee: sf.PackagingFee,
	})
	if costErr != nil {
		return CreateResult{}, &validationError{Code: costErr.Code, Message: costErr.Message, Status: 400}
	}
	if totals.Total <= 0 {
		return CreateResult{}, badRequest("empty_total", "This order has no value: please review your cart")
	}
	// Tracked products: refuse a cart asking for more than is in stock, with
	// a message the customer can act on ("Only 2 left of Veg Momo").
	if err := inventory.CheckLines(ctx, h.q, tenantID, stockLines(lines)); err != nil {
		return CreateResult{}, stockError(err)
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)
	var stockAlerts []inventory.Alert

	customerID := pgtype.UUID{}
	source := "GUEST"
	if principal != nil && principal.TenantID == tenantID {
		customerID = pgutil.UUID(principal.ID)
		source = "CUSTOMER"
		// A signed-in customer's saved name is a better default than a blank
		// field, but what they typed on this order always wins.
		if name == "" {
			name = principal.Name
		}
	}
	if bypassStoreClosed {
		source = "COUNTER"
	}

	number, err := qtx.NextOrderNumber(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return CreateResult{}, err
	}

	now := time.Now()
	readyAt := storefront.EstimatedReady(sf.PrepTimeMinutes, now)
	status := StatusPending
	if sf.Workflow.AutoAccept() {
		status = StatusAccepted
	}

	num := numericOf
	order, err := qtx.CreateOrder(ctx, sqlc.CreateOrderParams{
		TenantID:         pgutil.UUID(tenantID),
		CustomerID:       customerID,
		OrderNumber:      int32(number),
		Status:           status,
		OrderType:        OrderTypePickup,
		Subtotal:         num(totals.Subtotal),
		Tax:              num(totals.Tax),
		Discount:         num(totals.Discount),
		PackagingFee:     num(totals.Packaging),
		Total:            num(totals.Total),
		CustomerName:     name,
		CustomerPhone:    phone,
		CustomerEmail:    email,
		Notes:            sanitiseNotes(trim(req.Notes)),
		EstimatedReadyAt: timestamptzOf(readyAt),
		ClientToken:      clientToken,
		Source:           source,
	})
	if err != nil {
		// Two concurrent submits with the same token: the unique index caught
		// the duplicate, so hand back the order that won the race.
		if clientToken != "" && isUniqueViolation(err) {
			if existing, lookupErr := h.q.GetOrderByClientToken(ctx, sqlc.GetOrderByClientTokenParams{
				TenantID:    pgutil.UUID(tenantID),
				ClientToken: clientToken,
			}); lookupErr == nil {
				view, verr := h.hydrateWithStore(ctx, existing, sf)
				if verr == nil {
					return CreateResult{
						View: view, Duplicate: true, Payment: view.Payment,
						Methods: sf.Payments.Methods(), CanPayOnline: sf.Payments.PayOnline(),
						StoreName: sf.Name,
					}, nil
				}
			}
		}
		return CreateResult{}, err
	}

	for _, line := range priced {
		// A line with no extras must still write `[]`, not `null`: the column
		// is constrained to a JSON array so the storefront can read it without
		// a null check.
		addons := []byte("[]")
		if len(line.Addons) > 0 {
			addons, err = json.Marshal(line.Addons)
			if err != nil {
				return CreateResult{}, err
			}
		}
		if _, err := qtx.CreateOrderItem(ctx, sqlc.CreateOrderItemParams{
			TenantID:            pgutil.UUID(tenantID),
			OrderID:             order.ID,
			ProductID:           numericUUID(line.Product.ID),
			ProductNameSnapshot: line.Product.Name,
			UnitPrice:           num(line.UnitBase),
			Quantity:            int32(line.Quantity),
			Subtotal:            num(line.LineTotal),
			Addons:              addons,
			Notes:               line.Notes,
		}); err != nil {
			return CreateResult{}, err
		}
	}

	if _, err := qtx.CreatePayment(ctx, sqlc.CreatePaymentParams{
		TenantID: pgutil.UUID(tenantID),
		OrderID:  order.ID,
		Amount:   num(totals.Total),
		Method:   method,
		Status:   PaymentPending,
		// The gateway is "sandbox" until a real provider is configured. The
		// public payload never exposes this value.
		Provider: "sandbox",
	}); err != nil {
		return CreateResult{}, err
	}

	historyActor := "customer"
	if bypassStoreClosed {
		historyActor = "staff"
	}
	if _, err := qtx.AppendOrderStatusHistory(ctx, sqlc.AppendOrderStatusHistoryParams{
		TenantID:   pgutil.UUID(tenantID),
		OrderID:    order.ID,
		FromStatus: "",
		ToStatus:   StatusPending,
		Actor:      historyActor,
	}); err != nil {
		return CreateResult{}, err
	}
	if status == StatusAccepted {
		if _, err := qtx.StampOrderAccepted(ctx, sqlc.StampOrderAcceptedParams{
			ID:       order.ID,
			TenantID: pgutil.UUID(tenantID),
			// Inserted as ACCEPTED already; this only stamps accepted_at.
			ExpectedStatus: StatusAccepted,
		}); err != nil {
			return CreateResult{}, err
		}
		if _, err := qtx.AppendOrderStatusHistory(ctx, sqlc.AppendOrderStatusHistoryParams{
			TenantID:   pgutil.UUID(tenantID),
			OrderID:    order.ID,
			FromStatus: StatusPending,
			ToStatus:   StatusAccepted,
			Actor:      "system:auto_accept",
		}); err != nil {
			return CreateResult{}, err
		}
		// Accepted at placement: the stock leaves in the same transaction, so
		// a race for the last unit fails the order instead of overselling.
		stockAlerts, err = inventory.DeductForOrder(ctx, qtx, tenantID, uuid.UUID(order.ID.Bytes), "system:auto_accept")
		if err != nil {
			return CreateResult{}, stockError(err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, err
	}
	inventory.Notify(ctx, notify.Deps{Q: h.q, Log: h.log}, tenantID, stockAlerts)

	notify.Dispatch(ctx, notify.Deps{Q: h.q, Log: h.log}, &tenantID, notify.TypeOrderPlaced,
		fmt.Sprintf("New order #%d", number),
		fmt.Sprintf("%s: ₹%.2f", name, totals.Total),
		map[string]any{
			"order_id":      pgutil.UUIDString(order.ID),
			"order_number":  number,
			"customer_name": name,
			"total":         totals.Total,
		},
		notify.Contact{Email: email, Phone: phone})
	if status == StatusAccepted {
		// Accepted at placement (auto-accept): placed first, then accepted.
		h.publishTransition(ctx, tenantID, order, StatusAccepted, sf)
	}

	view, err := h.hydrateWithStore(ctx, order, sf)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		View:         view,
		Payment:      view.Payment,
		Methods:      sf.Payments.Methods(),
		CanPayOnline: sf.Payments.PayOnline(),
		StoreName:    sf.Name,
	}, nil
}

// resolveCart loads the products named in a cart and normalises the requested
// lines. Products that are gone or off sale are reported as unavailable.
func (h *Handler) resolveCart(ctx context.Context, tenantID uuid.UUID, requested []RequestedLine) (map[string]ProductRef, []RequestedLine, *validationError) {
	ids := make([]uuid.UUID, 0, len(requested))
	seen := map[string]bool{}
	for _, line := range requested {
		id, err := uuid.Parse(trim(line.ProductID))
		if err != nil {
			return nil, nil, badRequest("invalid_product", "Your cart contains an item we could not recognise")
		}
		ids = append(ids, id)
		seen[id.String()] = true
	}
	public, err := h.catalog.LoadForOrder(ctx, tenantID, ids)
	if err != nil {
		return nil, nil, &validationError{Code: "menu_unavailable", Message: "We could not load your items. Please try again.", Status: 503}
	}
	products := make(map[string]ProductRef, len(public))
	for id, p := range public {
		products[id] = ProductRef{
			ID:           p.ID,
			Name:         p.Name,
			Price:        p.Price,
			Available:    p.Available,
			AllowsNotes:  p.AllowsNotes,
			Addons:       toAddonRefs(p.Addons),
			OptionGroups: toOptionGroupRefs(p.OptionGroups),
		}
	}
	// Merge duplicate lines for the same product so a tampered payload cannot
	// dodge the per-line quantity ceiling by splitting rows.
	merged := make([]RequestedLine, 0, len(requested))
	index := map[string]int{}
	for _, line := range requested {
		id, _ := uuid.Parse(trim(line.ProductID))
		key := id.String()
		if at, ok := index[key]; ok {
			merged[at].Quantity += line.Quantity
			merged[at].Addons = append(merged[at].Addons, line.Addons...)
			merged[at].Notes = joinNotes(merged[at].Notes, line.Notes)
			continue
		}
		index[key] = len(merged)
		merged = append(merged, line)
	}
	return products, merged, nil
}

func joinNotes(a, b string) string {
	a, b = trim(a), trim(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

func toAddonRefs(addons []storefront.Addon) []AddonRef {
	out := make([]AddonRef, 0, len(addons))
	for _, a := range addons {
		out = append(out, AddonRef{ID: a.ID, Name: a.Name, Price: a.Price, MaxQty: a.MaxQty})
	}
	return out
}

func toOptionGroupRefs(groups []storefront.OptionGroup) []OptionGroupRef {
	out := make([]OptionGroupRef, 0, len(groups))
	for _, g := range groups {
		if !g.IsActive {
			continue
		}
		ids := make([]string, 0, len(g.Options))
		for _, o := range g.Options {
			if o.IsActive {
				ids = append(ids, o.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		out = append(out, OptionGroupRef{
			ID: g.ID, Name: g.Name, Selection: g.Selection, Required: g.Required, OptionIDs: ids,
		})
	}
	return out
}

func (h *Handler) hydrateWithStore(ctx context.Context, row sqlc.Order, sf *storefront.Storefront) (OrderView, error) {
	return (&Viewer{q: h.q}).hydrate(ctx, row, *sf)
}

// Cancel marks an order cancelled, enforcing that cancellation is only legal
// from PENDING or ACCEPTED.
func (h *Handler) Cancel(ctx context.Context, tenantID uuid.UUID, order sqlc.Order, reason, actor string) (OrderView, error) {
	if !CanTransition(order.Status, StatusCancelled) {
		return OrderView{}, &ErrInvalidTransition{
			From: order.Status, To: StatusCancelled,
			Reason:  "This order is already being prepared and can no longer be cancelled",
			Allowed: NextStatuses(order.Status),
		}
	}
	return h.Move(ctx, tenantID, order, StatusCancelled, reason, actor)
}

// Move applies a state change after checking the transition and the workflow
// gates. Every order status write in the system goes through here.
func (h *Handler) Move(ctx context.Context, tenantID uuid.UUID, order sqlc.Order, to, reason, actor string) (OrderView, error) {
	if !CanTransition(order.Status, to) {
		return OrderView{}, &ErrInvalidTransition{From: order.Status, To: to, Allowed: NextStatuses(order.Status)}
	}
	sf, err := h.store.Load(ctx, tenantID)
	if err != nil {
		return OrderView{}, err
	}
	if err := h.checkGate(ctx, order, to, sf); err != nil {
		return OrderView{}, err
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return OrderView{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	// Every write is a compare-and-swap on the status validated above, so a
	// concurrent move (double tap, two devices, payment landing mid-accept)
	// cannot slip an illegal transition past CanTransition.
	var updated sqlc.Order
	from := order.Status
	switch to {
	case StatusAccepted:
		updated, err = qtx.StampOrderAccepted(ctx, sqlc.StampOrderAcceptedParams{ID: order.ID, TenantID: order.TenantID, ExpectedStatus: from})
	case StatusPreparing:
		updated, err = qtx.StampOrderPreparing(ctx, sqlc.StampOrderPreparingParams{ID: order.ID, TenantID: order.TenantID, ExpectedStatus: from})
	case StatusReady:
		updated, err = qtx.StampOrderReady(ctx, sqlc.StampOrderReadyParams{ID: order.ID, TenantID: order.TenantID, ExpectedStatus: from})
	case StatusCompleted:
		updated, err = qtx.StampOrderCompleted(ctx, sqlc.StampOrderCompletedParams{ID: order.ID, TenantID: order.TenantID, ExpectedStatus: from})
	case StatusCancelled:
		updated, err = qtx.StampOrderCancelled(ctx, sqlc.StampOrderCancelledParams{ID: order.ID, TenantID: order.TenantID, CancelReason: reason, ExpectedStatus: from})
	default:
		return OrderView{}, &ErrInvalidTransition{From: order.Status, To: to}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return OrderView{}, &ErrInvalidTransition{
			From: from, To: to, Stale: true,
			Reason: "This order was just updated by someone else. Refresh and try again.",
		}
	}
	if err != nil {
		return OrderView{}, err
	}
	if _, err := qtx.AppendOrderStatusHistory(ctx, sqlc.AppendOrderStatusHistoryParams{
		TenantID:   order.TenantID,
		OrderID:    order.ID,
		FromStatus: order.Status,
		ToStatus:   to,
		Actor:      actor,
	}); err != nil {
		return OrderView{}, err
	}

	// Stock follows the order, inside the same transaction as the status
	// write: accepting takes stock (and fails if there is not enough),
	// cancelling gives back whatever was taken.
	var stockAlerts []inventory.Alert
	switch to {
	case StatusAccepted:
		stockAlerts, err = inventory.DeductForOrder(ctx, qtx, tenantID, uuid.UUID(order.ID.Bytes), actor)
	case StatusCancelled:
		err = inventory.RestoreForOrder(ctx, qtx, tenantID, uuid.UUID(order.ID.Bytes), actor)
	}
	if err != nil {
		return OrderView{}, err
	}

	// Auto-complete: a shop that does not track the pickup counter wants the
	// order closed as soon as it is bagged.
	autoCompleted := false
	if to == StatusReady && sf.Workflow.AutoComplete {
		autoCompleted = true
		final, err := qtx.StampOrderCompleted(ctx, sqlc.StampOrderCompletedParams{ID: order.ID, TenantID: order.TenantID, ExpectedStatus: StatusReady})
		if err != nil {
			return OrderView{}, err
		}
		if _, err := qtx.AppendOrderStatusHistory(ctx, sqlc.AppendOrderStatusHistoryParams{
			TenantID:   order.TenantID,
			OrderID:    order.ID,
			FromStatus: StatusReady,
			ToStatus:   StatusCompleted,
			Actor:      "system:auto_complete",
		}); err != nil {
			return OrderView{}, err
		}
		updated = final
	}

	if err := tx.Commit(ctx); err != nil {
		return OrderView{}, err
	}

	h.publishTransition(ctx, tenantID, order, to, sf)
	if autoCompleted {
		// READY and COMPLETED both happened; each gets its own event.
		h.publishTransition(ctx, tenantID, order, StatusCompleted, sf)
	}
	inventory.Notify(ctx, notify.Deps{Q: h.q, Log: h.log}, tenantID, stockAlerts)
	if to == StatusCancelled && h.payments != nil {
		// An order that was paid online and then cancelled gets its money back.
		// Outside the transaction on purpose: a cancel must never fail or wait
		// on a payment gateway.
		h.payments.RefundOnCancel(ctx, tenantID, order.ID)
	}

	return h.hydrateWithStore(ctx, updated, sf)
}

// orderEvents is the notification each order status publishes when an order
// ENTERS it. PENDING is absent on purpose: an order is "placed" (ORDER_PLACED)
// by the checkout, not moved there. A status with no row here would change
// state silently, which TestEveryOrderStatusPublishesAnEvent forbids.
var orderEvents = map[string]struct{ code, title string }{
	StatusAccepted:  {notify.TypeOrderAccepted, "Order #%d was accepted"},
	StatusPreparing: {notify.TypeOrderPreparing, "Order #%d is being prepared"},
	StatusReady:     {notify.TypeOrderReady, "Order #%d is ready"},
	StatusCompleted: {notify.TypeOrderCompleted, "Order #%d was completed"},
	StatusCancelled: {notify.TypeOrderCancelled, "Order #%d was cancelled"},
}

// EventForStatus names the notification published when an order enters status.
func EventForStatus(status string) (string, bool) {
	e, ok := orderEvents[status]
	return e.code, ok
}

// publishTransition dispatches the notification for a status change. Which
// channels actually fire (in-app, customer email, SMS) is the rules engine's
// call, not this handler's; see notify.Dispatch. This is called from Move, the
// only writer of order status, and from placement when the shop auto-accepts.
func (h *Handler) publishTransition(ctx context.Context, tenantID uuid.UUID, order sqlc.Order, to string, sf *storefront.Storefront) {
	e, ok := orderEvents[to]
	if !ok {
		return
	}
	notify.Dispatch(ctx, notify.Deps{Q: h.q, Log: h.log}, &tenantID, e.code, fmt.Sprintf(e.title, order.OrderNumber), "",
		map[string]any{
			"order_id":      pgutil.UUIDString(order.ID),
			"order_number":  order.OrderNumber,
			"customer_name": order.CustomerName,
		},
		notify.Contact{Email: order.CustomerEmail, Phone: order.CustomerPhone})
}

// checkGate enforces the tenant's workflow rules. A shop that requires payment
// before preparation cannot start cooking an unpaid online order, and a
// cancelled order can never be revived.
func (h *Handler) checkGate(ctx context.Context, order sqlc.Order, to string, sf *storefront.Storefront) error {
	if to == StatusCancelled {
		return nil
	}
	if order.Status == StatusCancelled {
		return &ErrInvalidTransition{
			From: order.Status, To: to,
			Reason:  "This order was cancelled",
			Allowed: NextStatuses(order.Status),
		}
	}
	if to == StatusPreparing && sf.Workflow.RequiresPaymentUpfront() {
		pay, err := h.q.GetPaymentByOrderID(ctx, sqlc.GetPaymentByOrderIDParams{
			OrderID:  order.ID,
			TenantID: order.TenantID,
		})
		if err != nil {
			return err
		}
		// Cash at pickup is settled at the counter, so it never blocks the
		// kitchen. Only an unpaid online order is held back.
		if pay.Method == storefront.MethodOnline && pay.Status != PaymentPaid {
			return &ErrInvalidTransition{
				From: order.Status, To: to,
				Reason:  "Payment has not been received yet, so preparation cannot start",
				Allowed: NextStatuses(order.Status),
			}
		}
	}
	return nil
}

// AutoAcceptIfNeeded accepts a freshly paid order when the tenant runs
// automatic acceptance, so a prepaid order starts cooking without a staff tap.
func (h *Handler) AutoAcceptIfNeeded(ctx context.Context, tenantID uuid.UUID, order sqlc.Order) (OrderView, error) {
	sf, err := h.store.Load(ctx, tenantID)
	if err != nil {
		return OrderView{}, err
	}
	if !sf.Workflow.AutoAccept() || order.Status != StatusPending {
		return h.hydrateWithStore(ctx, order, sf)
	}
	view, err := h.Move(ctx, tenantID, order, StatusAccepted, "", "system:auto_accept")
	var it *ErrInvalidTransition
	var short *inventory.InsufficientError
	if errors.As(err, &short) {
		// Paid, but not enough stock to accept it automatically. The payment
		// stands; the order waits PENDING for staff to decide.
		h.log.Warn("auto accept skipped: not enough stock", "order", order.OrderNumber, "reason", short.Error())
		current, getErr := h.q.GetOrderByID(ctx, sqlc.GetOrderByIDParams{ID: order.ID, TenantID: order.TenantID})
		if getErr != nil {
			return OrderView{}, getErr
		}
		return h.hydrateWithStore(ctx, current, sf)
	}
	if errors.As(err, &it) && it.Stale {
		// Staff moved the order between our read and our write; the order is
		// already where auto-accept would have put it or further. Report that,
		// rather than failing a payment that was recorded successfully.
		current, getErr := h.q.GetOrderByID(ctx, sqlc.GetOrderByIDParams{ID: order.ID, TenantID: order.TenantID})
		if getErr != nil {
			return OrderView{}, getErr
		}
		return h.hydrateWithStore(ctx, current, sf)
	}
	return view, err
}

// stockLines turns merged cart lines into what the stock check needs.
func stockLines(lines []RequestedLine) []inventory.Line {
	out := make([]inventory.Line, 0, len(lines))
	for _, l := range lines {
		if id, err := uuid.Parse(trim(l.ProductID)); err == nil && l.Quantity > 0 {
			out = append(out, inventory.Line{ProductID: id, Quantity: int32(l.Quantity)})
		}
	}
	return out
}

// stockError turns a stock failure into the customer-facing validation error;
// anything else is a real failure and passes through.
func stockError(err error) error {
	var short *inventory.InsufficientError
	if errors.As(err, &short) {
		return &validationError{Code: "out_of_stock", Message: short.Error(), Status: 409}
	}
	return err
}
