package payments

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PaymentProvider is the gateway seam (book Ch.27). Everything that moves money
// goes through one of these; nothing else in the codebase knows which gateway
// is behind it. CheckStatus and Refund join the interface with the first real
// gateway (plan P-C / P-D), when there is something to implement them against.
type PaymentProvider interface {
	Name() string
	// Initiate opens a payment attempt with the gateway.
	Initiate(ctx context.Context, in IntentInput) (Intent, error)
	// VerifyWebhook authenticates a gateway callback and decodes it. It must
	// return ErrBadSignature for anything it cannot prove came from the gateway.
	VerifyWebhook(h http.Header, body []byte) (Event, error)
	// CheckStatus asks the gateway whether an order was paid, for a lost or
	// late webhook. A nil event means nothing to report.
	CheckStatus(ctx context.Context, providerOrderID string) (*Event, error)
	// Refund returns money for one gateway payment. It must be idempotent on
	// RefundInput.Key: asking twice with the same key refunds once.
	Refund(ctx context.Context, in RefundInput) (RefundOutcome, error)
}

// RefundInput describes one refund.
type RefundInput struct {
	ProviderPaymentID string
	AmountMinor       int64
	Reason            string
	// Key identifies this refund to the gateway so a retry cannot double it.
	Key string
}

// Refund states a gateway can answer with.
const (
	RefundDone    = "DONE"
	RefundPending = "PENDING"
)

// RefundOutcome is the gateway's answer.
type RefundOutcome struct {
	ID     string
	Status string // RefundDone | RefundPending
}

// IntentInput describes one attempt.
type IntentInput struct {
	PaymentID   string
	AmountMinor int64
	Currency    string
}

// Intent is the gateway's answer to Initiate.
type Intent struct {
	ProviderOrderID string
	// ClientToken is what the storefront presents back. Sandbox only: a real
	// gateway's proof is its signed webhook, not anything the browser holds.
	ClientToken string
	ExpiresAt   time.Time
	// Checkout is what the storefront needs to open the gateway's payment
	// widget. Public values only: never a secret.
	Checkout map[string]any
}

// Event kinds a provider can report.
const (
	EventPaid         = "PAID"
	EventFailed       = "FAILED"
	EventRefunded     = "REFUNDED"
	EventRefundFailed = "REFUND_FAILED"
)

// Event is a verified gateway callback, normalised.
type Event struct {
	ID                string
	Type              string
	ProviderOrderID   string
	ProviderPaymentID string
	AmountMinor       int64
	Reason            string
	// RefundID is set on refund events.
	RefundID string
	Raw      []byte
}

// ErrBadSignature marks a callback that could not be authenticated.
var ErrBadSignature = errors.New("invalid webhook signature")

// ---------------------------------------------------------------------------
// Sandbox provider
// ---------------------------------------------------------------------------

// SignatureHeader carries "t=<unix>,v1=<hex hmac-sha256 of "<t>.<body>">".
const SignatureHeader = "X-Orderly-Signature"

const (
	sandboxName      = "sandbox"
	sandboxTolerance = 5 * time.Minute
	sandboxIntentTTL = 30 * time.Minute
)

// Sandbox is the dummy gateway. It behaves like a real one: the "gateway"
// signs a callback and the same verify, replay-guard and state machine run, so
// a sandbox test exercises the production path. No money ever moves.
type Sandbox struct {
	secret []byte
	now    func() time.Time
}

// NewSandbox builds the sandbox with its signing secret.
func NewSandbox(secret string) *Sandbox {
	return &Sandbox{secret: []byte(secret), now: time.Now}
}

func (s *Sandbox) Name() string { return sandboxName }

func (s *Sandbox) Initiate(_ context.Context, _ IntentInput) (Intent, error) {
	return Intent{
		ProviderOrderID: "sbx_" + randomToken(12),
		ClientToken:     newIntentToken(),
		ExpiresAt:       s.now().Add(sandboxIntentTTL),
	}, nil
}

// sandboxBody is the callback payload.
type sandboxBody struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ProviderOrderID string `json:"provider_order_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Reason          string `json:"reason,omitempty"`
}

// Sign produces the callback body and headers the sandbox "gateway" would send.
func (s *Sandbox) Sign(ev Event) ([]byte, http.Header) {
	body, _ := json.Marshal(sandboxBody{
		ID: ev.ID, Type: ev.Type, ProviderOrderID: ev.ProviderOrderID,
		AmountMinor: ev.AmountMinor, Reason: ev.Reason,
	})
	ts := strconv.FormatInt(s.now().Unix(), 10)
	h := http.Header{}
	h.Set(SignatureHeader, "t="+ts+",v1="+s.mac(ts, body))
	return body, h
}

func (s *Sandbox) Refund(_ context.Context, in RefundInput) (RefundOutcome, error) {
	// Deterministic from the key, so a retry gets the same refund back.
	return RefundOutcome{ID: "sbx_rf_" + in.Key, Status: RefundDone}, nil
}

func (s *Sandbox) CheckStatus(context.Context, string) (*Event, error) { return nil, nil }

func (s *Sandbox) VerifyWebhook(h http.Header, body []byte) (Event, error) {
	var ts, sig string
	for _, part := range strings.Split(h.Get(SignatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return Event{}, ErrBadSignature
	}
	if d := s.now().Sub(time.Unix(unix, 0)); d > sandboxTolerance || d < -sandboxTolerance {
		return Event{}, ErrBadSignature
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(ts, body))) {
		return Event{}, ErrBadSignature
	}
	var b sandboxBody
	if err := json.Unmarshal(body, &b); err != nil || b.ID == "" || b.ProviderOrderID == "" {
		return Event{}, ErrBadSignature
	}
	return Event{
		ID: b.ID, Type: b.Type, ProviderOrderID: b.ProviderOrderID,
		AmountMinor: b.AmountMinor, Reason: b.Reason, Raw: body,
	}, nil
}

func (s *Sandbox) mac(ts string, body []byte) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
