package platform

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
IAM and the audit log, from inside a business.

Staff management answers "who works here". IAM answers "what can they reach" —
the same people seen through a different question, which is why this returns the
role catalogue and the permission matrix alongside the users rather than making
the console hard-code either. The catalogue comes from pkg/identity, the one
place that also decides what the API enforces, so the screen cannot promise
access the middleware refuses.
*/

// ShopIAM returns everyone in the business with what they can reach.
func (h *Handler) ShopIAM(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok || actor.TenantID == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}
	tenantID := *actor.TenantID
	ctx := r.Context()

	rows, err := h.q.ListTenantUsers(ctx, pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load users")
		return
	}

	// Sessions are counted in one query rather than per user: a shop with
	// twenty staff should not cost twenty round trips to draw one table.
	sessionRows, err := h.q.CountActiveSessionsByTenant(ctx, pgutil.UUID(tenantID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load sessions")
		return
	}
	sessions := make(map[uuid.UUID]int64, len(sessionRows))
	for _, row := range sessionRows {
		sessions[uuid.UUID(row.UserID.Bytes)] = row.ActiveSessions
	}

	users := make([]any, 0, len(rows))
	counts := map[string]int{}
	for _, row := range rows {
		view := viewOfRow(row)
		id := uuid.UUID(view.ID.Bytes)
		status := view.Status
		if view.MustSetPassword {
			status = "INVITED"
		}
		counts[view.Role]++

		entry := map[string]any{
			"id":                pgutil.UUIDString(view.ID),
			"name":              view.Name,
			"email":             view.Email,
			"phone":             view.Phone,
			"role":              view.Role,
			"role_label":        identity.RoleLabel(view.Role),
			"status":            status,
			"must_set_password": view.MustSetPassword,
			"permissions":       identity.PermissionsFor(view.Role),
			"active_sessions":   sessions[id],
			"created_at":        view.CreatedAt.Time.Format(time.RFC3339),
			"last_activity":     nil,
			// The caller cannot change their own role or disable themselves;
			// the console greys those actions out rather than letting someone
			// lock themselves out of the shop they run.
			"is_self": id == actor.ID,
		}
		if view.LastActivity.Valid {
			entry["last_activity"] = view.LastActivity.Time.Format(time.RFC3339)
		}
		users = append(users, entry)
	}

	// Owners first, then managers, then staff: the table reads as a hierarchy.
	sort.SliceStable(users, func(i, j int) bool {
		a := users[i].(map[string]any)["role"].(string)
		b := users[j].(map[string]any)["role"].(string)
		return identity.RoleRank(a) < identity.RoleRank(b)
	})

	response.JSON(w, http.StatusOK, map[string]any{
		"users":       users,
		"roles":       identity.BusinessRoles(),
		"permissions": identity.Permissions(),
		"summary": map[string]any{
			"owners":   counts[identity.RoleTenantAdmin],
			"managers": counts[identity.RoleManager],
			"staff":    counts[identity.RoleStaff],
			"total":    len(users),
		},
	})
}

// ShopAuditLogs returns this business's own audit trail.
//
// It reuses the platform query with the tenant pinned from the token, never
// from a parameter — a business can only ever read its own history.
func (h *Handler) ShopAuditLogs(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok || actor.TenantID == nil {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
		return
	}

	limit := int32(100)
	resultFilter := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("result")))

	out := []any{}
	if resultFilter != "" {
		rows, err := h.q.ListAuditLogsByTenantAndResult(r.Context(), sqlc.ListAuditLogsByTenantAndResultParams{
			TenantID: pgutil.UUID(*actor.TenantID), Result: resultFilter, RowLimit: limit,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load the audit log")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOfTenantResult(a)))
		}
	} else {
		rows, err := h.q.ListAuditLogsEnrichedByTenant(r.Context(), sqlc.ListAuditLogsEnrichedByTenantParams{
			TenantID: pgutil.UUID(*actor.TenantID), RowLimit: limit,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load the audit log")
			return
		}
		for _, a := range rows {
			out = append(out, auditLogJSON(rowOfTenant(a)))
		}
	}

	response.JSON(w, http.StatusOK, map[string]any{"audit_logs": out})
}
