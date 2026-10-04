package tenantctx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/pkg/pgutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

// TenantSlugHeader is the browser/SSR signal for the shop tenant when dialing
// the shared API host (api.{BASE_DOMAIN}). Slug only — never a client-supplied
// tenant UUID.
const TenantSlugHeader = "X-Tenant-Slug"

var tenantSlugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

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

	// Public tenant API hosts: {slug}.api.{base} (e.g. vm-food.api.orderly.qd.je).
	// Checked before the generic {slug}.{base} rule so "vm-food.api" is not
	// mistaken for a tenant slug.
	apiSuffix := ".api." + base
	if strings.HasSuffix(host, apiSuffix) {
		slug := strings.TrimSuffix(host, apiSuffix)
		if slug != "" && !strings.Contains(slug, ".") {
			info.Kind = HostTenant
			info.Slug = slug
			return info
		}
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
		// Frontend-style shop hosts ({slug}.{base}) and legacy proxy Hosts.
		if strings.Contains(sub, ".") {
			return info
		}
		info.Kind = HostTenant
		info.Slug = sub
		return info
	}

	return info
}

// firstForwardedHost returns the left-most X-Forwarded-Host value.
func firstForwardedHost(header string) string {
	v := strings.TrimSpace(header)
	if v == "" {
		return ""
	}
	if i := strings.Index(v, ","); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// EffectiveHost is the hostname used for tenant/admin routing.
//
// When the request already arrived on a tenant or admin host, that Host wins
// (clients cannot override it with X-Forwarded-Host). Otherwise — API host,
// internal Docker hostname, unknown — honor X-Forwarded-Host so SSR can dial
// the API container while advertising the shared public api.{base} host.
func EffectiveHost(r *http.Request, baseDomain string) string {
	direct := ParseHost(r.Host, baseDomain)
	if direct.Kind == HostTenant || direct.Kind == HostAdmin {
		return r.Host
	}
	if fwd := firstForwardedHost(r.Header.Get("X-Forwarded-Host")); fwd != "" {
		return fwd
	}
	return r.Host
}

// NormalizeTenantSlug returns a lowercase slug when it is a single valid label.
func NormalizeTenantSlug(raw string) string {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if slug == "" || strings.Contains(slug, ".") || !tenantSlugRe.MatchString(slug) {
		return ""
	}
	return slug
}

// slugFromOriginOrReferer extracts a frontend tenant slug from Origin/Referer
// when the request hit the shared API host without X-Tenant-Slug.
func slugFromOriginOrReferer(r *http.Request, baseDomain string) string {
	for _, raw := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		info := ParseHost(u.Hostname(), baseDomain)
		if info.Kind == HostTenant && info.Slug != "" {
			return info.Slug
		}
	}
	return ""
}

// ResolveTenantBinding decides the host classification and tenant slug for a
// request on the shared API architecture:
//  1. HostTenant from EffectiveHost ({slug}.api.{base} legacy or {slug}.{base})
//  2. Else X-Tenant-Slug on HostAPI / unknown
//  3. Else Origin/Referer frontend host {slug}.{base}
func ResolveTenantBinding(r *http.Request, baseDomain string) HostInfo {
	hostInfo := ParseHost(EffectiveHost(r, baseDomain), baseDomain)
	if hostInfo.Kind == HostTenant && hostInfo.Slug != "" {
		return hostInfo
	}
	if hostInfo.Kind == HostAdmin {
		return hostInfo
	}

	if slug := NormalizeTenantSlug(r.Header.Get(TenantSlugHeader)); slug != "" {
		return HostInfo{Kind: HostTenant, Slug: slug, Raw: hostInfo.Raw}
	}
	if slug := slugFromOriginOrReferer(r, baseDomain); slug != "" {
		return HostInfo{Kind: HostTenant, Slug: slug, Raw: hostInfo.Raw}
	}
	return hostInfo
}

func infoFromTenantRow(row sqlc.Tenant, licenseStatus string, caps map[string]bool) Info {
	return Info{
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
}

// LoadTenantBySlug loads an ACTIVE licensed tenant by slug for request context.
// Returns (nil, err) where err is pgx.ErrNoRows when missing.
func LoadTenantBySlug(ctx context.Context, q *sqlc.Queries, slug string) (*Info, error) {
	row, err := q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return loadTenantGates(ctx, q, row)
}

// LoadTenantByID loads an ACTIVE licensed tenant by id for JWT HostAPI fallback.
func LoadTenantByID(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*Info, error) {
	row, err := q.GetTenantByID(ctx, pgutil.UUID(id))
	if err != nil {
		return nil, err
	}
	return loadTenantGates(ctx, q, row)
}

func loadTenantGates(ctx context.Context, q *sqlc.Queries, row sqlc.Tenant) (*Info, error) {
	if row.Status == "SUSPENDED" {
		return nil, errTenantSuspended
	}
	licenseStatus, err := q.GetTenantLicenseStatus(ctx, row.ID)
	if err != nil && err != pgx.ErrNoRows {
		return nil, err
	}
	if licenseStatus != "ACTIVE" {
		return nil, errLicenseInactive
	}
	capRows, err := q.ListEnabledCapabilities(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	caps := make(map[string]bool, len(capRows))
	for _, c := range capRows {
		caps[c] = true
	}
	info := infoFromTenantRow(row, licenseStatus, caps)
	return &info, nil
}

var (
	errTenantSuspended = errors.New("tenant suspended")
	errLicenseInactive = errors.New("license inactive")
)

// Middleware resolves tenant from Host, X-Tenant-Slug, or Origin.
// Platform API hosts with no slug pass through without a tenant context.
func Middleware(pool *pgxpool.Pool, baseDomain string) func(http.Handler) http.Handler {
	q := sqlc.New(pool)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hostInfo := ResolveTenantBinding(r, baseDomain)
			ctx := WithHost(r.Context(), hostInfo)

			if hostInfo.Kind != HostTenant {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			info, err := LoadTenantBySlug(ctx, q, hostInfo.Slug)
			if err != nil {
				if err == pgx.ErrNoRows {
					response.Error(w, http.StatusNotFound, "tenant_not_found", "unknown tenant subdomain")
					return
				}
				if err == errTenantSuspended {
					response.Error(w, http.StatusForbidden, "tenant_suspended", "this shop is suspended")
					return
				}
				if err == errLicenseInactive {
					response.Error(w, http.StatusForbidden, "license_inactive", "this business's license is not active")
					return
				}
				response.Error(w, http.StatusInternalServerError, "internal_error", "failed to resolve tenant")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithTenant(ctx, *info)))
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
