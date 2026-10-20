package httpserver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/marketing"
)

// Marketing framework, Phase 1 and 2: plan gate, tenant isolation, consent,
// trackable links, and a campaign run end to end through the real router,
// database and worker against a fake SMTP server.

// mailSink is just enough SMTP to accept one message and keep its body.
type mailSink struct {
	ln       net.Listener
	port     int
	mu       sync.Mutex
	messages []string
}

func startMailSink(t *testing.T) *mailSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mailSink{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return s
}

func (s *mailSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

func (s *mailSink) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return ""
	}
	return s.messages[len(s.messages)-1]
}

func (s *mailSink) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(line string) { _, _ = fmt.Fprint(conn, line+"\r\n") }
	write("220 sink ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250-sink")
			write("250 8BITMIME")
		case strings.HasPrefix(cmd, "DATA"):
			write("354 go ahead")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body.WriteString(l)
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
			s.mu.Unlock()
			write("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		case strings.HasPrefix(cmd, "STARTTLS"):
			write("454 no tls")
		default:
			write("250 ok")
		}
	}
}

// marketingShop is a shop whose plan includes MARKETING, with its own SMTP
// pointed at the sink and three customers: two with email, one without.
func marketingShop(t *testing.T, env *testEnv, sink *mailSink) *shopFixture {
	t.Helper()
	shop := newStockShop(t, env, "ANALYTICS", "MARKETING")
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/configurations/smtp", map[string]any{
		"provider": "smtp",
		"config": map[string]any{
			"host": "127.0.0.1", "port": sink.port, "encryption": "NONE",
			"from_name": "Test Kitchen", "from_email": "hello@test.local",
		},
		"enabled": true,
	})
	env.mustStatus(http.StatusOK, status, "tenant saves SMTP", body)
	for _, c := range []struct{ name, email, phone string }{
		{"Asha", "asha@example.test", "+919800000001"},
		{"Bilal", "bilal@example.test", ""},
		{"Chen", "", "+919800000003"},
	} {
		var email, phone any
		if c.email != "" {
			email = c.email
		}
		if c.phone != "" {
			phone = c.phone
		}
		if _, err := env.server.pool.Exec(tctx(),
			`INSERT INTO customers (tenant_id, name, email, phone) VALUES ($1, $2, $3, $4)`,
			shop.tenantID, c.name, email, phone); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
	}
	return shop
}

func (s *shopFixture) mk(method, path string, body any) (int, map[string]any) {
	return s.admin(method, "/api/v1/tenant/marketing"+path, body)
}

func TestMarketingPlanGate(t *testing.T) {
	env := newTestEnv(t)
	shop := newStockShop(t, env, "ANALYTICS") // no MARKETING
	status, body := shop.mk(http.MethodGet, "/overview", nil)
	if status != http.StatusForbidden || body["error"].(map[string]any)["code"] != "feature_not_in_plan" {
		t.Fatalf("plan without MARKETING: %d %v", status, body["__raw"])
	}
}

func TestMarketingCampaignEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	sink := startMailSink(t)
	shop := marketingShop(t, env, sink)
	other := newStockShop(t, env, "ANALYTICS", "MARKETING")

	// Readiness: email connected, SMS not in plan.
	status, body := shop.mk(http.MethodGet, "/overview", nil)
	env.mustStatus(http.StatusOK, status, "overview", body)
	channels := body["channels"].(map[string]any)
	if !channels["EMAIL"].(map[string]any)["ready"].(bool) {
		t.Fatalf("email should be ready: %v", channels)
	}
	if channels["SMS"].(map[string]any)["allowed"].(bool) {
		t.Fatalf("SMS should not be allowed without NOTIFY_SMS: %v", channels)
	}

	// A trackable link, and its public redirect counts a visit.
	status, link := shop.mk(http.MethodPost, "/links", map[string]any{"label": "Table cards", "target_path": "/menu"})
	env.mustStatus(http.StatusCreated, status, "create link", link)
	if status, _ := shop.mk(http.MethodPost, "/links", map[string]any{"target_path": "https://evil.example"}); status != http.StatusBadRequest {
		t.Fatalf("absolute URL accepted as link target: %d", status)
	}
	code := link["code"].(string)
	req, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/go/"+code, nil)
	req.Host = "api." + env.cfg.BaseDomain
	client := env.http.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The redirect lands on the storefront page and carries the link code so
	// an order placed afterwards can be attributed to it.
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.Contains(loc, "/menu?mc="+code) || !strings.Contains(loc, shop.slug) {
		t.Fatalf("redirect: %d %q", resp.StatusCode, loc)
	}
	status, links := shop.mk(http.MethodGet, "/links", nil)
	env.mustStatus(http.StatusOK, status, "list links", links)
	if v := links["links"].([]any)[0].(map[string]any)["visits"].(float64); v != 1 {
		t.Fatalf("visits = %v, want 1", v)
	}

	// Audience before consent: two have email, none may be contacted.
	status, prev := shop.mk(http.MethodPost, "/audience/preview", map[string]any{"audience": "ALL", "channel": "EMAIL"})
	env.mustStatus(http.StatusOK, status, "preview", prev)
	if prev["eligible"].(float64) != 0 || prev["no_consent"].(float64) != 2 || prev["no_address"].(float64) != 1 {
		t.Fatalf("preview before consent: %v", prev["__raw"])
	}
	if status, _ := shop.mk(http.MethodPost, "/consent/grant-all", map[string]any{"channel": "EMAIL"}); status != http.StatusBadRequest {
		t.Fatalf("grant without attestation accepted: %d", status)
	}
	status, granted := shop.mk(http.MethodPost, "/consent/grant-all", map[string]any{"channel": "EMAIL", "attested": true})
	env.mustStatus(http.StatusOK, status, "grant all", granted)
	if granted["granted"].(float64) != 2 {
		t.Fatalf("granted = %v, want 2", granted["granted"])
	}

	// Draft, then isolation: the other tenant cannot see or edit it.
	status, c := shop.mk(http.MethodPost, "/campaigns", map[string]any{
		"name": "Weekend brunch", "goal": "PROMOTE", "audience": "ALL", "channel": "EMAIL",
		"subject": "Brunch at {{business_name}}", "body": "Come by this weekend. {{link}}",
		"button_label": "See the menu", "link_id": link["id"],
	})
	env.mustStatus(http.StatusCreated, status, "create campaign", c)
	id := c["id"].(string)
	if status, _ := other.admin(http.MethodGet, "/api/v1/tenant/marketing/campaigns/"+id, nil); status != http.StatusNotFound {
		t.Fatalf("cross-tenant read = %d, want 404", status)
	}
	if status, _ := other.admin(http.MethodPost, "/api/v1/tenant/marketing/campaigns/"+id+"/schedule", map[string]any{}); status != http.StatusNotFound {
		t.Fatalf("cross-tenant schedule = %d, want 404", status)
	}
	// The other tenant's link cannot be attached to our campaign.
	_, otherLink := other.admin(http.MethodPost, "/api/v1/tenant/marketing/links", map[string]any{"target_path": "/"})
	if status, _ := shop.mk(http.MethodPatch, "/campaigns/"+id, map[string]any{
		"name": "x", "channel": "EMAIL", "body": "b", "subject": "s", "link_id": otherLink["id"],
	}); status != http.StatusBadRequest {
		t.Fatalf("foreign link accepted: %d", status)
	}

	status, v := shop.mk(http.MethodPost, "/campaigns/"+id+"/validate", nil)
	env.mustStatus(http.StatusOK, status, "validate", v)
	if !v["ready"].(bool) {
		t.Fatalf("not ready: %v", v["problems"])
	}

	// Send now: SCHEDULED, then the worker materialises and delivers.
	status, scheduled := shop.mk(http.MethodPost, "/campaigns/"+id+"/schedule", map[string]any{})
	env.mustStatus(http.StatusOK, status, "schedule", scheduled)
	if scheduled["status"] != marketing.StatusScheduled {
		t.Fatalf("status = %v", scheduled["status"])
	}
	if status, _ := shop.mk(http.MethodPatch, "/campaigns/"+id, map[string]any{"name": "late edit", "channel": "EMAIL", "body": "b", "subject": "s"}); status != http.StatusConflict {
		t.Fatalf("edit after schedule = %d, want 409", status)
	}

	worker := marketing.NewWorker(sqlc.New(env.server.pool), env.server.configService, env.server.marketingURLs(), env.server.log)
	worker.Tick(context.Background()) // claim + materialise + deliver
	worker.Tick(context.Background()) // finish

	status, done := shop.mk(http.MethodGet, "/campaigns/"+id, nil)
	env.mustStatus(http.StatusOK, status, "get campaign", done)
	if done["status"] != marketing.StatusCompleted || done["total_count"].(float64) != 2 || done["sent_count"].(float64) != 2 {
		t.Fatalf("after run: status=%v total=%v sent=%v failed=%v reason=%v",
			done["status"], done["total_count"], done["sent_count"], done["failed_count"], done["status_reason"])
	}
	if sink.count() != 2 {
		t.Fatalf("sink got %d messages, want 2", sink.count())
	}
	mail := sink.last()
	if !strings.Contains(mail, "/api/v1/go/"+code) || !strings.Contains(mail, "/api/v1/marketing/unsubscribe/") {
		t.Fatalf("mail lacks tracked link or unsubscribe:\n%s", mail)
	}
	if strings.Contains(mail, "{{") {
		t.Fatalf("unfilled variable in mail:\n%s", mail)
	}

	// A second tick is idempotent: nothing is resent.
	worker.Tick(context.Background())
	if sink.count() != 2 {
		t.Fatalf("resend: sink has %d messages", sink.count())
	}

	// Unsubscribe via the public link revokes consent for the next campaign.
	recipients := done["recipients"].([]any)
	rid := recipients[0].(map[string]any)["id"].(string)
	ureq, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/marketing/unsubscribe/"+rid, nil)
	ureq.Host = "api." + env.cfg.BaseDomain
	uresp, err := client.Do(ureq)
	if err != nil {
		t.Fatal(err)
	}
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unsubscribe = %d", uresp.StatusCode)
	}
	status, prev = shop.mk(http.MethodPost, "/audience/preview", map[string]any{"audience": "ALL", "channel": "EMAIL"})
	env.mustStatus(http.StatusOK, status, "preview after unsubscribe", prev)
	if prev["eligible"].(float64) != 1 {
		t.Fatalf("eligible after unsubscribe = %v, want 1", prev["eligible"])
	}
	// A bulk grant never overrides a revocation.
	shop.mk(http.MethodPost, "/consent/grant-all", map[string]any{"channel": "EMAIL", "attested": true})
	_, prev = shop.mk(http.MethodPost, "/audience/preview", map[string]any{"audience": "ALL", "channel": "EMAIL"})
	if prev["eligible"].(float64) != 1 {
		t.Fatalf("grant-all overrode an unsubscribe: eligible = %v", prev["eligible"])
	}

	// Nobody eligible for SMS (feature not in plan) is a schedule refusal, not a silent empty run.
	status, smsC := shop.mk(http.MethodPost, "/campaigns", map[string]any{"name": "sms", "channel": "SMS", "body": "hi"})
	env.mustStatus(http.StatusCreated, status, "create sms", smsC)
	status, refused := shop.mk(http.MethodPost, "/campaigns/"+smsC["id"].(string)+"/schedule", map[string]any{})
	if status != http.StatusUnprocessableEntity || refused["error"].(map[string]any)["code"] != "campaign_not_ready" {
		t.Fatalf("sms schedule = %d %v", status, refused["__raw"])
	}

	// Cancel a scheduled campaign: queued recipients never go out.
	_, c2 := shop.mk(http.MethodPost, "/campaigns", map[string]any{"name": "later", "channel": "EMAIL", "subject": "s", "body": "b"})
	id2 := c2["id"].(string)
	status, _ = shop.mk(http.MethodPost, "/campaigns/"+id2+"/schedule", map[string]any{"scheduled_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)})
	if status != http.StatusOK {
		t.Fatalf("schedule later = %d", status)
	}
	status, cancelled := shop.mk(http.MethodPost, "/campaigns/"+id2+"/cancel", nil)
	env.mustStatus(http.StatusOK, status, "cancel", cancelled)
	if cancelled["status"] != marketing.StatusCancelled {
		t.Fatalf("cancel status = %v", cancelled["status"])
	}
	if status, _ = shop.mk(http.MethodPost, "/campaigns/"+id2+"/resume", nil); status != http.StatusConflict {
		t.Fatalf("resume cancelled = %d, want 409", status)
	}
	worker.Tick(context.Background())
	if sink.count() != 2 {
		t.Fatalf("cancelled campaign sent mail: %d", sink.count())
	}
}
