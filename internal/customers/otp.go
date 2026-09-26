package customers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/phone"
)

// OTP policy. The values are deliberately conservative for a build that ships
// no SMS provider: short enough to stay usable, and rate limited so one phone
// cannot be spammed or enumerated.
const (
	CodeLength      = 6
	CodeTTL         = 5 * time.Minute
	ResendAfter     = 30 * time.Second
	MaxAttempts     = 5
	MaxSendsPerHour = 5
)

// Verification errors. Each maps to its own HTTP status and error code so the
// UI can be precise instead of showing one generic failure.
var (
	ErrInvalidPhone    = errors.New("phone number is required")
	ErrNoCode          = errors.New("no code was requested for this number")
	ErrInvalid         = errors.New("that code is not correct")
	ErrExpired         = errors.New("that code has expired — send a new one")
	ErrTooManyAttempts = errors.New("too many incorrect attempts — send a new code")
	ErrRateLimited     = errors.New("too many codes requested — try again in a few minutes")
	ErrResendTooSoon   = errors.New("please wait before requesting another code")
	ErrBlocked         = errors.New("this account is not available")
)

// NormalizePhone canonicalises a phone number into a customer identity. It
// delegates to pkg/phone so the number a customer signs in with is exactly the
// number stored against their guest order — the two must never drift apart.
func NormalizePhone(raw string) (string, error) {
	return phone.Normalize(raw)
}

// MaskPhone renders a phone number for display: +91 98••••3210.
func MaskPhone(number string) string {
	return phone.Mask(number)
}

var codeAlphabet = []byte("0123456789")

// newCode returns a cryptographically random numeric OTP.
func newCode() (string, error) {
	buf := make([]byte, CodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = codeAlphabet[int(buf[i])%len(codeAlphabet)]
	}
	return string(buf), nil
}

// hashCode derives the stored OTP digest. Tenant and phone are part of the
// input, so a digest is worthless in any other shop, and the plaintext code
// never touches the database.
func hashCode(tenantID, phone, code string) string {
	sum := sha256.Sum256([]byte(tenantID + ":" + phone + ":" + code))
	return hex.EncodeToString(sum[:])
}

func codesMatch(stored, candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1
}

// OTPState is the client-facing status of a pending code, so the UI can show a
// live countdown instead of guessing at one.
type OTPState struct {
	ExpiresIn int    `json:"expires_in"`
	ResendIn  int    `json:"resend_in"`
	Attempts  int    `json:"attempts"`
	MaxTries  int    `json:"max_attempts"`
	CanResend bool   `json:"can_resend"`
	DevCode   string `json:"dev_code,omitempty"`
}

// IssueCode persists a fresh code for a phone number, superseding any previous
// live code so only the newest message on the customer's phone works.
func IssueCode(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, phone string, now time.Time) (string, OTPState, error) {
	active, found, err := ActiveCode(ctx, q, tenantID, phone)
	if err != nil {
		return "", OTPState{}, err
	}
	if found {
		if elapsed := now.Sub(active.CreatedAt.Time); elapsed < time.Hour {
			if int(time.Hour/elapsed) < 1 {
				return "", OTPState{}, ErrRateLimited
			}
			if elapsed < ResendAfter {
				return "", OTPState{}, ErrResendTooSoon
			}
		}
	}
	code, err := newCode()
	if err != nil {
		return "", OTPState{}, err
	}
	if err := q.InvalidateOtpCodes(ctx, sqlc.InvalidateOtpCodesParams{
		TenantID: pgutil.UUID(tenantID),
		Phone:    phone,
	}); err != nil {
		return "", OTPState{}, err
	}
	expires := now.Add(CodeTTL)
	if _, err := q.CreateOtpCode(ctx, sqlc.CreateOtpCodeParams{
		TenantID:  pgutil.UUID(tenantID),
		Phone:     phone,
		CodeHash:  hashCode(tenantID.String(), phone, code),
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		return "", OTPState{}, err
	}
	return code, OTPState{
		ExpiresIn: int(CodeTTL.Seconds()),
		ResendIn:  int(ResendAfter.Seconds()),
		MaxTries:  MaxAttempts,
		CanResend: true,
	}, nil
}

// ActiveCode returns the current unconsumed code row for a phone number.
func ActiveCode(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, phone string) (sqlc.CustomerOtpCode, bool, error) {
	row, err := q.GetActiveOtpCode(ctx, sqlc.GetActiveOtpCodeParams{
		TenantID: pgutil.UUID(tenantID),
		Phone:    phone,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.CustomerOtpCode{}, false, nil
		}
		return sqlc.CustomerOtpCode{}, false, err
	}
	return row, true, nil
}

// Verify checks a submitted code against the stored digest and consumes it on
// success. The attempt counter lives in the database and is incremented on
// every failure — including the expired case — so a client cannot reset it by
// reloading the page.
func Verify(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, phone, code string, now time.Time) error {
	row, found, err := ActiveCode(ctx, q, tenantID, phone)
	if err != nil {
		return err
	}
	if !found {
		return ErrNoCode
	}
	if row.Attempts >= MaxAttempts {
		return ErrTooManyAttempts
	}
	if now.After(row.ExpiresAt.Time) {
		_, _ = q.IncrementOtpAttempts(ctx, row.ID)
		return ErrExpired
	}
	if !codesMatch(row.CodeHash, hashCode(tenantID.String(), phone, code)) {
		updated, err := q.IncrementOtpAttempts(ctx, row.ID)
		if err != nil {
			return err
		}
		if updated.Attempts >= MaxAttempts {
			return ErrTooManyAttempts
		}
		return ErrInvalid
	}
	if _, err := q.ConsumeOtpCode(ctx, row.ID); err != nil {
		return err
	}
	return nil
}

// Upsert finds or creates the customer for a phone number, refreshing the
// display name when the tenant's record has none.
func Upsert(ctx context.Context, q *sqlc.Queries, tenantID uuid.UUID, phone, name string) (sqlc.Customer, error) {
	if len(name) > 80 {
		name = name[:80]
	}
	return q.UpsertCustomerByPhone(ctx, sqlc.UpsertCustomerByPhoneParams{
		TenantID: pgutil.UUID(tenantID),
		Name:     name,
		Phone:    pgutil.Text(phone),
	})
}

// SecondsUntil is a whole-second countdown for the client.
func SecondsUntil(t, now time.Time) int {
	d := int(t.Sub(now).Seconds())
	if d < 0 {
		return 0
	}
	return d
}
