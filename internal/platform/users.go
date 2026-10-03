package platform

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

const (
	maxUsersPerTenant = 50
)

var (
	errNoTenantID    = errors.New("tenant_id is required")
	errBadRole       = errors.New("role must be TENANT_ADMIN, MANAGER or STAFF")
	errBadUserStatus = errors.New("status must be ACTIVE or DISABLED")
	errUserLimit     = errors.New("tenant user limit reached")
	errLastAdmin     = errors.New("cannot remove the last active administrator")
)

type createUserRequest struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Role     string `json:"role"`
}

type updateUserRequest struct {
	Name   *string `json:"name"`
	Phone  *string `json:"phone"`
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// userView normalises the two row shapes sqlc returns for a user.
type userView struct {
	ID              pgtype.UUID
	TenantID        pgtype.UUID
	Name            string
	Email           string
	Phone           string
	Role            string
	Status          string
	MustSetPassword bool
	CreatedAt       pgtype.Timestamptz
	LastActivity    pgtype.Timestamptz
}

func viewOf(u sqlc.User) userView {
	return userView{
		ID: u.ID, TenantID: u.TenantID, Name: u.Name, Email: u.Email, Phone: u.Phone,
		Role: u.Role, Status: u.Status, MustSetPassword: u.MustSetPassword, CreatedAt: u.CreatedAt,
	}
}

func viewOfRow(u sqlc.ListTenantUsersRow) userView {
	v := viewOf(sqlc.User{
		ID: u.ID, TenantID: u.TenantID, Name: u.Name, Email: u.Email, Phone: u.Phone,
		Role: u.Role, Status: u.Status, MustSetPassword: u.MustSetPassword, CreatedAt: u.CreatedAt,
	})
	if t, ok := u.LastActivity.(time.Time); ok {
		v.LastActivity = pgtype.Timestamptz{Time: t, Valid: true}
	}
	return v
}

func userJSON(u userView) map[string]any {
	out := map[string]any{
		"id":                pgutil.UUIDString(u.ID),
		"name":              u.Name,
		"email":             u.Email,
		"phone":             u.Phone,
		"role":              u.Role,
		"status":            u.Status,
		"must_set_password": u.MustSetPassword,
		"created_at":        u.CreatedAt.Time.Format(time.RFC3339),
		"last_activity":     nil,
	}
	if u.TenantID.Valid {
		out["tenant_id"] = pgutil.UUIDString(u.TenantID)
	}
	if u.LastActivity.Valid {
		out["last_activity"] = u.LastActivity.Time.Format(time.RFC3339)
	}
	return out
}

// ListTenantUsers returns every user on a tenant, not just admins.
func (h *Handler) ListTenantUsers(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	rows, err := h.q.ListTenantUsers(r.Context(), pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list users")
		return
	}
	out := make([]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, userJSON(viewOfRow(u)))
	}
	response.JSON(w, http.StatusOK, map[string]any{"users": out})
}

// CreateTenantUser invites an additional admin or staff member. The tenant is
// taken from the request body.
func (h *Handler) CreateTenantUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	tenantUUID, err := uuid.Parse(strings.TrimSpace(req.TenantID))
	if err != nil {
		response.Error(w, http.StatusBadRequest, errNoTenantID.Error(), errNoTenantID.Error())
		return
	}
	h.createUserFor(w, r, req, tenantUUID)
}

// CreateTenantUserForTenant is the nested form, where the tenant comes from
// the URL rather than the body.
func (h *Handler) CreateTenantUserForTenant(w http.ResponseWriter, r *http.Request) {
	tenantUUID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return
	}
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	h.createUserFor(w, r, req, tenantUUID)
}

func (h *Handler) createUserFor(w http.ResponseWriter, r *http.Request, req createUserRequest, tenantUUID uuid.UUID) {
	name := strings.TrimSpace(req.Name)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if name == "" || !strings.Contains(email, "@") {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name and a valid email are required")
		return
	}
	role := strings.ToUpper(strings.TrimSpace(req.Role))
	if role == "" {
		role = identity.RoleTenantAdmin
	}
	if !identity.IsBusinessRole(role) {
		response.Error(w, http.StatusBadRequest, errBadRole.Error(), errBadRole.Error())
		return
	}

	ctx := r.Context()
	tuid := pgutil.UUID(tenantUUID)
	tenant, err := h.q.GetTenantByID(ctx, tuid)
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "tenant not found")
		return
	}
	if tenant.Status == "SUSPENDED" {
		response.Error(w, http.StatusBadRequest, "tenant_suspended", "cannot add users to a suspended tenant")
		return
	}

	total, err := h.q.CountTenantUsers(ctx, tuid)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to count users")
		return
	}
	if total >= maxUsersPerTenant {
		response.Error(w, http.StatusBadRequest, errUserLimit.Error(), errUserLimit.Error())
		return
	}

	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create invite")
		return
	}
	// Unusable until the invite is completed.
	placeholderHash, _ := bcrypt.GenerateFromPassword([]byte(uuid.NewString()), bcrypt.DefaultCost)

	user, err := h.q.CreateUser(ctx, sqlc.CreateUserParams{
		TenantID: tuid, Name: name, Email: email, Phone: strings.TrimSpace(req.Phone),
		PasswordHash: string(placeholderHash), Role: role, Status: "ACTIVE",
		MustSetPassword: true,
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			response.Error(w, http.StatusConflict, "email_taken", "a user with that email already exists")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create user")
		return
	}

	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, h.q, &tuid, &actor.ID, "Admin Created", "user", user.ID)

	setupURL := h.tenantFrontendURL(tenant.Slug) + "/setup-password?token=" + inviteToken
	emailSent, emailErr := h.deliverInvite(r.Context(), user.Email, tenant.Name, setupURL)

	response.JSON(w, http.StatusCreated, map[string]any{
		"user":        userJSON(viewOf(user)),
		"setup_url":   setupURL,
		"email_sent":  emailSent,
		"email_error": emailErr,
	})
}

