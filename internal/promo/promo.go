// Package promo is offers and loyalty: coupon codes with server-side rules,
// and a points programme whose balances are a ledger, never a client number.
//
// Checkout integration is two steps inside the order transaction so the
// discount and its redemption commit with the order or not at all:
//
//	applied := promo.Prepare(...)      // locks the offer row, validates, prices the discount
//	order   := CreateOrder(discount)   // the caller writes the order
//	promo.Record(ctx, qtx, applied, order.ID)
//
// Loyalty follows the same shape (PrepareRedeem / RecordRedeem), and
// EarnForOrder runs when an order completes. Both are idempotent per order
// through a partial unique index on the ledger.
package promo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Problem is a customer-presentable refusal.
type Problem struct {
	Code    string
	Message string
}

func (p *Problem) Error() string { return p.Message }

func problem(code, msg string) *Problem { return &Problem{Code: code, Message: msg} }

// Applied is a validated coupon ready to be recorded against an order.
type Applied struct {
	PromotionID pgtype.UUID
	Code        string
	Discount    float64
	Message     string
}

// Context is what a redemption is judged against.
type Context struct {
	TenantID   uuid.UUID
	Phone      string
	CustomerID *uuid.UUID
	Subtotal   float64
}

func money(v float64) float64 { return math.Round(v*100) / 100 }

// NormalizeCode upper-cases and strips a coupon code.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.Join(strings.Fields(code), ""))
}

// Evaluate judges an offer against a cart without touching the database
// beyond the counts it needs. Shared by the quote (unlocked read) and the
// order (locked row) paths so both say the same thing.
func Evaluate(ctx context.Context, q *sqlc.Queries, p sqlc.Promotion, c Context) (Applied, *Problem) {
	now := time.Now()
	if !p.Active {
		return Applied{}, problem("offer_inactive", "This offer is no longer available")
	}
	if p.StartsAt.Valid && now.Before(p.StartsAt.Time) {
		return Applied{}, problem("offer_not_started", "This offer has not started yet")
	}
	if p.EndsAt.Valid && now.After(p.EndsAt.Time) {
		return Applied{}, problem("offer_expired", "This offer has expired")
	}
	minSub := pgutil.NumericToFloat(p.MinSubtotal)
	if minSub > 0 && c.Subtotal < minSub {
		return Applied{}, problem("offer_minimum", fmt.Sprintf("Add %.2f more to use this offer", minSub-c.Subtotal))
	}
	if p.MaxRedemptions > 0 {
		used, err := q.CountPromotionRedemptions(ctx, p.ID)
		if err != nil {
			return Applied{}, problem("internal_error", "Could not check this offer")
		}
		if used >= int64(p.MaxRedemptions) {
			return Applied{}, problem("offer_exhausted", "This offer has been fully claimed")
		}
	}
	if p.PerCustomerLimit > 0 && c.Phone != "" {
		used, err := q.CountPromotionRedemptionsByPhone(ctx, sqlc.CountPromotionRedemptionsByPhoneParams{PromotionID: p.ID, Phone: c.Phone})
		if err != nil {
			return Applied{}, problem("internal_error", "Could not check this offer")
		}
		if used >= int64(p.PerCustomerLimit) {
			return Applied{}, problem("offer_used", "You have already used this offer")
		}
	}
	if p.FirstOrderOnly && c.Phone != "" {
		n, err := q.CountOrdersByPhone(ctx, sqlc.CountOrdersByPhoneParams{TenantID: pgutil.UUID(c.TenantID), Phone: c.Phone})
		if err != nil {
			return Applied{}, problem("internal_error", "Could not check this offer")
		}
		if n > 0 {
			return Applied{}, problem("offer_first_order", "This offer is for first orders only")
		}
	}
	value := pgutil.NumericToFloat(p.Value)
	var discount float64
	if p.Kind == "PERCENT" {
		discount = c.Subtotal * value / 100
	} else {
		discount = value
	}
	if cap := pgutil.NumericToFloat(p.MaxDiscount); cap > 0 && discount > cap {
		discount = cap
	}
	if discount > c.Subtotal {
		discount = c.Subtotal
	}
	discount = money(discount)
	label := p.Name
	if label == "" {
		label = p.Code
	}
	return Applied{PromotionID: p.ID, Code: p.Code, Discount: discount, Message: fmt.Sprintf("%s applied: %.2f off", label, discount)}, nil
}

