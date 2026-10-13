package httpserver

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/orders"
)

// N1 / N3: every order status change publishes an event, and every V1 event
// ships with real wording.

// TestEveryOrderStatusPublishesAnEvent: a status added to the order machine
// without an event would change state silently. PENDING is the one exception —
// an order is placed by checkout (ORDER_PLACED), never moved into PENDING.
func TestEveryOrderStatusPublishesAnEvent(t *testing.T) {
	env := newTestEnv(t)
	for _, m := range frozenMachines {
		if m.name != "order" {
			continue
		}
		for state := range m.transitions {
			if state == "PENDING" {
				continue
			}
			code, ok := orders.EventForStatus(state)
			if !ok {
				t.Errorf("order status %s publishes no event", state)
				continue
			}
			var exists bool
			if err := env.server.pool.QueryRow(tctx(), `SELECT EXISTS (SELECT 1 FROM notification_events WHERE code = $1)`, code).Scan(&exists); err != nil || !exists {
				t.Errorf("status %s publishes %s, which has no row in notification_events (Dispatch would silently drop it)", state, code)
			}
		}
	}
}

// TestDefaultTemplatesUseOnlyDeclaredVariables: substitution leaves an
// undeclared {{token}} in the sent message verbatim, so a typo would reach a
// customer. Every V1 event must also have both an EMAIL and an SMS default.
func TestDefaultTemplatesUseOnlyDeclaredVariables(t *testing.T) {
	env := newTestEnv(t)
	v1 := []string{"ORDER_PLACED", "ORDER_ACCEPTED", "ORDER_PREPARING", "ORDER_READY", "ORDER_COMPLETED",
		"ORDER_CANCELLED", "PAYMENT_RECEIVED", "PAYMENT_FAILED", "INVENTORY_LOW"}
	token := regexp.MustCompile(`\{\{(\w+)\}\}`)
	for _, code := range v1 {
		var declared []string
		rows, err := env.server.pool.Query(tctx(), `SELECT jsonb_array_elements_text(variables) FROM notification_events WHERE code = $1`, code)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var v string
			_ = rows.Scan(&v)
			declared = append(declared, v)
		}
		rows.Close()
		if len(declared) == 0 {
			t.Errorf("%s declares no variables", code)
		}
		for _, channel := range []string{"EMAIL", "SMS"} {
			var subject, body string
			err := env.server.pool.QueryRow(tctx(),
				`SELECT subject, body FROM notification_templates WHERE tenant_id IS NULL AND event_code = $1 AND channel = $2 AND is_active`,
				code, channel).Scan(&subject, &body)
			if err != nil {
				t.Errorf("%s has no default %s template: %v", code, channel, err)
				continue
			}
			if strings.TrimSpace(body) == "" || (channel == "EMAIL" && strings.TrimSpace(subject) == "") {
				t.Errorf("%s %s template is empty", code, channel)
			}
			for _, m := range token.FindAllStringSubmatch(subject+" "+body, -1) {
				found := false
				for _, d := range declared {
					found = found || d == m[1]
				}
				if !found {
					t.Errorf("%s %s template uses {{%s}}, which the event does not declare (%v)", code, channel, m[1], declared)
				}
			}
		}
		// And the rendered sample has no raw token left.
		subj, body := notify.RenderSample(tctx(), sqlc.New(env.server.pool), nil, code, "EMAIL")
		if strings.Contains(subj+body, "{{") {
			t.Errorf("%s EMAIL sample still contains a raw token: %q / %q", code, subj, body)
		}
	}
}

func inAppEvents(t *testing.T, env *testEnv, tenantID uuid.UUID) []string {
	t.Helper()
	rows, err := env.server.pool.Query(tctx(),
		// One recipient (the owner): every user who may sell gets their own copy.
		`SELECT n.event_code FROM notifications n JOIN users u ON u.id = n.user_id
		 WHERE n.tenant_id = $1 AND u.role = 'TENANT_ADMIN' AND n.event_code LIKE 'ORDER_%'
		 ORDER BY n.created_at, n.id`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		out = append(out, c)
	}
	return out
}

func deliveries(t *testing.T, env *testEnv, tenantID uuid.UUID, channel string) []string {
	t.Helper()
	rows, err := env.server.pool.Query(tctx(),
		`SELECT event_code FROM notification_deliveries WHERE tenant_id = $1 AND channel = $2 ORDER BY created_at, id`, tenantID, channel)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		out = append(out, c)
	}
	return out
}

