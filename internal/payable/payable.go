// Package payable is the seam between the payments framework and whatever is
// being paid for. Payments depends on this package, never on a vertical
// (orders today; appointments and hotel folios later), and each vertical
// implements Source. See PAYMENTS-PLAN.md §2a.
package payable

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Payment states. Only the payments framework moves these.
const (
	StatusPending  = "PENDING"
	StatusPaid     = "PAID"
	StatusFailed   = "FAILED"
	StatusRefunded = "REFUNDED"
	StatusExpired  = "EXPIRED"
)

// TypeOrder is the payable_type of a storefront order.
const TypeOrder = "ORDER"

// ErrNotFound means the payable does not exist for this caller. A caller who
// fails the ownership proof gets the same error, so existence is not leaked.
var ErrNotFound = errors.New("payable not found")

// Ref is everything the payment flow needs to know about the thing being paid.
type Ref struct {
	Type     string
	ID       pgtype.UUID
	TenantID pgtype.UUID
	// Number is the human-facing reference shown to the customer (order #).
	Number int32
	Amount pgtype.Numeric
	// Active is false once the payable can no longer be paid (cancelled, done).
	Active      bool
	Status      string
	StatusLabel string

	CreatedAt     time.Time
	CustomerEmail string
	CustomerPhone string
}

// Source is implemented by each vertical that can be paid for.
type Source interface {
	// ParseNumber validates the reference segment from the URL.
	ParseNumber(raw string) (int32, error)
	// Resolve loads the payable and proves the caller may act on it: a
	// signed-in customer (customerID set) for their own, or a guest who supplies
	// the phone used at checkout.
	Resolve(ctx context.Context, tenantID uuid.UUID, number int32, customerID *uuid.UUID, phone string) (Ref, error)
	// Load fetches a payable with no caller proof, for system callers (webhooks,
	// staff actions) that already hold the payable id.
	Load(ctx context.Context, tenantID uuid.UUID, id pgtype.UUID) (Ref, error)
	// AfterPaid lets the vertical react to captured money (e.g. auto-accept an
	// order). It returns the payable's resulting status and label. A failure
	// must not undo the payment.
	AfterPaid(ctx context.Context, tenantID uuid.UUID, ref Ref) (status, label string, err error)
	// Detail is the customer-facing view of the payable.
	Detail(ctx context.Context, tenantID uuid.UUID, ref Ref) (map[string]any, error)
}
