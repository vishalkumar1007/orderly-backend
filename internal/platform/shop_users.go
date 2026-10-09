package platform

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// mustShopTenant returns the caller's tenant id for shop-console user
// management. Super-admin routes take the tenant from the URL instead.
func mustShopTenant(r *http.Request) (uuid.UUID, bool) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return uuid.Nil, false
	}
	return *user.TenantID, true
}

// ShopListUsers lists every staff/admin user on the caller's tenant.
func (h *Handler) ShopListUsers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := mustShopTenant(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	rows, err := h.q.ListTenantUsers(r.Context(), pgutil.UUID(tenantID))
	if err != nil {
		if h.log != nil {
			h.log.Error("list shop users", "err", err, "tenant_id", tenantID)
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to list users")
		return
	}
	out := make([]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, userJSON(viewOfRow(u)))
	}
	response.JSON(w, http.StatusOK, map[string]any{"users": out})
}

// ShopCreateUser invites a new admin or staff member on the caller's tenant.
func (h *Handler) ShopCreateUser(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := mustShopTenant(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return
	}
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid json")
		return
	}
	h.createUserFor(w, r, req, tenantID)
}

// ShopUpdateUser edits a user that belongs to the caller's tenant.
func (h *Handler) ShopUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !h.rewriteShopUserRoute(w, r) {
		return
	}
	h.UpdateTenantUser(w, r)
}

// ShopResetUserAccess resets access for a user on the caller's tenant.
func (h *Handler) ShopResetUserAccess(w http.ResponseWriter, r *http.Request) {
	if !h.rewriteShopUserRoute(w, r) {
		return
	}
	h.ResetTenantUserAccess(w, r)
}

// ShopResendUserInvite regenerates the setup link for a user on the caller's tenant.
func (h *Handler) ShopResendUserInvite(w http.ResponseWriter, r *http.Request) {
	if !h.rewriteShopUserRoute(w, r) {
		return
	}
	h.ResendTenantUserInvite(w, r)
}

// ShopResetUserMFA turns off two-factor authentication for a locked-out user
// on the caller's tenant.
func (h *Handler) ShopResetUserMFA(w http.ResponseWriter, r *http.Request) {
	if !h.rewriteShopUserRoute(w, r) {
		return
	}
	h.ResetUserMFA(w, r)
}

// rewriteShopUserRoute sets chi URL params so the existing admin user handlers
// can reuse parseUserRoute (expects :id = tenant, :userId = user).
func (h *Handler) rewriteShopUserRoute(w http.ResponseWriter, r *http.Request) bool {
	tenantID, ok := mustShopTenant(r)
	if !ok {
		response.Error(w, http.StatusForbidden, "forbidden", "tenant required")
		return false
	}
	userID := strings.TrimSpace(chi.URLParam(r, "userId"))
	if _, err := uuid.Parse(userID); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid_request", "invalid user id")
		return false
	}
	chi.RouteContext(r.Context()).URLParams.Add("id", tenantID.String())
	return true
}
