package pgutil

import (
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func UUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func ParseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

func UUIDString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func UUIDPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

func NullUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return UUID(*id)
}

func Text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func NullText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return Text(*s)
}

func NumericFromFloat(v float64) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if err := n.Scan(fmt.Sprintf("%.2f", v)); err != nil {
		return pgtype.Numeric{}, err
	}
	return n, nil
}

func NumericToFloat(n pgtype.Numeric) float64 {
	if !n.Valid {
		return 0
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		// Fallback via big.Int / Exp
		if n.Int == nil {
			return 0
		}
		rat := new(big.Rat).SetInt(n.Int)
		if n.Exp < 0 {
			den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)
			rat.Quo(rat, new(big.Rat).SetInt(den))
		} else if n.Exp > 0 {
			mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)
			rat.Mul(rat, new(big.Rat).SetInt(mul))
		}
		f64, _ := rat.Float64()
		return f64
	}
	return f.Float64
}

func NumericString(n pgtype.Numeric) string {
	return fmt.Sprintf("%.2f", NumericToFloat(n))
}
