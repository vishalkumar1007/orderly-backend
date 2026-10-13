package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/payable"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Refund errors the caller can act on.
var (
	ErrNotRefundable    = errors.New("this payment cannot be refunded")
	ErrRefundInProgress = errors.New("a refund for this payment is already in progress")
	// ErrRefundRejected means the gateway refused outright; the payment is
	// still paid and the refund is recorded as failed.
	ErrRefundRejected = errors.New("the payment provider rejected the refund")
)

// RefundResult is what a refund request achieved so far.
type RefundResult struct {
	Payment sqlc.Payment
	Refund  sqlc.PaymentRefund
	// Done is true once the money is back. False means accepted and still
	// settling: the gateway confirms by webhook.
	Done bool
}

// IsTest reports whether a payment went through the dummy gateway: an online
// payment opened through the sandbox provider. Payments recorded before the
// gateway framework (no gateway order id) are not labelled, so existing history
// reads exactly as it did.
func IsTest(pay sqlc.Payment) bool {
	return pay.Method == storefront.MethodOnline && pay.Mode == string(ModeSandbox) && pay.ProviderOrderID != ""
}

// gatewayPaymentID is the id the gateway knows this payment by.
func gatewayPaymentID(pay sqlc.Payment) string {
	if pay.ProviderPaymentID != "" {
		return pay.ProviderPaymentID
	}
	if pay.Provider == sandboxName {
		// The sandbox has no separate payment id; derive a stable one.
		return "sbx_pay_" + pgutil.UUIDString(pay.ID)
	}
	return ""
}