// UpdateTenantUser edits a tenant user's profile, role, or status.
func (h *Handler) UpdateTenantUser(w http.ResponseWriter, r *http.Request) {
	userUUID, tenantUUID, ok := h.parseUserRoute(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	ctx := r.Context()
	current, err := h.q.GetUserByID(ctx, pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return
	}

	nextRole := current.Role
	if req.Role != nil {
		nextRole = strings.ToUpper(strings.TrimSpace(*req.Role))
		if !identity.IsBusinessRole(nextRole) {
			response.Error(w, http.StatusBadRequest, errBadRole.Error(), errBadRole.Error())
			return
		}
	}
	nextStatus := current.Status
	if req.Status != nil {
		nextStatus = strings.ToUpper(strings.TrimSpace(*req.Status))
		if nextStatus != "ACTIVE" && nextStatus != "DISABLED" {
			response.Error(w, http.StatusBadRequest, errBadUserStatus.Error(), errBadUserStatus.Error())
			return
		}
	}

	// Never let a tenant be left with nobody who can administer it.
	if wouldOrphanTenant(current.Role, current.Status, nextRole, nextStatus) {
		activeAdmins, err := h.q.CountTenantUsersByRole(ctx, sqlc.CountTenantUsersByRoleParams{
			TenantID: pgutil.UUID(tenantUUID), Role: identity.RoleTenantAdmin,
		})
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to verify admins")
			return
		}
		if orphanedByRemoval(activeAdmins) {
			response.Error(w, http.StatusConflict, "last_admin", errLastAdmin.Error())
			return
		}
	}

	if req.Name != nil || req.Phone != nil {
		if _, err := h.q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
			ID:    pgutil.UUID(userUUID),
			Name:  optionalText(req.Name),
			Phone: optionalText(req.Phone),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update user")
			return
		}
	}
	if req.Role != nil && nextRole != current.Role {
		if _, err := h.q.SetUserRole(ctx, sqlc.SetUserRoleParams{
			ID: pgutil.UUID(userUUID), Role: nextRole,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update role")
			return
		}
	}
	if req.Status != nil && nextStatus != current.Status {
		if _, err := h.q.SetUserStatus(ctx, sqlc.SetUserStatusParams{
			ID: pgutil.UUID(userUUID), Status: nextStatus,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update status")
			return
		}
		// Disabling must not leave a live session behind.
		if nextStatus == "DISABLED" {
			_ = h.q.DeleteRefreshTokensByUser(ctx, pgutil.UUID(userUUID))
		}
	}

	actor, _ := identity.UserFromContext(ctx)
	tid := pgutil.UUID(tenantUUID)
	uid := userUUID
	entity := pgutil.UUID(uid)
	statusChanged := req.Status != nil && nextStatus != current.Status
	roleChanged := req.Role != nil && nextRole != current.Role
	switch {
	case statusChanged && nextStatus == "DISABLED":
		_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Disabled", "user", entity)
	case statusChanged:
		_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Enabled", "user", entity)
	case roleChanged:
		_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Role Changed to "+nextRole, "user", entity)
	default:
		_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Updated", "user", entity)
	}

	updated, err := h.q.GetUserByID(ctx, pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reload user")
		return
	}
	response.JSON(w, http.StatusOK, userJSON(viewOf(updated)))
}

// ResetTenantUserAccess forces a user to set a new password and revokes
// every session they hold.
func (h *Handler) ResetTenantUserAccess(w http.ResponseWriter, r *http.Request) {
	userUUID, tenantUUID, ok := h.parseUserRoute(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	user, err := h.q.GetUserByID(ctx, pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create invite")
		return
	}
	// ResetUserAccess is a RETURNING query, so this is the post-reset state.
	reset, err := h.q.ResetUserAccess(ctx, sqlc.ResetUserAccessParams{
		ID:              pgutil.UUID(userUUID),
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reset access")
		return
	}
	if err := h.q.DeleteRefreshTokensByUser(ctx, pgutil.UUID(userUUID)); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to revoke sessions")
		return
	}

	actor, _ := identity.UserFromContext(ctx)
	tid := pgutil.UUID(tenantUUID)
	uid := userUUID
	entity := pgutil.UUID(uid)
	_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Access Reset", "user", entity)

	tenant, terr := h.q.GetTenantByID(ctx, tid)
	setupURL := ""
	emailSent, emailErr := false, ""
	if terr == nil {
		setupURL = h.tenantFrontendURL(tenant.Slug) + "/setup-password?token=" + inviteToken
		emailSent, emailErr = h.deliverInvite(r.Context(), user.Email, tenant.Name, setupURL)
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"user":        userJSON(viewOf(reset)),
		"setup_url":   setupURL,
		"email_sent":  emailSent,
		"email_error": emailErr,
	})
}

// ResendTenantUserInvite issues a fresh setup link without revoking sessions.
func (h *Handler) ResendTenantUserInvite(w http.ResponseWriter, r *http.Request) {
	userUUID, tenantUUID, ok := h.parseUserRoute(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	user, err := h.q.GetUserByID(ctx, pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create invite")
		return
	}
	updated, err := h.q.UpdateUserInviteToken(ctx, sqlc.UpdateUserInviteTokenParams{
		ID:              pgutil.UUID(userUUID),
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update invite")
		return
	}
	actor, _ := identity.UserFromContext(ctx)
	tid := pgutil.UUID(tenantUUID)
	uid := userUUID
	entity := pgutil.UUID(uid)
	_ = insertAudit(ctx, h.q, &tid, &actor.ID, "Admin Invite Resent", "user", entity)

	setupURL := ""
	emailSent, emailErr := false, ""
	if tenant, terr := h.q.GetTenantByID(ctx, tid); terr == nil {
		setupURL = h.tenantFrontendURL(tenant.Slug) + "/setup-password?token=" + inviteToken
		emailSent, emailErr = h.deliverInvite(r.Context(), user.Email, tenant.Name, setupURL)
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"user":        userJSON(viewOf(updated)),
		"setup_url":   setupURL,
		"email_sent":  emailSent,
		"email_error": emailErr,
	})
}

/* ---------- shared helpers ---------- */

// wouldOrphanTenant reports whether moving a user from their current role and
// status to the requested ones takes them out of the active-admin set. Sending
// the state a user is already in is a no-op, not an orphaned tenant, so this
// only fires on a real transition.
func wouldOrphanTenant(currentRole, currentStatus, nextRole, nextStatus string) bool {
	wasActiveAdmin := currentRole == identity.RoleTenantAdmin && currentStatus == "ACTIVE"
	becomesActiveAdmin := nextRole == identity.RoleTenantAdmin && nextStatus == "ACTIVE"
	return wasActiveAdmin && !becomesActiveAdmin
}

// orphanedByRemoval reports whether removing the target from the active admins
// would leave none behind. activeAdmins counts every active admin on the
// tenant, including the target when they are currently one, so the target is
// discounted before the comparison.
func orphanedByRemoval(activeAdmins int64) bool {
	return activeAdmins-1 < 1
}

// parseUserRoute validates :tenantId and :userId and returns them as UUIDs.
// parseUserRoute resolves the (user, tenant) pair a request addresses.
//
// Two shapes reach here. The nested form, /admin/tenants/{id}/users/{userId},
// names both and is checked for agreement. The flat form, /admin/users/{id},
// names only the user — it is how the platform-wide IAM screen addresses
// someone it found by searching across businesses — so the tenant is read off
// the user rather than taken from the caller.
func (h *Handler) parseUserRoute(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	if strings.TrimSpace(chi.URLParam(r, "userId")) == "" {
		return h.parseFlatUserRoute(w, r)
	}
	tenantUUID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid tenant id")
		return uuid.Nil, uuid.Nil, false
	}
	userUUID, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return uuid.Nil, uuid.Nil, false
	}
	// The user must actually belong to this tenant — never trust the pairing.
	user, err := h.q.GetUserByID(r.Context(), pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return uuid.Nil, uuid.Nil, false
	}
	if !user.TenantID.Valid || user.TenantID.Bytes != [16]byte(pgutil.UUID(tenantUUID).Bytes) {
		response.Error(w, http.StatusForbidden, "tenant_mismatch", "user does not belong to this tenant")
		return uuid.Nil, uuid.Nil, false
	}
	return userUUID, tenantUUID, true
}

// parseFlatUserRoute resolves /admin/users/{id}, where {id} is the user.
func (h *Handler) parseFlatUserRoute(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userUUID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return uuid.Nil, uuid.Nil, false
	}
	user, err := h.q.GetUserByID(r.Context(), pgutil.UUID(userUUID))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "user not found")
		return uuid.Nil, uuid.Nil, false
	}
	// The platform owner has no tenant, and none of the tenant user lifecycle
	// applies to them: their role cannot be changed and they cannot be
	// disabled from here, because doing so would lock the console.
	if !user.TenantID.Valid {
		response.Error(w, http.StatusForbidden, "forbidden",
			"the platform owner account cannot be changed from user management")
		return uuid.Nil, uuid.Nil, false
	}
	return userUUID, uuid.UUID(user.TenantID.Bytes), true
}

func optionalText(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: strings.TrimSpace(*v), Valid: true}
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate")
}
