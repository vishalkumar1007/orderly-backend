package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrMustSetPassword    = errors.New("must set password")
	ErrSetupNotNeeded     = errors.New("super admin already exists")
)

type Service struct {
	q   *sqlc.Queries
	cfg config.Config
}

func NewService(pool *pgxpool.Pool, cfg config.Config) *Service {
	return &Service{q: sqlc.New(pool), cfg: cfg}
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

type Claims struct {
	Role     string  `json:"role"`
	TenantID *string `json:"tenant_id,omitempty"`
	Email    string  `json:"email"`
	Name     string  `json:"name"`
	// CustomerID is set only on storefront customer tokens. Staff and admin
	// tokens identify a `users` row through Subject instead.
	CustomerID *string `json:"customer_id,omitempty"`
	jwt.RegisteredClaims
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type SetupPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type AdminSetupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Service) passwordMin(ctx context.Context) int {
	row, err := s.q.GetPlatformSettings(ctx)
	if err != nil || len(row.Config) == 0 {
		return 8
	}
	var cfg struct {
		PasswordMinLength int `json:"password_min_length"`
	}
	if err := json.Unmarshal(row.Config, &cfg); err != nil || cfg.PasswordMinLength < 6 {
		return 8
	}
	return cfg.PasswordMinLength
}

type UserResponse struct {
	ID              string  `json:"id"`
	Email           string  `json:"email"`
	Name            string  `json:"name"`
	Role            string  `json:"role"`
	TenantID        *string `json:"tenant_id"`
	Status          string  `json:"status"`
	MustSetPassword bool    `json:"must_set_password"`

	// RoleLabel and Permissions travel with the identity so the console can
	// draw a rail that matches what the API will actually allow. Without them
	// the client would need its own copy of the role table, and the first time
	// the two disagreed a user would see a menu item that 403s when clicked.
	RoleLabel   string                `json:"role_label"`
	Permissions []identity.Permission `json:"permissions"`
}

func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	has, err := s.q.HasSuperAdmin(ctx)
	if err != nil {
		return false, err
	}
	return !has, nil
}

func (s *Service) SetupFirstSuperAdmin(ctx context.Context, email, password, name string) (UserResponse, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || !strings.Contains(email, "@") {
		return UserResponse{}, fmt.Errorf("valid email required")
	}
	minLen := s.passwordMin(ctx)
	if len(password) < minLen {
		return UserResponse{}, fmt.Errorf("password too short")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Super Admin"
	}
	needs, err := s.NeedsSetup(ctx)
	if err != nil {
		return UserResponse{}, err
	}
	if !needs {
		return UserResponse{}, ErrSetupNotNeeded
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return UserResponse{}, err
	}
	user, err := s.q.CreateSuperAdmin(ctx, sqlc.CreateSuperAdminParams{
		Name:         name,
		Email:        email,
		PasswordHash: string(hash),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return UserResponse{}, ErrSetupNotNeeded
		}
		return UserResponse{}, err
	}
	_, _ = s.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID:     user.ID,
		Action:     "platform.super_admin_created",
		EntityType: "user",
		EntityID:   user.ID,
	})
	return toUserResponse(user), nil
}

// Audit results recorded on authentication events.
const (
	auditSuccess = "SUCCESS"
	auditFailure = "FAILURE"
	auditDenied  = "DENIED"
)

// auditLogin records an authentication attempt. Only the email is captured —
// never a password — and the row is platform-scoped (no tenant).
func (s *Service) auditLogin(ctx context.Context, email, action, result string) {
	addr := strings.ToLower(strings.TrimSpace(email))
	if addr == "" || len(addr) > 254 {
		return
	}
	meta, _ := json.Marshal(map[string]string{"email": addr})
	_, _ = s.q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		Action:     action,
		EntityType: "user",
		Metadata:   meta,
		Result:     result,
	})
}

func (s *Service) AdminLogin(ctx context.Context, email, password string) (TokenPair, UserResponse, error) {
	user, err := s.authenticateUser(ctx, email, password)
	if err != nil {
		s.auditLogin(ctx, email, "admin.login", auditFailure)
		return TokenPair{}, UserResponse{}, err
	}
	if user.Role != identity.RoleSuperAdmin {
		// Valid credentials, wrong portal — worth distinguishing from a bad password.
		s.auditLogin(ctx, email, "admin.login", auditDenied)
		return TokenPair{}, UserResponse{}, ErrInvalidCredentials
	}
	pair, err := s.issueTokens(ctx, user)
	if err != nil {
		s.auditLogin(ctx, email, "admin.login", auditFailure)
		return TokenPair{}, UserResponse{}, err
	}
	s.auditLogin(ctx, email, "admin.login", auditSuccess)
	return pair, toUserResponse(user), nil
}

