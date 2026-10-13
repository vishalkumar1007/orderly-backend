// Package inventory is stock tracking, Food first (Release V1 §3.6, Ch.19).
//
// A product is tracked when it has an inventory_items row. Orders talk to this
// package through DeductForOrder / RestoreForOrder / CheckLines and never touch
// its tables; the console talks to it through Handler.
//
// Rules the rest of the system relies on:
//   - quantity never goes below zero (DB CHECK, and every change is computed first);
//   - every change to quantity writes one ledger row in the same transaction;
//   - an order takes stock once and returns it once (unique index on the ledger);
//   - stock at zero hides the product from the storefront; restocking shows it
//     again — but only if stock was why it was hidden (inventory_items.auto_disabled);
//   - nothing here is active unless the tenant's plan has the INVENTORY feature.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Movement reasons (inventory_movements.reason).
const (
	ReasonInitial    = "INITIAL"
	ReasonAdd        = "ADD"
	ReasonWaste      = "WASTE"
	ReasonCorrection = "CORRECTION"
	ReasonOrder      = "ORDER"
	ReasonCancel     = "CANCEL"
)

// InsufficientError means a change would take stock below zero.
type InsufficientError struct {
	Product   string
	Available int32
}

func (e *InsufficientError) Error() string {
	if e.Available <= 0 {
		return fmt.Sprintf("%s is out of stock", e.Product)
	}
	return fmt.Sprintf("Only %d left of %s", e.Available, e.Product)
}

// Alert describes a product that has just dropped to or below its reorder level.
type Alert struct {
	ProductID   uuid.UUID
	ProductName string
	Quantity    int32
	Threshold   int32
	Unit        string
}

// Line is a product and how many of it an order wants.
type Line struct {
	ProductID uuid.UUID
	Quantity  int32
}

// Enabled reports whether the tenant's plan includes inventory. A lookup
// failure counts as disabled: stock rules must never block an order because a
// plan lookup hiccuped.
func Enabled(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID) bool {
	plan, err := q.GetTenantPlanLimits(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return false
	}
	return slices.Contains(plan.Entitlements, entitlement.FeatureInventory)
}

// apply changes one locked item by delta and writes the ledger row. applied is
// false when the ledger refused a duplicate (an order retried), in which case
// nothing changed. q must be a transaction's queries, and item must come from a
// locking query.
func apply(ctx context.Context, q *sqlc.Queries, item sqlc.InventoryItem, name string, delta int32, reason string, orderID pgtype.UUID, note, actor string) (sqlc.InventoryItem, *Alert, bool, error) {
	after := item.Quantity + delta
	if after < 0 {
		return item, nil, false, &InsufficientError{Product: name, Available: item.Quantity}
	}
	if _, err := q.InsertInventoryMovement(ctx, sqlc.InsertInventoryMovementParams{
		TenantID: item.TenantID, ItemID: item.ID, Delta: delta, QuantityAfter: after,
		Reason: reason, OrderID: orderID, Note: note, Actor: actor,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return item, nil, false, nil
		}
		return item, nil, false, err
	}

	autoDisabled := item.AutoDisabled
	if item.Track {
		switch {
		case after == 0 && item.Quantity > 0:
			hidden, err := q.HideProductForStock(ctx, sqlc.HideProductForStockParams{ID: item.ProductID, TenantID: item.TenantID})
			if err != nil {
				return item, nil, false, err
			}
			autoDisabled = autoDisabled || hidden > 0
		case after > 0 && autoDisabled:
			if err := q.SetProductAvailable(ctx, sqlc.SetProductAvailableParams{ID: item.ProductID, TenantID: item.TenantID, IsAvailable: true}); err != nil {
				return item, nil, false, err
			}
			autoDisabled = false
		}
	}
	updated, err := q.SetInventoryQuantity(ctx, sqlc.SetInventoryQuantityParams{
		ID: item.ID, TenantID: item.TenantID, Quantity: after, AutoDisabled: autoDisabled,
	})
	if err != nil {
		return item, nil, false, err
	}

	var alert *Alert
	// Alert on the way down across the reorder level, not on every sale below it.
	if item.Track && delta < 0 && after <= item.ReorderThreshold && item.Quantity > item.ReorderThreshold {
		alert = &Alert{
			ProductID: uuid.UUID(item.ProductID.Bytes), ProductName: name,
			Quantity: after, Threshold: item.ReorderThreshold, Unit: item.Unit,
		}
	}
	return updated, alert, true, nil
}

