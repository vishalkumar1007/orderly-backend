package orders

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
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

// Viewer exposes the read-only order surface to other packages.
func (h *Handler) Viewer() *Viewer { return &Viewer{q: h.q} }

// Queries exposes the query set for packages that need raw reads.
func (h *Handler) Queries() *sqlc.Queries { return h.q }

// StorefrontLoader exposes the tenant configuration loader.
func (h *Handler) StorefrontLoader() *storefront.Loader { return h.store }

// Catalog exposes the menu reader.
func (h *Handler) Catalog() *storefront.Catalog { return h.catalog }

// CreateRequest is the public checkout payload. It carries intent only — no
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
		return CreateResult{}, badRequest("invalid_token", "Could not place this order — please try again")
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
		return CreateResult{}, badRequest("empty_total", "This order has no value — please review your cart")
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

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
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateResult{}, err
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
	if err := h.checkGate(order, to, sf); err != nil {
		return OrderView{}, err
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return OrderView{}, err
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	var updated sqlc.Order
	switch to {
	case StatusAccepted:
		updated, err = qtx.StampOrderAccepted(ctx, sqlc.StampOrderAcceptedParams{ID: order.ID, TenantID: order.TenantID})
	case StatusPreparing:
		updated, err = qtx.StampOrderPreparing(ctx, sqlc.StampOrderPreparingParams{ID: order.ID, TenantID: order.TenantID})
	case StatusReady:
		updated, err = qtx.StampOrderReady(ctx, sqlc.StampOrderReadyParams{ID: order.ID, TenantID: order.TenantID})
	case StatusCompleted:
		updated, err = qtx.StampOrderCompleted(ctx, sqlc.StampOrderCompletedParams{ID: order.ID, TenantID: order.TenantID})
	case StatusCancelled:
		updated, err = qtx.StampOrderCancelled(ctx, sqlc.StampOrderCancelledParams{ID: order.ID, TenantID: order.TenantID, CancelReason: reason})
	default:
		return OrderView{}, &ErrInvalidTransition{From: order.Status, To: to}
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

	// Auto-complete: a shop that does not track the pickup counter wants the
	// order closed as soon as it is bagged.
	if to == StatusReady && sf.Workflow.AutoComplete {
		final, err := qtx.StampOrderCompleted(ctx, sqlc.StampOrderCompletedParams{ID: order.ID, TenantID: order.TenantID})
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
	return h.hydrateWithStore(ctx, updated, sf)
}

// checkGate enforces the tenant's workflow rules. A shop that requires payment
// before preparation cannot start cooking an unpaid online order, and a
// cancelled order can never be revived.
func (h *Handler) checkGate(order sqlc.Order, to string, sf *storefront.Storefront) error {
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
		pay, err := h.q.GetPaymentByOrderID(context.Background(), sqlc.GetPaymentByOrderIDParams{
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
	return h.Move(ctx, tenantID, order, StatusAccepted, "", "system:auto_accept")
}
