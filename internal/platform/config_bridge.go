package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// wireServiceConfig attaches the provider-configuration stack to the handler.
// It is called once during construction so every Super Admin and tenant request
// shares one store, one resolver and one secretbox.
//
// The handler keeps the configsvc service so existing call sites (tenant
// invitations, for example) can send mail through the resolver instead of the
// old inline SMTP path.
func (h *Handler) AttachConfigService(svc *configsvc.Service, box *secretbox.Box) {
	h.configs = svc
	h.secretBox = box
	h.notifications = svc.Notifications()
}

// AttachLogger lets the handler report audit failures.
func (h *Handler) AttachLogger(log *slog.Logger) { h.log = log }

// configsEnabled reports whether the configuration stack has been attached.
func (h *Handler) configsEnabled() bool { return h.configs != nil }

// AuditConfiguration records a configuration change on the platform audit log.
//
// It satisfies confighttp.AuditSink. The action names the service and provider
// only; no configuration value and no credential ever reaches this function,
// because the public view the HTTP layer holds contains none.
func (h *Handler) AuditConfiguration(
	ctx context.Context,
	tenantID *uuid.UUID,
	action, service, provider string,
) {
	if h == nil || h.q == nil {
		return
	}
	actor, _ := identity.UserFromContext(ctx)

	var tid pgtype.UUID
	if tenantID != nil {
		tid = pgutil.UUID(*tenantID)
	}

	entityType := "platform_configuration"
	if tenantID != nil {
		entityType = "tenant_configuration"
	}

	// A descriptive action string keeps the log readable without needing a
	// separate metadata column, and carries nothing sensitive.
	full := strings.TrimSpace(action)
	if service != "" {
		full = strings.TrimSpace(fmt.Sprintf("%s (%s)", full, service))
	}
	if provider != "" {
		full = strings.TrimSpace(fmt.Sprintf("%s [%s]", full, provider))
	}
	if full == "" {
		return
	}

	var actorID pgtype.UUID
	if actor.ID != uuid.Nil {
		actorID = pgutil.UUID(actor.ID)
	}
	if _, err := h.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		TenantID:   tid,
		UserID:     actorID,
		Action:     full,
		EntityType: entityType,
		Result:     auditSuccess,
		Metadata:   []byte("{}"),
	}); err != nil && h.log != nil {
		// Silently dropping an audit row hides a real problem: the change
		// happened but the trail did not.
		h.log.Error("could not write configuration audit row",
			"action", full, "service", service, "error", err.Error())
	}
}
