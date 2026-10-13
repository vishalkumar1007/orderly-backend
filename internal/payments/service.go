package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/payable"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Service owns every payment state change. Handlers, webhooks and staff
// actions all come through Apply, so the audit trail and events cannot be
// skipped by a new code path.
type Service struct {
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	src     payable.Source
	cfg     *configsvc.Service
	log     *slog.Logger
	v2      bool
	sandbox *Sandbox
	// lastRefresh throttles gateway lookups per payment.
	lastRefresh sync.Map
	// alerted remembers when each alert key last fired, so one broken thing
	// raises one alert, not hundreds.
	alerted sync.Map
	// badSigs counts rejected webhook signatures in a rolling window.
	badMu     sync.Mutex
	badCount  int
	badWindow time.Time
}

// NewService wires the payments service. v2 is the PAYMENTS_V2 rollout flag;
// sandboxSecret signs the sandbox gateway's callbacks.
func NewService(pool *pgxpool.Pool, src payable.Source, cfg *configsvc.Service, v2 bool, sandboxSecret string, log *slog.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), src: src, cfg: cfg, log: log, v2: v2, sandbox: NewSandbox(sandboxSecret)}
}

// V2 reports whether the provider/webhook path is switched on.
func (s *Service) V2() bool { return s.v2 }

// Provider returns the built-in gateway by name. The sandbox gateway only
// exists behind the rollout flag. Razorpay is not built-in: its credentials
// belong to each business, so it is resolved with razorpayFor.
func (s *Service) Provider(name string) (PaymentProvider, bool) {
	if name == sandboxName && s.v2 {
		return s.sandbox, true
	}
	return nil, false
}

// KnownProvider reports whether the webhook endpoint serves this gateway.
// Razorpay stays served whatever the flag says: a live payment already in
// flight must still be able to finish if the flag is turned off.
func (s *Service) KnownProvider(name string) bool {
	if name == razorpayName {
		return true
	}
	_, ok := s.Provider(name)
	return ok
}

// ErrUnknownPayment means a callback names a gateway order we never opened, so
// there is no business whose secret could authenticate it.
var ErrUnknownPayment = errors.New("callback does not match any payment")

// VerifyIncoming authenticates a gateway callback. For Razorpay the secret is
// the paying business's, found through our own record of the order id in the
// (still unverified) body — nothing else from the body is trusted until the
// signature passes.
func (s *Service) VerifyIncoming(ctx context.Context, name string, h http.Header, body []byte) (Event, error) {
	if name != razorpayName {
		p, ok := s.Provider(name)
		if !ok {
			return Event{}, ErrUnknownPayment
		}
		return p.VerifyWebhook(h, body)
	}
	var (
		pay sqlc.Payment
		err error
	)
	if orderID := razorpayOrderRef(body); orderID != "" {
		pay, err = s.q.GetPaymentByProviderOrder(ctx, sqlc.GetPaymentByProviderOrderParams{Provider: razorpayName, ProviderOrderID: orderID})
	} else if payID := razorpayPaymentRef(body); payID != "" {
		// Refund events name the payment, not the order.
		pay, err = s.q.GetPaymentByProviderPayment(ctx, sqlc.GetPaymentByProviderPaymentParams{Provider: razorpayName, ProviderPaymentID: payID})
	} else {
		return Event{}, ErrBadSignature
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrUnknownPayment
	}
	if err != nil {
		return Event{}, err
	}
	gw, err := s.razorpayFor(ctx, uuid.UUID(pay.TenantID.Bytes))
	if err != nil {
		return Event{}, err
	}
	return gw.VerifyWebhook(h, body)
}

