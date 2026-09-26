package identity

import (
	"context"

	"github.com/google/uuid"
)

type ctxKey int

const userKey ctxKey = 1

// User is the authenticated principal resolved from a JWT.
type User struct {
	ID       uuid.UUID
	Email    string
	Name     string
	Role     string
	TenantID *uuid.UUID
}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey).(User)
	return u, ok
}

const (
	RoleSuperAdmin  = "SUPER_ADMIN"
	RoleTenantAdmin = "TENANT_ADMIN"
	RoleStaff       = "STAFF"
)
