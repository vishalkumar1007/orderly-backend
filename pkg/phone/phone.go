// Package phone normalises and masks phone numbers.
//
// Checkout, guest order tracking and phone login must all agree on what
// "+91 98765 43210" means, or a customer who signs in will not find the order
// they placed as a guest. Keeping one implementation means there is exactly one
// definition of "the same number".
package phone

import (
	"errors"
	"strings"
	"unicode"
)

// ErrInvalid means the input cannot be read as a phone number.
var ErrInvalid = errors.New("enter a valid phone number")

// DefaultCountry is the country code assumed for a bare local number. It is the
// platform's primary market and is overridable per deployment.
const DefaultCountry = "91"

// minDigits and maxDigits are the E.164 bounds.
const (
	minDigits = 8
	maxDigits = 15
)

// Normalize reduces a user-typed number to a canonical +<digits> form.
//
// It accepts the shapes people actually type: spaces, dashes, brackets, dots and
// a leading country code with or without "+". A bare 10-digit local number gains
// the default country code; an 11-digit number starting with a zero — how this
// market writes a mobile number ("0987 654 3210") — loses the zero first, since
// keeping it would store an 11-digit number no gateway would recognise.
func Normalize(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == '/':
			// separator, ignore
		default:
			return "", ErrInvalid
		}
	}
	out := b.String()
	if out == "" {
		return "", ErrInvalid
	}
	if !strings.HasPrefix(out, "+") {
		if len(out) == 11 && out[0] == '0' {
			out = out[1:]
		}
		if len(out) == 10 {
			return "+" + DefaultCountry + out, nil
		}
		out = "+" + out
	}
	digits := out[1:]
	if len(digits) < minDigits || len(digits) > maxDigits {
		return "", ErrInvalid
	}
	return out, nil
}

// IsValid reports whether a number can be normalised.
func IsValid(raw string) bool {
	_, err := Normalize(raw)
	return err == nil
}

// Mask renders a number for display, hiding everything but the country code and
// the last four digits: "+91 98••••3210".
func Mask(number string) string {
	if len(number) < 8 {
		return number
	}
	head, tail := number[:3], number[len(number)-4:]
	return head + "••••" + tail
}

// Same reports whether two numbers refer to the same subscriber.
func Same(a, b string) bool {
	na, err1 := Normalize(a)
	nb, err2 := Normalize(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return na == nb
}
