package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// Email OTP policy. Shape mirrors internal/customers/otp.go's phone-OTP
// pattern exactly — the only rate-limiting precedent in this codebase —
// scoped by user_id instead of tenant_id+phone.
const (
	emailOTPCodeLength  = 6
	emailOTPTTL         = 10 * time.Minute
	emailOTPResendAfter = 30 * time.Second
)

var (
	ErrEmailOTPUnavailable     = errors.New("email is not configured for this account")
	ErrEmailOTPNoCode          = errors.New("no code was requested — request one first")
	ErrEmailOTPExpired         = errors.New("that code has expired — send a new one")
	ErrEmailOTPTooManyAttempts = errors.New("too many incorrect attempts — request a new code")
	ErrEmailOTPRateLimited     = errors.New("too many codes requested — try again in a few minutes")
	ErrEmailOTPResendTooSoon   = errors.New("please wait before requesting another code")
)

var emailOTPAlphabet = []byte("0123456789")

func newEmailOTPCode() (string, error) {
	buf := make([]byte, emailOTPCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = emailOTPAlphabet[int(buf[i])%len(emailOTPAlphabet)]
	}
	return string(buf), nil
}

func hashEmailOTP(userID, code string) string {
	sum := sha256.Sum256([]byte(userID + ":" + code))
	return hex.EncodeToString(sum[:])
}

func emailOTPCodesMatch(stored, candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1
}

func (s *Service) activeEmailOTP(ctx context.Context, userID uuid.UUID) (sqlc.MfaEmailOtpCode, bool, error) {
	row, err := s.q.GetActiveMfaEmailOtpCode(ctx, pgutil.UUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.MfaEmailOtpCode{}, false, nil
		}
		return sqlc.MfaEmailOtpCode{}, false, err
	}
	return row, true, nil
}

// RequestEmailOTP issues and emails a fresh code, provided Email OTP is
// allowed by this account's policy and a working, permitted email provider
// resolves for it (configsvc.Notifications.Send/SendPlatform answers that
// question itself — no separate availability precheck needed).
func (s *Service) RequestEmailOTP(ctx context.Context, user sqlc.User) error {
	if err := s.checkMethodAllowed(ctx, user, MethodEmailOTP); err != nil {
		return err
	}
	userID := uuid.UUID(user.ID.Bytes)
	now := time.Now()
	active, found, err := s.activeEmailOTP(ctx, userID)
	if err != nil {
		return err
	}
	if found && now.Sub(active.CreatedAt.Time) < emailOTPResendAfter {
		return ErrEmailOTPResendTooSoon
	}
	code, err := newEmailOTPCode()
	if err != nil {
		return err
	}
	if err := s.q.InvalidateMfaEmailOtpCodes(ctx, pgutil.UUID(userID)); err != nil {
		return err
	}
	if _, err := s.q.CreateMfaEmailOtpCode(ctx, sqlc.CreateMfaEmailOtpCodeParams{
		UserID: pgutil.UUID(userID), CodeHash: hashEmailOTP(userID.String(), code),
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(emailOTPTTL), Valid: true},
	}); err != nil {
		return err
	}
	return s.sendEmailOTPMail(ctx, user, code)
}

// verifyEmailOTP checks a submitted code against the stored digest and
// consumes it on success. The attempt counter lives in the database and is
// incremented on every failure — including the expired case — so a client
// cannot reset it by reloading the page.
func (s *Service) verifyEmailOTP(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	row, found, err := s.activeEmailOTP(ctx, userID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrEmailOTPNoCode
	}
	if row.Attempts >= row.MaxAttempts {
		return false, ErrEmailOTPTooManyAttempts
	}
	if time.Now().After(row.ExpiresAt.Time) {
		_, _ = s.q.IncrementMfaEmailOtpAttempts(ctx, row.ID)
		return false, ErrEmailOTPExpired
	}
	if !emailOTPCodesMatch(row.CodeHash, hashEmailOTP(userID.String(), code)) {
		updated, err := s.q.IncrementMfaEmailOtpAttempts(ctx, row.ID)
		if err != nil {
			return false, err
		}
		if updated.Attempts >= updated.MaxAttempts {
			return false, ErrEmailOTPTooManyAttempts
		}
		return false, nil
	}
	if _, err := s.q.ConsumeMfaEmailOtpCode(ctx, row.ID); err != nil {
		return false, err
	}
	_ = s.q.TouchUserMfaMethod(ctx, sqlc.TouchUserMfaMethodParams{UserID: pgutil.UUID(userID), Method: MethodEmailOTP})
	return true, nil
}

func (s *Service) sendEmailOTPMail(ctx context.Context, user sqlc.User, code string) error {
	if s.notifications == nil {
		return ErrEmailOTPUnavailable
	}
	msg := configsvc.MailMessage{
		To:      []string{user.Email},
		Subject: "Your Orderly verification code",
		Body: fmt.Sprintf(
			"Your verification code is %s. It expires in %d minutes.\r\n\r\nIf you did not request this, you can ignore this message.\r\n",
			code, int(emailOTPTTL.Minutes()),
		),
	}
	var err error
	if user.TenantID.Valid {
		tid := uuid.UUID(user.TenantID.Bytes)
		err = s.notifications.Send(ctx, &tid, msg)
	} else {
		err = s.notifications.SendPlatform(ctx, msg)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", ErrEmailOTPUnavailable, configsvc.SafeErrorMessage(err))
	}
	return nil
}

// emailOTPUnavailableMessage strips the wrapped sentinel's own text back off
// so the HTTP response shows only the resolver's reason, not a doubled
// "email is not configured: email is not configured: ...".
func emailOTPUnavailableMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), ErrEmailOTPUnavailable.Error()+": ")
	if msg == "" || msg == err.Error() {
		return "Email is not configured for this account yet"
	}
	return "Email is not configured for this account yet: " + msg
}

/* ------------------------------------------------------------------ *
 * Self-service HTTP handlers: enroll Email OTP as an additional method
 * ------------------------------------------------------------------ */

func (s *Service) HandleMFAEmailSend(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	user, err := s.q.GetUserByID(r.Context(), pgutil.UUID(actor.ID))
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load account")
		return
	}
	if err := s.RequestEmailOTP(r.Context(), user); err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

type mfaEmailConfirmRequest struct {
	Code string `json:"code"`
}

func (s *Service) HandleMFAEmailConfirm(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaEmailConfirmRequest
	if err := decodeJSON(r, &req); err != nil || req.Code == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "code required")
		return
	}
	matched, err := s.verifyEmailOTP(r.Context(), actor.ID, req.Code)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	if !matched {
		writeMFAError(w, ErrInvalidMFACode)
		return
	}
	hadBefore, err := s.countEnrolledMethods(r.Context(), actor.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	if _, err := s.q.UpsertUserMfaMethod(r.Context(), sqlc.UpsertUserMfaMethodParams{
		UserID: pgutil.UUID(actor.ID), Method: MethodEmailOTP, SecretEnc: pgtype.Text{},
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	s.syncMfaEnabledCache(r.Context(), actor.ID)
	codes, err := s.issueRecoveryCodesIfFirstMethod(r.Context(), actor.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	s.notifyMethodEnrolled(r.Context(), actor, MethodEmailOTP, hadBefore > 0)
	response.JSON(w, http.StatusOK, MFAConfirmResponse{RecoveryCodes: codes})
}
