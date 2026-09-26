package configsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// nowUnixNano is a seam so tests can produce distinct probe keys.
var nowUnixNano = func() int64 { return time.Now().UnixNano() }

// TenantPosture is one tenant's configuration standing for one service, as seen
// by a Super Admin. It deliberately carries no configuration values at all —
// not even non-secret ones — because the audience is a cross-tenant overview
// and a summary is all it needs.
type TenantPosture struct {
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	TenantSlug string `json:"tenant_slug"`
	Service    string `json:"service"`

	// AllowPlatform is the per-tenant grant.
	AllowPlatform bool `json:"allow_platform"`
	// Source is the tenant's elected level, or "" when never chosen.
	Source string `json:"source"`

	// OwnStatus / OwnEnabled describe the tenant's own configuration.
	OwnStatus  string `json:"own_status"`
	OwnEnabled bool   `json:"own_enabled"`

	// PlatformStatus / PlatformEnabled / PlatformAllowTenants describe the
	// shared configuration.
	PlatformStatus       string `json:"platform_status"`
	PlatformEnabled      bool   `json:"platform_enabled"`
	PlatformAllowTenants bool   `json:"platform_allow_tenants"`
}

// EffectiveSource reports which level this tenant will actually use right now,
// which can differ from its stated preference when access was revoked.
func (p TenantPosture) EffectiveSource() string {
	if p.Source == string(SourcePlatform) && p.CanUsePlatform() {
		return string(SourcePlatform)
	}
	return string(SourceOrganization)
}

// CanUsePlatform reports whether the platform configuration is both globally
// shared and granted to this tenant.
func (p TenantPosture) CanUsePlatform() bool {
	return p.PlatformAllowTenants && p.AllowPlatform && p.PlatformEnabled
}

// ListAllTenants returns the cross-tenant posture table, sorted by tenant name
// then service.
func (s *Store) ListAllTenants(ctx context.Context) ([]TenantPosture, error) {
	rows, err := s.q.ListServiceAccessForAllTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tenant configuration posture: %w", err)
	}
	out := make([]TenantPosture, 0, len(rows))
	for _, r := range rows {
		posture := TenantPosture{
			TenantID:             uuidString(r.TenantID),
			TenantName:           r.TenantName,
			TenantSlug:           r.TenantSlug,
			Service:              r.ServiceType,
			AllowPlatform:        r.AllowPlatform,
			OwnStatus:            orUnknown(text(r.OwnStatus)),
			OwnEnabled:           boolOf(r.OwnEnabled),
			PlatformStatus:       orUnknown(text(r.PlatformStatus)),
			PlatformEnabled:      boolOf(r.PlatformEnabled),
			PlatformAllowTenants: boolOf(r.PlatformAllowTenants),
		}
		if r.Source.Valid {
			posture.Source = r.Source.String
		}
		out = append(out, posture)
	}
	sortPostures(out)
	return out, nil
}

// serviceRank is the canonical display order.
var serviceRank = map[string]int{"SMTP": 0, "STORAGE": 1, "AI": 2}

// sortPostures groups rows by tenant, then by the canonical service order.
func sortPostures(rows []TenantPosture) {
	// Insertion sort keeps the dependency surface small and the input is short.
	for i := 1; i < len(rows); i++ {
		cur := rows[i]
		j := i - 1
		for j >= 0 && postureLess(cur, rows[j]) {
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = cur
	}
}

func postureLess(a, b TenantPosture) bool {
	if a.TenantName != b.TenantName {
		return a.TenantName < b.TenantName
	}
	ra, oka := serviceRank[a.Service]
	rb, okb := serviceRank[b.Service]
	switch {
	case oka && okb:
		return ra < rb
	case oka:
		return true
	case okb:
		return false
	}
	return a.Service < b.Service
}

func orUnknown(v string) string {
	if v == "" {
		return string(StatusUnconfigured)
	}
	return v
}

// text unwraps a nullable text column.
func text(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

// boolOf unwraps a nullable boolean column.
func boolOf(b pgtype.Bool) bool { return b.Valid && b.Bool }

// uuidString renders a pgtype.UUID, tolerating the invalid value.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
