package auth

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestGenerateRecoveryCodesAreWellFormedAndUnique(t *testing.T) {
	codes, err := generateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		t.Fatalf("generateRecoveryCodes: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), recoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 9 || c[4] != '-' {
			t.Errorf("code %q is not in the xxxx-xxxx shape", c)
		}
		if seen[c] {
			t.Errorf("duplicate recovery code generated: %q", c)
		}
		seen[c] = true
	}
}

func TestRecoveryCodeHashRoundTripsRegardlessOfCaseOrSpacing(t *testing.T) {
	codes, err := generateRecoveryCodes(1)
	if err != nil {
		t.Fatalf("generateRecoveryCodes: %v", err)
	}
	code := codes[0]
	stored := hashToken(normalizeRecoveryCode(code))

	// A user may paste it back lowercase or with stray whitespace — the
	// comparison at verify time must still match.
	typed := "  " + code + "  "
	if hashToken(normalizeRecoveryCode(typed)) != stored {
		t.Error("recovery code hash did not match after trimming whitespace")
	}
	lower := "  " + toLowerASCII(code) + "  "
	if hashToken(normalizeRecoveryCode(lower)) != stored {
		t.Error("recovery code hash did not match after lowercasing")
	}

	if hashToken(normalizeRecoveryCode("0000-0000")) == stored {
		t.Error("an unrelated code must not hash to the same value")
	}
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func TestTOTPValidatesTheRightCodeAndRejectsAWrongOne(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Orderly", AccountName: "test@example.com"})
	if err != nil {
		t.Fatalf("totp.Generate: %v", err)
	}
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	if err != nil {
		t.Fatalf("totp.GenerateCode: %v", err)
	}
	if !totp.Validate(code, key.Secret()) {
		t.Error("the freshly generated code did not validate against its own secret")
	}
	if totp.Validate("000000", key.Secret()) {
		// Astronomically unlikely to collide; if this ever flakes, the RNG is
		// the story, not this test.
		t.Error("an arbitrary wrong code validated — something is off")
	}
	if totp.Validate(code, "") {
		t.Error("a code validated against an empty secret")
	}
}
