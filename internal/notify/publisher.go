package notify

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Publish writes one notification row per recipient. Best-effort: a failure
// here never fails the request that triggered it — the same tolerance this
// codebase already gives audit logging (`_ = insertAudit(...)`) and invite
// email (reported via email_sent/email_error, never a hard failure). Errors
// are logged so a systemic problem (bad DB, bad data shape) is still visible.
//
// priority and eventCode are the bell's icon/sound hint and its link back to
// the catalog; Dispatch always supplies them, but a caller still invoking
// Publish directly (there is no such caller left in this codebase, but the
// function stays exported) may pass "" for either.
func Publish(
	ctx context.Context,
	q *sqlc.Queries,
	log *slog.Logger,
	tenantID *uuid.UUID,
	recipients []uuid.UUID,
	typ, title, body string,
	data map[string]any,
	priority string,
) {
	if len(recipients) == 0 {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte("{}")
	}
	var tid pgtype.UUID
	if tenantID != nil {
		tid = pgutil.UUID(*tenantID)
	}
	if priority == "" {
		priority = "NORMAL"
	}
	eventCode := pgtype.Text{String: typ, Valid: typ != ""}
	for _, userID := range recipients {
		if _, err := q.CreateNotification(ctx, sqlc.CreateNotificationParams{
			TenantID:  tid,
			UserID:    pgutil.UUID(userID),
			Type:      typ,
			Title:     title,
			Body:      body,
			Data:      payload,
			Priority:  priority,
			EventCode: eventCode,
		}); err != nil && log != nil {
			log.Error("notify: create notification failed", "type", typ, "user_id", userID, "error", err)
		}
	}
}

// Recipient is a resolved notification target: always a user id, with an
// email address when one is on file (platform and tenant users always have
// one; it travels with the id so the EMAIL channel needs no second lookup).
type Recipient struct {
	UserID uuid.UUID
	Email  string
}

// UserIDs extracts the id half of a recipient list, for the IN_APP channel.
func UserIDs(recipients []Recipient) []uuid.UUID {
	out := make([]uuid.UUID, len(recipients))
	for i, r := range recipients {
		out[i] = r.UserID
	}
	return out
}

// RecipientsByPermission resolves a tenant's active staff who hold the given
// permission — e.g. everyone who can work Selling, for a new-order event.
// This exact filter (ListTenantUsers + identity.Can) has no precedent
// elsewhere in the codebase; it is the one new piece of recipient logic.
func RecipientsByPermission(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, permission identity.Permission) ([]Recipient, error) {
	rows, err := q.ListTenantUsers(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, err
	}
	return filterByPermission(rows, permission), nil
}

// filterByPermission is the pure part of RecipientsByPermission, split out so
// the recipient logic can be tested without a database.
func filterByPermission(rows []sqlc.ListTenantUsersRow, permission identity.Permission) []Recipient {
	var out []Recipient
	for _, u := range rows {
		if u.Status != "ACTIVE" {
			continue
		}
		if !identity.Can(u.Role, permission) {
			continue
		}
		out = append(out, Recipient{UserID: uuid.UUID(u.ID.Bytes), Email: u.Email})
	}
	return out
}

// RecipientsPlatform resolves every active platform/console user — used for
// events with no single tenant to scope to, e.g. a new business onboarding.
func RecipientsPlatform(ctx context.Context, q *sqlc.Queries) ([]Recipient, error) {
	rows, err := q.ListActivePlatformUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Recipient, 0, len(rows))
	for _, u := range rows {
		out = append(out, Recipient{UserID: uuid.UUID(u.ID.Bytes), Email: u.Email})
	}
	return out, nil
}
