package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/*
Console access.

Who can reach the platform console, and what each of them may do once they are
in. This is deliberately not the same list as the people who work in a business:
a console identity has no tenant, and a business identity is refused here. The
two are separate authentication contexts and the product keeps them that way.

Nothing in this file can create or change a business user. That is the tenant's
own IAM, reached on the tenant's own host by someone who works there.
*/

type consoleUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
	Role  string `json:"role"`
}

type consoleUserPatch struct {
	Name   *string `json:"name"`
	Phone  *string `json:"phone"`
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// ListConsoleUsers returns everyone with console access.
func (h *Handler) ListConsoleUsers(w http.ResponseWriter, r *http.Request) {
	actor, _ := identity.UserFromContext(r.Context())
	rows, err := h.q.ListConsoleUsers(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load console access")
		return
	}

	users := make([]any, 0, len(rows))
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Role]++
		status := row.Status
		if row.MustSetPassword {
			status = "INVITED"
		}
		entry := map[string]any{
			"id":                pgutil.UUIDString(row.ID),
			"name":              row.Name,
			"email":             row.Email,
			"phone":             row.Phone,
			"role":              row.Role,
			"role_label":        identity.RoleLabel(row.Role),
			"status":            status,
			"must_set_password": row.MustSetPassword,
			"permissions":       identity.PermissionsFor(row.Role),
			"active_sessions":   row.ActiveSessions,
			"created_at":        row.CreatedAt.Time.Format(time.RFC3339),
			"last_activity":     nil,
			// The owner cannot be demoted or disabled, and nobody can change
			// their own access — both would leave the console unreachable.
			"is_self":  uuid.UUID(row.ID.Bytes) == actor.ID,
			"is_owner": row.Role == identity.RoleSuperAdmin,
		}
		if ts := timestampOrEmpty(row.LastActivity); ts != "" {
			entry["last_activity"] = ts
		}
		users = append(users, entry)
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"users":       users,
		"roles":       identity.PlatformRoles(),
		"permissions": identity.PlatformPermissions(),
		"summary": map[string]any{
			"owners":     counts[identity.RoleSuperAdmin],
			"admins":     counts[identity.RolePlatformAdmin],
			"support":    counts[identity.RoleSupport],
			"total":      len(users),
			"can_invite": true,
		},
	})
}

// CreateConsoleUser invites someone to the platform console.
func (h *Handler) CreateConsoleUser(w http.ResponseWriter, r *http.Request) {
	var req consoleUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Role = strings.ToUpper(strings.TrimSpace(req.Role))

	if req.Name == "" || req.Email == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "name and email are required")
		return
	}
	// Only the two delegable roles. The owner is the account that was created
	// at setup; there is never a second one.
	if !identity.IsAssignablePlatformRole(req.Role) {
		response.Error(w, http.StatusBadRequest, "invalid_request",
			"role must be PLATFORM_ADMIN or SUPPORT")
		return
	}

	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create the invitation")
		return
	}
	// Unusable until the invitation is completed: the hash is of a random value
	// nobody holds, so the account cannot be signed into before then.
	placeholder, _ := bcrypt.GenerateFromPassword([]byte(uuid.NewString()), bcrypt.DefaultCost)

	ctx := r.Context()
	user, err := h.q.CreateConsoleUser(ctx, sqlc.CreateConsoleUserParams{
		Name:            req.Name,
		Email:           req.Email,
		Phone:           strings.TrimSpace(req.Phone),
		PasswordHash:    string(placeholder),
		Role:            req.Role,
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			response.Error(w, http.StatusConflict, "duplicate", "that email address already has an account")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create the account")
		return
	}

	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, h.q, nil, &actor.ID,
		"Console Access Granted: "+identity.RoleLabel(req.Role), "user", user.ID)

	setupURL := h.consoleSetupURL(inviteToken)
	emailSent, emailErr := h.deliverConsoleInvite(ctx, user.Email, setupURL)

	response.JSON(w, http.StatusCreated, map[string]any{
		"user":        consoleUserJSON(user),
		"setup_url":   setupURL,
		"email_sent":  emailSent,
		"email_error": emailErr,
	})
}

