package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

/*
The business type is a template; tenant_capabilities is the truth. The console
shapes its whole rail from what /auth/me reports, so these pin down the two
ends of that contract for every supported type: what a fresh tenant of each
type starts with, and that onboarding honours an owner's choice only where the
matrix says it may.
*/

// Every type offered at onboarding (db/migrations: business_type_catalog,
// capabilities_catalog, business_types_retail_services).
var supportedBusinessTypes = []string{
	"FOOD_SHOP", "GROCERY", "FASHION", "ELECTRONICS", "CAFE", "RESTAURANT",
	"BARBER", "SALON", "AUTOMOTIVE", "HOTEL", "GENERAL",
}

func capabilitySet(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	host, ok := body["host_tenant"].(map[string]any)
	if !ok {
		t.Fatalf("no host_tenant in /auth/me: %v", body)
	}
	raw, ok := host["capabilities"].([]any)
	if !ok {
		t.Fatalf("host_tenant has no capabilities list: %v", host)
	}
	out := make(map[string]bool, len(raw))
	for _, c := range raw {
		out[c.(string)] = true
	}
	return out
}

// TestEveryBusinessTypeReportsItsDefaultCapabilities: for each type, a new
// tenant's /auth/me capability list is exactly the matrix's default_enabled
// rows. A type whose defaults drift from the matrix, or a type missing from
// it, fails here by name.
func TestEveryBusinessTypeReportsItsDefaultCapabilities(t *testing.T) {
	env := newTestEnv(t)
	for _, kind := range supportedBusinessTypes {
		t.Run(kind, func(t *testing.T) {
			rows, err := env.server.pool.Query(context.Background(),
				`SELECT capability_code FROM business_type_capabilities
				 WHERE business_type_code = $1 AND default_enabled`, kind)
			if err != nil {
				t.Fatalf("matrix query: %v", err)
			}
			want := map[string]bool{}
			for rows.Next() {
				var code string
				if err := rows.Scan(&code); err != nil {
					t.Fatalf("scan: %v", err)
				}
				want[code] = true
			}
			rows.Close()
			if len(want) == 0 {
				t.Fatalf("%s has no business_type_capabilities rows: it cannot be onboarded", kind)
			}

			id := env.createTenantOfType("Default "+kind, "def-"+strings.ToLower(kind)+"-"+randSuffix(), kind)
			status, body := env.doAsTenant(http.MethodGet, "/api/v1/auth/me", env.tenantAdmin(id), nil, id)
			env.mustStatus(http.StatusOK, status, kind+" identity", body)
			got := capabilitySet(t, body)

			for code := range want {
				if !got[code] {
					t.Errorf("%s: default capability %s missing from /auth/me", kind, code)
				}
			}
			for code := range got {
				if !want[code] {
					t.Errorf("%s: /auth/me reports %s, which is not on by default", kind, code)
				}
			}
		})
	}
}

// TestOnboardingHonoursOnlyConfigurableOverrides walks the real onboarding
// endpoint with a BARBER whose owner asked for three things: a catalogue
// (configurable: honoured), no appointments (fixed on: ignored) and a kitchen
// (no BARBER row at all: ignored). The console must then see exactly what the
// matrix allows.
func TestOnboardingHonoursOnlyConfigurableOverrides(t *testing.T) {
	env := newTestEnv(t)
	email := "owner-" + randSuffix() + "@test.local"
	status, body := env.do(http.MethodPost, "/api/v1/admin/tenants", env.superAdmin(), map[string]any{
		"name":           "Override Barber",
		"slug":           "override-barber-" + randSuffix(),
		"business_type":  "BARBER",
		"owner_name":     "Owner",
		"admin_name":     "Admin",
		"admin_email":    email,
		"email":          email,
		"terms_accepted": true,
		"capability_overrides": map[string]bool{
			"CATALOG":      true,
			"APPOINTMENTS": false,
			"KITCHEN":      true,
		},
	})
	env.mustStatus(http.StatusCreated, status, "onboard a barber with overrides", body)
	tenant, _ := body["tenant"].(map[string]any)
	id, err := uuid.Parse(tenant["id"].(string))
	if err != nil {
		t.Fatalf("tenant id: %v (%v)", err, body)
	}

	status, body = env.doAsTenant(http.MethodGet, "/api/v1/auth/me", env.tenantAdmin(id), nil, id)
	env.mustStatus(http.StatusOK, status, "barber identity", body)
	got := capabilitySet(t, body)

	if !got["CATALOG"] {
		t.Errorf("configurable CATALOG override was not applied: %v", got)
	}
	if !got["APPOINTMENTS"] {
		t.Errorf("fixed APPOINTMENTS must not be switched off by an override: %v", got)
	}
	if got["KITCHEN"] {
		t.Errorf("KITCHEN has no BARBER row and must never be granted: %v", got)
	}

	// The gate agrees with the list: the catalogue it asked for works, the
	// kitchen it cannot have is refused.
	owner := env.tenantAdmin(id)
	status, body = env.doAsTenant(http.MethodGet, "/api/v1/tenant/categories", owner, nil, id)
	env.mustStatus(http.StatusOK, status, "barber with a catalogue reads categories", body)
	status, _ = env.doAsTenant(http.MethodGet, "/api/v1/tenant/orders", owner, nil, id)
	if status != http.StatusForbidden {
		t.Errorf("GET /orders for a barber without ORDERS: status = %d, want 403", status)
	}
}
