package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Deps is what Dispatch needs from the rest of the backend. A struct rather
// than five parameters, since every call site builds the same one.
type Deps struct {
	Q   *sqlc.Queries
	Log *slog.Logger
}

// Contact is the customer-facing address a CUSTOMER-policy rule sends to.
// Both fields are optional: an order always has an email (storefront
// checkout captures one) and a phone; an appointment or hotel reservation
// only ever has a phone (the customers table carries no email column), so
// Email stays "" there and any CUSTOMER/EMAIL rule is simply skipped.
type Contact struct {
	Email string
	Phone string
}

// Dispatch resolves the effective rules for an event (platform default,
// overridden per-rule by the tenant), and for each enabled one: writes an
// IN_APP notification directly, or enqueues an EMAIL/SMS delivery for the
// worker to send. It is the single entry point every business event goes
// through — orders, payments, appointments, hotel, staff, security and
// platform code never touch notification_rules or the channel plumbing
// directly.
//
// Best-effort and silent on failure, same contract as the Publish it
// replaces: a notification problem must never surface as an order, payment
// or booking failure. tenantID is nil for a platform-scoped event (e.g.
// BUSINESS_ONBOARDED).
func Dispatch(ctx context.Context, d Deps, tenantID *uuid.UUID, eventCode, title, body string, data map[string]any, contact Contact) {
	defer func() {
		if r := recover(); r != nil && d.Log != nil {
			d.Log.Error("notify: dispatch panicked", "event", eventCode, "recover", r)
		}
	}()

	var tid pgtype.UUID
	if tenantID != nil {
		tid = pgutil.UUID(*tenantID)
	}
	rules, err := d.Q.ListEffectiveRulesForEvent(ctx, sqlc.ListEffectiveRulesForEventParams{
		EventCode: eventCode,
		TenantID:  tid,
	})
	if err != nil {
		if d.Log != nil {
			d.Log.Error("notify: load rules failed", "event", eventCode, "error", err)
		}
		return
	}

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		switch rule.Channel {
		case ChannelInApp:
			dispatchInApp(ctx, d, tenantID, rule, eventCode, title, body, data)
		case ChannelEmail:
			dispatchOutbound(ctx, d, tenantID, rule, eventCode, ChannelEmail, title, body, data, contact)
		case ChannelSMS:
			dispatchOutbound(ctx, d, tenantID, rule, eventCode, ChannelSMS, title, body, data, contact)
		}
	}
}

func dispatchInApp(ctx context.Context, d Deps, tenantID *uuid.UUID, rule sqlc.NotificationRule, eventCode, title, body string, data map[string]any) {
	recipients := resolveStaffRecipients(ctx, d, tenantID, rule.RecipientPolicy)
	if len(recipients) == 0 {
		return
	}
	Publish(ctx, d.Q, d.Log, tenantID, UserIDs(recipients), eventCode, title, body, data, rule.Priority)
}

// dispatchOutbound enqueues an EMAIL or SMS delivery. It only ever sends to a
// CUSTOMER today (the seeded rules never pair EMAIL/SMS with a staff
// recipient_policy), but a tenant override could create one later, so
// ROLE:/PLATFORM policies still resolve to the matching staff/platform
// addresses rather than being silently dropped.
func dispatchOutbound(ctx context.Context, d Deps, tenantID *uuid.UUID, rule sqlc.NotificationRule, eventCode, channel, title, body string, data map[string]any, contact Contact) {
	var addresses []string
	switch {
	case rule.RecipientPolicy == PolicyCustomer:
		addr := contact.Email
		if channel == ChannelSMS {
			addr = contact.Phone
		}
		if strings.TrimSpace(addr) != "" {
			addresses = []string{addr}
		}
	case channel == ChannelEmail:
		for _, r := range resolveStaffRecipients(ctx, d, tenantID, rule.RecipientPolicy) {
			if r.Email != "" {
				addresses = append(addresses, r.Email)
			}
		}
	// SMS has no staff phone lookup in Phase 1 — no seeded rule needs it, and
	// staff members are not guaranteed to have a phone on file.
	default:
		return
	}
	if len(addresses) == 0 {
		return
	}

	var tid pgtype.UUID
	if tenantID != nil {
		tid = pgutil.UUID(*tenantID)
	}
	for _, addr := range addresses {
		key := idempotencyKey(eventCode, tenantID, channel, addr, data)
		_, err := d.Q.EnqueueNotificationDelivery(ctx, sqlc.EnqueueNotificationDeliveryParams{
			TenantID:       tid,
			EventCode:      eventCode,
			Channel:        channel,
			Recipient:      addr,
			IdempotencyKey: key,
		})
		if err != nil && err != pgx.ErrNoRows && d.Log != nil {
			// pgx.ErrNoRows here means ON CONFLICT DO NOTHING matched an
			// existing row — a duplicate event, not a failure.
			d.Log.Error("notify: enqueue delivery failed", "event", eventCode, "channel", channel, "error", err)
		}
	}
}

// resolveStaffRecipients maps a rule's recipient_policy onto concrete users.
// Unknown policies resolve to nobody rather than erroring, so a malformed
// tenant override cannot take down an entire event's delivery.
func resolveStaffRecipients(ctx context.Context, d Deps, tenantID *uuid.UUID, policy string) []Recipient {
	switch {
	case policy == PolicyPlatform:
		recipients, err := RecipientsPlatform(ctx, d.Q)
		if err != nil {
			if d.Log != nil {
				d.Log.Error("notify: resolve platform recipients failed", "error", err)
			}
			return nil
		}
		return recipients
	case strings.HasPrefix(policy, rolePolicyPrefix):
		if tenantID == nil {
			return nil
		}
		perm := identity.Permission(strings.TrimPrefix(policy, rolePolicyPrefix))
		recipients, err := RecipientsByPermission(ctx, d.Q, *tenantID, perm)
		if err != nil {
			if d.Log != nil {
				d.Log.Error("notify: resolve role recipients failed", "policy", policy, "error", err)
			}
			return nil
		}
		return recipients
	default:
		return nil
	}
}

// idempotencyKey makes a retried or duplicate-published event collapse onto
// the same delivery row instead of sending twice. entity_id (order id,
// appointment id, etc.) is read from data when present so the SAME event for
// a DIFFERENT entity still gets its own key.
func idempotencyKey(eventCode string, tenantID *uuid.UUID, channel, recipient string, data map[string]any) string {
	h := sha256.New()
	h.Write([]byte(eventCode))
	h.Write([]byte{0})
	if tenantID != nil {
		h.Write([]byte(tenantID.String()))
	}
	h.Write([]byte{0})
	h.Write([]byte(channel))
	h.Write([]byte{0})
	h.Write([]byte(recipient))
	h.Write([]byte{0})
	h.Write([]byte(entityIDOf(data)))
	return hex.EncodeToString(h.Sum(nil))
}

// entityIDOf pulls whichever "*_id" field identifies the thing this event is
// about, so the hash is stable across retries of the same event but distinct
// across different entities. Any of these keys being present is sufficient;
// absence (a platform event with no single entity) just means the key is
// per (event, tenant, channel, recipient) instead, which is correct for
// those events since they are not expected to repeat for the same recipient
// within the same dispatch.
func entityIDOf(data map[string]any) string {
	for _, key := range []string{"order_id", "appointment_id", "reservation_id", "tenant_id", "user_id"} {
		if v, ok := data[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