func (s *Service) TenantLogin(ctx context.Context, email, password string, hostTenantID uuid.UUID) (TokenPair, UserResponse, error) {
	user, err := s.authenticateUser(ctx, email, password)
	if err != nil {
		s.auditLogin(ctx, email, "tenant.login", auditFailure)
		return TokenPair{}, UserResponse{}, err
	}
	if user.Role != identity.RoleTenantAdmin && user.Role != identity.RoleStaff {
		s.auditLogin(ctx, email, "tenant.login", auditDenied)
		return TokenPair{}, UserResponse{}, ErrInvalidCredentials
	}
	if !user.TenantID.Valid || uuid.UUID(user.TenantID.Bytes) != hostTenantID {
		// Cross-tenant attempt: a valid account on the wrong subdomain.
		s.auditLogin(ctx, email, "tenant.login", auditDenied)
		return TokenPair{}, UserResponse{}, ErrInvalidCredentials
	}
	if user.MustSetPassword {
		s.auditLogin(ctx, email, "tenant.login", auditDenied)
		return TokenPair{}, toUserResponse(user), ErrMustSetPassword
	}
	pair, err := s.issueTokens(ctx, user)
	if err != nil {
		s.auditLogin(ctx, email, "tenant.login", auditFailure)
		return TokenPair{}, UserResponse{}, err
	}
	s.auditLogin(ctx, email, "tenant.login", auditSuccess)
	return pair, toUserResponse(user), nil
}

func (s *Service) authenticateUser(ctx context.Context, email, password string) (sqlc.User, error) {
	user, err := s.q.GetUserByEmail(ctx, strings.TrimSpace(strings.ToLower(email)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, ErrInvalidCredentials
		}
		return sqlc.User{}, err
	}
	if user.Status != "ACTIVE" {
		return sqlc.User{}, ErrInvalidCredentials
	}
	if user.MustSetPassword {
		return user, nil // password not usable until set via invite
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return sqlc.User{}, ErrInvalidCredentials
	}
	return user, nil
}

func (s *Service) ChangePassword(ctx context.Context, userID uuid.UUID, currentPassword, newPassword string) error {
	minLen := s.passwordMin(ctx)
	if len(newPassword) < minLen {
		return fmt.Errorf("password too short")
	}
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(userID))
	if err != nil {
		return ErrUnauthorized
	}
	if user.MustSetPassword {
		return ErrUnauthorized
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)); err != nil {
		return ErrInvalidCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.q.SetUserPasswordWithCurrent(ctx, sqlc.SetUserPasswordWithCurrentParams{
		ID:           user.ID,
		PasswordHash: string(hash),
	})
	return err
}

func (s *Service) SetupPassword(ctx context.Context, token, password string) (TokenPair, UserResponse, error) {
	minLen := s.passwordMin(ctx)
	if len(password) < minLen {
		return TokenPair{}, UserResponse{}, fmt.Errorf("password too short")
	}
	user, err := s.q.GetUserByInviteTokenHash(ctx, pgtype.Text{String: hashToken(token), Valid: true})
	if err != nil {
		return TokenPair{}, UserResponse{}, ErrUnauthorized
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return TokenPair{}, UserResponse{}, err
	}
	updated, err := s.q.SetUserPassword(ctx, sqlc.SetUserPasswordParams{
		ID:           user.ID,
		PasswordHash: string(hash),
	})
	if err != nil {
		return TokenPair{}, UserResponse{}, err
	}
	pair, err := s.issueTokens(ctx, updated)
	if err != nil {
		return TokenPair{}, UserResponse{}, err
	}
	return pair, toUserResponse(updated), nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	claims, err := s.parseToken(refreshToken, s.cfg.JWTRefreshSecret)
	if err != nil {
		return TokenPair{}, ErrUnauthorized
	}
	hash := hashToken(refreshToken)
	stored, err := s.q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		return TokenPair{}, ErrUnauthorized
	}
	if stored.ExpiresAt.Time.Before(time.Now()) {
		_ = s.q.DeleteRefreshTokenByHash(ctx, hash)
		return TokenPair{}, ErrUnauthorized
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return TokenPair{}, ErrUnauthorized
	}
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(userID))
	if err != nil || user.Status != "ACTIVE" || user.MustSetPassword {
		return TokenPair{}, ErrUnauthorized
	}
	_ = s.q.DeleteRefreshTokenByHash(ctx, hash)
	return s.issueTokens(ctx, user)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	return s.q.DeleteRefreshTokenByHash(ctx, hashToken(refreshToken))
}