// razorpayFor builds the gateway client from the business's own credentials.
func (s *Service) razorpayFor(ctx context.Context, tenantID uuid.UUID) (*Razorpay, error) {
	r, err := s.cfg.Resolver().Resolve(ctx, configsvc.ScopeTenant, &tenantID, configsvc.ServicePayments)
	if err != nil {
		return nil, err
	}
	if r.Provider != configsvc.ProviderRazorpay || r.Secret("key_secret") == "" {
		return nil, fmt.Errorf("%w: gateway credentials incomplete", ErrGatewayUnavailable)
	}
	gw := NewRazorpay(configsvc.RazorpayBaseURL(), r.String("key_id"), r.Secret("key_secret"), r.Secret("webhook_secret"))
	gw.log = s.log
	return gw, nil
}

// Actor records who caused a state change.
type Actor struct {
	Type string // customer | staff | system | provider
	ID   string
}

// Apply moves a payment to PAID or FAILED, writes the audit row in the same
// transaction, and then (best effort) raises the notification. changed is false
// when the payment was already captured, so replays and double taps are no-ops.
// A captured payment is never moved back.
func (s *Service) Apply(ctx context.Context, ref payable.Ref, pay sqlc.Payment, to, reason string, actor Actor) (sqlc.Payment, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return pay, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)

	var updated sqlc.Payment
	switch to {
	case payable.StatusPaid:
		updated, err = q.MarkPaymentPaid(ctx, sqlc.MarkPaymentPaidParams{ID: pay.ID, TenantID: pay.TenantID})
	case payable.StatusExpired:
		updated, err = q.MarkPaymentExpired(ctx, sqlc.MarkPaymentExpiredParams{ID: pay.ID, TenantID: pay.TenantID})
	case payable.StatusFailed:
		updated, err = q.MarkPaymentFailed(ctx, sqlc.MarkPaymentFailedParams{ID: pay.ID, TenantID: pay.TenantID, FailureReason: reason})
	default:
		return pay, false, fmt.Errorf("payments: unsupported transition to %q", to)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return pay, false, nil // already captured
	}
	if err != nil {
		return pay, false, err
	}
	if err := q.InsertPaymentAudit(ctx, sqlc.InsertPaymentAuditParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, EventType: to,
		OldStatus: pay.Status, NewStatus: to,
		ActorType: actor.Type, ActorID: actor.ID, Reason: reason,
	}); err != nil {
		return pay, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pay, false, err
	}
	s.notify(ctx, ref, updated, to, reason)
	return updated, true, nil
}

func (s *Service) notify(ctx context.Context, ref payable.Ref, pay sqlc.Payment, to, reason string) {
	tenantID := uuid.UUID(ref.TenantID.Bytes)
	deps := notify.Deps{Q: s.q, Log: s.log}
	contact := notify.Contact{Email: ref.CustomerEmail, Phone: ref.CustomerPhone}
	data := map[string]any{
		"order_id":     pgutil.UUIDString(ref.ID),
		"order_number": ref.Number,
	}
	switch to {
	case payable.StatusFailed:
		data["failure_reason"] = reason
		notify.Dispatch(ctx, deps, &tenantID, notify.TypePaymentFailed,
			fmt.Sprintf("Payment failed for order #%d", ref.Number), reason, data, contact)
	case payable.StatusPaid:
		// New event: only raised with the rollout flag on, so flag-off
		// behaviour is exactly what it was.
		if !s.v2 {
			return
		}
		data["amount"] = pgutil.NumericToFloat(pay.Amount)
		notify.Dispatch(ctx, deps, &tenantID, notify.TypePaymentReceived,
			fmt.Sprintf("Payment received for order #%d", ref.Number),
			fmt.Sprintf("%.2f paid online", pgutil.NumericToFloat(pay.Amount)), data, contact)
	}
}

// Outcome of processing a gateway event.
const (
	KindApplied   = "applied"
	KindDuplicate = "duplicate"
	KindIgnored   = "ignored"
	KindRejected  = "rejected"
)

// Result describes what ProcessEvent did.
type Result struct {
	Kind    string
	Payment sqlc.Payment
	Ref     payable.Ref
	// Status and Label are the payable's status after any follow-up (an order
	// auto-accepting on payment). MoveErr is that follow-up failing; the
	// payment itself is already recorded either way.
	Status, Label string
	MoveErr       error
}

