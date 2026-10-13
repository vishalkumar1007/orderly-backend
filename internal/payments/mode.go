package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Mode is whether payments are dummy or real.
type Mode string

const (
	ModeSandbox Mode = "SANDBOX"
	ModeLive    Mode = "LIVE"
)

// ParseMode validates a mode string.
func ParseMode(v string) (Mode, bool) {
	switch Mode(upperTrim(v)) {
	case ModeSandbox:
		return ModeSandbox, true
	case ModeLive:
		return ModeLive, true
	}
	return "", false
}

// ErrGatewayUnavailable means no gateway can take a payment for this business
// right now. It is never answered by quietly using the sandbox.
var ErrGatewayUnavailable = errors.New("payment gateway unavailable")

// ErrNotReady means a business cannot go live yet.
var ErrNotReady = errors.New("payment gateway is not ready for live payments")

// Where an effective mode came from.
const (
	SourceTenant   = "business"
	SourcePlatform = "platform"
	SourceDefault  = "default"
)

// Check is one line of the go-live checklist.
type Check struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Readiness answers "could this business take real payments right now?".
type Readiness struct {
	Ready    bool    `json:"ready"`
	Checks   []Check `json:"checks"`
	Source   string  `json:"credentials_source,omitempty"` // PLATFORM | ORGANIZATION
	Provider string  `json:"provider,omitempty"`
	// KeyKind is "test" or "live": which gateway environment the keys belong
	// to. Going live with test keys is allowed (it is how a gateway account is
	// proven) but must be visible.
	KeyKind string `json:"key_kind,omitempty"`
}

// LiveAvailable reports whether this build can process a live gateway payment.
func (s *Service) LiveAvailable() bool { return true }

// Readiness evaluates the go-live checklist for the platform (tenantID nil) or
// for one business, using the credentials that business would actually use.
func (s *Service) Readiness(ctx context.Context, tenantID *uuid.UUID) (Readiness, error) {
	out := Readiness{Checks: []Check{}}
	resolved, err := s.cfg.Resolver().Resolve(ctx, scopeOf(tenantID), tenantID, configsvc.ServicePayments)
	var unavailable *configsvc.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		out.Checks = append(out.Checks,
			Check{Key: "credentials", Label: "Gateway credentials saved and enabled", Detail: unavailable.Reason},
			Check{Key: "webhook", Label: "Webhook secret saved"},
			Check{Key: "tested", Label: "Connection test passed"})
		return out, nil
	case err != nil:
		return out, err
	}
	out.Source, out.Provider, out.KeyKind = string(resolved.Source), resolved.Provider, keyKind(resolved.String("key_id"))

	webhook := Check{Key: "webhook", Label: "Webhook secret saved", OK: resolved.Secret("webhook_secret") != ""}
	if !webhook.OK {
		webhook.Detail = "Add the webhook signing secret so payment confirmations can be verified."
	}
	tested := Check{Key: "tested", Label: "Connection test passed", OK: resolved.TestedAt != nil && resolved.Status == configsvc.StatusEnabled}
	if !tested.OK {
		tested.Detail = "Run “Test connection” after saving the credentials."
	}
	out.Checks = append(out.Checks, Check{Key: "credentials", Label: "Gateway credentials saved and enabled", OK: true}, webhook, tested)
	out.Ready = webhook.OK && tested.OK
	return out, nil
}

func keyKind(keyID string) string {
	c := configsvc.PaymentsConfig{KeyID: keyID}
	return c.KeyKind()
}

func scopeOf(tenantID *uuid.UUID) configsvc.Scope {
	if tenantID == nil {
		return configsvc.ScopePlatform
	}
	return configsvc.ScopeTenant
}

// ModeSetting is a stored mode and when/by whom it was set.
type ModeSetting struct {
	Mode      Mode       `json:"mode"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty"`
}

func settingOf(row sqlc.PaymentSetting) *ModeSetting {
	m := &ModeSetting{Mode: Mode(row.Mode)}
	if row.UpdatedAt.Valid {
		t := row.UpdatedAt.Time
		m.UpdatedAt = &t
	}
	if row.UpdatedBy.Valid {
		id := uuid.UUID(row.UpdatedBy.Bytes)
		m.UpdatedBy = &id
	}
	return m
}