func (s *Service) Me(ctx context.Context, userID uuid.UUID) (UserResponse, error) {
	user, err := s.q.GetUserByID(ctx, pgutil.UUID(userID))
	if err != nil {
		return UserResponse{}, err
	}
	return toUserResponse(user), nil
}

func (s *Service) issueTokens(ctx context.Context, user sqlc.User) (TokenPair, error) {
	uid := uuid.UUID(user.ID.Bytes)
	var tenantID *string
	if user.TenantID.Valid {
		str := pgutil.UUIDString(user.TenantID)
		tenantID = &str
	}
	now := time.Now()
	accessClaims := Claims{
		Role: user.Role, TenantID: tenantID, Email: user.Email, Name: user.Name,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uid.String(), ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.AccessTokenTTL)),
		},
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString([]byte(s.cfg.JWTAccessSecret))
	if err != nil {
		return TokenPair{}, err
	}
	refreshClaims := Claims{
		Role: user.Role, TenantID: tenantID, Email: user.Email, Name: user.Name,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uid.String(), ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.RefreshTokenTTL)),
		},
	}
	refresh, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString([]byte(s.cfg.JWTRefreshSecret))
	if err != nil {
		return TokenPair{}, err
	}
	_, err = s.q.InsertRefreshToken(ctx, sqlc.InsertRefreshTokenParams{
		UserID: user.ID, TokenHash: hashToken(refresh),
		ExpiresAt: pgtypeTimestamptz(now.Add(s.cfg.RefreshTokenTTL)),
	})
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: int64(s.cfg.AccessTokenTTL.Seconds()),
	}, nil
}

func (s *Service) parseToken(tokenStr, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrUnauthorized
	}
	return claims, nil
}

// staffRoles is the set of roles that may act as an operator of the platform.
// A storefront customer is deliberately not in it.
var staffRoles = map[string]bool{
	identity.RoleSuperAdmin:    true,
	identity.RolePlatformAdmin: true,
	identity.RoleSupport:       true,
	identity.RoleTenantAdmin:   true,
	identity.RoleManager:       true,
	identity.RoleStaff:         true,
}

// AuthenticateRequest resolves the operator behind a request from its bearer
// token.
//
// Two things are checked beyond the signature. First, the role must be a staff
// or admin role: a storefront customer token is signed with the same secret, so
// without this check a customer could present it anywhere `auth.Middleware` is
// mounted. Second, the user row is re-read, so a token for a deleted or disabled
// account stops working immediately rather than at the end of its lifetime.
func (s *Service) AuthenticateRequest(r *http.Request) (identity.User, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if raw == "" || raw == r.Header.Get("Authorization") {
		return identity.User{}, ErrUnauthorized
	}
	claims, err := s.parseToken(raw, s.cfg.JWTAccessSecret)
	if err != nil {
		return identity.User{}, ErrUnauthorized
	}
	if !staffRoles[claims.Role] {
		return identity.User{}, ErrUnauthorized
	}
	uid, err := uuid.Parse(claims.Subject)
	if err != nil {
		return identity.User{}, ErrUnauthorized
	}
	u := identity.User{ID: uid, Email: claims.Email, Name: claims.Name, Role: claims.Role}
	if claims.TenantID != nil && *claims.TenantID != "" {
		tid, err := uuid.Parse(*claims.TenantID)
		if err != nil {
			return identity.User{}, ErrUnauthorized
		}
		u.TenantID = &tid
	}

	// The token is only a claim; the account behind it is the authority.
	row, err := s.q.GetUserByID(r.Context(), pgutil.UUID(uid))
	if err != nil {
		return identity.User{}, ErrUnauthorized
	}
	if row.Status != "ACTIVE" {
		return identity.User{}, ErrUnauthorized
	}
	if !staffRoles[row.Role] {
		return identity.User{}, ErrUnauthorized
	}
	// A tenant admin's token must not outlive a change of shop, and a super
	// admin must not be able to act through a tenant claim.
	switch row.Role {
	case identity.RoleSuperAdmin, identity.RolePlatformAdmin, identity.RoleSupport:
		// A console identity has no business. A token claiming one is either
		// stale or forged; either way it is not this account.
		if u.TenantID != nil {
			return identity.User{}, ErrUnauthorized
		}
	case identity.RoleTenantAdmin, identity.RoleManager, identity.RoleStaff:
		if !row.TenantID.Valid || u.TenantID == nil || uuid.UUID(row.TenantID.Bytes) != *u.TenantID {
			return identity.User{}, ErrUnauthorized
		}
	}
	return u, nil
}

