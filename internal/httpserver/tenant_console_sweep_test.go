package httpserver

import (
	"testing"
)

/*
Every screen in the business console, fetched the way the console fetches it.

The console is a browser app: its routes render an empty shell server-side and
then call the API. A page that 500s therefore looks like a working route to any
check that only renders HTML — the failure is in the payload, one layer down.
This sweep is that layer. It signs in as the business owner, asks for every
read the console performs, and fails on any 5xx.

A 500 here is never acceptable: it means the handler broke rather than refused.
403 and 404 are fine and are not asserted against — what may be reached is the
subject of the IAM tests, not this one.
*/
func TestTenantConsoleReadsNeverFail(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Sweep Cafe", "sweep-cafe")
	token := env.tenantAdmin(tenantID)

	// Every GET the tenant router registers, in the order the rail lists them.
	// Parameterised routes are covered by their collection endpoints; the
	// per-id handlers are exercised in their own tests with a real id.
	paths := []string{
		// Running
		"/api/v1/tenant/dashboard",
		"/api/v1/tenant/orders",
		"/api/v1/tenant/order-history",
		// Catalogue
		"/api/v1/tenant/products",
		"/api/v1/tenant/categories",
		// People
		"/api/v1/tenant/customers",
		"/api/v1/tenant/customers/guest",
		"/api/v1/tenant/users",
		"/api/v1/tenant/iam",
		// Storefront
		"/api/v1/tenant/storefront",
		"/api/v1/tenant/storefront/preview",
		"/api/v1/tenant/storefront/qr",
		"/api/v1/tenant/customize",
		"/api/v1/tenant/customize/preview",
		"/api/v1/tenant/customize/qr",
		"/api/v1/tenant/store-link",
		// Organization
		"/api/v1/tenant/payment-settings",
		"/api/v1/tenant/order-workflow",
		"/api/v1/tenant/setup",
		"/api/v1/tenant/analytics",
		"/api/v1/tenant/activity",
		"/api/v1/tenant/audit-logs",
		// Shell
		"/api/v1/tenant/theme",
		"/api/v1/tenant/theme-presets",
		"/api/v1/tenant/me/appearance",
		// Integrations
		"/api/v1/tenant/configurations",
		"/api/v1/tenant/service-preferences",
		"/api/v1/tenant/configurations/SMTP",
		"/api/v1/tenant/configurations/SMTP/effective",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			status, body := env.doAsTenant("GET", path, token, nil, tenantID)
			if status >= 500 {
				t.Errorf("GET %s returned %d: %v", path, status, body)
			}
		})
	}
}

// The identity call is the console's first request; if it fails nothing else
// renders, so it is asserted separately and on its own terms.
func TestTenantIdentityCarriesTheBusinessType(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Type Carrier", "type-carrier")
	token := env.tenantAdmin(tenantID)

	status, body := env.doAsTenant("GET", "/api/v1/auth/me", token, nil, tenantID)
	if status != 200 {
		t.Fatalf("GET /auth/me returned %d: %v", status, body)
	}
	host, ok := body["host_tenant"].(map[string]any)
	if !ok {
		t.Fatalf("no host_tenant in /auth/me: %v", body)
	}
	if _, ok := host["business_type"]; !ok {
		t.Errorf("host_tenant has no business_type; the console shell cannot label itself: %v", host)
	}
}
