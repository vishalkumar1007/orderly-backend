package payments

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const razorpayName = "razorpay"

// razorpayIntentTTL is how long a customer has to finish paying.
const razorpayIntentTTL = 30 * time.Minute

// Razorpay is the live gateway. Credentials are per business, so a Razorpay
// value is built for one payment's tenant and never shared.
type Razorpay struct {
	baseURL       string
	keyID         string
	keySecret     string
	webhookSecret string
	client        *http.Client
	log           *slog.Logger
	now           func() time.Time
}

// NewRazorpay builds the gateway client for one set of credentials.
func NewRazorpay(baseURL, keyID, keySecret, webhookSecret string) *Razorpay {
	return &Razorpay{
		baseURL: strings.TrimRight(baseURL, "/"), keyID: keyID, keySecret: keySecret,
		webhookSecret: webhookSecret, now: time.Now,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (r *Razorpay) Name() string { return razorpayName }

// Initiate opens a gateway order. The amount always comes from the caller (the
// database), never from the browser; the checkout widget is then handed only
// the public key id and this order id.
func (r *Razorpay) Initiate(ctx context.Context, in IntentInput) (Intent, error) {
	payload := map[string]any{
		"amount":   in.AmountMinor,
		"currency": in.Currency,
		"receipt":  truncate(in.PaymentID, 40),
		"notes":    map[string]string{"orderly_payable": in.PaymentID},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := r.do(ctx, http.MethodPost, "/v1/orders", payload, &out); err != nil {
		return Intent{}, err
	}
	if out.ID == "" {
		return Intent{}, errors.New("gateway returned no order id")
	}
	return Intent{
		ProviderOrderID: out.ID,
		ExpiresAt:       r.now().Add(razorpayIntentTTL),
		Checkout: map[string]any{
			"gateway":      razorpayName,
			"key_id":       r.keyID,
			"order_id":     out.ID,
			"amount_minor": in.AmountMinor,
			"currency":     in.Currency,
		},
	}, nil
}

// Refund returns money for one captured payment. The refund's own id is sent
// as the idempotency key (and receipt), so a retry after a crash or timeout
// cannot refund twice.
func (r *Razorpay) Refund(ctx context.Context, in RefundInput) (RefundOutcome, error) {
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	payload := map[string]any{
		"amount":  in.AmountMinor,
		"receipt": truncate(in.Key, 40),
		"notes":   map[string]string{"reason": truncate(in.Reason, 100)},
	}
	if err := r.doWith(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(in.ProviderPaymentID)+"/refund",
		payload, &out, map[string]string{"X-Refund-Idempotency": in.Key}); err != nil {
		return RefundOutcome{}, err
	}
	if out.ID == "" {
		return RefundOutcome{}, fmt.Errorf("%w: refund returned no id", ErrGateway)
	}
	switch out.Status {
	case "processed":
		return RefundOutcome{ID: out.ID, Status: RefundDone}, nil
	case "failed":
		return RefundOutcome{}, &GatewayError{Status: http.StatusUnprocessableEntity}
	}
	// "pending": the gateway accepted it and will confirm by webhook.
	return RefundOutcome{ID: out.ID, Status: RefundPending}, nil
}

// CheckStatus asks the gateway whether an order has been paid, for when the
// webhook is late or lost. It reports only a captured payment: a failed
// attempt does not close the order, because the customer may retry inside the
// same checkout and succeed.
func (r *Razorpay) CheckStatus(ctx context.Context, providerOrderID string) (*Event, error) {
	var out struct {
		Items []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Amount int64  `json:"amount"`
		} `json:"items"`
	}
	if err := r.do(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(providerOrderID)+"/payments", nil, &out); err != nil {
		return nil, err
	}
	for _, p := range out.Items {
		if p.Status == "captured" {
			return &Event{
				ID: "fetch_" + p.ID + "_captured", Type: EventPaid,
				ProviderOrderID: providerOrderID, ProviderPaymentID: p.ID, AmountMinor: p.Amount,
			}, nil
		}
	}
	return nil, nil
}

// VerifyWebhook authenticates a callback with the business's webhook secret
// (HMAC-SHA256 of the raw body, hex) and normalises it. Events that do not
// change a payment's state come back with an empty Type.
func (r *Razorpay) VerifyWebhook(h http.Header, body []byte) (Event, error) {
	sig := strings.TrimSpace(h.Get("X-Razorpay-Signature"))
	if sig == "" || r.webhookSecret == "" {
		return Event{}, ErrBadSignature
	}
	m := hmac.New(sha256.New, []byte(r.webhookSecret))
	m.Write(body)
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(m.Sum(nil)))) {
		return Event{}, ErrBadSignature
	}
	ev, err := decodeRazorpayEvent(body)
	if err != nil {
		return Event{}, ErrBadSignature
	}
	// Razorpay numbers every delivery; a retry carries the same id, which is
	// exactly what the replay guard needs. Fall back to a body hash.
	ev.ID = strings.TrimSpace(h.Get("X-Razorpay-Event-Id"))
	if ev.ID == "" {
		sum := sha256.Sum256(body)
		ev.ID = "body_" + hex.EncodeToString(sum[:16])
	}
	// Keep only what is needed to understand the event later. The delivery
	// carries the customer's email, phone and payment instrument; none of that
	// belongs in our event log.
	ev.Raw = redactedEvent(ev)
	return ev, nil
}

