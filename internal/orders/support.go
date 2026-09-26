package orders

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/phone"
)

func trim(s string) string { return strings.TrimSpace(s) }

func upperTrim(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func utf8Len(s string) int { return utf8.RuneCountInString(s) }

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// looksLikeEmail is a deliberately permissive check: the only thing that
// matters is that a typo is caught before the order is written, and that
// delivery is never blocked on address validation in this build.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.Contains(s, " ") {
		return false
	}
	domain := s[at+1:]
	dot := strings.LastIndex(domain, ".")
	return dot > 0 && dot < len(domain)-1
}

// normalisePhoneInput canonicalises a phone number so guest checkout, guest
// tracking and phone login all agree on what a given number is. The rules live
// in pkg/phone because getting them subtly different in two places is how a
// customer ends up unable to find their own order.
func normalisePhoneInput(raw string) string {
	out, err := phone.Normalize(raw)
	if err != nil {
		return ""
	}
	return out
}

func numericOf(v float64) pgtype.Numeric {
	n, err := pgutil.NumericFromFloat(money(v))
	if err != nil {
		return pgtype.Numeric{}
	}
	return n
}

func numericUUID(id string) pgtype.UUID {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgutil.UUID(parsed)
}

// timestamptzOf converts a time for a nullable timestamptz column.
func timestamptzOf(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// optionalTime converts a time pointer, or nil, for a nullable column.
func optionalTime(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// isUniqueViolation reports a Postgres unique-constraint failure. It is how a
// duplicate order or payment is detected at the database level rather than by
// a racy read-then-write in Go.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
