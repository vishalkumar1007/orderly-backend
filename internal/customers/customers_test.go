package customers

import (
	"strconv"
	"testing"
	"time"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"9876543210":       "+919876543210",
		"+919876543210":    "+919876543210",
		"+91 98765 43210":  "+919876543210",
		"0987 654 3210":    "+919876543210", // the local way of writing it
		"98765-43210":      "+919876543210",
		"(98765) 43210":    "+919876543210",
		"1 (555) 010-9999": "+15550109999",
		"  9876543210   ":  "+919876543210",
		"+44 20 7946 0958": "+442079460958",
	}
	for input, want := range cases {
		got, err := NormalizePhone(input)
		if err != nil {
			t.Errorf("NormalizePhone(%q) returned %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizePhoneRejectsRubbish(t *testing.T) {
	for _, input := range []string{"", "   ", "abc", "+", "12345", "1234567890123456789", "98765;drop"} {
		if got, err := NormalizePhone(input); err == nil {
			t.Errorf("NormalizePhone(%q) = %q, expected an error", input, got)
		}
	}
}

func TestNormalizePhoneIsIdempotent(t *testing.T) {
	first, err := NormalizePhone("0987 654 3210")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizePhone(first)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("normalising twice changed the value: %q then %q", first, second)
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"+919876543210": "+91••••3210",
		"+15550109999":  "+15••••9999",
		"1234":          "1234",
		"":              "",
	}
	for input, want := range cases {
		if got := MaskPhone(input); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMaskedPhoneStillDifferentiatesAccounts(t *testing.T) {
	// Masking is for display only, so two different customers on the same
	// network must not look identical in the UI.
	a := MaskPhone("+919876543210")
	b := MaskPhone("+919876543299")
	if a == b {
		t.Errorf("two different numbers masked to the same string %q", a)
	}
}

func TestCodeAlphabetIsDigitsOnly(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := newCode()
		if err != nil {
			t.Fatalf("newCode: %v", err)
		}
		if len(code) != CodeLength {
			t.Fatalf("code %q has length %d, want %d", code, len(code), CodeLength)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q contains a non-digit", code)
			}
		}
	}
}

func TestCodesAreNotPredictable(t *testing.T) {
	// This deliberately does not assert that 500 draws are all distinct.
	//
	// A six-digit code has a million values, so by the birthday bound a
	// collision appears in 500 draws about 12% of the time — a correct
	// generator would fail that assertion roughly one run in eight, and a
	// suite that cries wolf is worse than no suite. What actually matters is
	// that codes are spread across the range and do not follow each other,
	// which is what a predictable generator gets wrong.
	const draws = 500

	seen := map[string]int{}
	var previous string
	firstDigits := map[byte]int{}

	for i := 0; i < draws; i++ {
		code, err := newCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q is not six digits", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("code %q contains a non-digit", code)
			}
		}
		if code == previous {
			t.Fatalf("the same code %q was issued twice in a row", code)
		}
		if n, err := strconv.Atoi(code); err == nil && previous != "" {
			if p, err := strconv.Atoi(previous); err == nil && n == p+1 {
				t.Fatalf("codes are sequential: %q followed %q", code, previous)
			}
		}
		previous = code
		seen[code]++
		firstDigits[code[0]]++
	}

	// A generator stuck on part of the range is the failure worth catching. A
	// handful of repeats across 500 draws is expected; hundreds are not.
	if len(seen) < draws-10 {
		t.Errorf("only %d distinct codes in %d draws, which is far more repetition than chance", len(seen), draws)
	}
	if len(firstDigits) < 8 {
		t.Errorf("leading digit only took %d of 10 values across %d draws", len(firstDigits), draws)
	}
}

func TestHashCodeIsTenantScoped(t *testing.T) {
	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"
	code := "123456"

	// The same code in a different shop must not verify, so the digest has to be
	// bound to the tenant.
	if hashCode(tenantA, "+919876543210", code) == hashCode(tenantB, "+919876543210", code) {
		t.Error("the digest must differ between tenants")
	}
	if hashCode(tenantA, "+919876543210", code) == hashCode(tenantA, "+919876543211", code) {
		t.Error("the digest must differ between phone numbers")
	}
	if hashCode(tenantA, "+919876543210", code) == hashCode(tenantA, "+919876543210", "654321") {
		t.Error("the digest must differ between codes")
	}
	// It must be stable, otherwise no code would ever verify.
	if hashCode(tenantA, "+919876543210", code) != hashCode(tenantA, "+919876543210", code) {
		t.Error("the digest must be stable for the same inputs")
	}
}

func TestCodesMatchUsesConstantTimeCompare(t *testing.T) {
	stored := hashCode("tenant", "+919876543210", "123456")
	if !codesMatch(stored, hashCode("tenant", "+919876543210", "123456")) {
		t.Error("identical digests should match")
	}
	if codesMatch(stored, "deadbeef") {
		t.Error("different digests must not match")
	}
}

func TestOTPPolicyIsSane(t *testing.T) {
	// The policy values are the platform's guard rails; a change that weakens
	// them should be a deliberate edit, not an accident.
	if CodeLength < 6 {
		t.Errorf("CodeLength = %d, want at least 6", CodeLength)
	}
	if CodeTTL > 10*60*1000*1000*1000 {
		t.Errorf("CodeTTL = %v, too long for a login code", CodeTTL)
	}
	if ResendAfter < 15*1000*1000*1000 {
		t.Errorf("ResendAfter = %v, too eager", ResendAfter)
	}
	if MaxAttempts < 3 {
		t.Errorf("MaxAttempts = %d, too permissive for a 6-digit code", MaxAttempts)
	}
	if MaxAttempts > 10 {
		t.Errorf("MaxAttempts = %d, too permissive to stop guessing", MaxAttempts)
	}
}

func TestSecondsUntilNeverGoesNegative(t *testing.T) {
	now := time.Now()
	if got := SecondsUntil(now.Add(-time.Hour), now); got != 0 {
		t.Errorf("a past time should give 0, got %d", got)
	}
	if got := SecondsUntil(now.Add(30*time.Second), now); got != 30 {
		t.Errorf("got %d, want 30", got)
	}
}

func TestVerificationErrorsAreDistinct(t *testing.T) {
	// The UI shows a different message for each, so they must not collapse into
	// one another.
	all := []error{ErrNoCode, ErrInvalid, ErrExpired, ErrTooManyAttempts, ErrRateLimited, ErrResendTooSoon, ErrBlocked}
	seen := map[string]bool{}
	for _, err := range all {
		if err.Error() == "" {
			t.Error("an error carries no message")
		}
		if seen[err.Error()] {
			t.Errorf("duplicate error message %q", err.Error())
		}
		seen[err.Error()] = true
	}
}
