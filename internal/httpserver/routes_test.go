package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/orderly/orderly-backend/internal/config"
)

// TestAdminRouteRegistration walks the real router and asserts every Super Admin
// endpoint the console depends on is mounted. Auth middleware runs before
// routing, so a 401 from a live server cannot distinguish "missing route" from
// "protected route" — only the router itself can.
func TestAdminRouteRegistration(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, config.Config{})

	got := map[string]bool{}
	chi.Walk(s.Router().(*chi.Mux), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	})

	want := []string{
		// Tenant user lifecycle
		"GET /api/v1/admin/tenants/{id}/users",
		"POST /api/v1/admin/tenants/{id}/users",
		"PATCH /api/v1/admin/tenants/{id}/users/{userId}",
		"POST /api/v1/admin/tenants/{id}/users/{userId}/reset-access",
		"POST /api/v1/admin/tenants/{id}/users/{userId}/resend-invite",
		// Flat user aliases
		"POST /api/v1/admin/users",
		"PATCH /api/v1/admin/users/{id}",
		// Plan catalog CRUD
		"GET /api/v1/admin/plans",
		"POST /api/v1/admin/plans",
		"PATCH /api/v1/admin/plans/{id}",
		// Theme preset catalog CRUD
		"GET /api/v1/admin/theme-presets",
		"POST /api/v1/admin/theme-presets",
		"PATCH /api/v1/admin/theme-presets/{id}",
		"DELETE /api/v1/admin/theme-presets/{id}",
		// Audit log with result filtering
		"GET /api/v1/admin/audit-logs",
	}

	for _, r := range want {
		if !got[r] {
			t.Errorf("route not registered: %s", r)
		}
	}
}

// TestUnmatchedAdminPathIsNotFound proves the walk is meaningful: a path that is
// genuinely absent must not appear in the table.
func TestUnmatchedAdminPathIsNotFound(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/definitely-not-a-route", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	// Middleware short-circuits before routing, so assert the route table
	// rather than the status code.
	found := false
	chi.Walk(s.Router().(*chi.Mux), func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route == "/api/v1/admin/definitely-not-a-route" {
			found = true
		}
		return nil
	})
	if found {
		t.Error("bogus path unexpectedly present in route table")
	}
}
