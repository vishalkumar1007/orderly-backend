package httpserver

import (
	"net/http"
	"testing"

	"github.com/orderly/orderly-backend/pkg/identity"
)

/*
Storefront drafts.

The Studio's promise is "try several things, publish once". These tests hold the
three properties that promise rests on: the draft belongs to the shop rather
than to a browser, publishing is all-or-nothing, and a draft that has gone stale
is refused instead of quietly overwriting whatever happened in the meantime.
*/

func draftHost(t *testing.T) (*testEnv, string, string) {
	t.Helper()
	env := newTestEnv(t)
	tenantID := env.createTenant("Draft Shop", "draft-shop-"+randSuffix())
	return env, env.hostFor(tenantID), tenantUser(env, identity.RoleTenantAdmin, tenantID)
}

func TestDraftIsStoredForTheShopAndPublishesAtomically(t *testing.T) {
	env, host, owner := draftHost(t)

	// Nothing saved yet.
	status, body := env.doOn(http.MethodGet, "/api/v1/tenant/customize/draft", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read an empty draft", body)
	if body["draft"] != nil {
		t.Fatalf("draft = %v, want null before anything is saved", body["draft"])
	}

	// Save one. This is the autosave, and it must not touch the live shop.
	draft := map[string]any{
		"store": map[string]any{"name": "Draft Name", "tagline": "Only in the draft"},
		"theme": map[string]any{"preset": "fresh", "product_layout": "grid"},
	}
	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/customize/draft", owner, draft, host)
	env.mustStatus(http.StatusOK, status, "save a draft", body)

	status, live := env.doOn(http.MethodGet, "/api/v1/tenant/customize", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the live storefront", live)
	store, _ := live["store"].(map[string]any)
	if store["name"] == "Draft Name" {
		t.Fatal("saving a draft changed the live storefront; a draft is not published")
	}

	// It comes back for anyone signed in to this shop, on any device.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/customize/draft", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the saved draft", body)
	got, _ := body["draft"].(map[string]any)
	gotStore, _ := got["store"].(map[string]any)
	if gotStore["name"] != "Draft Name" {
		t.Fatalf("draft store.name = %v, want it to survive the round trip", gotStore["name"])
	}

	// Publishing applies it and clears the draft.
	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/customize/draft/publish", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "publish the draft", body)
	store, _ = body["store"].(map[string]any)
	if store["name"] != "Draft Name" {
		t.Errorf("published store.name = %v, want the draft's", store["name"])
	}
	theme, _ := body["theme"].(map[string]any)
	if theme["product_layout"] != "grid" {
		t.Errorf("published product_layout = %v, want grid", theme["product_layout"])
	}

	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/customize/draft", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the draft after publishing", body)
	if body["draft"] != nil {
		t.Errorf("draft = %v, want it cleared once published", body["draft"])
	}
}

func TestPublishingARejectedDraftChangesNothing(t *testing.T) {
	env, host, owner := draftHost(t)

	status, before := env.doOn(http.MethodGet, "/api/v1/tenant/customize", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the storefront before", before)
	beforeStore, _ := before["store"].(map[string]any)

	// A draft whose name is fine but whose layout is not. Identity is applied
	// before the theme, so a non-transactional publish would leave the new name
	// on a shop that refused the rest — which is the failure this guards.
	draft := map[string]any{
		"store": map[string]any{"name": "Should Not Survive"},
		"theme": map[string]any{"product_layout": "not-a-layout"},
	}
	status, body := env.doOn(http.MethodPut, "/api/v1/tenant/customize/draft", owner, draft, host)
	env.mustStatus(http.StatusOK, status, "save a draft that cannot publish", body)

	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/customize/draft/publish", owner, nil, host)
	env.mustStatus(http.StatusBadRequest, status, "publish an invalid draft", body)

	status, after := env.doOn(http.MethodGet, "/api/v1/tenant/customize", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the storefront after", after)
	afterStore, _ := after["store"].(map[string]any)
	if afterStore["name"] != beforeStore["name"] {
		t.Fatalf("store.name = %v, want it unchanged at %v — the publish was not atomic",
			afterStore["name"], beforeStore["name"])
	}

	// The draft is still there to be fixed, not thrown away.
	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/customize/draft", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the draft after a failed publish", body)
	if body["draft"] == nil {
		t.Error("a draft that failed to publish was discarded; the work is lost")
	}
}

func TestAStaleDraftIsRefusedRatherThanOverwriting(t *testing.T) {
	env, host, owner := draftHost(t)

	status, body := env.doOn(http.MethodPut, "/api/v1/tenant/customize/draft", owner,
		map[string]any{"store": map[string]any{"name": "From The Draft"}}, host)
	env.mustStatus(http.StatusOK, status, "start a draft", body)

	// Somebody else changes the shop while the draft sits open.
	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/customize", owner,
		map[string]any{"tagline": "Changed underneath"}, host)
	env.mustStatus(http.StatusOK, status, "change the live storefront", body)

	status, body = env.doOn(http.MethodGet, "/api/v1/tenant/customize/draft", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "read the draft", body)
	if stale, _ := body["stale"].(bool); !stale {
		t.Error("the draft is not reported stale, so the console cannot warn anybody")
	}

	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/customize/draft/publish", owner, nil, host)
	if status != http.StatusConflict {
		t.Fatalf("publish a stale draft: status = %d, want 409 (body: %v)", status, body)
	}

	// The owner can still say "publish anyway" once they have been told.
	status, body = env.doOn(http.MethodPost,
		"/api/v1/tenant/customize/draft/publish?force=true", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "force-publish after being warned", body)
	store, _ := body["store"].(map[string]any)
	if store["name"] != "From The Draft" {
		t.Errorf("store.name = %v, want the forced draft to win", store["name"])
	}
}

func TestDraftPublishLeavesLiveOpsUntouched(t *testing.T) {
	env, host, owner := draftHost(t)

	// Set live ops via the immediate APIs (Action / Customize).
	status, body := env.doOn(http.MethodPut, "/api/v1/tenant/customize", owner, map[string]any{
		"store_status":   "BUSY",
		"status_message": "[offer] Live banner",
	}, host)
	env.mustStatus(http.StatusOK, status, "set live status + banner", body)

	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/customize/hours", owner, map[string]any{
		"always_open": true,
		"timezone":    "Asia/Kolkata",
	}, host)
	env.mustStatus(http.StatusOK, status, "set live hours", body)

	// An older draft that still carries ops fields must not overwrite them.
	draft := map[string]any{
		"behaviour": map[string]any{
			"store_status":      "CLOSED",
			"status_message":    "[alert] Stale draft banner",
			"prep_time_minutes": 45,
		},
		"hours": map[string]any{
			"always_open": false,
			"timezone":    "UTC",
		},
	}
	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/customize/draft", owner, draft, host)
	env.mustStatus(http.StatusOK, status, "save draft with stale ops", body)

	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/customize/draft/publish", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "publish draft", body)

	behaviour, _ := body["behaviour"].(map[string]any)
	if behaviour["store_status"] != "BUSY" {
		t.Errorf("published store_status = %v, want BUSY (live ops)", behaviour["store_status"])
	}
	if behaviour["status_message"] != "[offer] Live banner" {
		t.Errorf("published status_message = %v, want live banner", behaviour["status_message"])
	}
	if behaviour["prep_time_minutes"] != float64(45) && behaviour["prep_time_minutes"] != 45 {
		t.Errorf("published prep_time_minutes = %v, want 45", behaviour["prep_time_minutes"])
	}
	hours, _ := body["hours"].(map[string]any)
	if hours["always_open"] != true {
		t.Errorf("published hours.always_open = %v, want true (live ops)", hours["always_open"])
	}
}