// Quote evaluates a code for the cart screen. No lock, no write.
func Quote(ctx context.Context, q *sqlc.Queries, code string, c Context) (Applied, *Problem) {
	code = NormalizeCode(code)
	if code == "" {
		return Applied{}, nil
	}
	p, err := q.GetPromotionByCode(ctx, sqlc.GetPromotionByCodeParams{TenantID: pgutil.UUID(c.TenantID), Code: code})
	if errors.Is(err, pgx.ErrNoRows) {
		return Applied{}, problem("offer_unknown", "That code is not valid")
	}
	if err != nil {
		return Applied{}, problem("internal_error", "Could not check this offer")
	}
	return Evaluate(ctx, q, p, c)
}

// Prepare locks the offer inside the order transaction and re-validates it,
// so two checkouts racing for the last redemption are serialised.
func Prepare(ctx context.Context, qtx *sqlc.Queries, code string, c Context) (Applied, *Problem) {
	code = NormalizeCode(code)
	if code == "" {
		return Applied{}, nil
	}
	p, err := qtx.LockPromotionByCode(ctx, sqlc.LockPromotionByCodeParams{TenantID: pgutil.UUID(c.TenantID), Code: code})
	if errors.Is(err, pgx.ErrNoRows) {
		return Applied{}, problem("offer_unknown", "That code is not valid")
	}
	if err != nil {
		return Applied{}, problem("internal_error", "Could not check this offer")
	}
	return Evaluate(ctx, qtx, p, c)
}

// Record writes the redemption for an order. Idempotent per order.
func Record(ctx context.Context, qtx *sqlc.Queries, a Applied, c Context, orderID pgtype.UUID) error {
	if !a.PromotionID.Valid {
		return nil
	}
	disc, err := pgutil.NumericFromFloat(a.Discount)
	if err != nil {
		return err
	}
	return qtx.InsertPromotionRedemption(ctx, sqlc.InsertPromotionRedemptionParams{
		TenantID: pgutil.UUID(c.TenantID), PromotionID: a.PromotionID, OrderID: orderID,
		CustomerID: pgutil.NullUUID(c.CustomerID), Phone: c.Phone, Discount: disc,
	})
}

/* ---------- loyalty ---------- */

// Program is the tenant's points programme with its defaults filled in.
type Program struct {
	Enabled          bool    `json:"enabled"`
	PointsPer100     int32   `json:"points_per_100"`
	PointValue       float64 `json:"point_value"`
	MinRedeemPoints  int32   `json:"min_redeem_points"`
	MaxRedeemPercent int32   `json:"max_redeem_percent"`
}

// LoadProgram returns the programme, or a disabled default when none is saved.
func LoadProgram(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID) Program {
	row, err := q.GetLoyaltyProgram(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return Program{PointsPer100: 10, PointValue: 0.10, MinRedeemPoints: 100, MaxRedeemPercent: 50}
	}
	return Program{
		Enabled: row.Enabled, PointsPer100: row.PointsPer100, PointValue: pgutil.NumericToFloat(row.PointValue),
		MinRedeemPoints: row.MinRedeemPoints, MaxRedeemPercent: row.MaxRedeemPercent,
	}
}

// PointsFor is how many points a completed order of this total earns.
func (p Program) PointsFor(total float64) int32 {
	if !p.Enabled || p.PointsPer100 <= 0 || total <= 0 {
		return 0
	}
	return int32(math.Floor(total / 100 * float64(p.PointsPer100)))
}

// MaxRedeemable caps a redemption by balance, the programme's share of the
// cart, and whole points.
func (p Program) MaxRedeemable(balance int32, subtotal float64) int32 {
	if !p.Enabled || p.PointValue <= 0 || balance < p.MinRedeemPoints || balance <= 0 {
		return 0
	}
	byCart := int32(math.Floor(subtotal * float64(p.MaxRedeemPercent) / 100 / p.PointValue))
	if byCart < balance {
		return byCart
	}
	return balance
}

// Redemption is a validated points spend ready to be recorded.
type Redemption struct {
	AccountID pgtype.UUID
	Points    int32
	Discount  float64
}

