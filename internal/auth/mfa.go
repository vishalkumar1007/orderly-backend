package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp/totp"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

const (
	mfaPurposeSetup         = "mfa_setup"
	mfaPurposeChallenge     = "mfa_challenge"
	mfaPurposeEnrollRequired = "mfa_enroll_required"
	mfaTokenTTL             = 5 * time.Minute
	// mfaEnrollTokenTTL is longer than mfaTokenTTL: this flow is a QR-scan-
	// then-confirm (or email-send-then-confirm) done by someone who is not yet
	// signed in at all.
	mfaEnrollTokenTTL = 15 * time.Minute
	recoveryCodeCount = 10
)

var (
	// ErrMFANotAvailable is returned when a tenant user tries to enroll before
	// the platform has turned the feature on for businesses.
	ErrMFANotAvailable    = errors.New("mfa is not available for this account yet")
	ErrMFAAlreadyOn       = errors.New("mfa is already enabled")
	ErrMFANotOn           = errors.New("mfa is not enabled")
	ErrInvalidMFACode     = errors.New("invalid code")
	ErrMFAMethodNotAllowed = errors.New("that method is not allowed by this business's policy")
)

/* ------------------------------------------------------------------ *
 * Short-lived tokens — setup, login-challenge, forced-enrollment
 * ------------------------------------------------------------------ */

// signMFAToken mints a narrowly-scoped, short-lived token for one of the MFA
// sub-flows. It is signed with the same access secret and library as a real
// session token, but carries no Role — a route that forgot to check Purpose
// would still reject it as an unauthenticated request.
func (s *Service) signMFAToken(purpose, userID, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		Purpose:   purpose,
		MFASecret: secret,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: userID, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(s.cfg.JWTAccessSecret))
}

// parseMFAToken verifies signature, expiry, and that the token was minted for
// exactly this purpose — a setup token can never be replayed as a login
// challenge or vice versa.
func (s *Service) parseMFAToken(tokenStr, wantPurpose string) (*Claims, error) {
	claims, err := s.parseToken(tokenStr, s.cfg.JWTAccessSecret)
	if err != nil {
		return nil, ErrUnauthorized
	}
	if claims.Purpose != wantPurpose {
		return nil, ErrUnauthorized
	}
	return claims, nil
}

/* ------------------------------------------------------------------ *
 * Per-business gate (permission, distinct from policy — see
 * mfa_policy.go for mode/methods/enforcement).
 *
 * Not a platform-wide switch: a super admin grants this to one business at a
 * time, from that business's own Configuration tab (or at onboarding) —
 * tenants.mfa_allowed. A console/platform account (TenantID NULL) has no
 * business to gate against and is always allowed.
 * ------------------------------------------------------------------ */

func (s *Service) tenantAllowsMFA(ctx context.Context, tenantID pgtype.UUID) bool {
	if !tenantID.Valid {
		return true
	}
	tenant, err := s.q.GetTenantByID(ctx, tenantID)
	if err != nil {
		return false
	}
	return tenant.MfaAllowed
}

/* ------------------------------------------------------------------ *
 * QR rendering — same library and PNG-data-URI shape as the storefront's
 * marketing QR code (internal/storefront/qr.go), kept local rather than
 * importing storefront into auth for one function.
 * ------------------------------------------------------------------ */

