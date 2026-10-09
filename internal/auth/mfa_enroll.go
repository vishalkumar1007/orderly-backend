package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp/totp"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

/* ------------------------------------------------------------------ *
 * Forced enrollment: a user with zero enrolled methods, under a policy that
 * now requires one, past its grace period (MFAEnrollRequiredError). These
 * four endpoints are unauthenticated — the enrollment token itself is the
 * credential, the same shape /auth/mfa/verify already uses for a login
 * challenge. Each confirm endpoint completes the login on success, issuing
 * a real token pair with MFAVerified true, since a factor was just proven.
 * ------------------------------------------------------------------ */

// userFromMFAToken resolves the account behind a purpose-gated token.
func (s *Service) userFromMFAToken(ctx context.Context, tokenStr, purpose string) (sqlc.User, *Claims, error) {
	claims, err := s.parseMFAToken(tokenStr, purpose)
	if err != nil {
		return sqlc.User{}, nil, err
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return sqlc.User{}, nil, ErrUnauthorized
	}
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(userID))
	if err != nil {
		return sqlc.User{}, nil, ErrUnauthorized
	}
	return user, claims, nil
}

func actorFromUser(user sqlc.User) identity.User {
	actor := identity.User{ID: uuid.UUID(user.ID.Bytes), Email: user.Email, Name: user.Name, Role: user.Role}
	if user.TenantID.Valid {
		tid := uuid.UUID(user.TenantID.Bytes)
		actor.TenantID = &tid
	}
	return actor
}

type mfaEnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
}

func (s *Service) HandleMFAEnrollStart(w http.ResponseWriter, r *http.Request) {
	var req mfaEnrollRequest
	if err := decodeJSON(r, &req); err != nil || req.EnrollmentToken == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "enrollment_token required")
		return
	}
	user, _, err := s.userFromMFAToken(r.Context(), req.EnrollmentToken, mfaPurposeEnrollRequired)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	if err := s.checkMethodAllowed(r.Context(), user, MethodTOTP); err != nil {
		writeMFAError(w, err)
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Orderly", AccountName: user.Email})
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not start enrollment")
		return
	}
	qr, err := qrDataURI(key.URL(), 220)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not start enrollment")
		return
	}
	setupToken, err := s.signMFAToken(mfaPurposeSetup, uuid.UUID(user.ID.Bytes).String(), key.Secret(), mfaTokenTTL)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not start enrollment")
		return
	}
	response.JSON(w, http.StatusOK, MFASetupResponse{Secret: key.Secret(), QRCodeDataURI: qr, SetupToken: setupToken})
}

type mfaEnrollConfirmRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	SetupToken      string `json:"setup_token"`
	Code            string `json:"code"`
}

func (s *Service) HandleMFAEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	var req mfaEnrollConfirmRequest
	if err := decodeJSON(r, &req); err != nil || req.EnrollmentToken == "" || req.SetupToken == "" || req.Code == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "enrollment_token, setup_token, and code required")
		return
	}
	user, enrollClaims, err := s.userFromMFAToken(r.Context(), req.EnrollmentToken, mfaPurposeEnrollRequired)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	setupClaims, err := s.parseMFAToken(req.SetupToken, mfaPurposeSetup)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	if setupClaims.Subject != enrollClaims.Subject {
		writeMFAError(w, ErrUnauthorized)
		return
	}
	if !totp.Validate(strings.TrimSpace(req.Code), setupClaims.MFASecret) {
		writeMFAError(w, ErrInvalidMFACode)
		return
	}
	userID := uuid.UUID(user.ID.Bytes)
	sealed, err := s.secretBox.SealString(setupClaims.MFASecret, "user:"+userID.String())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	if _, err := s.q.UpsertUserMfaMethod(r.Context(), sqlc.UpsertUserMfaMethodParams{
		UserID: pgutil.UUID(userID), Method: MethodTOTP, SecretEnc: pgutil.Text(sealed),
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	s.finishForcedEnrollment(w, r, user, MethodTOTP)
}

func (s *Service) HandleMFAEnrollEmailSend(w http.ResponseWriter, r *http.Request) {
	var req mfaEnrollRequest
	if err := decodeJSON(r, &req); err != nil || req.EnrollmentToken == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "enrollment_token required")
		return
	}
	user, _, err := s.userFromMFAToken(r.Context(), req.EnrollmentToken, mfaPurposeEnrollRequired)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	if err := s.RequestEmailOTP(r.Context(), user); err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

type mfaEnrollEmailConfirmRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	Code            string `json:"code"`
}

func (s *Service) HandleMFAEnrollEmailConfirm(w http.ResponseWriter, r *http.Request) {
	var req mfaEnrollEmailConfirmRequest
	if err := decodeJSON(r, &req); err != nil || req.EnrollmentToken == "" || req.Code == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "enrollment_token and code required")
		return
	}
	user, _, err := s.userFromMFAToken(r.Context(), req.EnrollmentToken, mfaPurposeEnrollRequired)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	userID := uuid.UUID(user.ID.Bytes)
	matched, err := s.verifyEmailOTP(r.Context(), userID, req.Code)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	if !matched {
		writeMFAError(w, ErrInvalidMFACode)
		return
	}
	if _, err := s.q.UpsertUserMfaMethod(r.Context(), sqlc.UpsertUserMfaMethodParams{
		UserID: pgutil.UUID(userID), Method: MethodEmailOTP, SecretEnc: pgtype.Text{},
	}); err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	s.finishForcedEnrollment(w, r, user, MethodEmailOTP)
}

// finishForcedEnrollment is the shared tail of both confirm endpoints: sync
// the cosmetic cache, issue the one-time recovery codes (this is always the
// user's first method — forced enrollment only ever fires when they had
// zero), notify, and complete the login with a real token pair.
func (s *Service) finishForcedEnrollment(w http.ResponseWriter, r *http.Request, user sqlc.User, method string) {
	userID := uuid.UUID(user.ID.Bytes)
	s.syncMfaEnabledCache(r.Context(), userID)
	codes, err := s.issueRecoveryCodesIfFirstMethod(r.Context(), userID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	s.notifyMethodEnrolled(r.Context(), actorFromUser(user), method, false)
	pair, err := s.issueTokens(r.Context(), user, true)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "could not complete enrollment")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair, "user": toUserResponse(user), "recovery_codes": codes})
}
