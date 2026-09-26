package confighttp

import "net/http"

// The tenant handlers take the caller's tenant as a parameter so the routing
// layer supplies it from the verified JWT. These adapters adapt a
// http.HandlerFunc to the plain http.HandlerFunc chi expects, keeping the
// tenant id out of every request body and path.

// ListTenantServicesFunc adapts ListTenantServices.
func (h *Handler) ListTenantServicesFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.ListTenantServices(w, r, tenant) }
}

// GetTenantServiceFunc adapts GetTenantService.
func (h *Handler) GetTenantServiceFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.GetTenantService(w, r, tenant) }
}

// PutTenantServiceFunc adapts PutTenantService.
func (h *Handler) PutTenantServiceFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.PutTenantService(w, r, tenant) }
}

// DeleteTenantServiceFunc adapts DeleteTenantService.
func (h *Handler) DeleteTenantServiceFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.DeleteTenantService(w, r, tenant) }
}

// TestTenantServiceFunc adapts TestTenantService.
func (h *Handler) TestTenantServiceFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.TestTenantService(w, r, tenant) }
}

// TestTenantServiceActionFunc adapts TestTenantServiceAction.
func (h *Handler) TestTenantServiceActionFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.TestTenantServiceAction(w, r, tenant) }
}

// GetEffectiveFunc adapts GetEffective.
func (h *Handler) GetEffectiveFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.GetEffective(w, r, tenant) }
}

// ListPreferencesFunc adapts ListPreferences.
func (h *Handler) ListPreferencesFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.ListPreferences(w, r, tenant) }
}

// SetPreferenceFunc adapts SetPreference.
func (h *Handler) SetPreferenceFunc(tenant TenantIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h.SetPreference(w, r, tenant) }
}
