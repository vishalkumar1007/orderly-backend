package platform

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// shopStaffMember loads the {userId} route user and checks they belong to the
// caller's shop. A miss is a 404 either way, so ids from other shops leak nothing.
func (h *Handler) shopStaffMember(w http.ResponseWriter, r *http.Request) (sqlc.User, pgtype.UUID, bool) {
	tenantID, ok := mustShopTenant(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return sqlc.User{}, pgtype.UUID{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return sqlc.User{}, pgtype.UUID{}, false
	}
	tid := pgutil.UUID(tenantID)
	u, err := h.q.GetUserByID(r.Context(), pgutil.UUID(id))
	if err != nil || u.TenantID != tid {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return sqlc.User{}, pgtype.UUID{}, false
	}
	return u, tid, true
}

// ShopUserLogins lists a staff member's recent sign-ins with device and IP.
func (h *Handler) ShopUserLogins(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := h.shopStaffMember(w, r)
	if !ok {
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	rows, err := h.q.ListUserLogins(r.Context(), sqlc.ListUserLoginsParams{
		TenantID: tid, UserID: u.ID, RowLimit: int32(limit),
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load sign-ins")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var meta struct {
			IP        string `json:"ip"`
			UserAgent string `json:"user_agent"`
		}
		_ = json.Unmarshal(row.Metadata, &meta)
		out = append(out, map[string]any{
			"action":     row.Action,
			"result":     row.Result,
			"ip":         meta.IP,
			"user_agent": meta.UserAgent,
			"at":         row.CreatedAt.Time.Format(time.RFC3339),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"logins": out})
}

// ShopUserSessions lists the devices a staff member is signed in on.
func (h *Handler) ShopUserSessions(w http.ResponseWriter, r *http.Request) {
	u, _, ok := h.shopStaffMember(w, r)
	if !ok {
		return
	}
	rows, err := h.q.ListUserSessions(r.Context(), u.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load sessions")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":          pgutil.UUIDString(row.ID),
			"ip":          row.Ip,
			"user_agent":  row.UserAgent,
			"last_active": row.CreatedAt.Time.Format(time.RFC3339),
			"expires_at":  row.ExpiresAt.Time.Format(time.RFC3339),
		})
	}
	response.JSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// ShopSignOutUser ends every session of a staff member. Their current access
// token still works until it expires (minutes), then refresh fails.
func (h *Handler) ShopSignOutUser(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := h.shopStaffMember(w, r)
	if !ok {
		return
	}
	actor, _ := identity.UserFromContext(r.Context())
	// A manager cannot sign out the owner.
	if identity.RoleRank(u.Role) < identity.RoleRank(actor.Role) {
		response.Error(w, http.StatusForbidden, "forbidden", "you cannot sign out this user")
		return
	}
	if err := h.q.DeleteRefreshTokensByUser(r.Context(), u.ID); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not sign out")
		return
	}
	_ = insertAudit(r.Context(), h.q, &tid, &actor.ID, "user.signed_out_everywhere", "user", u.ID)
	response.JSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}

// ShopUserStats is the per-person summary on the staff profile. Each figure is
// present only when the shop runs the matching module.
func (h *Handler) ShopUserStats(w http.ResponseWriter, r *http.Request) {
	u, tid, ok := h.shopStaffMember(w, r)
	if !ok {
		return
	}
	now := time.Now()
	from := now.AddDate(0, 0, -30)
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 366 {
		from = now.AddDate(0, 0, -d)
	}
	fromTS := pgtype.Timestamptz{Time: from, Valid: true}
	toTS := pgtype.Timestamptz{Time: now, Valid: true}
	st, err := h.q.StaffStats(r.Context(), sqlc.StaffStatsParams{TenantID: tid, UserID: u.ID, FromAt: fromTS, ToAt: toTS})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not load stats")
		return
	}
	info, _ := tenantctx.FromContext(r.Context())
	out := map[string]any{
		"from":          from.Format(time.RFC3339),
		"to":            now.Format(time.RFC3339),
		"last_login_at": nil,
	}
	if u.LastLoginAt.Valid {
		out["last_login_at"] = u.LastLoginAt.Time.Format(time.RFC3339)
	}
	if info.HasCapability(tenantctx.CapOrders) {
		out["orders_handled"] = st.OrdersHandled
	}
	if info.HasCapability(tenantctx.CapAppointments) {
		out["appointments_completed"] = st.AppointmentsCompleted
	}
	if info.HasEntitlement(entitlement.FeatureAttendance) {
		secs, err := h.q.WorkedSeconds(r.Context(), sqlc.WorkedSecondsParams{TenantID: tid, UserID: u.ID, FromAt: fromTS, ToAt: toTS})
		if err == nil {
			out["hours_worked"] = secs / 3600
		}
	}
	response.JSON(w, http.StatusOK, out)
}
