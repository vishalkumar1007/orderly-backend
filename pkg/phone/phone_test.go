package phone

import "testing"

func TestNormalizeAcceptsRealWorldInput(t *testing.T) {
	cases := map[string]string{
		"9876543210":       "+919876543210",
		"+919876543210":    "+919876543210",
		"+91 98765 43210":  "+919876543210",
		"0987 654 3210":    "+919876543210", // the local way of writing a mobile number
		"98765-43210":      "+919876543210",
		"(98765) 43210":    "+919876543210",
		"98765.43210":      "+919876543210",
		"  9876543210  ":   "+919876543210",
		"1 (555) 010-9999": "+15550109999",
		"+44 20 7946 0958": "+442079460958",
		// An 11-digit number with a leading zero is this market's way of
		// writing a mobile number, so the zero is dropped and the default
		// country code applied. A foreign number is typed with its own code.
		"020 7946 0958": "+912079460958",
	}
	for input, want := range cases {
		got, err := Normalize(input)
		if err != nil {
			t.Errorf("Normalize(%q) returned %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeRejectsRubbish(t *testing.T) {
	for _, input := range []string{
		"", "   ", "abc", "+", "12345", "1234567890123456789",
		"98765;drop", "98765\n12345", "<script>", "98765@12345",
	} {
		if got, err := Normalize(input); err == nil {
			t.Errorf("Normalize(%q) = %q, expected an error", input, got)
		}
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	// Checkout and tracking both normalise, sometimes twice for one value. A
	// second pass must not change the result.
	for _, input := range []string{"0987 654 3210", "+91 98765 43210", "9876543210", "+1 (555) 010-9999"} {
		once, err := Normalize(input)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", input, err)
		}
		twice, err := Normalize(once)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", once, err)
		}
		if once != twice {
			t.Errorf("normalising %q twice changed it: %q then %q", input, once, twice)
		}
	}
}

func TestNormalizeStripsASecondPlus(t *testing.T) {
	// A second "+" is not a digit, a separator, or the leading sign, so the
	// number is rejected rather than silently concatenated.
	if _, err := Normalize("+91+9876543210"); err == nil {
		t.Error("a second + should be rejected")
	}
}

func TestMaskHidesTheMiddle(t *testing.T) {
	cases := map[string]string{
		"+919876543210": "+91••••3210",
		"+15550109999":  "+15••••9999",
		"1234":          "1234",
		"":              "",
	}
	for input, want := range cases {
		if got := Mask(input); got != want {
			t.Errorf("Mask(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMaskDoesNotCollapseDistinctNumbers(t *testing.T) {
	a := Mask("+919876543210")
	b := Mask("+919876543299")
	if a == b {
		t.Errorf("two different numbers masked to the same string %q", a)
	}
}

func TestSame(t *testing.T) {
	if !Same("0987 654 3210", "+91 98765 43210") {
		t.Error("the same number written two ways should compare equal")
	}
	if Same("9876543210", "9876543211") {
		t.Error("different numbers must not compare equal")
	}
	if Same("abc", "abc") {
		t.Error("two invalid numbers must not compare equal")
	}
}

func TestIsValid(t *testing.T) {
	if !IsValid("9876543210") {
		t.Error("9876543210 should be valid")
	}
	if IsValid("nope") {
		t.Error("nope should be invalid")
	}
}

func TestE164LengthBoundsAreEnforced(t *testing.T) {
	// E.164 allows at most 15 digits and a real number needs at least 8.
	if _, err := Normalize("+1234567"); err == nil {
		t.Error("7 digits should be too short")
	}
	if _, err := Normalize("+1234567890123456"); err == nil {
		t.Error("16 digits should be too long")
	}
}