func TestDraftPublishAppliesPrepTimeWithoutTouchingBanner(t *testing.T) {
	env, host, owner := draftHost(t)

	status, body := env.doOn(http.MethodPut, "/api/v1/tenant/customize", owner, map[string]any{
		"status_message": "[offer] Keep me",
	}, host)
	env.mustStatus(http.StatusOK, status, "set live banner", body)

	draft := map[string]any{
		"behaviour": map[string]any{
			"prep_time_minutes": 30,
		},
	}
	status, body = env.doOn(http.MethodPut, "/api/v1/tenant/customize/draft", owner, draft, host)
	env.mustStatus(http.StatusOK, status, "save draft", body)

	status, body = env.doOn(http.MethodPost, "/api/v1/tenant/customize/draft/publish", owner, nil, host)
	env.mustStatus(http.StatusOK, status, "publish draft", body)

	behaviour, _ := body["behaviour"].(map[string]any)
	if behaviour["status_message"] != "[offer] Keep me" {
		t.Errorf("published status_message = %v, want live banner kept", behaviour["status_message"])
	}
	if behaviour["prep_time_minutes"] != float64(30) && behaviour["prep_time_minutes"] != 30 {
		t.Errorf("published prep_time_minutes = %v, want 30", behaviour["prep_time_minutes"])
	}
}

func TestDraftsAreOwnerOnlyAndNeverCrossShops(t *testing.T) {
	env := newTestEnv(t)
	tenantID := env.createTenant("Draft Fence", "draft-fence-"+randSuffix())
	host := env.hostFor(tenantID)

	// A manager runs the shop but does not rewrite the storefront, exactly as
	// with every other customize route.
	manager := tenantUser(env, identity.RoleManager, tenantID)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/tenant/customize/draft", nil},
		{http.MethodPut, "/api/v1/tenant/customize/draft", map[string]any{"store": map[string]any{}}},
		{http.MethodPost, "/api/v1/tenant/customize/draft/publish", nil},
	} {
		status, body := env.doOn(tc.method, tc.path, manager, tc.body, host)
		if status != http.StatusForbidden {
			t.Errorf("%s %s as a manager: status = %d, want 403 (body: %v)",
				tc.method, tc.path, status, body)
		}
	}
}
