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
	// Platform roles. All three have a NULL tenant_id and none of them grants
	// anything inside a business.
	RoleSuperAdmin    = "SUPER_ADMIN"
	RolePlatformAdmin = "PLATFORM_ADMIN"
	RoleSupport       = "SUPPORT"

	RoleTenantAdmin = "TENANT_ADMIN"
	// RoleManager runs a shop day to day without being able to reconfigure the
	// business. See permissions.go for exactly what that means.
	RoleManager = "MANAGER"
	RoleStaff   = "STAFF"
)