// enableInApp turns on the in-app rule for the quiet-by-default events so a test
// can observe every one of them.
func enableInApp(t *testing.T, env *testEnv, tenantID uuid.UUID, codes ...string) {
	t.Helper()
	for _, c := range codes {
		if _, err := env.server.pool.Exec(tctx(),
			`INSERT INTO notification_rules (tenant_id, event_code, channel, enabled, recipient_policy, priority, locked)
			 VALUES ($1, $2, 'IN_APP', TRUE, 'ROLE:selling', 'NORMAL', FALSE)`, tenantID, c); err != nil {
			t.Fatalf("enable %s: %v", c, err)
		}
	}
}

func TestOrderLifecyclePublishesAnEventAtEveryStep(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	enableInApp(t, env, shop.tenantID, "ORDER_ACCEPTED", "ORDER_PREPARING", "ORDER_COMPLETED")

	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	id := env.orderID(shop.tenantID, number(t, order, "order_number"))
	for _, step := range []string{"accept", "prepare", "ready", "complete"} {
		if status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+id+"/"+step, nil); status != http.StatusOK {
			t.Fatalf("%s: %d %v", step, status, body["__raw"])
		}
	}
	want := "[ORDER_PLACED ORDER_ACCEPTED ORDER_PREPARING ORDER_READY ORDER_COMPLETED]"
	if got := fmt.Sprint(inAppEvents(t, env, shop.tenantID)); got != want {
		t.Errorf("events = %s, want %s — one per status, in order, none twice", got, want)
	}

	// A cancelled order publishes ORDER_CANCELLED.
	second := shop.order(t, "Ravi Kumar", "9811122233", "CASH")
	if status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+env.orderID(shop.tenantID, number(t, second, "order_number"))+"/cancel", nil); status != http.StatusOK {
		t.Fatalf("cancel: %d %v", status, body["__raw"])
	}
	events := inAppEvents(t, env, shop.tenantID)
	if last := events[len(events)-1]; last != "ORDER_CANCELLED" {
		t.Errorf("last event after a cancel = %s, want ORDER_CANCELLED (all: %v)", last, events)
	}
}

func TestAutoAcceptAndAutoCompleteStillPublishEveryStep(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	enableInApp(t, env, shop.tenantID, "ORDER_ACCEPTED", "ORDER_COMPLETED")
	if status, body := shop.admin(http.MethodPut, "/api/v1/tenant/order-workflow",
		map[string]any{"acceptance_mode": "AUTO", "auto_complete": true}); status != http.StatusOK {
		t.Fatalf("workflow: %d %v", status, body["__raw"])
	}
	order := shop.order(t, "Asha Rao", "9876543210", "CASH")
	if got := str(t, order, "status"); got != "ACCEPTED" {
		t.Fatalf("auto-accept status = %q", got)
	}
	id := env.orderID(shop.tenantID, number(t, order, "order_number"))
	for _, step := range []string{"prepare", "ready"} { // READY auto-completes
		if status, body := shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+id+"/"+step, nil); status != http.StatusOK {
			t.Fatalf("%s: %d %v", step, status, body["__raw"])
		}
	}
	if got, want := fmt.Sprint(inAppEvents(t, env, shop.tenantID)), "[ORDER_PLACED ORDER_ACCEPTED ORDER_READY ORDER_COMPLETED]"; got != want {
		t.Errorf("events = %s, want %s (auto-accept at placement and auto-complete at ready each publish)", got, want)
	}
}

// Defaults are quiet: the customer is emailed on acceptance, ready and cancel,
// not on every kitchen step.
func TestDefaultCustomerEmailsAreOnlyTheUsefulOnes(t *testing.T) {
	env := newTestEnv(t)
	shop := newShop(t, env)
	status, created := shop.guest(http.MethodPost, "/api/v1/public/orders", map[string]any{
		"customer_name": "Asha Rao", "customer_phone": "9876543210", "customer_email": "asha@test.local",
		"payment_method": "CASH", "client_token": "mail-" + randSuffix(),
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}},
	})
	env.mustStatus(http.StatusCreated, status, "order with an email", created)
	id := env.orderID(shop.tenantID, number(t, created, "order_number"))
	for _, step := range []string{"accept", "prepare", "ready", "complete"} {
		shop.admin(http.MethodPost, "/api/v1/tenant/orders/"+id+"/"+step, nil)
	}
	got := fmt.Sprint(deliveries(t, env, shop.tenantID, "EMAIL"))
	if got != "[ORDER_ACCEPTED ORDER_READY]" {
		t.Errorf("customer emails = %s, want [ORDER_ACCEPTED ORDER_READY] (preparing and completed are off by default)", got)
	}
}
