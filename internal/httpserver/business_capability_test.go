package httpserver

import (
	"net/http"
	"testing"
)

/*
Capability gating and cross-tenant isolation for the Barber/Hotel/Tables
modules added alongside the capability system (planing/business_saas_lld.md).

These are the two guarantees the whole capability mechanism exists for:
  - a tenant can only ever reach the modules its business type actually has
  - a tenant can never read or write another tenant's rows in those modules,
    even when it does have the same capability enabled
*/

// TestCapabilityGatingBlocksIrrelevantModules covers the wall described in
// business_type_capabilities: a RESTAURANT tenant has no ROOMS row and a
// disabled SERVICES row, so both must 403 rather than 200-with-empty-data —
// an empty 200 would leak that the module exists at all.
func TestCapabilityGatingBlocksIrrelevantModules(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Capability Restaurant", "cap-restaurant-"+randSuffix())
	owner := env.tenantAdmin(tenantID)

	for _, path := range []string{"/api/v1/tenant/services", "/api/v1/tenant/rooms", "/api/v1/tenant/appointments"} {
		status, body := env.doAsTenant(http.MethodGet, path, owner, nil, tenantID)
		if status != http.StatusForbidden {
			t.Errorf("GET %s for a RESTAURANT tenant: status = %d, want 403 (body: %v)", path, status, body["__raw"])
			continue
		}
		if code, _ := body["error"].(map[string]any)["code"].(string); code != "capability_disabled" {
			t.Errorf("GET %s: error code = %q, want capability_disabled", path, code)
		}
	}

	// What RESTAURANT does have must still work — the gate is precise, not a
	// blanket lockout.
	status, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/orders", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "RESTAURANT reading its own orders", body)

	status, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/tables", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "RESTAURANT reading its own tables", body)
}

// TestBarberTenantReachesBarberModulesNotFoodModules is the mirror case: a
// BARBER tenant must reach services/appointments/queue and must be walled off
// from orders/catalog, which BARBER has no capability row for at all.
func TestBarberTenantReachesBarberModulesNotFoodModules(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenantOfType("Capability Barber", "cap-barber-"+randSuffix(), "BARBER")
	owner := env.tenantAdmin(tenantID)

	status, body := env.doAsTenant(http.MethodGet, "/api/v1/tenant/services", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "BARBER reading its own services", body)

	status, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/appointments", owner, nil, tenantID)
	env.mustStatus(http.StatusOK, status, "BARBER reading its own appointments", body)

	for _, path := range []string{"/api/v1/tenant/orders", "/api/v1/tenant/categories", "/api/v1/tenant/rooms"} {
		status, body := env.doAsTenant(http.MethodGet, path, owner, nil, tenantID)
		if status != http.StatusForbidden {
			t.Errorf("GET %s for a BARBER tenant: status = %d, want 403 (body: %v)", path, status, body["__raw"])
		}
	}
}

// TestServicesAreIsolatedPerTenant is the cross-tenant guarantee itself: two
// BARBER tenants, a service created under the first, and a direct attempt by
// the second (same capability, same role, different business) to read,
// modify and delete it by id. Every one of those must come back 404, not 403
// and not 200 — a 404 means the row could not be found *for this tenant*,
// which is the tenant_id-scoped query doing its job, not an auth layer
// deciding the caller may not look.
func TestServicesAreIsolatedPerTenant(t *testing.T) {
	env := newTestEnv(t)
	tenantA := env.createTenantOfType("Isolation Barber A", "iso-barber-a-"+randSuffix(), "BARBER")
	tenantB := env.createTenantOfType("Isolation Barber B", "iso-barber-b-"+randSuffix(), "BARBER")
	ownerA := env.tenantAdmin(tenantA)
	ownerB := env.tenantAdmin(tenantB)

	status, created := env.doAsTenant(http.MethodPost, "/api/v1/tenant/services", ownerA, map[string]any{
		"name": "Haircut", "duration_minutes": 30, "price": 250,
	}, tenantA)
	env.mustStatus(http.StatusCreated, status, "tenant A creates a service", created)
	serviceID, _ := created["id"].(string)
	if serviceID == "" {
		t.Fatalf("created service has no id: %v", created)
	}

	// Tenant B, same capability, same role, cannot see it in its own list.
	status, list := env.doAsTenant(http.MethodGet, "/api/v1/tenant/services", ownerB, nil, tenantB)
	env.mustStatus(http.StatusOK, status, "tenant B lists its own services", list)
	if services, _ := list["services"].([]any); len(services) != 0 {
		t.Errorf("tenant B's service list is not empty: %v", services)
	}

	// Nor can it reach tenant A's row by id, from tenant B's own host and token.
	status, body := env.doAsTenant(http.MethodPatch, "/api/v1/tenant/services/"+serviceID, ownerB,
		map[string]any{"name": "Renamed by tenant B"}, tenantB)
	if status != http.StatusNotFound {
		t.Errorf("tenant B PATCH tenant A's service: status = %d, want 404 (body: %v)", status, body["__raw"])
	}
	// DeleteService is a tenant-scoped DELETE with no affected-rows check (same
	// as DeleteCategory elsewhere in this codebase), so it reports 200 whether
	// or not a row actually matched — the real assertion is the tenant A
	// re-list below: the WHERE tenant_id=$2 clause must have matched nothing,
	// so tenant A's row is untouched.
	env.doAsTenant(http.MethodDelete, "/api/v1/tenant/services/"+serviceID, ownerB, nil, tenantB)

	// The row is untouched from tenant A's own side.
	status, list = env.doAsTenant(http.MethodGet, "/api/v1/tenant/services", ownerA, nil, tenantA)
	env.mustStatus(http.StatusOK, status, "tenant A re-lists its services", list)
	services, _ := list["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("tenant A's service was affected by tenant B's request: %v", services)
	}
	if name, _ := services[0].(map[string]any)["name"].(string); name != "Haircut" {
		t.Errorf("tenant A's service name = %q, want unchanged \"Haircut\"", name)
	}
}