// PrepareRedeem locks the customer's account inside the order transaction and
// validates the spend. Guests cannot redeem: points need an identity.
func PrepareRedeem(ctx context.Context, qtx *sqlc.Queries, tenantID uuid.UUID, customerID *uuid.UUID, points int32, subtotal float64) (Redemption, *Problem) {
	if points <= 0 {
		return Redemption{}, nil
	}
	if customerID == nil {
		return Redemption{}, problem("loyalty_sign_in", "Sign in to use your points")
	}
	prog := LoadProgram(ctx, qtx, tenantID)
	if !prog.Enabled {
		return Redemption{}, problem("loyalty_disabled", "Points cannot be used right now")
	}
	acct, err := qtx.LockLoyaltyAccount(ctx, sqlc.LockLoyaltyAccountParams{TenantID: pgutil.UUID(tenantID), CustomerID: pgutil.UUID(*customerID)})
	if err != nil {
		return Redemption{}, problem("internal_error", "Could not check your points")
	}
	max := prog.MaxRedeemable(acct.Balance, subtotal)
	if points > max {
		if max == 0 {
			return Redemption{}, problem("loyalty_insufficient", fmt.Sprintf("You need at least %d points to redeem", prog.MinRedeemPoints))
		}
		return Redemption{}, problem("loyalty_too_many", fmt.Sprintf("You can use up to %d points on this order", max))
	}
	return Redemption{AccountID: acct.ID, Points: points, Discount: money(float64(points) * prog.PointValue)}, nil
}

// RecordRedeem moves the points and writes the ledger row. Idempotent per order.
func RecordRedeem(ctx context.Context, qtx *sqlc.Queries, tenantID uuid.UUID, r Redemption, orderID pgtype.UUID) error {
	if r.Points <= 0 {
		return nil
	}
	n, err := qtx.InsertLoyaltyTransaction(ctx, sqlc.InsertLoyaltyTransactionParams{
		TenantID: pgutil.UUID(tenantID), AccountID: r.AccountID, Kind: "REDEEM", Points: -r.Points, OrderID: orderID, Note: "Used at checkout",
	})
	if err != nil || n == 0 {
		return err
	}
	_, err = qtx.ApplyLoyaltyDelta(ctx, sqlc.ApplyLoyaltyDeltaParams{Delta: -r.Points, ID: r.AccountID})
	return err
}

// EarnForOrder credits a completed order. Runs inside the completion
// transaction; a retried transition finds the ledger row and does nothing.
func EarnForOrder(ctx context.Context, qtx *sqlc.Queries, tenantID uuid.UUID, order sqlc.Order) (int32, error) {
	if !order.CustomerID.Valid {
		return 0, nil
	}
	prog := LoadProgram(ctx, qtx, tenantID)
	points := prog.PointsFor(pgutil.NumericToFloat(order.Total))
	if points <= 0 {
		return 0, nil
	}
	acct, err := qtx.LockLoyaltyAccount(ctx, sqlc.LockLoyaltyAccountParams{TenantID: pgutil.UUID(tenantID), CustomerID: order.CustomerID})
	if err != nil {
		return 0, err
	}
	n, err := qtx.InsertLoyaltyTransaction(ctx, sqlc.InsertLoyaltyTransactionParams{
		TenantID: pgutil.UUID(tenantID), AccountID: acct.ID, Kind: "EARN", Points: points, OrderID: order.ID,
		Note: fmt.Sprintf("Order #%d", order.OrderNumber),
	})
	if err != nil || n == 0 {
		return 0, err
	}
	if _, err := qtx.ApplyLoyaltyDelta(ctx, sqlc.ApplyLoyaltyDeltaParams{Delta: points, ID: acct.ID}); err != nil {
		return 0, err
	}
	return points, nil
}

// Balance is a customer's points and what they are worth, for the profile
// and checkout screens.
type Balance struct {
	Enabled   bool    `json:"enabled"`
	Points    int32   `json:"points"`
	Value     float64 `json:"value"`
	MinRedeem int32   `json:"min_redeem_points"`
	PerPoint  float64 `json:"point_value"`
}

func BalanceFor(ctx context.Context, q *sqlc.Queries, tenantID, customerID uuid.UUID) Balance {
	prog := LoadProgram(ctx, q, tenantID)
	out := Balance{Enabled: prog.Enabled, MinRedeem: prog.MinRedeemPoints, PerPoint: prog.PointValue}
	if acct, err := q.GetLoyaltyAccount(ctx, sqlc.GetLoyaltyAccountParams{TenantID: pgutil.UUID(tenantID), CustomerID: pgutil.UUID(customerID)}); err == nil {
		out.Points = acct.Balance
		out.Value = money(float64(acct.Balance) * prog.PointValue)
	}
	return out
}
