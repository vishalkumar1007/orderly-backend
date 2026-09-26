package payments

import (
	"strings"
	"unicode"
)

// Small shared helpers, kept here so the payment flow does not reach into the
// orders package for string handling.

func trim(s string) string { return strings.TrimSpace(s) }

func upperTrim(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// normalisePhone matches the normalisation used at checkout and by phone login,
// so the same number always resolves to the same customer.
func normalisePhone(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if !strings.HasPrefix(out, "+") {
		if len(out) == 10 {
			return "+91" + out
		}
		return "+" + out
	}
	return out
}
