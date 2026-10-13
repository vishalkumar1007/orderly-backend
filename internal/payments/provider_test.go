package payments

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestSandboxSignVerifyRoundTrip(t *testing.T) {
	s := NewSandbox("secret")
	ev := Event{ID: "evt_1", Type: EventPaid, ProviderOrderID: "sbx_1", AmountMinor: 12500}
	body, hdr := s.Sign(ev)
	got, err := s.VerifyWebhook(hdr, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != ev.ID || got.Type != ev.Type || got.ProviderOrderID != ev.ProviderOrderID || got.AmountMinor != ev.AmountMinor {
		t.Errorf("round trip changed the event: %+v", got)
	}
}

func TestSandboxRejectsAnythingUnproven(t *testing.T) {
	s := NewSandbox("secret")
	body, hdr := s.Sign(Event{ID: "evt_1", Type: EventPaid, ProviderOrderID: "sbx_1", AmountMinor: 100})

	other := NewSandbox("another-secret")
	stale := NewSandbox("secret")
	stale.now = func() time.Time { return time.Now().Add(-time.Hour) }
	staleBody, staleHdr := stale.Sign(Event{ID: "evt_1", Type: EventPaid, ProviderOrderID: "sbx_1"})

	cases := map[string]struct {
		h    http.Header
		body []byte
	}{
		"missing header":  {http.Header{}, body},
		"garbage header":  {http.Header{SignatureHeader: {"nope"}}, body},
		"tampered body":   {hdr, append([]byte(nil), append(body[:len(body)-1], []byte(`,"x":1}`)...)...)},
		"other secret":    {func() http.Header { _, h := other.Sign(Event{ID: "evt_1"}); return h }(), body},
		"outside window":  {staleHdr, staleBody},
		"missing sig":     {http.Header{SignatureHeader: {"t=1"}}, body},
		"bad timestamp":   {http.Header{SignatureHeader: {"t=abc,v1=00"}}, body},
		"empty event ids": {func() http.Header { b, h := s.Sign(Event{}); _ = b; return h }(), []byte(`{}`)},
	}
	for name, c := range cases {
		if _, err := s.VerifyWebhook(c.h, c.body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
}

func TestMinorUnitsRoundsOnce(t *testing.T) {
	for in, want := range map[float64]int64{0: 0, 12.5: 1250, 0.07: 7, 19.999: 2000, 1.005: 100} {
		// 1.005 is 1.00499999... in binary; documents that the helper rounds the
		// float it is given rather than guessing the decimal the user meant.
		if got := minorUnits(in); got != want {
			t.Errorf("minorUnits(%v) = %d, want %d", in, got, want)
		}
	}
}
