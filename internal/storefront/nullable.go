package storefront

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// The helpers here exist to keep the admin handlers free of pgtype noise: a
// "field was not supplied" is a NULL parameter, which is how every PUT in this
// package becomes a partial update rather than a full overwrite.

type nullableText = pgtype.Text

func nullText() pgtype.Text { return pgtype.Text{} }

// boolPtr converts an optional JSON boolean into a nullable column value.
func boolPtr(v *bool) pgtype.Bool {
	if v == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *v, Valid: true}
}

// int32Ptr converts an optional JSON number into a nullable column value.
func int32Ptr(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func numericOrNil(value, fallback float64) pgtype.Numeric {
	if value == fallback {
		return pgtype.Numeric{}
	}
	n, err := pgutil.NumericFromFloat(value)
	if err != nil {
		return pgtype.Numeric{}
	}
	return n
}
