package notify

import "testing"

// A template using a variable the sender did not supply must fall back to the
// sender's own copy rather than mail a raw "{{token}}".
func TestRenderedTemplateFallsBackOnUnfilledVariable(t *testing.T) {
	allowed := []string{"customer_name", "starts_at"}

	got := filled(substitute("Hi {{customer_name}}, see you {{starts_at}}", allowed,
		map[string]any{"customer_name": "Asha"}), allowed, "fallback")
	if got != "fallback" {
		t.Fatalf("unfilled variable: got %q, want fallback", got)
	}

	got = filled(substitute("Hi {{customer_name}}, see you {{starts_at}}", allowed,
		map[string]any{"customer_name": "Asha", "starts_at": "5pm"}), allowed, "fallback")
	if got != "Hi Asha, see you 5pm" {
		t.Fatalf("filled template: got %q", got)
	}
}