func Middleware(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := svc.AuthenticateRequest(r)
			if err != nil {
				response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			next.ServeHTTP(w, r.WithContext(identity.WithUser(r.Context(), user)))
		})
	}
}

func RequireRoles(roles ...string) func(http.Handler) http.Handler {
	allowed := map[string]struct{}{}
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := identity.UserFromContext(r.Context())
			if !ok {
				response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			if _, ok := allowed[user.Role]; !ok {
				response.Error(w, http.StatusForbidden, "forbidden", "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermission guards a route with a capability rather than a role list.
//
// Roles change shape — a manager was added after staff and owner already
// existed — and every route that named roles directly had to be revisited when
// that happened. Naming the capability instead means the role table is the only
// thing that moves.
func RequirePermission(permission identity.Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := identity.UserFromContext(r.Context())
			if !ok {
				response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}
			if !identity.Can(user.Role, permission) {
				// The message names what is missing, not the role that is
				// missing it: "you need X" is actionable, "you are not an
				// owner" invites an argument with the person who is.
				response.Error(w, http.StatusForbidden, "forbidden",
					"this account does not have access to "+string(permission))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := identity.UserFromContext(r.Context())
		if !ok || user.TenantID == nil {
			response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MatchHostTenant ensures JWT tenant_id equals the request tenant context.
//
// On the shared API host, the tenant usually comes from X-Tenant-Slug (or
// Origin). When that signal is absent but the JWT carries a tenant_id, the
// tenant is loaded from the claim so authenticated clients remain usable —
// the JWT is still the authority and is re-checked against the DB user row
// in AuthenticateRequest.
func MatchHostTenant(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	q := sqlc.New(pool)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := identity.UserFromContext(r.Context())
			if !ok || user.TenantID == nil {
				response.Error(w, http.StatusForbidden, "forbidden", "tenant context required")
				return
			}
			ctx := r.Context()
			hostTenant, ok := tenantctx.FromContext(ctx)
			if !ok {
				info, err := tenantctx.LoadTenantByID(ctx, q, *user.TenantID)
				if err != nil {
					response.Error(w, http.StatusForbidden, "forbidden", "tenant host required")
					return
				}
				hostTenant = *info
				ctx = tenantctx.WithTenant(ctx, hostTenant)
			}
			if *user.TenantID != hostTenant.ID {
				response.Error(w, http.StatusForbidden, "forbidden", "tenant host mismatch")
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (s *Service) HandleAdminSetupStatus(w http.ResponseWriter, r *http.Request) {
	host, _ := tenantctx.HostFromContext(r.Context())
	if host.Kind == tenantctx.HostTenant {
		response.Error(w, http.StatusForbidden, "forbidden", "use platform host for admin setup")
		return
	}
	needs, err := s.NeedsSetup(r.Context())
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to check setup status")
		return
	}
	response.JSON(w, http.StatusOK, map[string]bool{"needs_setup": needs})
}

func (s *Service) HandleAdminSetup(w http.ResponseWriter, r *http.Request) {
	host, _ := tenantctx.HostFromContext(r.Context())
	if host.Kind == tenantctx.HostTenant {
		response.Error(w, http.StatusForbidden, "forbidden", "use platform host for admin setup")
		return
	}
	var req AdminSetupRequest
	if err := decodeJSON(r, &req); err != nil || req.Email == "" || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "email and password required")
		return
	}
	user, err := s.SetupFirstSuperAdmin(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		if errors.Is(err, ErrSetupNotNeeded) {
			response.Error(w, http.StatusConflict, "setup_not_needed", "super admin already exists")
			return
		}
		if err.Error() == "password too short" || err.Error() == "valid email required" {
			response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "setup failed")
		return
	}
	response.JSON(w, http.StatusCreated, map[string]string{"id": user.ID, "email": user.Email})
}

func (s *Service) HandleAdminLogin(w http.ResponseWriter, r *http.Request) {
	host, _ := tenantctx.HostFromContext(r.Context())
	if host.Kind == tenantctx.HostTenant {
		response.Error(w, http.StatusForbidden, "forbidden", "use tenant login on this host")
		return
	}
	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil || req.Email == "" || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "email and password required")
		return
	}
	pair, user, err := s.AdminLogin(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "login failed")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair, "user": user})
}

func (s *Service) HandleTenantLogin(w http.ResponseWriter, r *http.Request) {
	hostTenant, ok := tenantctx.FromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusBadRequest, "invalid_host", "tenant subdomain required")
		return
	}
	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil || req.Email == "" || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "email and password required")
		return
	}
	pair, user, err := s.TenantLogin(r.Context(), req.Email, req.Password, hostTenant.ID)
	if err != nil {
		if errors.Is(err, ErrMustSetPassword) {
			response.Error(w, http.StatusForbidden, "must_set_password", "complete password setup via invite link first")
			return
		}
		if errors.Is(err, ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password for this shop")
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "login failed")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair, "user": user})
}

