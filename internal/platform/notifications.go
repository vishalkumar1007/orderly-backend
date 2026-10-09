package platform

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// notificationFeedLimit is the recent-feed window. There is no pagination
// here — the bell shows "what happened recently," not a full archive.
const notificationFeedLimit = 50

func notificationJSON(n sqlc.Notification) map[string]any {
	var data map[string]any
	if len(n.Data) > 0 {
		_ = json.Unmarshal(n.Data, &data)
	}
	out := map[string]any{
		"id":         pgutil.UUIDString(n.ID),
		"type":       n.Type,
		"title":      n.Title,
		"body":       n.Body,
		"data":       data,
		"read":       n.ReadAt.Valid,
		"priority":   n.Priority,
		"created_at": n.CreatedAt.Time.Format(time.RFC3339),
	}
	if n.EventCode.Valid {
		out["event_code"] = n.EventCode.String
	}
	return out
}

// ListNotifications returns the signed-in user's own recent notifications —
// shop staff and platform console accounts both land here; user_id alone
// scopes the result, see notifications.sql.
//
// With ?before_id=&before_created_at= (both required together) it instead
// returns the next older page, for the bell's "view all" history — the
// unread_count in that response is still the live total, not "unread in this
// page", so a caller paging back through history never has to re-derive it.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	var rows []sqlc.Notification
	var err error
	beforeID := r.URL.Query().Get("before_id")
	beforeCreatedAt := r.URL.Query().Get("before_created_at")
	if beforeID != "" && beforeCreatedAt != "" {
		id, perr := uuid.Parse(beforeID)
		if perr != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid before_id")
			return
		}
		ts, perr := time.Parse(time.RFC3339, beforeCreatedAt)
		if perr != nil {
			response.Error(w, http.StatusBadRequest, "invalid_request", "invalid before_created_at")
			return
		}
		rows, err = h.q.ListNotificationsForUserBefore(r.Context(), sqlc.ListNotificationsForUserBeforeParams{
			UserID:          pgutil.UUID(actor.ID),
			BeforeCreatedAt: pgtype.Timestamptz{Time: ts, Valid: true},
			BeforeID:        pgutil.UUID(id),
			Limit:           notificationFeedLimit,
		})
	} else {
		rows, err = h.q.ListNotificationsForUser(r.Context(), sqlc.ListNotificationsForUserParams{
			UserID: pgutil.UUID(actor.ID),
			Limit:  notificationFeedLimit,
		})
	}
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load notifications")
		return
	}
	unread, err := h.q.CountUnreadForUser(r.Context(), pgutil.UUID(actor.ID))
	if err != nil {
		unread = 0
	}
	out := make([]any, 0, len(rows))
	for _, n := range rows {
		out = append(out, notificationJSON(n))
	}
	response.JSON(w, http.StatusOK, map[string]any{"notifications": out, "unread_count": unread})
}

// MarkNotificationRead marks one of the signed-in user's own notifications
// read. The user_id guard in the query makes marking someone else's a no-op
// rather than a leak.
func (h *Handler) MarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid notification id")
		return
	}
	if err := h.q.MarkNotificationRead(r.Context(), sqlc.MarkNotificationReadParams{
		ID:     pgutil.UUID(id),
		UserID: pgutil.UUID(actor.ID),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update notification")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// MarkAllNotificationsRead clears the signed-in user's unread count.
func (h *Handler) MarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in required")
		return
	}
	if err := h.q.MarkAllNotificationsRead(r.Context(), pgutil.UUID(actor.ID)); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update notifications")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
