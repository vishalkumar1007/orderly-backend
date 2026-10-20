// Package signin decides which customer sign-in methods a business can offer
// and delivers the codes. A method is a promise that a code will arrive, so
// it is only offered when its channel is set up in Configuration: phone codes
// need SMS, email codes need email. The owner then chooses which of the ready
// methods customers get.
package signin

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/storefront"
)

// Channel is whether one method's delivery channel works for a business.
type Channel = storefront.SignInChannel

// ErrNotOffered means the method is switched off or its channel is not set up.
var ErrNotOffered = errors.New("this sign-in method is not available here")

// Service checks channels and sends codes.
type Service struct {
	cfg *configsvc.Service
	q   *sqlc.Queries
	// fallback treats every channel as ready and sends nothing. It exists for
	// automated tests, which have no SMS or mail account, and is never set
	// in production (see NewService).
	fallback bool
}

// NewService wires the service. fallback is honoured only outside production.
func NewService(pool *pgxpool.Pool, cfg *configsvc.Service, production, fallback bool) *Service {
	return &Service{cfg: cfg, q: sqlc.New(pool), fallback: fallback && !production}
}

var serviceFor = map[string]configsvc.ServiceType{
	storefront.LoginMethodPhone: configsvc.ServiceSMS,
	storefront.LoginMethodEmail: configsvc.ServiceSMTP,
}

// Channels reports, for each method, whether its channel works for tenantID.
func (s *Service) Channels(ctx context.Context, tenantID uuid.UUID) map[string]Channel {
	out := make(map[string]Channel, len(serviceFor))
	for method, service := range serviceFor {
		if s.fallback {
			out[method] = Channel{Ready: true, Source: "TEST"}
			continue
		}
		if s.cfg == nil {
			out[method] = Channel{Reason: "Not set up yet"}
			continue
		}
		resolved, err := s.cfg.Resolver().Resolve(ctx, configsvc.ScopeTenant, &tenantID, service)
		if err != nil {
			var un *configsvc.UnavailableError
			reason := "Not set up yet"
			if errors.As(err, &un) && un.Reason != "" {
				reason = un.Reason
			}
			out[method] = Channel{Reason: reason}
			continue
		}
		out[method] = Channel{Ready: true, Source: string(resolved.Source)}
	}
	return out
}

// Offered is what customers see: the owner's chosen methods whose channel is
// ready, in a stable order. Empty means nobody can sign in.
func (s *Service) Offered(ctx context.Context, tenantID uuid.UUID, sf *storefront.Storefront) []string {
	if sf == nil {
		return []string{}
	}
	channels := s.Channels(ctx, tenantID)
	out := make([]string, 0, 2)
	for _, m := range sf.CustomerLoginMethods {
		if channels[m].Ready {
			out = append(out, m)
		}
	}
	return out
}

// Allows reports whether method is offered to customers of this business.
func (s *Service) Allows(ctx context.Context, tenantID uuid.UUID, sf *storefront.Storefront, method string) bool {
	for _, m := range s.Offered(ctx, tenantID, sf) {
		if m == method {
			return true
		}
	}
	return false
}

// SendCode delivers a sign-in code by SMS or email through the business's
// effective configuration. In test fallback nothing is sent.
func (s *Service) SendCode(ctx context.Context, tenantID uuid.UUID, storeName, method, to, code string, minutes int) error {
	if s.fallback {
		return nil
	}
	if s.cfg == nil {
		return ErrNotOffered
	}
	switch method {
	case storefront.LoginMethodPhone:
		body := fmt.Sprintf("%s sign-in code: %s. It expires in %d minutes. Do not share it with anyone.", storeName, code, minutes)
		_, err := s.cfg.SMSService().Send(ctx, &tenantID, to, body)
		return err
	case storefront.LoginMethodEmail:
		msg := notify.ComposeEmail(ctx, s.q, &tenantID, "ACCOUNT_VERIFICATION_CODE",
			map[string]any{"code": code, "minutes": minutes},
			notify.EmailContent{
				Subject: fmt.Sprintf("Your %s sign-in code", storeName),
				Body: fmt.Sprintf(
					"Your sign-in code for %s is %s. It expires in %d minutes.\r\n\r\nIf you did not ask for it, you can ignore this email.\r\n",
					storeName, code, minutes,
				),
			}, to)
		return s.cfg.Notifications().Send(ctx, &tenantID, msg)
	}
	return ErrNotOffered
}
