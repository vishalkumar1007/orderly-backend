package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// RoleCustomer marks a token as belonging to a storefront customer rather than
// to a member of staff. It is deliberately not a `users` row: a customer who
// orders a meal is not an operator of the shop.
const RoleCustomer = "CUSTOMER"

// CustomerPrincipal is the authenticated storefront customer for a request.
type CustomerPrincipal struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Name     string
	Phone    string
}

type customerCtxKey int

const customerKey customerCtxKey = 1

// WithCustomer stores the customer principal on the request context.
func WithCustomer(ctx context.Context, c CustomerPrincipal) context.Context {
	return context.WithValue(ctx, customerKey, c)
}

// CustomerFromContext returns the customer principal, if the request carried
// a valid customer token.
func CustomerFromContext(ctx context.Context) (CustomerPrincipal, bool) {
	c, ok := ctx.Value(customerKey).(CustomerPrincipal)
	return c, ok
}

// IssueCustomerTokens mints a token pair for a storefront customer. The tenant
// claim is mandatory: every customer token is bound to exactly one shop, so a
// token from one storefront can never be replayed against another.
func (s *Service) IssueCustomerTokens(customerID, tenantID uuid.UUID, name, phone string) (TokenPair, error) {
	now := time.Now()
	access := Claims{
		Role:       RoleCustomer,
		TenantID:   ptr(tenantID.String()),
		CustomerID: ptr(customerID.String()),
		Email:      "",
		Name:       name,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   customerID.String(),
			Issuer:    "orderly-customer",
			Audience:  jwt.ClaimStrings{tenantID.String()},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.AccessTokenTTL)),
		},
	}
	refresh := access
	refresh.ExpiresAt = jwt.NewNumericDate(now.Add(s.cfg.RefreshTokenTTL))
	refresh.Issuer = "orderly-customer-refresh"

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, access).SignedString([]byte(s.cfg.JWTAccessSecret))
	if err != nil {
		return TokenPair{}, err
	}
	refreshSigned, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refresh).SignedString([]byte(s.cfg.JWTRefreshSecret))
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:  signed,
		RefreshToken: refreshSigned,
		TokenType:    "Bearer",
		ExpiresIn:    int64(s.cfg.AccessTokenTTL.Seconds()),
	}, nil
}

// CustomerTTL is how long a storefront customer token pair stays valid.
func (s *Service) CustomerTTL() (access, refresh int64) {
	return int64(s.cfg.AccessTokenTTL.Seconds()), int64(s.cfg.RefreshTokenTTL.Seconds())
}

// CustomerAuthenticate validates a bearer token and returns the customer it
// belongs to. The customer row is re-read so a deleted or blocked account
// cannot keep using a token that is still inside its lifetime.
func (s *Service) CustomerAuthenticate(r *http.Request) (CustomerPrincipal, error) {
	raw := BearerToken(r)
	if raw == "" {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	claims, err := s.parseToken(raw, s.cfg.JWTAccessSecret)
	if err != nil {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	if claims.Role != RoleCustomer || claims.TenantID == nil || claims.CustomerID == nil {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	tenantID, err1 := uuid.Parse(*claims.TenantID)
	customerID, err2 := uuid.Parse(*claims.CustomerID)
	if err1 != nil || err2 != nil {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	row, err := s.q.GetCustomerByID(r.Context(), sqlc.GetCustomerByIDParams{
		ID:       pgutil.UUID(customerID),
		TenantID: pgutil.UUID(tenantID),
	})
	if err != nil {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	if row.IsBlocked {
		return CustomerPrincipal{}, ErrUnauthorized
	}
	return CustomerPrincipal{
		ID:       customerID,
		TenantID: tenantID,
		Name:     row.Name,
		Phone:    row.Phone.String,
	}, nil
}

// CustomerMiddleware requires a valid customer token.
func CustomerMiddleware(s *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := s.CustomerAuthenticate(r)
			if err != nil {
				response.Error(w, http.StatusUnauthorized, "unauthorized", "sign in to continue")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithCustomer(r.Context(), principal)))
		})
	}
}

// OptionalCustomerMiddleware attaches the customer principal when a valid
// customer token is present and otherwise passes the request through
// untouched. Public storefront endpoints use this so a signed-in customer gets
// a richer response while a guest keeps working with no token at all.
func OptionalCustomerMiddleware(s *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := s.CustomerAuthenticate(r)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithCustomer(r.Context(), principal)))
		})
	}
}

// OptionalMiddleware attaches a staff/admin identity when a valid staff token
// is present. It is how the tenant admin "preview store" button can read an
// unpublished storefront over the public API without a query-string bypass.
func OptionalMiddleware(s *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := s.AuthenticateRequest(r)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(identity.WithUser(r.Context(), user)))
		})
	}
}

// BearerToken extracts the bearer credential from the Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func ptr[T any](v T) *T { return &v }
