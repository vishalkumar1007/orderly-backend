package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/marketing"
)

// Marketing framework, Phases 3 to 5: offers at checkout, loyalty earn and
// redeem, order attribution from a tracked link, checkout consent capture, a
// welcome automation, and the Meta webhook (handshake, signature, STOP).

func (s *shopFixture) placeWith(t *testing.T, extra map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"customer_name": "Asha", "customer_phone": "+919800000011", "payment_method": "CASH",
		"client_token": "test-" + randSuffix(),
		"items":        []map[string]any{{"product_id": s.productID, "quantity": 2}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return s.guest(http.MethodPost, "/api/v1/public/orders", body)
}

func (s *shopFixture) complete(t *testing.T, orderID string) {
	t.Helper()
	for _, a := range []string{"accept", "prepare", "ready", "complete"} {
		if status, body := s.act(orderID, a); status != http.StatusOK {
			t.Fatalf("%s: %d %v", a, status, body["__raw"])
		}
	}
}

func TestMarketingGrowthEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	env.resetService("SMTP")
	env.resetService("META")
	sink := startMailSink(t)
	shop := marketingShop(t, env, sink)
	price := shop.price * 2 // two units

	/* ---------- offers ---------- */

	status, offer := shop.mk(http.MethodPost, "/offers", map[string]any{
		"code": "welcome10", "name": "Welcome", "kind": "PERCENT", "value": 10, "per_customer_limit": 1, "first_order_only": true,
	})
	env.mustStatus(http.StatusCreated, status, "create offer", offer)
	if offer["code"] != "WELCOME10" {
		t.Fatalf("code not normalised: %v", offer["code"])
	}
	if status, _ := shop.mk(http.MethodPost, "/offers", map[string]any{"code": "WELCOME10", "kind": "FIXED", "value": 5}); status != http.StatusConflict {
		t.Fatalf("duplicate code = %d, want 409", status)
	}

	// Quote shows the discount; a bad code is a message, not a failure.
	status, quote := shop.guest(http.MethodPost, "/api/v1/public/quote", map[string]any{
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 2}}, "coupon_code": "welcome10",
	})
	env.mustStatus(http.StatusOK, status, "quote", quote)
	if q := quote["offer"].(map[string]any); q["valid"] != true || q["discount"].(float64) != price*0.10 {
		t.Fatalf("quote offer: %v", quote["__raw"])
	}
	if tot := quote["totals"].(map[string]any); tot["discount"].(float64) != price*0.10 {
		t.Fatalf("quote totals: %v", tot)
	}
	_, bad := shop.guest(http.MethodPost, "/api/v1/public/quote", map[string]any{
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}}, "coupon_code": "NOPE",
	})
	if q := bad["offer"].(map[string]any); q["valid"] != false {
		t.Fatalf("bad code accepted: %v", bad["__raw"])
	}

	/* ---------- attribution + consent capture on a guest order ---------- */

	_, link := shop.mk(http.MethodPost, "/links", map[string]any{"label": "Poster", "target_path": "/"})
	status, placed := shop.placeWith(t, map[string]any{
		"coupon_code": "WELCOME10", "marketing_opt_in": true, "customer_email": "asha.k@example.test",
		"attribution": map[string]any{"link_code": link["code"]},
	})
	env.mustStatus(http.StatusCreated, status, "order with offer", placed)
	if tot := placed["totals"].(map[string]any); tot["discount"].(float64) != price*0.10 {
		t.Fatalf("order discount: %v", tot)
	}
	orderID := shop.orderIDOf(t, placed)
	var coupon string
	var hasPromo, hasLink bool
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT coupon_code, promotion_id IS NOT NULL, marketing_link_id IS NOT NULL FROM orders WHERE id = $1`, orderID,
	).Scan(&coupon, &hasPromo, &hasLink); err != nil {
		t.Fatal(err)
	}
	if coupon != "WELCOME10" || !hasPromo || !hasLink {
		t.Fatalf("order marketing columns: coupon=%q promo=%v link=%v", coupon, hasPromo, hasLink)
	}
	// Opt-in made the guest a customer with email and consent on every channel.
	var consents int
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM marketing_consents mc JOIN customers c ON c.id = mc.customer_id
		 WHERE c.tenant_id = $1 AND c.phone = '+919800000011' AND c.email = 'asha.k@example.test' AND mc.status = 'GRANTED'`,
		shop.tenantID).Scan(&consents); err != nil {
		t.Fatal(err)
	}
	if consents != 3 {
		t.Fatalf("checkout opt-in consents = %d, want 3", consents)
	}
	// The same phone cannot use a first-order, once-per-customer code again.
	status, again := shop.placeWith(t, map[string]any{"coupon_code": "WELCOME10"})
	if status != http.StatusBadRequest || !strings.Contains(again["__raw"].(string), "offer_") {
		t.Fatalf("second use = %d %v", status, again["__raw"])
	}
	_, offers := shop.mk(http.MethodGet, "/offers", nil)
	if offers["offers"].([]any)[0].(map[string]any)["redemptions"].(float64) != 1 {
		t.Fatalf("redemptions: %v", offers["__raw"])
	}
	_, ov := shop.mk(http.MethodGet, "/overview", nil)
	if ov["stats"].(map[string]any)["attributed_orders_30d"].(float64) != 1 {
		t.Fatalf("attributed orders: %v", ov["stats"])
	}

	/* ---------- loyalty ---------- */

	status, prog := shop.mk(http.MethodPut, "/loyalty", map[string]any{
		"enabled": true, "points_per_100": 10, "point_value": 1, "min_redeem_points": 10, "max_redeem_percent": 50,
	})
	env.mustStatus(http.StatusOK, status, "enable loyalty", prog)

	phone := "+919800000022"
	token := env.verifyOTP(t, shop, phone)
	status, mine := shop.guestAuth(http.MethodPost, "/api/v1/public/orders", token, map[string]any{
		"customer_name": "Bela", "customer_phone": phone, "payment_method": "CASH", "client_token": "test-" + randSuffix(),
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 2}},
	})
	env.mustStatus(http.StatusCreated, status, "customer order", mine)
	shop.complete(t, shop.orderIDOf(t, mine))
	// floor(total / 100 * points_per_100), on the server-priced total.
	expected := int(math.Floor(mine["totals"].(map[string]any)["total"].(float64) / 100 * 10))
	_, profile := shop.guestAuth(http.MethodGet, "/api/v1/customer/profile", token, nil)
	loy := profile["loyalty"].(map[string]any)
	if loy["enabled"] != true || int(loy["points"].(float64)) != expected {
		t.Fatalf("points after completion: %v (want %d)", profile["__raw"], expected)
	}

	// Spend them: the quote caps by balance and share, the order deducts.
	status, q2 := shop.guestAuth(http.MethodPost, "/api/v1/public/quote", token, map[string]any{
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}}, "redeem_points": 1000,
	})
	env.mustStatus(http.StatusOK, status, "quote with points", q2)
	lq := q2["loyalty"].(map[string]any)
	maxPts := int(lq["max_points"].(float64))
	if maxPts <= 0 || maxPts > expected || int(lq["redeem_points"].(float64)) != maxPts {
		t.Fatalf("quote loyalty: %v", q2["__raw"])
	}
	status, spent := shop.guestAuth(http.MethodPost, "/api/v1/public/orders", token, map[string]any{
		"customer_name": "Bela", "customer_phone": phone, "payment_method": "CASH", "client_token": "test-" + randSuffix(),
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}}, "redeem_points": maxPts,
	})
	env.mustStatus(http.StatusCreated, status, "order with points", spent)
	if tot := spent["totals"].(map[string]any); tot["discount"].(float64) != float64(maxPts) {
		t.Fatalf("points discount: %v", tot)
	}
	_, profile = shop.guestAuth(http.MethodGet, "/api/v1/customer/profile", token, nil)
	if int(profile["loyalty"].(map[string]any)["points"].(float64)) != expected-maxPts {
		t.Fatalf("balance after spend: %v", profile["loyalty"])
	}
	if status, _ := shop.guestAuth(http.MethodPost, "/api/v1/public/orders", token, map[string]any{
		"customer_name": "Bela", "customer_phone": phone, "payment_method": "CASH", "client_token": "test-" + randSuffix(),
		"items": []map[string]any{{"product_id": shop.productID, "quantity": 1}}, "redeem_points": 100000,
	}); status != http.StatusBadRequest {
		t.Fatalf("overspend = %d, want 400", status)
	}
	// Profile consent toggles.
	status, upd := shop.guestAuth(http.MethodPut, "/api/v1/customer/profile", token, map[string]any{"marketing": map[string]bool{"EMAIL": true, "SMS": false}})
	env.mustStatus(http.StatusOK, status, "consent toggle", upd)
	if m := upd["marketing"].(map[string]any); m["EMAIL"] != true || m["SMS"] != false {
		t.Fatalf("profile marketing: %v", upd["__raw"])
	}

	/* ---------- automation: welcome after consent ---------- */

	status, rule := shop.mk(http.MethodPost, "/automations", map[string]any{
		"name": "Welcome", "trigger": "WELCOME", "channel": "EMAIL", "subject": "Welcome to {{business_name}}",
		"body": "Here is {{offer_code}} for next time.", "promotion_id": offer["id"], "quiet_start": 0, "quiet_end": 0,
	})
	env.mustStatus(http.StatusCreated, status, "create rule", rule)
	before := sink.count()
	worker := marketing.NewWorker(sqlc.New(env.server.pool), env.server.configService, env.server.marketingURLs(), env.server.log)
	worker.RunAutomationsNow(context.Background())
	worker.Tick(context.Background())
	worker.Tick(context.Background())
	// Asha (checkout opt-in with email) got a welcome; nobody else has email consent.
	if sink.count() != before+1 {
		t.Fatalf("welcome mails = %d, want 1", sink.count()-before)
	}
	if !strings.Contains(sink.last(), "WELCOME10") {
		t.Fatalf("welcome mail lacks offer code:\n%s", sink.last())
	}
	// Idempotent: a second evaluation sends nothing new.
	worker.RunAutomationsNow(context.Background())
	worker.Tick(context.Background())
	if sink.count() != before+1 {
		t.Fatalf("welcome re-sent: %d", sink.count()-before)
	}
	_, autos := shop.mk(http.MethodGet, "/automations", nil)
	if autos["rules"].([]any)[0].(map[string]any)["runs"].(float64) != 1 {
		t.Fatalf("rule runs: %v", autos["__raw"])
	}
	_, camps := shop.mk(http.MethodGet, "/campaigns?kind=AUTOMATION", nil)
	if len(camps["campaigns"].([]any)) != 1 {
		t.Fatalf("automation campaigns: %v", camps["__raw"])
	}

	/* ---------- WhatsApp readiness and Meta webhook ---------- */

	_, ov = shop.mk(http.MethodGet, "/overview", nil)
	if ov["channels"].(map[string]any)["WHATSAPP"].(map[string]any)["ready"] != false {
		t.Fatalf("whatsapp ready without Meta: %v", ov["channels"])
	}
	status, body := shop.admin(http.MethodPut, "/api/v1/tenant/configurations/meta", map[string]any{
		"provider": "meta",
		"config": map[string]any{
			"access_token": "EAAG-test", "app_secret": "s3cret", "phone_number_id": "1234567890", "verify_token": "verify-me",
		},
		"enabled": true,
	})
	env.mustStatus(http.StatusOK, status, "tenant saves Meta", body)
	if strings.Contains(body["__raw"].(string), "EAAG-test") || strings.Contains(body["__raw"].(string), "s3cret") {
		t.Fatalf("secret leaked in config view: %s", body["__raw"])
	}
	_, ov = shop.mk(http.MethodGet, "/overview", nil)
	if ov["channels"].(map[string]any)["WHATSAPP"].(map[string]any)["ready"] != true {
		t.Fatalf("whatsapp not ready with Meta: %v", ov["channels"])
	}

	client := env.http.Client()
	req, _ := http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/webhooks/meta?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=42", nil)
	req.Host = "api." + env.cfg.BaseDomain
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || buf.String() != "42" {
		t.Fatalf("handshake: %d %q", resp.StatusCode, buf.String())
	}
	req, _ = http.NewRequest(http.MethodGet, env.http.URL+"/api/v1/webhooks/meta?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=42", nil)
	req.Host = "api." + env.cfg.BaseDomain
	if resp, err := client.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad verify token accepted")
	}

	// Asha texts STOP: unsigned is refused, signed revokes WhatsApp consent.
	payload, _ := json.Marshal(map[string]any{"entry": []map[string]any{{"changes": []map[string]any{{"value": map[string]any{
		"metadata": map[string]any{"phone_number_id": "1234567890"},
		"messages": []map[string]any{{"from": "919800000011", "type": "text", "text": map[string]any{"body": "STOP"}}},
	}}}}}})
	post := func(sig string) int {
		req, _ := http.NewRequest(http.MethodPost, env.http.URL+"/api/v1/webhooks/meta", bytes.NewReader(payload))
		req.Host = "api." + env.cfg.BaseDomain
		req.Header.Set("Content-Type", "application/json")
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook = %d, want 401", code)
	}
	if code := post("sha256=deadbeef"); code != http.StatusUnauthorized {
		t.Fatalf("forged webhook = %d, want 401", code)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(payload)
	if code := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); code != http.StatusOK {
		t.Fatalf("signed webhook = %d, want 200", code)
	}
	var waStatus string
	if err := env.server.pool.QueryRow(context.Background(),
		`SELECT mc.status FROM marketing_consents mc JOIN customers c ON c.id = mc.customer_id
		 WHERE c.tenant_id = $1 AND c.phone = '+919800000011' AND mc.channel = 'WHATSAPP'`, shop.tenantID).Scan(&waStatus); err != nil {
		t.Fatal(err)
	}
	if waStatus != "REVOKED" {
		t.Fatalf("STOP did not revoke: %s", waStatus)
	}

	/* ---------- social: without Facebook/Instagram ids, posts are manual ---------- */

	status, postRow := shop.mk(http.MethodPost, "/social", map[string]any{
		"caption": "Brunch this weekend", "platforms": []string{"FACEBOOK"}, "link_id": link["id"],
	})
	env.mustStatus(http.StatusCreated, status, "create post", postRow)
	if !strings.Contains(postRow["share_text"].(string), "/api/v1/go/") {
		t.Fatalf("share text lacks link: %v", postRow["share_text"])
	}
	status, _ = shop.mk(http.MethodPost, "/social/"+postRow["id"].(string)+"/publish", nil)
	env.mustStatus(http.StatusOK, status, "publish post", nil)
	worker.Tick(context.Background())
	_, posts := shop.mk(http.MethodGet, "/social", nil)
	if posts["posts"].([]any)[0].(map[string]any)["status"] != "MANUAL" {
		t.Fatalf("post without a Page should be MANUAL: %v", posts["__raw"])
	}
	if status, _ := shop.mk(http.MethodPost, "/social", map[string]any{"caption": "x", "platforms": []string{"INSTAGRAM"}}); status != http.StatusBadRequest {
		t.Fatalf("instagram without image accepted: %d", status)
	}
}