// ProcessEvent applies a verified gateway event: replay guard, amount check,
// state change, follow-up. The webhook endpoint and the sandbox confirm both
// end here.
func (s *Service) ProcessEvent(ctx context.Context, provider string, ev Event) (Result, error) {
	refundEvent := ev.Type == EventRefunded || ev.Type == EventRefundFailed
	if ev.Type != EventPaid && ev.Type != EventFailed && !refundEvent {
		// A verified event that changes nothing (a gateway sends many kinds).
		return Result{Kind: KindIgnored}, nil
	}
	var (
		pay sqlc.Payment
		err error
	)
	if ev.ProviderOrderID != "" {
		pay, err = s.q.GetPaymentByProviderOrder(ctx, sqlc.GetPaymentByProviderOrderParams{
			Provider: provider, ProviderOrderID: ev.ProviderOrderID,
		})
	} else {
		pay, err = s.q.GetPaymentByProviderPayment(ctx, sqlc.GetPaymentByProviderPaymentParams{
			Provider: provider, ProviderPaymentID: ev.ProviderPaymentID,
		})
	}
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}

	rec, err := s.q.InsertPaymentEvent(ctx, sqlc.InsertPaymentEventParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, Provider: provider,
		EventID: ev.ID, Type: ev.Type, Payload: rawJSON(ev.Raw),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{Kind: KindDuplicate, Payment: pay}, nil
	}
	if err != nil {
		return Result{}, err
	}
	finish := func(msg string) {
		if err := s.q.FinishPaymentEvent(ctx, sqlc.FinishPaymentEventParams{ID: rec.ID, Error: msg}); err != nil {
			s.log.Error("payments: finish event", "error", err)
		}
	}
	if !found {
		// Not ours or not current: acknowledge so the gateway stops retrying.
		finish("unknown_payment")
		return Result{Kind: KindIgnored}, nil
	}

	if refundEvent {
		msg, err := s.settleRefundEvent(ctx, pay, ev)
		finish(msg)
		if err != nil {
			return Result{}, err
		}
		if msg == "unknown_refund" {
			// A refund we never asked for (made in the gateway's own
			// dashboard, say): acknowledged, not ours to apply.
			return Result{Kind: KindIgnored, Payment: pay}, nil
		}
		return Result{Kind: KindApplied, Payment: pay}, nil
	}

	if ev.AmountMinor > 0 && ev.AmountMinor != minorUnits(pgutil.NumericToFloat(pay.Amount)) {
		// Never mark paid for the wrong amount.
		finish("amount_mismatch")
		s.Alert(ctx, "mismatch:"+pgutil.UUIDString(pay.ID), "A payment arrived for the wrong amount",
			fmt.Sprintf("The gateway reported %d (minor units) but the order is %d. It was NOT marked paid. Check it in the gateway dashboard.",
				ev.AmountMinor, minorUnits(pgutil.NumericToFloat(pay.Amount))),
			map[string]any{"payment_id": pgutil.UUIDString(pay.ID)})
		s.audit(ctx, pay, "AMOUNT_MISMATCH", fmt.Sprintf("event %d, expected %d", ev.AmountMinor, minorUnits(pgutil.NumericToFloat(pay.Amount))))
		return Result{Kind: KindRejected, Payment: pay}, nil
	}

	ref, err := s.src.Load(ctx, uuid.UUID(pay.TenantID.Bytes), pay.PayableID)
	if err != nil {
		finish("payable_missing")
		return Result{}, err
	}
	to := payable.StatusFailed
	if ev.Type == EventPaid {
		to = payable.StatusPaid
	}
	updated, changed, err := s.Apply(ctx, ref, pay, to, ev.Reason, Actor{Type: "provider", ID: provider})
	if err != nil {
		finish("apply_failed")
		return Result{}, err
	}
	if changed && to == payable.StatusPaid && ev.ProviderPaymentID != "" {
		// Remember which gateway payment this is before anything else (a refund
		// of a late payment, below) needs to name it.
		_ = s.q.SetPaymentProviderPaymentID(ctx, sqlc.SetPaymentProviderPaymentIDParams{
			ID: pay.ID, TenantID: pay.TenantID, ProviderPaymentID: ev.ProviderPaymentID,
		})
	}
	if !changed && to == payable.StatusPaid && ev.ProviderPaymentID != "" && ev.ProviderPaymentID != pay.ProviderPaymentID {
		s.Alert(ctx, "duplicate:"+pgutil.UUIDString(pay.ID), "A customer was charged twice",
			"Two gateway payments were captured for one order. The extra one is being refunded automatically; confirm it completed.",
			map[string]any{"payment_id": pgutil.UUIDString(pay.ID), "gateway_payment_id": ev.ProviderPaymentID})
		// The customer was charged twice for one order (two attempts both
		// captured). The first stands; this one needs refunding — flagged for
		// the owner rather than silently kept.
		s.audit(ctx, pay, "DUPLICATE_PAYMENT", "second capture "+ev.ProviderPaymentID+" for an order already paid")
		s.log.Error("payments: duplicate capture", "payment", pgutil.UUIDString(pay.ID), "gateway_payment", ev.ProviderPaymentID)
		// The customer paid twice: send the extra one back.
		s.refundDuplicate(ctx, pay, ev)
	}
	if changed && to == payable.StatusPaid && ref.Status == "CANCELLED" {
		// Paid after the order was cancelled: never keep money for a dead order.
		if _, err := s.RefundPayment(ctx, uuid.UUID(pay.TenantID.Bytes), pay.ID, "Paid after the order was cancelled", Actor{Type: "system", ID: "late_payment"}); err != nil {
			s.log.Warn("payments: refund of a late payment not completed", "payment", pgutil.UUIDString(pay.ID), "error", err)
		}
	}
	finish("")

	res := Result{Kind: KindApplied, Payment: updated, Ref: ref, Status: ref.Status, Label: ref.StatusLabel}
	if updated.Status == payable.StatusPaid {
		res.Status, res.Label, res.MoveErr = s.src.AfterPaid(ctx, uuid.UUID(pay.TenantID.Bytes), ref)
		if res.MoveErr != nil {
			s.log.Error("payments: follow-up after payment failed", "error", res.MoveErr)
			res.Status, res.Label = ref.Status, ref.StatusLabel
		}
	}
	return res, nil
}