func qrDataURI(content string, size int) (string, error) {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	png, err := code.PNG(size)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

/* ------------------------------------------------------------------ *
 * Recovery codes — a fallback, not a selectable method. Issued once, the
 * first time a user enrolls any method; never regenerated just because a
 * second method is added.
 * ------------------------------------------------------------------ */

// generateRecoveryCodes returns n random, human-typeable codes (xxxx-xxxx,
// uppercase hex). Never returned again after the confirm/regenerate response
// that creates them — only their sha256 hash is stored.
func generateRecoveryCodes(n int) ([]string, error) {
	codes := make([]string, n)
	for i := range codes {
		buf := make([]byte, 5)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		hexStr := strings.ToUpper(fmt.Sprintf("%x", buf))
		codes[i] = hexStr[:4] + "-" + hexStr[4:8]
	}
	return codes, nil
}

func normalizeRecoveryCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (s *Service) issueRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]string, error) {
	codes, err := generateRecoveryCodes(recoveryCodeCount)
	if err != nil {
		return nil, err
	}
	for _, code := range codes {
		if err := s.q.CreateRecoveryCode(ctx, sqlc.CreateRecoveryCodeParams{
			UserID:   pgutil.UUID(userID),
			CodeHash: hashToken(normalizeRecoveryCode(code)),
		}); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// issueRecoveryCodesIfFirstMethod generates the one-time set of recovery
// codes only when the user had none before — i.e. this is their first ever
// enrolled method. Adding a second method (TOTP after Email OTP, or vice
// versa) must not invalidate codes already saved by the user; it returns nil
// in that case rather than a fresh set.
func (s *Service) issueRecoveryCodesIfFirstMethod(ctx context.Context, userID uuid.UUID) ([]string, error) {
	existing, err := s.q.ListUnusedRecoveryCodes(ctx, pgutil.UUID(userID))
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, nil
	}
	return s.issueRecoveryCodes(ctx, userID)
}

/* ------------------------------------------------------------------ *
 * Enrollment status helpers
 * ------------------------------------------------------------------ */

type mfaMethodStatus struct {
	Method     string     `json:"method"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (s *Service) enrolledMethods(ctx context.Context, userID uuid.UUID) ([]mfaMethodStatus, error) {
	rows, err := s.q.ListUserMfaMethods(ctx, pgutil.UUID(userID))
	if err != nil {
		return nil, err
	}
	out := make([]mfaMethodStatus, 0, len(rows))
	for _, row := range rows {
		m := mfaMethodStatus{Method: row.Method}
		if row.LastUsedAt.Valid {
			t := row.LastUsedAt.Time
			m.LastUsedAt = &t
		}
		out = append(out, m)
	}
	return out, nil
}

func (s *Service) countEnrolledMethods(ctx context.Context, userID uuid.UUID) (int64, error) {
	return s.q.CountUserMfaMethods(ctx, pgutil.UUID(userID))
}

func (s *Service) syncMfaEnabledCache(ctx context.Context, userID uuid.UUID) {
	// Cosmetic only — a failure here can only ever produce a wrong display
	// value later, never a wrong authorization decision, so it is logged and
	// swallowed rather than propagated.
	if err := s.q.SyncUserMfaEnabledCache(ctx, pgutil.UUID(userID)); err != nil && s.log != nil {
		s.log.Error("mfa: failed to sync mfa_enabled cache", "user_id", userID.String(), "error", err)
	}
}

/* ------------------------------------------------------------------ *
 * Self-service: TOTP setup, confirm, remove, status, regenerate
 * ------------------------------------------------------------------ */

type MFASetupResponse struct {
	Secret        string `json:"secret"`
	QRCodeDataURI string `json:"qr_code_data_uri"`
	SetupToken    string `json:"setup_token"`
}

func (s *Service) SetupMFA(ctx context.Context, actor identity.User) (MFASetupResponse, error) {
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(actor.ID))
	if err != nil {
		return MFASetupResponse{}, err
	}
	if _, err := s.q.GetUserMfaMethod(ctx, sqlc.GetUserMfaMethodParams{UserID: pgutil.UUID(actor.ID), Method: MethodTOTP}); err == nil {
		return MFASetupResponse{}, ErrMFAAlreadyOn
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return MFASetupResponse{}, err
	}
	if err := s.checkMethodAllowed(ctx, user, MethodTOTP); err != nil {
		return MFASetupResponse{}, err
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Orderly", AccountName: user.Email})
	if err != nil {
		return MFASetupResponse{}, err
	}
	qr, err := qrDataURI(key.URL(), 220)
	if err != nil {
		return MFASetupResponse{}, err
	}
	setupToken, err := s.signMFAToken(mfaPurposeSetup, actor.ID.String(), key.Secret(), mfaTokenTTL)
	if err != nil {
		return MFASetupResponse{}, err
	}
	return MFASetupResponse{Secret: key.Secret(), QRCodeDataURI: qr, SetupToken: setupToken}, nil
}

// checkMethodAllowed gates self-service enrollment: the business must be
// permitted to use MFA at all, its policy must not be DISABLED, and (for a
// tenant) the method itself must be in the policy's allowed_methods list. A
// console account is always allowed every method, same as before.
func (s *Service) checkMethodAllowed(ctx context.Context, user sqlc.User, method string) error {
	policy, available, err := s.policyForUser(ctx, user)
	if err != nil {
		return err
	}
	if !available {
		return ErrMFANotAvailable
	}
	if !user.TenantID.Valid {
		return nil
	}
	for _, m := range policy.AllowedMethods {
		if m == method {
			return nil
		}
	}
	return ErrMFAMethodNotAllowed
}

type MFAConfirmResponse struct {
	RecoveryCodes []string `json:"recovery_codes,omitempty"`
}

func (s *Service) ConfirmMFA(ctx context.Context, actor identity.User, setupToken, code string) (MFAConfirmResponse, error) {
	claims, err := s.parseMFAToken(setupToken, mfaPurposeSetup)
	if err != nil {
		return MFAConfirmResponse{}, err
	}
	if claims.Subject != actor.ID.String() {
		// The token was minted for someone else's enrollment.
		return MFAConfirmResponse{}, ErrUnauthorized
	}
	if !totp.Validate(strings.TrimSpace(code), claims.MFASecret) {
		return MFAConfirmResponse{}, ErrInvalidMFACode
	}
	sealed, err := s.secretBox.SealString(claims.MFASecret, "user:"+actor.ID.String())
	if err != nil {
		return MFAConfirmResponse{}, err
	}
	hadMethodBefore, err := s.countEnrolledMethods(ctx, actor.ID)
	if err != nil {
		return MFAConfirmResponse{}, err
	}
	if _, err := s.q.UpsertUserMfaMethod(ctx, sqlc.UpsertUserMfaMethodParams{
		UserID: pgutil.UUID(actor.ID), Method: MethodTOTP, SecretEnc: pgutil.Text(sealed),
	}); err != nil {
		return MFAConfirmResponse{}, err
	}
	s.syncMfaEnabledCache(ctx, actor.ID)
	codes, err := s.issueRecoveryCodesIfFirstMethod(ctx, actor.ID)
	if err != nil {
		return MFAConfirmResponse{}, err
	}
	s.notifyMethodEnrolled(ctx, actor, MethodTOTP, hadMethodBefore > 0)
	return MFAConfirmResponse{RecoveryCodes: codes}, nil
}

func (s *Service) notifyMethodEnrolled(ctx context.Context, actor identity.User, method string, hadMethodBefore bool) {
	action := "mfa.method_added"
	eventType := notify.TypeSecurityMFAMethodAdded
	if !hadMethodBefore {
		action = "mfa.enabled"
		eventType = notify.TypeSecurityMFAEnabled
	}
	_, _ = s.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID: pgutil.UUID(actor.ID), Action: action, EntityType: "user", EntityID: pgutil.UUID(actor.ID),
		Result: auditSuccess,
	})
	if actor.TenantID != nil {
		notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, actor.TenantID, eventType,
			fmt.Sprintf("%s enabled two-factor authentication (%s)", actor.Name, method), "",
			map[string]any{"user_id": actor.ID.String(), "user_name": actor.Name, "method": method}, notify.Contact{})
	}
}

// RemoveMFAMethod removes one enrolled method after re-checking the caller's
// password. When this was the last enrolled method, recovery codes are
// cleared too — they are meaningless (and risky to keep around) once nothing
// else authenticates the account.
func (s *Service) RemoveMFAMethod(ctx context.Context, actor identity.User, method, password string) error {
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(actor.ID))
	if err != nil {
		return err
	}
	if _, err := s.q.GetUserMfaMethod(ctx, sqlc.GetUserMfaMethodParams{UserID: pgutil.UUID(actor.ID), Method: method}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMFANotOn
		}
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	if err := s.q.DeleteUserMfaMethod(ctx, sqlc.DeleteUserMfaMethodParams{UserID: pgutil.UUID(actor.ID), Method: method}); err != nil {
		return err
	}
	remaining, err := s.countEnrolledMethods(ctx, actor.ID)
	if err != nil {
		return err
	}
	s.syncMfaEnabledCache(ctx, actor.ID)
	if remaining == 0 {
		_ = s.q.DeleteRecoveryCodesForUser(ctx, pgutil.UUID(actor.ID))
	}
	_, _ = s.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID: pgutil.UUID(actor.ID), Action: "mfa.method_removed", EntityType: "user", EntityID: pgutil.UUID(actor.ID),
		Result: auditSuccess,
	})
	if remaining == 0 && actor.TenantID != nil {
		notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, actor.TenantID, notify.TypeSecurityMFADisabled,
			fmt.Sprintf("%s disabled two-factor authentication", actor.Name), "",
			map[string]any{"user_id": actor.ID.String(), "user_name": actor.Name}, notify.Contact{})
	}
	return nil
}

// DisableMFA preserves the original single-method self-service contract
// (password only, TOTP implied) for the existing frontend; RemoveMFAMethod
// is the general form a multi-method manager calls directly.
func (s *Service) DisableMFA(ctx context.Context, actor identity.User, password string) error {
	return s.RemoveMFAMethod(ctx, actor, MethodTOTP, password)
}

func (s *Service) RegenerateRecoveryCodes(ctx context.Context, actor identity.User, password string) ([]string, error) {
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(actor.ID))
	if err != nil {
		return nil, err
	}
	count, err := s.countEnrolledMethods(ctx, actor.ID)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrMFANotOn
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := s.q.DeleteRecoveryCodesForUser(ctx, pgutil.UUID(actor.ID)); err != nil {
		return nil, err
	}
	return s.issueRecoveryCodes(ctx, actor.ID)
}

/* ------------------------------------------------------------------ *
 * Login completion
 * ------------------------------------------------------------------ */

// VerifyMFALogin is the second step of a login that returned mfa_required:
// the challenge token proves the password was already correct, and a valid
// code (or recovery code) proves the second factor. Issues the same token
// pair a normal login would, with MFAVerified true since a factor was just
// proven.
func (s *Service) VerifyMFALogin(ctx context.Context, challengeToken, method, code, recoveryCode string) (TokenPair, UserResponse, error) {
	claims, err := s.parseMFAToken(challengeToken, mfaPurposeChallenge)
	if err != nil {
		return TokenPair{}, UserResponse{}, ErrUnauthorized
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return TokenPair{}, UserResponse{}, ErrUnauthorized
	}
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenPair{}, UserResponse{}, ErrUnauthorized
		}
		return TokenPair{}, UserResponse{}, err
	}
	count, err := s.countEnrolledMethods(ctx, userID)
	if err != nil {
		return TokenPair{}, UserResponse{}, err
	}
	if count == 0 {
		// Every method was removed between the password step and now —
		// nothing left to verify.
		return TokenPair{}, UserResponse{}, ErrUnauthorized
	}

	ok, verr := s.checkMFACode(ctx, user, method, code, recoveryCode)
	if verr != nil {
		return TokenPair{}, UserResponse{}, verr
	}
	if !ok {
		s.auditLogin(ctx, user.Email, "mfa.verify", auditDenied)
		return TokenPair{}, UserResponse{}, ErrInvalidMFACode
	}

	pair, err := s.issueTokens(ctx, user, true)
	if err != nil {
		s.auditLogin(ctx, user.Email, "mfa.verify", auditFailure)
		return TokenPair{}, UserResponse{}, err
	}
	s.auditLogin(ctx, user.Email, "mfa.verify", auditSuccess)
	return pair, toUserResponse(user), nil
}

// checkMFACode validates one of: a recovery code (works regardless of which
// primary method it is standing in for), a TOTP code, or an Email OTP code.
// method defaults to TOTP when empty, for callers that predate multi-method
// (a client that only ever had one method never needs to name it).
func (s *Service) checkMFACode(ctx context.Context, user sqlc.User, method, code, recoveryCode string) (bool, error) {
	if strings.TrimSpace(recoveryCode) != "" {
		return s.consumeRecoveryCode(ctx, user.ID, recoveryCode)
	}
	if method == "" {
		method = MethodTOTP
	}
	switch method {
	case MethodTOTP:
		row, err := s.q.GetUserMfaMethod(ctx, sqlc.GetUserMfaMethodParams{UserID: user.ID, Method: MethodTOTP})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		secret, err := s.secretBox.OpenString(row.SecretEnc.String, "user:"+uuid.UUID(user.ID.Bytes).String())
		if err != nil {
			return false, err
		}
		if !totp.Validate(strings.TrimSpace(code), secret) {
			return false, nil
		}
		_ = s.q.TouchUserMfaMethod(ctx, sqlc.TouchUserMfaMethodParams{UserID: user.ID, Method: MethodTOTP})
		return true, nil
	case MethodEmailOTP:
		return s.verifyEmailOTP(ctx, uuid.UUID(user.ID.Bytes), code)
	default:
		return false, nil
	}
}

func (s *Service) consumeRecoveryCode(ctx context.Context, userID pgtype.UUID, raw string) (bool, error) {
	hash := hashToken(normalizeRecoveryCode(raw))
	rows, err := s.q.ListUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if secretbox.ConstantTimeEqual(row.CodeHash, hash) {
			if err := s.q.MarkRecoveryCodeUsed(ctx, row.ID); err != nil {
				return false, err
			}
			s.notifyRecoveryCodeUsed(ctx, userID)
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) notifyRecoveryCodeUsed(ctx context.Context, userID pgtype.UUID) {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return
	}
	_, _ = s.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID: userID, Action: "mfa.recovery_used", EntityType: "user", EntityID: userID, Result: auditSuccess,
	})
	if user.TenantID.Valid {
		tid := uuid.UUID(user.TenantID.Bytes)
		notify.Dispatch(ctx, notify.Deps{Q: s.q, Log: s.log}, &tid, notify.TypeSecurityMFARecoveryUsed,
			fmt.Sprintf("%s signed in using a recovery code", user.Name), "",
			map[string]any{"user_id": pgutil.UUIDString(userID), "user_name": user.Name}, notify.Contact{})
	}
}

/* ------------------------------------------------------------------ *
 * HTTP handlers
 * ------------------------------------------------------------------ */

func (s *Service) HandleMFAStatus(w http.ResponseWriter, r *http.Request) {
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
	methods, err := s.enrolledMethods(r.Context(), actor.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load mfa status")
		return
	}
	_, available, err := s.policyForUser(r.Context(), user)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load mfa status")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{
		"enabled":   len(methods) > 0,
		"available": available,
		"methods":   methods,
	})
}

func (s *Service) HandleMFASetup(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	out, err := s.SetupMFA(r.Context(), actor)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

type mfaConfirmRequest struct {
	SetupToken string `json:"setup_token"`
	Code       string `json:"code"`
}

func (s *Service) HandleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaConfirmRequest
	if err := decodeJSON(r, &req); err != nil || req.SetupToken == "" || req.Code == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "setup_token and code required")
		return
	}
	out, err := s.ConfirmMFA(r.Context(), actor, req.SetupToken, req.Code)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

type mfaPasswordRequest struct {
	Password string `json:"password"`
}

func (s *Service) HandleMFADisable(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaPasswordRequest
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "password required")
		return
	}
	if err := s.DisableMFA(r.Context(), actor, req.Password); err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type mfaRemoveMethodRequest struct {
	Method   string `json:"method"`
	Password string `json:"password"`
}

func (s *Service) HandleMFARemoveMethod(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaRemoveMethodRequest
	if err := decodeJSON(r, &req); err != nil || req.Password == "" || req.Method == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "method and password required")
		return
	}
	if err := s.RemoveMFAMethod(r.Context(), actor, strings.ToUpper(req.Method), req.Password); err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleMFARecoveryCodesRegenerate(w http.ResponseWriter, r *http.Request) {
	actor, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaPasswordRequest
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "password required")
		return
	}
	codes, err := s.RegenerateRecoveryCodes(r.Context(), actor, req.Password)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type mfaVerifyRequest struct {
	ChallengeToken string `json:"challenge_token"`
	Method         string `json:"method"`
	Code           string `json:"code"`
	RecoveryCode   string `json:"recovery_code"`
}

func (s *Service) HandleMFAVerify(w http.ResponseWriter, r *http.Request) {
	var req mfaVerifyRequest
	if err := decodeJSON(r, &req); err != nil || req.ChallengeToken == "" || (req.Code == "" && req.RecoveryCode == "") {
		response.Error(w, http.StatusBadRequest, "invalid_request", "challenge_token and code (or recovery_code) required")
		return
	}
	pair, user, err := s.VerifyMFALogin(r.Context(), req.ChallengeToken, strings.ToUpper(req.Method), req.Code, req.RecoveryCode)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair, "user": user})
}

type mfaChallengeEmailSendRequest struct {
	ChallengeToken string `json:"challenge_token"`
}

// HandleMFAChallengeEmailSend sends an Email OTP code during a login
// challenge — the counterpart to HandleMFAEmailSend (self-service) and
// HandleMFAEnrollEmailSend (forced enrollment), for a user who is already
// enrolled in Email OTP and is now completing a normal login with it.
func (s *Service) HandleMFAChallengeEmailSend(w http.ResponseWriter, r *http.Request) {
	var req mfaChallengeEmailSendRequest
	if err := decodeJSON(r, &req); err != nil || req.ChallengeToken == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "challenge_token required")
		return
	}
	claims, err := s.parseMFAToken(req.ChallengeToken, mfaPurposeChallenge)
	if err != nil {
		writeMFAError(w, err)
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		writeMFAError(w, ErrUnauthorized)
		return
	}
	user, err := s.q.GetUserByID(r.Context(), pgutil.UUID(userID))
	if err != nil {
		writeMFAError(w, ErrUnauthorized)
		return
	}
	if _, err := s.q.GetUserMfaMethod(r.Context(), sqlc.GetUserMfaMethodParams{UserID: pgutil.UUID(userID), Method: MethodEmailOTP}); err != nil {
		writeMFAError(w, ErrMFANotOn)
		return
	}
	if err := s.RequestEmailOTP(r.Context(), user); err != nil {
		writeMFAError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func writeMFAError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMFANotAvailable):
		response.Error(w, http.StatusForbidden, "mfa_not_available", "MFA is not available for this account yet")
	case errors.Is(err, ErrMFAMethodNotAllowed):
		response.Error(w, http.StatusForbidden, "mfa_method_not_allowed", "That method is not allowed by this business's policy")
	case errors.Is(err, ErrMFAAlreadyOn):
		response.Error(w, http.StatusConflict, "mfa_already_enabled", "Two-factor authentication is already on")
	case errors.Is(err, ErrMFANotOn):
		response.Error(w, http.StatusConflict, "mfa_not_enabled", "Two-factor authentication is not on")
	case errors.Is(err, ErrInvalidMFACode):
		response.Error(w, http.StatusUnauthorized, "invalid_code", "That code didn't match — try again")
	case errors.Is(err, ErrInvalidCredentials):
		response.Error(w, http.StatusUnauthorized, "invalid_credentials", "Incorrect password")
	case errors.Is(err, ErrUnauthorized):
		response.Error(w, http.StatusUnauthorized, "unauthorized", "This setup link has expired — start again")
	case errors.Is(err, ErrEmailOTPUnavailable):
		response.Error(w, http.StatusForbidden, "email_otp_unavailable", emailOTPUnavailableMessage(err))
	case errors.Is(err, ErrEmailOTPRateLimited), errors.Is(err, ErrEmailOTPResendTooSoon), errors.Is(err, ErrEmailOTPTooManyAttempts):
		response.Error(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, ErrEmailOTPExpired), errors.Is(err, ErrEmailOTPNoCode):
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		response.Error(w, http.StatusInternalServerError, "internal_error", "Could not complete that request")
	}
}
