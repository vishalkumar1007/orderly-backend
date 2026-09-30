package tenantctx

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/response"
)

type ctxKey int

const (
	tenantKey ctxKey = 1
	hostKey   ctxKey = 2
)

// HostKind classifies the request hostname.
type HostKind string

const (
	HostAdmin   HostKind = "admin"
	HostAPI     HostKind = "api"
	HostTenant  HostKind = "tenant"
	HostUnknown HostKind = "unknown"
)

// Info is the resolved tenant for the request host.
type Info struct {
	ID            uuid.UUID
	Slug          string
	Name          string
	Status        string
	SetupStatus   string
	IsPublished   bool
	BusinessType  string
	LicenseStatus string
	// Capabilities is the tenant's enabled capability set, loaded once per
	// request from tenant_capabilities. RequireCapability reads it; nothing
	// else should query tenant_capabilities per-request.
	Capabilities map[string]bool
}

// HasCapability reports whether the tenant has a capability enabled. Safe to
// call on a zero-value Info (e.g. a host with no resolved tenant).
func (i Info) HasCapability(code string) bool {
	return i.Capabilities[code]
}

// Capability codes. This is the exact set seeded in business_type_capabilities
// (see db/migrations/20260929190000_capabilities_catalog.sql) — the platform
// capability matrix from HLD §6, as Go constants instead of bare strings.
const (
	CapCustomers    = "CUSTOMERS"
	CapStaff        = "STAFF"
	CapPayments     = "PAYMENTS"
	CapBilling      = "BILLING"
	CapCatalog      = "CATALOG"
	CapCart         = "CART"
	CapOrders       = "ORDERS"
	CapKitchen      = "KITCHEN"
	CapServices     = "SERVICES"
	CapAppointments = "APPOINTMENTS"
	CapQueue        = "QUEUE"
	CapReservations = "RESERVATIONS"
	CapTables       = "TABLES"
	CapRooms        = "ROOMS"
	CapHousekeeping = "HOUSEKEEPING"
)

type HostInfo struct {
	Kind HostKind
	Slug string // tenant slug when Kind == HostTenant
	Raw  string
}

func WithTenant(ctx context.Context, t Info) context.Context {
	return context.WithValue(ctx, tenantKey, t)
}

func FromContext(ctx context.Context) (Info, bool) {
	t, ok := ctx.Value(tenantKey).(Info)
	return t, ok
}

func WithHost(ctx context.Context, h HostInfo) context.Context {
	return context.WithValue(ctx, hostKey, h)
}

func HostFromContext(ctx context.Context) (HostInfo, bool) {
	h, ok := ctx.Value(hostKey).(HostInfo)
	return h, ok
}

// ParseHost extracts admin/api/tenant from hostname given base domain (e.g. "localhost").
func ParseHost(hostHeader, baseDomain string) HostInfo {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	base := strings.ToLower(strings.TrimSpace(baseDomain))
	if base == "" {
		base = "localhost"
	}

	info := HostInfo{Raw: host, Kind: HostUnknown}

	if host == "admin."+base || host == "admin" {
		info.Kind = HostAdmin
		return info
	}
	if host == "api."+base || host == "api" || host == base {
		info.Kind = HostAPI
		return info
	}

	suffix := "." + base
	if strings.HasSuffix(host, suffix) {
		sub := strings.TrimSuffix(host, suffix)
		if sub == "" || sub == "www" {
			info.Kind = HostAPI
			return info
		}
		if sub == "admin" {
			info.Kind = HostAdmin
			return info
		}
		if sub == "api" {
			info.Kind = HostAPI
			return info
		}
		info.Kind = HostTenant
		info.Slug = sub
		return info
	}

	return info
}

// Middleware resolves tenant from subdomain for tenant hosts.
// Admin/API hosts pass through without a tenant context.
func Middleware(pool *pgxpool.Pool, baseDomain string) func(http.Handler) http.Handler {
	q := sqlc.New(pool)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hostInfo := ParseHost(r.Host, baseDomain)
			ctx := WithHost(r.Context(), hostInfo)

			if hostInfo.Kind != HostTenant {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			row, err := q.GetTenantBySlug(ctx, hostInfo.Slug)
			if err != nil {
				if err == pgx.ErrNoRows {
					response.Error(w, http.StatusNotFound, "tenant_not_found", "unknown tenant subdomain")
					return
				}
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to resolve tenant")
				return
			}
			if row.Status == "SUSPENDED" {
				response.Error(w, http.StatusForbidden, "tenant_suspended", "this shop is suspended")
				return
			}

			// License is the enforcement gate (HLD §16), separate from
			// tenant.status: a business can be ACTIVE on billing and still have
			// its license SUSPENDED/EXPIRED/REVOKED (e.g. a compliance hold). A
			// missing license row is treated the same as an inactive one — fail
			// closed, matching the SUSPENDED check above rather than silently
			// letting an unlicensed tenant trade.
			licenseStatus, err := q.GetTenantLicenseStatus(ctx, row.ID)
			if err != nil && err != pgx.ErrNoRows {
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to resolve license")
				return
			}
			if licenseStatus != "ACTIVE" {
				response.Error(w, http.StatusForbidden, "license_inactive", "this business's license is not active")
				return
			}

			capRows, err := q.ListEnabledCapabilities(ctx, row.ID)
			if err != nil {
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to resolve capabilities")
				return
			}
			caps := make(map[string]bool, len(capRows))
			for _, c := range capRows {
				caps[c] = true
			}

			info := Info{
				ID:            uuid.UUID(row.ID.Bytes),
				Slug:          row.Slug,
				Name:          row.Name,
				Status:        row.Status,
				SetupStatus:   row.SetupStatus,
				IsPublished:   row.IsPublished,
				BusinessType:  row.BusinessType,
				LicenseStatus: licenseStatus,
				Capabilities:  caps,
			}
			next.ServeHTTP(w, r.WithContext(WithTenant(ctx, info)))
		})
	}
}

// RequireCapability guards a route with a business capability rather than a
// role or a permission. A route names the module it needs; whether that
// module exists for this tenant at all is decided by business_type
// (business_type_capabilities → tenant_capabilities), never by the caller's
// role. This is what stops a Hotel tenant's token from ever reaching an
// orders/kitchen endpoint, and vice versa, regardless of who is asking.
//
// Requires tenantctx.Middleware to have already run (it is mounted globally),
// so Capabilities is always populated for a resolved tenant host.
func RequireCapability(code string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := FromContext(r.Context())
			if !ok || !t.HasCapability(code) {
				response.Error(w, http.StatusForbidden, "capability_disabled",
					"this business does not have the \""+code+"\" module enabled")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