// PlatformMode is the platform default, or nil when never set (meaning SANDBOX).
func (s *Service) PlatformMode(ctx context.Context) (*ModeSetting, error) {
	row, err := s.q.GetPlatformPaymentMode(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return settingOf(row), nil
}

// TenantMode is one business's override, or nil when it inherits.
func (s *Service) TenantMode(ctx context.Context, tenantID uuid.UUID) (*ModeSetting, error) {
	row, err := s.q.GetTenantPaymentMode(ctx, pgutil.UUID(tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return settingOf(row), nil
}

// EffectiveMode is the mode a business's next payment runs in: its own
// override, else the platform default, else SANDBOX.
func (s *Service) EffectiveMode(ctx context.Context, tenantID uuid.UUID) (Mode, string, error) {
	if t, err := s.TenantMode(ctx, tenantID); err != nil {
		return "", "", err
	} else if t != nil {
		return t.Mode, SourceTenant, nil
	}
	if p, err := s.PlatformMode(ctx); err != nil {
		return "", "", err
	} else if p != nil {
		return p.Mode, SourcePlatform, nil
	}
	return ModeSandbox, SourceDefault, nil
}

// SetPlatformMode changes the platform default. Going LIVE needs a ready
// gateway; going back to SANDBOX never does.
func (s *Service) SetPlatformMode(ctx context.Context, mode Mode, actor uuid.UUID) (*ModeSetting, Readiness, error) {
	ready, err := s.guardLive(ctx, mode, nil)
	if err != nil {
		return nil, ready, err
	}
	row, err := s.q.UpsertPlatformPaymentMode(ctx, sqlc.UpsertPlatformPaymentModeParams{Mode: string(mode), UpdatedBy: pgutil.UUID(actor)})
	if err != nil {
		return nil, ready, err
	}
	return settingOf(row), ready, nil
}

// SetTenantMode sets (mode != nil) or clears (mode == nil) a business override.
func (s *Service) SetTenantMode(ctx context.Context, tenantID uuid.UUID, mode *Mode, actor uuid.UUID) (*ModeSetting, Readiness, error) {
	if mode == nil {
		return nil, Readiness{}, s.q.DeleteTenantPaymentMode(ctx, pgutil.UUID(tenantID))
	}
	ready, err := s.guardLive(ctx, *mode, &tenantID)
	if err != nil {
		return nil, ready, err
	}
	row, err := s.q.UpsertTenantPaymentMode(ctx, sqlc.UpsertTenantPaymentModeParams{
		TenantID: pgutil.UUID(tenantID), Mode: string(*mode), UpdatedBy: pgutil.UUID(actor)})
	if err != nil {
		return nil, ready, err
	}
	return settingOf(row), ready, nil
}

func (s *Service) guardLive(ctx context.Context, mode Mode, tenantID *uuid.UUID) (Readiness, error) {
	if mode != ModeLive {
		return Readiness{}, nil
	}
	r, err := s.Readiness(ctx, tenantID)
	if err != nil {
		return r, err
	}
	if !r.Ready {
		return r, fmt.Errorf("%w", ErrNotReady)
	}
	return r, nil
}

// ProviderFor picks the gateway for a business's next payment, from its
// effective mode. In LIVE mode it never falls back to the sandbox.
func (s *Service) ProviderFor(ctx context.Context, tenantID uuid.UUID) (PaymentProvider, Mode, error) {
	mode, _, err := s.EffectiveMode(ctx, tenantID)
	if err != nil {
		return nil, "", err
	}
	if mode == ModeLive {
		gw, err := s.razorpayFor(ctx, tenantID)
		if err != nil {
			// Credentials missing, disabled or undecryptable: say so, never
			// fall back to the sandbox.
			return nil, mode, ErrGatewayUnavailable
		}
		return gw, mode, nil
	}
	p, ok := s.Provider(sandboxName)
	if !ok {
		return nil, mode, ErrGatewayUnavailable
	}
	return p, mode, nil
}