// RefundPayment returns the full amount of a captured online payment. Only the
// payments service moves money back, and only through here, so the state
// change, the refund record and the audit row always agree.
//
// The payment goes PAID -> REFUNDING and the refund row is written in one
// transaction, before the gateway is called: a crash can then never leave money
// refunded with no record, and ResumeRefunds finishes anything left half-sent.
func (s *Service) RefundPayment(ctx context.Context, tenantID uuid.UUID, paymentID pgtype.UUID, reason string, actor Actor) (RefundResult, error) {
	pay, err := s.q.GetPaymentByID(ctx, sqlc.GetPaymentByIDParams{ID: paymentID, TenantID: pgutil.UUID(tenantID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{}, ErrNotRefundable
	}
	if err != nil {
		return RefundResult{}, err
	}
	gid := gatewayPaymentID(pay)
	if pay.Method != storefront.MethodOnline || gid == "" || pgutil.NumericToFloat(pay.Amount) <= 0 {
		// Cash never passed through us, so there is nothing for us to return.
		return RefundResult{Payment: pay}, ErrNotRefundable
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RefundResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	moved, err := q.MarkPaymentRefunding(ctx, sqlc.MarkPaymentRefundingParams{ID: pay.ID, TenantID: pay.TenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		if pay.Status == payable.StatusRefunded || pay.Status == "REFUNDING" {
			return RefundResult{Payment: pay}, ErrRefundInProgress
		}
		return RefundResult{Payment: pay}, ErrNotRefundable
	}
	if err != nil {
		return RefundResult{}, err
	}
	rf, err := q.InsertPaymentRefund(ctx, sqlc.InsertPaymentRefundParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, ProviderPaymentID: gid, Amount: pay.Amount, Reason: truncate(reason, 200),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RefundResult{Payment: pay}, ErrRefundInProgress
	}
	if err != nil {
		return RefundResult{}, err
	}
	if err := q.InsertPaymentAudit(ctx, sqlc.InsertPaymentAuditParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, EventType: "REFUNDING",
		OldStatus: payable.StatusPaid, NewStatus: "REFUNDING",
		ActorType: actor.Type, ActorID: actor.ID, Reason: truncate(reason, 200),
	}); err != nil {
		return RefundResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundResult{}, err
	}

	done, err := s.executeRefund(ctx, moved, rf, true)
	fresh, _ := s.q.GetPaymentByID(ctx, sqlc.GetPaymentByIDParams{ID: pay.ID, TenantID: pay.TenantID})
	latest, _ := s.q.GetPaymentRefund(ctx, rf.ID)
	return RefundResult{Payment: fresh, Refund: latest, Done: done}, err
}

// executeRefund sends a recorded refund to the gateway and records the answer.
// whole is true when the refund returns the payment itself (as opposed to one
// extra gateway payment of a double charge, which leaves the order's payment
// status alone).
func (s *Service) executeRefund(ctx context.Context, pay sqlc.Payment, rf sqlc.PaymentRefund, whole bool) (bool, error) {
	p, err := s.refundProvider(ctx, pay)
	if err != nil {
		// Cannot reach for the gateway's credentials right now: leave the
		// refund pending; the sweeper retries it.
		return false, fmt.Errorf("%w: %v", ErrGatewayUnavailable, err)
	}
	out, err := p.Refund(ctx, RefundInput{
		ProviderPaymentID: rf.ProviderPaymentID,
		AmountMinor:       minorUnits(pgutil.NumericToFloat(rf.Amount)),
		Reason:            rf.Reason,
		Key:               pgutil.UUIDString(rf.ID),
	})
	if err != nil {
		if IsPermanentGatewayError(err) {
			s.failRefund(ctx, pay, rf, whole, "The payment provider declined the refund.")
			return false, ErrRefundRejected
		}
		s.log.Warn("payments: refund not sent, will retry", "refund", pgutil.UUIDString(rf.ID), "error", err)
		return false, fmt.Errorf("%w: %v", ErrGatewayUnavailable, err)
	}
	if out.Status == RefundDone {
		return true, s.completeRefund(ctx, pay, rf, out.ID, whole)
	}
	_, err = s.q.SetPaymentRefundResult(ctx, sqlc.SetPaymentRefundResultParams{
		ID: rf.ID, Status: "PENDING", GatewayRefundID: out.ID,
	})
	return false, err
}

func (s *Service) refundProvider(ctx context.Context, pay sqlc.Payment) (PaymentProvider, error) {
	if pay.Provider == razorpayName {
		return s.razorpayFor(ctx, uuid.UUID(pay.TenantID.Bytes))
	}
	return s.sandbox, nil
}

func (s *Service) completeRefund(ctx context.Context, pay sqlc.Payment, rf sqlc.PaymentRefund, gatewayID string, whole bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := s.q.WithTx(tx)
	if _, err := q.SetPaymentRefundResult(ctx, sqlc.SetPaymentRefundResultParams{
		ID: rf.ID, Status: "COMPLETED", GatewayRefundID: gatewayID,
	}); err != nil {
		return err
	}
	event := "DUPLICATE_REFUNDED"
	if whole {
		if _, err := q.MarkPaymentRefunded(ctx, sqlc.MarkPaymentRefundedParams{ID: pay.ID, TenantID: pay.TenantID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		event = "REFUNDED"
	}
	if err := q.InsertPaymentAudit(ctx, sqlc.InsertPaymentAuditParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, EventType: event,
		OldStatus: "REFUNDING", NewStatus: map[bool]string{true: "REFUNDED", false: pay.Status}[whole],
		ActorType: "provider", ActorID: gatewayID, Reason: rf.Reason,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyRefund(ctx, pay, rf, notify.TypePaymentRefunded, "")
	return nil
}

func (s *Service) failRefund(ctx context.Context, pay sqlc.Payment, rf sqlc.PaymentRefund, whole bool, msg string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("payments: could not record refund failure", "error", err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q := s.q.WithTx(tx)
	if _, err := q.SetPaymentRefundResult(ctx, sqlc.SetPaymentRefundResultParams{ID: rf.ID, Status: "FAILED", Error: msg}); err != nil {
		s.log.Error("payments: could not record refund failure", "error", err)
		return
	}
	if whole {
		// The money is still with the shop: the payment goes back to paid.
		if _, err := q.RevertPaymentRefunding(ctx, sqlc.RevertPaymentRefundingParams{ID: pay.ID, TenantID: pay.TenantID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("payments: could not revert refund", "error", err)
			return
		}
	}
	_ = q.InsertPaymentAudit(ctx, sqlc.InsertPaymentAuditParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, EventType: "REFUND_FAILED",
		OldStatus: "REFUNDING", NewStatus: payable.StatusPaid, ActorType: "provider", Reason: msg,
	})
	if err := tx.Commit(ctx); err != nil {
		s.log.Error("payments: could not record refund failure", "error", err)
		return
	}
	s.notifyRefund(ctx, pay, rf, notify.TypePaymentRefundFailed, msg)
}

func (s *Service) notifyRefund(ctx context.Context, pay sqlc.Payment, rf sqlc.PaymentRefund, code, failure string) {
	ref, err := s.src.Load(ctx, uuid.UUID(pay.TenantID.Bytes), pay.PayableID)
	if err != nil {
		return
	}
	tenantID := uuid.UUID(pay.TenantID.Bytes)
	amount := pgutil.NumericToFloat(rf.Amount)
	data := map[string]any{"order_id": pgutil.UUIDString(ref.ID), "order_number": ref.Number, "amount": amount}
	contact := notify.Contact{Email: ref.CustomerEmail, Phone: ref.CustomerPhone}
	if code == notify.TypePaymentRefundFailed {
		data["failure_reason"] = failure
		notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, &tenantID, code,
			fmt.Sprintf("Refund failed for order #%d", ref.Number),
			fmt.Sprintf("%.2f could not be refunded. %s", amount, failure), data, contact)
		return
	}
	notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, &tenantID, code,
		fmt.Sprintf("Refund issued for order #%d", ref.Number),
		fmt.Sprintf("%.2f was refunded to the customer.", amount), data, contact)
}

// settleRefundEvent applies a gateway's refund confirmation or failure.
func (s *Service) settleRefundEvent(ctx context.Context, pay sqlc.Payment, ev Event) (string, error) {
	rf, err := s.q.GetPaymentRefundByGatewayID(ctx, ev.RefundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "unknown_refund", nil
	}
	if err != nil {
		return "", err
	}
	if rf.Status != "PENDING" {
		return "", nil // already settled: a retried delivery
	}
	whole := rf.ProviderPaymentID == gatewayPaymentID(pay)
	if ev.Type == EventRefunded {
		return "", s.completeRefund(ctx, pay, rf, ev.RefundID, whole)
	}
	s.failRefund(ctx, pay, rf, whole, "The payment provider could not complete the refund.")
	return "", nil
}

// RefundOnCancel returns the money for an order that was paid and then
// cancelled. Best effort and silent when there is nothing to return: a cancel
// must never fail because of money, but a refund that cannot be sent is
// recorded and flagged for the owner.
func (s *Service) RefundOnCancel(ctx context.Context, tenantID uuid.UUID, orderID pgtype.UUID) {
	if !s.v2 {
		return
	}
	pay, err := s.q.GetPaymentByOrderID(ctx, sqlc.GetPaymentByOrderIDParams{OrderID: orderID, TenantID: pgutil.UUID(tenantID)})
	if err != nil || pay.Status != payable.StatusPaid || pay.Method != storefront.MethodOnline {
		return
	}
	if _, err := s.RefundPayment(ctx, tenantID, pay.ID, "Order cancelled", Actor{Type: "system", ID: "order_cancelled"}); err != nil {
		s.log.Warn("payments: refund on cancel not completed", "payment", pgutil.UUIDString(pay.ID), "error", err)
	}
}

// refundDuplicate returns a second capture for an order that was already paid.
func (s *Service) refundDuplicate(ctx context.Context, pay sqlc.Payment, ev Event) {
	amount, err := pgutil.NumericFromFloat(float64(ev.AmountMinor) / 100)
	if err != nil || ev.AmountMinor <= 0 {
		return
	}
	rf, err := s.q.InsertPaymentRefund(ctx, sqlc.InsertPaymentRefundParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, ProviderPaymentID: ev.ProviderPaymentID,
		Amount: amount, Reason: "Duplicate payment",
	})
	if err != nil { // no row: already being handled
		return
	}
	if _, err := s.executeRefund(ctx, pay, rf, false); err != nil {
		s.log.Warn("payments: duplicate refund not completed", "refund", pgutil.UUIDString(rf.ID), "error", err)
	}
}

// ResumeRefunds re-sends refunds that were recorded but never reached the
// gateway. The refund id is the gateway idempotency key, so this cannot refund
// twice.
func (s *Service) ResumeRefunds(ctx context.Context, limit int32) (int, error) {
	rows, err := s.q.ListStaleUnsentRefunds(ctx, limit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, rf := range rows {
		pay, err := s.q.GetPaymentByID(ctx, sqlc.GetPaymentByIDParams{ID: rf.PaymentID, TenantID: rf.TenantID})
		if err != nil {
			continue
		}
		whole := rf.ProviderPaymentID == gatewayPaymentID(pay)
		if _, err := s.executeRefund(ctx, pay, rf, whole); err == nil {
			sent++
		}
	}
	return sent, nil
}
