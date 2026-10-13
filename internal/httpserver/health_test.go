package httpserver

import (
	"net/http"
	"testing"
)

// The console keys its health rows by id; a duplicate id crashes the page into
// a permanent loading state, and a blank name renders an unlabeled row.
func TestSystemHealthRowsAreUniqueAndNamed(t *testing.T) {
	env := newTestEnv(t)
	status, body := env.do(http.MethodGet, "/api/v1/admin/system-health", env.superAdmin(), nil)
	env.mustStatus(http.StatusOK, status, "system health", body)

	seen := map[string]bool{}
	rows, _ := body["components"].([]any)
	if len(rows) == 0 {
		t.Fatal("no components returned")
	}
	for _, r := range rows {
		c, _ := r.(map[string]any)
		id, _ := c["id"].(string)
		name, _ := c["name"].(string)
		if seen[id] {
			t.Errorf("component id %q is listed twice", id)
		}
		seen[id] = true
		if name == "" {
			t.Errorf("component %q has no name", id)
		}
	}
}
