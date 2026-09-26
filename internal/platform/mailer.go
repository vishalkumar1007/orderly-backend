package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/internal/configsvc"
)

// deliverInvite sends a tenant setup link.
//
// It resolves SMTP through the configuration service rather than reading a
// stored blob, so a platform that has moved its mail settings keeps working and
// a tenant-level configuration is honoured if one is ever selected for mail.
//
// A nil tenant pointer means platform scope, which is how invitations are sent
// today: they are platform mail about a tenant, not mail on that tenant's
// behalf.
func (h *Handler) deliverInvite(ctx context.Context, to, tenantName, setupURL string) (bool, string) {
	if !h.configsEnabled() {
		// Only reachable if the stack was never attached, e.g. in a unit test.
		return false, "email service unavailable"
	}

	msg := configsvc.MailMessage{
		To:      []string{to},
		Subject: "Set up " + tenantName + " on Orderly",
		Body:    inviteBody(tenantName, setupURL),
	}
	if err := h.notifications.SendPlatform(ctx, msg); err != nil {
		// The reason is safe to return: the resolver's UnavailableError explains
		// which level is unusable and why, and provider errors are redacted
		// before they reach here.
		return false, configsvc.SafeErrorMessage(err)
	}
	return true, ""
}

// sendTenantMail sends mail on behalf of a tenant, honouring whichever
// configuration resolves for it. Callers get the same "sent, or here's why not"
// contract as deliverInvite so email never becomes a hard dependency of a
// business operation.
func (h *Handler) sendTenantMail(ctx context.Context, tenantID *string, msg configsvc.MailMessage) (bool, string) {
	if !h.configsEnabled() {
		return false, "email service unavailable"
	}
	var tenantPtr = tenantIDPtr(tenantID)
	if err := h.notifications.Send(ctx, tenantPtr, msg); err != nil {
		return false, configsvc.SafeErrorMessage(err)
	}
	return true, ""
}

// tenantIDPtr converts an optional tenant id string into the pointer the
// resolver expects. An unparseable id resolves to platform scope, which the
// resolver will refuse unless the platform itself is configured.
func tenantIDPtr(s *string) *uuid.UUID {
	if s == nil {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*s))
	if err != nil {
		return nil
	}
	return &id
}

// inviteBody renders the tenant setup email. Kept here so the copy can evolve
// without touching the provider layer.
func inviteBody(tenantName, setupURL string) string {
	return fmt.Sprintf(
		"You have been invited to set up %s on Orderly.\r\n\r\nOpen this link to choose a password and sign in:\r\n%s\r\n\r\nIf you were not expecting this message, you can ignore it.\r\n",
		tenantName, setupURL,
	)
}
