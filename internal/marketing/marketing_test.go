package marketing

import (
	"errors"
	"strings"
	"testing"

	"github.com/orderly/orderly-backend/db/sqlc"
)

func TestDraftClean(t *testing.T) {
	d := Draft{Name: "  Lunch deal ", Channel: "email", Goal: "", Audience: " repeat "}
	if err := d.Clean(); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if d.Name != "Lunch deal" || d.Channel != ChannelEmail || d.Goal != "PROMOTE" || d.Audience != "REPEAT" {
		t.Fatalf("normalised wrong: %+v", d)
	}
	for _, bad := range []Draft{
		{Name: "", Channel: "EMAIL"},
		{Name: "x", Channel: "TELEGRAM"},
		{Name: "x", Channel: "EMAIL", Audience: "VIP"},
		{Name: "x", Channel: "EMAIL", LinkID: "not-a-uuid"},
		{Name: strings.Repeat("n", maxNameLen+1), Channel: "EMAIL"},
	} {
		if err := bad.Clean(); err == nil {
			t.Errorf("expected error for %+v", bad)
		}
	}
}

func TestProblems(t *testing.T) {
	none := func(string) bool { return false }
	all := func(string) bool { return true }
	down := errors.New("not configured")

	email := sqlc.MarketingCampaign{Channel: ChannelEmail, Subject: "Hi", Body: "Come by"}
	if p := Problems(email, none, Readiness{SMS: down, WhatsApp: down}); len(p) != 0 {
		t.Fatalf("ready email flagged: %v", p)
	}
	if p := Problems(sqlc.MarketingCampaign{Channel: ChannelEmail, Body: "x"}, none, Readiness{Email: down}); len(p) != 2 {
		t.Fatalf("want subject + provider problems, got %v", p)
	}

	sms := sqlc.MarketingCampaign{Channel: ChannelSMS, Body: "Deal today"}
	if p := Problems(sms, none, Readiness{}); len(p) != 1 || !strings.Contains(p[0], "plan") {
		t.Fatalf("sms without plan feature: %v", p)
	}
	if p := Problems(sms, all, Readiness{SMS: down}); len(p) != 1 || !strings.Contains(p[0], "connected") {
		t.Fatalf("sms without provider: %v", p)
	}
	if p := Problems(sms, all, Readiness{}); len(p) != 0 {
		t.Fatalf("ready sms flagged: %v", p)
	}
	long := sqlc.MarketingCampaign{Channel: ChannelSMS, Body: strings.Repeat("a", maxSMSLen+1)}
	if p := Problems(long, all, Readiness{}); len(p) != 1 {
		t.Fatalf("long sms: %v", p)
	}
	wa := sqlc.MarketingCampaign{Channel: ChannelWhatsApp, Body: "Hi"}
	if p := Problems(wa, all, Readiness{WhatsApp: down}); len(p) != 2 {
		t.Fatalf("whatsapp without template or provider: %v", p)
	}
	wa.TemplateName = "promo_v1"
	if p := Problems(wa, all, Readiness{}); len(p) != 0 {
		t.Fatalf("ready whatsapp flagged: %v", p)
	}
}

func TestOutcome(t *testing.T) {
	cases := []struct {
		sent, failed, total int32
		want                string
	}{
		{10, 0, 10, StatusCompleted},
		{0, 5, 5, StatusFailed},
		{7, 3, 10, StatusPartiallyFailed},
		{0, 0, 0, StatusCompleted},
	}
	for _, c := range cases {
		got, _ := outcome(sqlc.MarketingCampaign{SentCount: c.sent, FailedCount: c.failed, TotalCount: c.total})
		if got != c.want {
			t.Errorf("sent=%d failed=%d: got %s want %s", c.sent, c.failed, got, c.want)
		}
	}
}

func TestCleanTargetPathAndCode(t *testing.T) {
	for _, ok := range []string{"", "/", "/menu", "/book?service=cut"} {
		if _, err := cleanTargetPath(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"menu", "//evil.com", "https://evil.com", "/a b", strings.Repeat("/x", maxPathLen)} {
		if _, err := cleanTargetPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	code, err := newCode()
	if err != nil || len(code) != 10 || strings.ToLower(code) != code {
		t.Fatalf("code %q err %v", code, err)
	}
}

func TestBackoffGrows(t *testing.T) {
	if !(backoffFor(1) < backoffFor(2) && backoffFor(2) < backoffFor(3)) {
		t.Fatal("backoff must grow")
	}
}