func redactedEvent(ev Event) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": ev.Type, "order_id": ev.ProviderOrderID, "payment_id": ev.ProviderPaymentID,
		"refund_id": ev.RefundID, "amount_minor": ev.AmountMinor, "reason": ev.Reason,
	})
	return b
}

// razorpayOrderRef extracts the gateway order id from an UNVERIFIED callback so
// the right business's webhook secret can be looked up. Nothing read here is
// trusted until VerifyWebhook passes.
func razorpayOrderRef(body []byte) string {
	ev, err := decodeRazorpayEvent(body)
	if err != nil {
		return ""
	}
	return ev.ProviderOrderID
}

// razorpayPaymentRef is the gateway payment id of an UNVERIFIED callback, used
// for refund events, which name a payment rather than an order.
func razorpayPaymentRef(body []byte) string {
	ev, err := decodeRazorpayEvent(body)
	if err != nil {
		return ""
	}
	return ev.ProviderPaymentID
}

func decodeRazorpayEvent(body []byte) (Event, error) {
	var in struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID               string `json:"id"`
					OrderID          string `json:"order_id"`
					Amount           int64  `json:"amount"`
					ErrorDescription string `json:"error_description"`
				} `json:"entity"`
			} `json:"payment"`
			Order struct {
				Entity struct {
					ID         string `json:"id"`
					AmountPaid int64  `json:"amount_paid"`
				} `json:"entity"`
			} `json:"order"`
			Refund struct {
				Entity struct {
					ID        string `json:"id"`
					PaymentID string `json:"payment_id"`
					Amount    int64  `json:"amount"`
				} `json:"entity"`
			} `json:"refund"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Event{}, err
	}
	pay := in.Payload.Payment.Entity
	ev := Event{
		ProviderOrderID: pay.OrderID, ProviderPaymentID: pay.ID, AmountMinor: pay.Amount,
		Reason: truncate(pay.ErrorDescription, 200),
	}
	if ev.ProviderOrderID == "" {
		ev.ProviderOrderID = in.Payload.Order.Entity.ID
	}
	if rf := in.Payload.Refund.Entity; rf.ID != "" {
		ev.RefundID, ev.ProviderPaymentID, ev.AmountMinor = rf.ID, rf.PaymentID, rf.Amount
	}
	switch in.Event {
	case "refund.processed":
		ev.Type = EventRefunded
	case "refund.failed":
		ev.Type = EventRefundFailed
	case "payment.captured", "order.paid":
		ev.Type = EventPaid
	case "payment.failed":
		ev.Type = EventFailed
		if ev.Reason == "" {
			ev.Reason = "Payment was declined"
		}
	}
	return ev, nil
}

// ErrGateway wraps a failed gateway call without leaking its response.
var ErrGateway = errors.New("payment gateway error")

// GatewayError is a gateway call that came back with a non-success status.
type GatewayError struct{ Status int }

func (e *GatewayError) Error() string { return fmt.Sprintf("%s: status %d", ErrGateway, e.Status) }
func (e *GatewayError) Unwrap() error { return ErrGateway }

// Permanent reports whether retrying the same request cannot help: the gateway
// understood it and said no (a 4xx other than rate limiting).
func (e *GatewayError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests
}

// IsPermanentGatewayError reports whether err is a definite "no" from the gateway.
func IsPermanentGatewayError(err error) bool {
	var ge *GatewayError
	return errors.As(err, &ge) && ge.Permanent()
}

// logCall records how the gateway behaved, never what it said: status and
// latency only.
func (r *Razorpay) logCall(method, path string, status int, started time.Time) {
	if r.log == nil {
		return
	}
	// Ids in the path vary per call; log the shape.
	op := method + " " + path
	if i := strings.Index(op, "/v1/payments/"); i >= 0 {
		op = method + " /v1/payments/{id}/…"
	} else if i := strings.Index(op, "/v1/orders/"); i >= 0 {
		op = method + " /v1/orders/{id}/…"
	}
	r.log.Info("payments: gateway call", "provider", razorpayName, "op", op, "status", status, "ms", time.Since(started).Milliseconds())
}

func (r *Razorpay) do(ctx context.Context, method, path string, body, out any) error {
	return r.doWith(ctx, method, path, body, out, nil)
}

func (r *Razorpay) doWith(ctx context.Context, method, path string, body, out any, headers map[string]string) error {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.keyID, r.keySecret)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	started := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		r.logCall(method, path, 0, started)
		return fmt.Errorf("%w: unreachable", ErrGateway)
	}
	defer resp.Body.Close()
	r.logCall(method, path, resp.StatusCode, started)
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The status only: gateway error bodies can echo request details.
		return &GatewayError{Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: unreadable response", ErrGateway)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