// UpdateConsoleUser changes a console account's role or status.
func (h *Handler) UpdateConsoleUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}
	var req consoleUserPatch
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}

	ctx := r.Context()
	current, err := h.q.GetUserByID(ctx, pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if current.TenantID.Valid {
		response.Error(w, http.StatusForbidden, "forbidden",
			"that account belongs to a business and is managed from its own console")
		return
	}
	if current.Role == identity.RoleSuperAdmin {
		response.Error(w, http.StatusForbidden, "forbidden",
			"the owner account cannot be changed here")
		return
	}
	actor, _ := identity.UserFromContext(ctx)
	if actor.ID == id {
		response.Error(w, http.StatusForbidden, "forbidden", "you cannot change your own access")
		return
	}

	nextRole := current.Role
	if req.Role != nil {
		nextRole = strings.ToUpper(strings.TrimSpace(*req.Role))
		if !identity.IsAssignablePlatformRole(nextRole) {
			response.Error(w, http.StatusBadRequest, "invalid_request",
				"role must be PLATFORM_ADMIN or SUPPORT")
			return
		}
	}
	nextStatus := current.Status
	if req.Status != nil {
		nextStatus = strings.ToUpper(strings.TrimSpace(*req.Status))
		if nextStatus != "ACTIVE" && nextStatus != "DISABLED" {
			response.Error(w, http.StatusBadRequest, "invalid_request", "status must be ACTIVE or DISABLED")
			return
		}
	}

	if req.Name != nil || req.Phone != nil {
		if _, err := h.q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
			ID:    pgutil.UUID(id),
			Name:  optionalText(req.Name),
			Phone: optionalText(req.Phone),
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update the account")
			return
		}
	}
	if nextRole != current.Role {
		if _, err := h.q.SetUserRole(ctx, sqlc.SetUserRoleParams{
			ID: pgutil.UUID(id), Role: nextRole,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to change the role")
			return
		}
		_ = insertAudit(ctx, h.q, nil, &actor.ID,
			"Console Role Changed: "+identity.RoleLabel(nextRole), "user", pgutil.UUID(id))
	}
	if nextStatus != current.Status {
		if _, err := h.q.SetUserStatus(ctx, sqlc.SetUserStatusParams{
			ID: pgutil.UUID(id), Status: nextStatus,
		}); err != nil {
			response.Error(w, http.StatusInternalServerError, "internal_error", "failed to change the status")
			return
		}
		if nextStatus == "DISABLED" {
			// Disabling has to end their sessions, or the account keeps working
			// until its access token happens to expire.
			_ = h.q.DeleteRefreshTokensByUser(ctx, pgutil.UUID(id))
		}
		action := "Console Access Restored"
		if nextStatus == "DISABLED" {
			action = "Console Access Revoked"
		}
		_ = insertAudit(ctx, h.q, nil, &actor.ID, action, "user", pgutil.UUID(id))
	}

	updated, err := h.q.GetUserByID(ctx, pgutil.UUID(id))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to reload the account")
		return
	}
	response.JSON(w, http.StatusOK, consoleUserJSON(updated))
}

// ResendConsoleInvite issues a fresh setup link for a console account.
func (h *Handler) ResendConsoleInvite(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(r, "id")
	if err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}
	ctx := r.Context()
	current, err := h.q.GetUserByID(ctx, pgutil.UUID(id))
	if err != nil || current.TenantID.Valid {
		response.Error(w, http.StatusNotFound, "not_found", "account not found")
		return
	}

	inviteToken, err := randomToken(32)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to create the invitation")
		return
	}
	updated, err := h.q.UpdateUserInviteToken(ctx, sqlc.UpdateUserInviteTokenParams{
		ID:              pgutil.UUID(id),
		InviteTokenHash: pgtype.Text{String: auth.HashInviteToken(inviteToken), Valid: true},
	})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to update the invitation")
		return
	}
	// A fresh link invalidates every existing session: the point of reissuing
	// one is usually that the old access should stop working.
	_ = h.q.DeleteRefreshTokensByUser(ctx, pgutil.UUID(id))

	actor, _ := identity.UserFromContext(ctx)
	_ = insertAudit(ctx, h.q, nil, &actor.ID, "Console Invite Reissued", "user", pgutil.UUID(id))

	setupURL := h.consoleSetupURL(inviteToken)
	emailSent, emailErr := h.deliverConsoleInvite(ctx, updated.Email, setupURL)
	response.JSON(w, http.StatusOK, map[string]any{
		"user":        consoleUserJSON(updated),
		"setup_url":   setupURL,
		"email_sent":  emailSent,
		"email_error": emailErr,
	})
}

/* ---------- helpers ---------- */

func consoleUserJSON(u sqlc.User) map[string]any {
	status := u.Status
	if u.MustSetPassword {
		status = "INVITED"
	}
	return map[string]any{
		"id":                pgutil.UUIDString(u.ID),
		"name":              u.Name,
		"email":             u.Email,
		"phone":             u.Phone,
		"role":              u.Role,
		"role_label":        identity.RoleLabel(u.Role),
		"status":            status,
		"must_set_password": u.MustSetPassword,
		"permissions":       identity.PermissionsFor(u.Role),
		"created_at":        u.CreatedAt.Time.Format(time.RFC3339),
	}
}

// consoleSetupURL builds the password-setup address on the platform host.
//
// Deliberately not a tenant host: a console account has no business to belong
// to, and the tenant setup page resolves its tenant from the hostname, so the
// link would be rejected there.
func (h *Handler) consoleSetupURL(token string) string {
	return "http://localhost:" + frontendPort() + "/superadmin/setup-password?token=" + token
}

// deliverConsoleInvite sends the setup link, reporting rather than failing when
// email is not configured — the console shows the link either way, so a
// platform without SMTP can still bring someone on board.
func (h *Handler) deliverConsoleInvite(ctx context.Context, to, setupURL string) (bool, string) {
	if !h.configsEnabled() {
		return false, "email service unavailable"
	}
	msg := configsvc.MailMessage{
		To:      []string{to},
		Subject: "You have been given access to the Orderly platform console",
		Body: "You have been invited to the Orderly platform console.\r\n\r\n" +
			"Open this link to choose a password and sign in:\r\n" + setupURL +
			"\r\n\r\nIf you were not expecting this message, you can ignore it.\r\n",
	}
	if err := h.notifications.SendPlatform(ctx, msg); err != nil {
		return false, configsvc.SafeErrorMessage(err)
	}
	return true, ""
}