func (s *Service) audit(ctx context.Context, pay sqlc.Payment, event, reason string) {
	if err := s.q.InsertPaymentAudit(ctx, sqlc.InsertPaymentAuditParams{
		TenantID: pay.TenantID, PaymentID: pay.ID, EventType: event,
		OldStatus: pay.Status, NewStatus: pay.Status, ActorType: "provider", Reason: reason,
	}); err != nil {
		s.log.Error("payments: audit", "error", err)
	}
}

// minorUnits converts a decimal amount to integer minor units (paise/cents),
// rounding half away from zero once.
func minorUnits(amount float64) int64 { return int64(math.Round(amount * 100)) }

func rawJSON(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// refreshGap keeps polling clients from hammering the gateway.
const refreshGap = 5 * time.Second

// RefreshPending reconciles one pending live payment with the gateway: if the
// customer has paid but the webhook has not arrived, this is what notices. It
// is safe to call often (throttled) and never marks anything paid on its own
// say-so — it asks the gateway and applies the answer through ProcessEvent.
func (s *Service) RefreshPending(ctx context.Context, pay sqlc.Payment) (Result, bool, error) {
	if pay.Status != payable.StatusPending || pay.Mode != string(ModeLive) || pay.ProviderOrderID == "" || pay.Provider != razorpayName {
		return Result{}, false, nil
	}
	key := pgutil.UUIDString(pay.ID)
	if last, ok := s.lastRefresh.Load(key); ok && time.Since(last.(time.Time)) < refreshGap {
		return Result{}, false, nil
	}
	s.lastRefresh.Store(key, time.Now())

	gw, err := s.razorpayFor(ctx, uuid.UUID(pay.TenantID.Bytes))
	if err != nil {
		return Result{}, false, err
	}
	ev, err := gw.CheckStatus(ctx, pay.ProviderOrderID)
	if err != nil || ev == nil {
		return Result{}, false, err
	}
	res, err := s.ProcessEvent(ctx, razorpayName, *ev)
	return res, err == nil && res.Kind == KindApplied, err
}

// SweepExpired closes payments whose time ran out. A live payment is first
// reconciled with the gateway, so a customer who paid but whose webhook was
// lost is marked paid, not expired. Returns how many were expired.
func (s *Service) SweepExpired(ctx context.Context, limit int32) (int, error) {
	rows, err := s.q.ListExpiredPendingPayments(ctx, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, pay := range rows {
		s.lastRefresh.Delete(pgutil.UUIDString(pay.ID))
		if _, _, err := s.RefreshPending(ctx, pay); err != nil {
			// Cannot ask the gateway: leave it pending and try next sweep,
			// rather than expiring a payment that may have been paid.
			s.log.Warn("payments: sweep could not reconcile", "payment", pgutil.UUIDString(pay.ID), "error", err)
			continue
		}
		current, err := s.q.GetPaymentByID(ctx, sqlc.GetPaymentByIDParams{ID: pay.ID, TenantID: pay.TenantID})
		if err != nil || current.Status != payable.StatusPending {
			continue
		}
		ref, err := s.src.Load(ctx, uuid.UUID(pay.TenantID.Bytes), pay.PayableID)
		if err != nil {
			continue
		}
		if _, changed, err := s.Apply(ctx, ref, current, payable.StatusExpired, "payment window elapsed", Actor{Type: "system", ID: "sweeper"}); err == nil && changed {
			expired++
		}
	}
	return expired, nil
}

// RunSweeper expires stale payments until ctx ends.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Watch(ctx)
			if n, err := s.ResumeRefunds(ctx, 20); err != nil {
				s.log.Error("payments: refund resume failed", "error", err)
			} else if n > 0 {
				s.log.Info("payments: re-sent interrupted refunds", "count", n)
			}
			if n, err := s.SweepExpired(ctx, 50); err != nil {
				s.log.Error("payments: sweep failed", "error", err)
			} else if n > 0 {
				s.log.Info("payments: expired stale payments", "count", n)
			}
		}
	}
}