// CheckLines is the checkout pre-check: it refuses a cart asking for more than
// is in stock, with a message the customer can act on. It does not lock; the
// real guarantee is DeductForOrder, which fails the same way under a race.
func CheckLines(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, lines []Line) error {
	if len(lines) == 0 || !Enabled(ctx, q, tenantID) {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, pgutil.UUID(l.ProductID))
	}
	rows, err := q.PeekTrackedInventoryForProducts(ctx, sqlc.PeekTrackedInventoryForProductsParams{
		TenantID: pgutil.UUID(tenantID), ProductIds: ids,
	})
	if err != nil {
		return err
	}
	want := map[uuid.UUID]int32{}
	for _, l := range lines {
		want[l.ProductID] += l.Quantity
	}
	for _, row := range rows {
		if need := want[uuid.UUID(row.ProductID.Bytes)]; row.Quantity < need {
			return &InsufficientError{Product: row.ProductName, Available: row.Quantity}
		}
	}
	return nil
}

// DeductForOrder takes an order's quantities out of stock. Call it inside the
// transaction that moves the order to ACCEPTED (or creates it already
// accepted): if it fails, the caller rolls back and the order does not move.
// Alerts are returned for the caller to publish after commit.
func DeductForOrder(ctx context.Context, q *sqlc.Queries, tenantID, orderID uuid.UUID, actor string) ([]Alert, error) {
	if !Enabled(ctx, q, tenantID) {
		return nil, nil
	}
	tid := pgutil.UUID(tenantID)
	lines, err := q.ListOrderLines(ctx, sqlc.ListOrderLinesParams{OrderID: pgutil.UUID(orderID), TenantID: tid})
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	ids := make([]pgtype.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.ProductID)
	}
	rows, err := q.LockTrackedInventoryForProducts(ctx, sqlc.LockTrackedInventoryForProductsParams{TenantID: tid, ProductIds: ids})
	if err != nil {
		return nil, err
	}
	byProduct := make(map[uuid.UUID]sqlc.LockTrackedInventoryForProductsRow, len(rows))
	for _, r := range rows {
		byProduct[uuid.UUID(r.ProductID.Bytes)] = r
	}

	var alerts []Alert
	for _, l := range lines {
		row, tracked := byProduct[uuid.UUID(l.ProductID.Bytes)]
		if !tracked {
			continue
		}
		item := sqlc.InventoryItem{
			ID: row.ID, TenantID: row.TenantID, ProductID: row.ProductID, Quantity: row.Quantity,
			Unit: row.Unit, ReorderThreshold: row.ReorderThreshold, Track: row.Track, AutoDisabled: row.AutoDisabled,
		}
		_, alert, _, err := apply(ctx, q, item, row.ProductName, -l.Quantity, ReasonOrder, pgutil.UUID(orderID), "", actor)
		if err != nil {
			return nil, err
		}
		if alert != nil {
			alerts = append(alerts, *alert)
		}
	}
	return alerts, nil
}

// RestoreForOrder gives back whatever DeductForOrder took for this order. It
// does not check the plan: stock that was taken must be returned even if the
// tenant has since been downgraded. An order that never took stock (still
// PENDING, or untracked products) returns nothing.
func RestoreForOrder(ctx context.Context, q *sqlc.Queries, tenantID, orderID uuid.UUID, actor string) error {
	tid := pgutil.UUID(tenantID)
	taken, err := q.ListOrderStockMovements(ctx, sqlc.ListOrderStockMovementsParams{TenantID: tid, OrderID: pgutil.UUID(orderID)})
	if err != nil {
		return err
	}
	for _, m := range taken {
		item, err := q.LockInventoryItemByID(ctx, sqlc.LockInventoryItemByIDParams{TenantID: tid, ID: m.ItemID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // tracking was removed with its product
			}
			return err
		}
		if _, _, _, err := apply(ctx, q, item, "", -m.Delta, ReasonCancel, pgutil.UUID(orderID), "", actor); err != nil {
			return err
		}
	}
	return nil
}

// Notify publishes low-stock alerts. Call after the transaction commits; a
// notification problem never fails the caller (notify.Dispatch swallows it).
func Notify(ctx context.Context, d notify.Deps, tenantID uuid.UUID, alerts []Alert) {
	for _, a := range alerts {
		title := fmt.Sprintf("%s is low on stock — %d %s left", a.ProductName, a.Quantity, a.Unit)
		if a.Quantity == 0 {
			title = fmt.Sprintf("%s is out of stock", a.ProductName)
		}
		notify.Dispatch(ctx, d, &tenantID, notify.TypeInventoryLow, title, "",
			map[string]any{
				"product_id": a.ProductID.String(), "product_name": a.ProductName,
				"quantity": a.Quantity, "threshold": a.Threshold, "unit": a.Unit,
			}, notify.Contact{})
	}
}
