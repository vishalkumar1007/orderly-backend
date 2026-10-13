package tenantctx

import (
	"testing"
	"time"
)

func TestEvaluateLicense(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	cases := []struct {
		name      string
		status    string
		expires   *time.Time
		grace     int
		wantState string
		wantBlock string
	}{
		{"never expires", "ACTIVE", nil, 3, LicenseActive, ""},
		{"not yet expired", "ACTIVE", at(day), 3, LicenseActive, ""},
		{"just expired, in grace", "ACTIVE", at(-day), 3, LicenseGrace, ""},
		{"last moment of grace", "ACTIVE", at(-3*day + time.Minute), 3, LicenseGrace, ""},
		{"grace over", "ACTIVE", at(-3 * day), 3, "", LicenseExpired},
		{"expired, no grace", "ACTIVE", at(-time.Minute), 0, "", LicenseExpired},
		{"suspended", "SUSPENDED", nil, 3, "", LicenseSuspended},
		{"revoked", "REVOKED", nil, 3, "", LicenseRevoked},
		{"stored expired", "EXPIRED", nil, 3, "", LicenseExpired},
		{"draft", "DRAFT", nil, 3, "", LicenseDraft},
		{"no license row", "", nil, 0, "", LicenseDraft},
	}
	for _, c := range cases {
		state, graceEnds, blocked := EvaluateLicense(c.status, c.expires, c.grace, now)
		if state != c.wantState || blocked != c.wantBlock {
			t.Errorf("%s: got state=%q blocked=%q, want state=%q blocked=%q",
				c.name, state, blocked, c.wantState, c.wantBlock)
		}
		if (state == LicenseGrace) != (graceEnds != nil) {
			t.Errorf("%s: grace_ends_at must be set exactly when state is GRACE", c.name)
		}
	}
}