func (s *Service) HandleSetupPassword(w http.ResponseWriter, r *http.Request) {
	var req SetupPasswordRequest
	if err := decodeJSON(r, &req); err != nil || req.Token == "" || req.Password == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "token and password required")
		return
	}
	pair, user, err := s.SetupPassword(r.Context(), req.Token, req.Password)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			response.Error(w, http.StatusUnauthorized, "unauthorized", "invalid or expired invite")
			return
		}
		response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair, "user": user})
}

func (s *Service) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "refresh_token required")
		return
	}
	pair, err := s.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "invalid refresh token")
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"tokens": pair})
}

func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	_ = decodeJSON(r, &req)
	_ = s.Logout(r.Context(), req.RefreshToken)
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req ChangePasswordRequest
	if err := decodeJSON(r, &req); err != nil || req.CurrentPassword == "" || req.NewPassword == "" {
		response.Error(w, http.StatusBadRequest, "invalid_request", "current_password and new_password required")
		return
	}
	if err := s.ChangePassword(r.Context(), user.ID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, "invalid_credentials", "current password is incorrect")
			return
		}
		if err.Error() == "password too short" {
			response.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		response.Error(w, http.StatusInternalServerError, "internal_error", "password change failed")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		response.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	me, err := s.Me(r.Context(), user.ID)
	if err != nil {
		response.Error(w, http.StatusInternalServerError, "internal_error", "failed to load user")
		return
	}
	if t, ok := tenantctx.FromContext(r.Context()); ok {
		response.JSON(w, http.StatusOK, map[string]any{
			"user": me,
			"host_tenant": map[string]any{
				"id": t.ID.String(), "slug": t.Slug, "name": t.Name,
				"setup_status": t.SetupStatus, "is_published": t.IsPublished,
				// The business type travels with the identity because the
				// console is shaped by it: the navigation, what a catalogue
				// and its groups are called, and which operational screens
				// exist at all. Fetching it separately would mean the shell
				// renders once with the wrong words and again with the right
				// ones.
				"business_type": t.BusinessType,
			},
		})
		return
	}
	response.JSON(w, http.StatusOK, me)
}

func toUserResponse(user sqlc.User) UserResponse {
	return UserResponse{
		ID: pgutil.UUIDString(user.ID), Email: user.Email, Name: user.Name,
		Role: user.Role, TenantID: pgutil.UUIDPtr(user.TenantID), Status: user.Status,
		MustSetPassword: user.MustSetPassword,
		RoleLabel:       identity.RoleLabel(user.Role),
		Permissions:     identity.PermissionsFor(user.Role),
	}
}

func HashInviteToken(token string) string { return hashToken(token) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	return jsonDecoder(r).Decode(dst)
}