// ResetRefreshThrottle clears the per-payment gateway lookup throttle, so a
// test does not have to wait out real seconds.
func (s *Service) ResetRefreshThrottle() {
	s.lastRefresh.Range(func(k, _ any) bool { s.lastRefresh.Delete(k); return true })
}

// alertEvery is how long an alert key stays quiet after it fires.
const alertEvery = 15 * time.Minute

// Alert tells the platform operator something about payments needs a human. The
// same key will not alert again for a while. Best effort, like every
// notification: it must never fail the payment path it is reporting on.
func (s *Service) Alert(ctx context.Context, key, title, body string, data map[string]any) {
	if last, ok := s.alerted.Load(key); ok && time.Since(last.(time.Time)) < alertEvery {
		return
	}
	s.alerted.Store(key, time.Now())
	s.log.Warn("payments: alert", "key", key, "title", title)
	if data == nil {
		data = map[string]any{}
	}
	data["alert_key"] = key
	notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, nil, notify.TypePlatformOperationalAlert, title, body, data, notify.Contact{})
}

// Health is the platform-wide payment health snapshot.
type Health struct {
	OverduePending    int64  `json:"overdue_pending"`
	StuckRefunding    int64  `json:"stuck_refunding"`
	StuckRefunds      int64  `json:"stuck_refunds"`
	FailedEvents24h   int64  `json:"failed_events_24h"`
	Mismatches24h     int64  `json:"mismatches_24h"`
	Duplicates24h     int64  `json:"duplicates_24h"`
	LivePaid24h       int64  `json:"live_paid_24h"`
	LivePaidAmount24h string `json:"live_paid_amount_24h"`
	// NeedsAttention is true when anything above needs a person.
	NeedsAttention bool `json:"needs_attention"`
}

// Health reports what needs a human right now.
func (s *Service) Health(ctx context.Context) (Health, error) {
	row, err := s.q.PaymentHealth(ctx)
	if err != nil {
		return Health{}, err
	}
	h := Health{
		OverduePending: row.OverduePending, StuckRefunding: row.StuckRefunding, StuckRefunds: row.StuckRefunds,
		FailedEvents24h: row.FailedEvents24h, Mismatches24h: row.Mismatches24h, Duplicates24h: row.Duplicates24h,
		LivePaid24h: row.LivePaid24h, LivePaidAmount24h: row.LivePaidAmount24h,
	}
	h.NeedsAttention = h.OverduePending+h.StuckRefunding+h.StuckRefunds+h.FailedEvents24h+h.Mismatches24h+h.Duplicates24h > 0
	return h, nil
}

// Reconcile asks the gateway about one pending live payment right now,
// ignoring the polling throttle. It is the operator's tool for "the customer
// says they paid": the answer is applied through the normal path.
func (s *Service) Reconcile(ctx context.Context, paymentID pgtype.UUID) (sqlc.Payment, bool, error) {
	pay, err := s.q.GetPaymentAnyTenant(ctx, paymentID)
	if err != nil {
		return sqlc.Payment{}, false, err
	}
	s.lastRefresh.Delete(pgutil.UUIDString(pay.ID))
	_, changed, err := s.RefreshPending(ctx, pay)
	fresh, ferr := s.q.GetPaymentAnyTenant(ctx, paymentID)
	if ferr != nil {
		return pay, changed, ferr
	}
	return fresh, changed, err
}

// Watch raises alerts for things that have been wrong for a while. Called by
// the sweeper each tick.
func (s *Service) Watch(ctx context.Context) {
	if rows, err := s.q.ListOverduePending(ctx, 20); err == nil {
		for _, p := range rows {
			s.Alert(ctx, "overdue:"+pgutil.UUIDString(p.ID),
				"A payment is stuck waiting for the gateway",
				"A payment is more than 30 minutes past its window and could not be closed. Check the gateway connection, then reconcile it from the payment health panel.",
				map[string]any{"payment_id": pgutil.UUIDString(p.ID)})
		}
	}
	if rows, err := s.q.ListStuckRefunds(ctx, 20); err == nil {
		for _, rf := range rows {
			s.Alert(ctx, "refund:"+pgutil.UUIDString(rf.ID),
				"A refund has not been confirmed by the gateway",
				"A refund was accepted over an hour ago but never confirmed. Check it in the gateway dashboard.",
				map[string]any{"refund_id": pgutil.UUIDString(rf.ID), "payment_id": pgutil.UUIDString(rf.PaymentID)})
		}
	}
}

// badSignatureThreshold is how many rejected callbacks in badSignatureWindow
// are worth waking someone for. One is noise; a run of them means either an
// attack or — far more often — a webhook secret that does not match.
const (
	badSignatureThreshold = 10
	badSignatureWindow    = 5 * time.Minute
)

// NoteBadSignature records a rejected callback and alerts when they pile up.
func (s *Service) NoteBadSignature(ctx context.Context, provider string) {
	s.badMu.Lock()
	if time.Since(s.badWindow) > badSignatureWindow {
		s.badWindow, s.badCount = time.Now(), 0
	}
	s.badCount++
	fire := s.badCount == badSignatureThreshold
	s.badMu.Unlock()
	if fire {
		s.Alert(ctx, "badsig:"+provider, "Payment webhooks are failing signature checks",
			"Several callbacks from "+provider+" were rejected in a few minutes. If you just changed a webhook secret, the business's saved secret no longer matches the gateway's. Otherwise someone may be probing the endpoint.",
			map[string]any{"provider": provider})
	}
}
