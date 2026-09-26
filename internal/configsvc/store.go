package configsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/pkg/pgutil"
)

// Record is a stored configuration row. SealedSecrets holds ciphertext; it is
// never serialised into an HTTP response.
type Record struct {
	ID            uuid.UUID
	ServiceType   ServiceType
	Provider      string
	Config        map[string]any
	SealedSecrets map[string]string
	Status        Status
	Enabled       bool
	AllowTenants  bool
	LastError     string
	LastTestedAt  *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// HasSecret reports whether a sealed secret is stored for key.
func (r *Record) HasSecret(key string) bool {
	v, ok := r.SealedSecrets[key]
	return ok && strings.TrimSpace(v) != ""
}

// HasAnySecret reports whether at least one secret is stored.
func (r *Record) HasAnySecret() bool {
	for k := range r.SealedSecrets {
		if strings.TrimSpace(r.SealedSecrets[k]) != "" {
			return true
		}
	}
	return false
}

// Store persists configuration rows, sealing secrets on the way in.
type Store struct {
	q   *sqlc.Queries
	box *secretbox.Box
	log Logger
}

// Logger is the minimal structured logger the store needs.
type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// NewStore builds a Store. A disabled box means no encryption key is
// configured; callers must check box.Disabled() before persisting secrets in
// production.
func NewStore(q *sqlc.Queries, box *secretbox.Box, log Logger) *Store {
	return &Store{q: q, box: box, log: log}
}

// Disabled reports whether secrets would be stored without encryption.
func (s *Store) Disabled() bool { return s.box.Disabled() }

/* ---------- AAD helpers ---------- */

// PlatformContextID binds a platform secret to its service row. Exported so the
// HTTP layer can seal an unsaved probe with the same AAD the store will use.
func PlatformContextID(service ServiceType) string { return "platform:" + string(service) }

// TenantContextID binds a tenant secret to one service on one tenant, so a
// ciphertext cannot be replayed onto a different tenant.
func TenantContextID(tenantID uuid.UUID, service ServiceType) string {
	return "tenant:" + tenantID.String() + ":" + string(service)
}

// Unexported aliases keep the internal call sites short.
func platformContext(service ServiceType) string { return PlatformContextID(service) }

func tenantContext(tenantID uuid.UUID, service ServiceType) string {
	return TenantContextID(tenantID, service)
}

/* ---------- row mapping ---------- */

func recordFromPlatform(row sqlc.PlatformConfiguration) *Record {
	return &Record{
		ID:            uuid.UUID(row.ID.Bytes),
		ServiceType:   ServiceType(row.ServiceType),
		Provider:      row.Provider,
		Config:        jsonMap(row.Config),
		SealedSecrets: jsonStringMap(row.SecretConfig),
		Status:        Status(row.Status),
		Enabled:       row.Enabled,
		AllowTenants:  row.AllowTenants,
		LastError:     row.LastError,
		LastTestedAt:  timePtr(row.LastTestedAt),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

func recordFromTenant(row sqlc.TenantConfiguration) *Record {
	return &Record{
		ID:            uuid.UUID(row.ID.Bytes),
		ServiceType:   ServiceType(row.ServiceType),
		Provider:      row.Provider,
		Config:        jsonMap(row.Config),
		SealedSecrets: jsonStringMap(row.SecretConfig),
		Status:        Status(row.Status),
		Enabled:       row.Enabled,
		LastError:     row.LastError,
		LastTestedAt:  timePtr(row.LastTestedAt),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

func jsonMap(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// jsonStringMap decodes a secret blob. A non-string value (or malformed JSON)
// is dropped rather than surfaced, so a corrupt row degrades to "no secret
// stored" instead of leaking or crashing.
func jsonStringMap(raw []byte) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return out
	}
	for k, v := range generic {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out[k] = s
		}
	}
	return out
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

/* ---------- secret merge ---------- */

// mergeSecrets combines an incoming config (which may carry MaskSentinel or an
// empty value in place of a secret) with the secrets already on disk.
//
// A masked or blank secret field means "unchanged". Anything else is sealed
// fresh. This is what allows the console to render a form it cannot read and
// still save the rest of the fields.
func (s *Store) mergeSecrets(
	service ServiceType,
	contextID string,
	incoming map[string]any,
	existing map[string]string,
) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range existing {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	for _, key := range secretFields[service] {
		raw, present := incoming[key]
		if !present {
			continue
		}
		str := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if str == "" || str == MaskSentinel {
			// Explicitly left blank: preserve whatever is stored.
			continue
		}
		sealed, err := s.box.SealString(str, contextID)
		if err != nil {
			return nil, fmt.Errorf("seal %s secret %q: %w", service, key, err)
		}
		out[key] = sealed
		// The plaintext must not survive in the blob we persist.
		delete(incoming, key)
	}
	// Defence in depth: strip any secret key the caller may have smuggled in
	// under a name this service does not recognise.
	for key := range incoming {
		if IsSecretField(service, key) {
			delete(incoming, key)
		}
	}
	return out, nil
}

// OpenSecrets decrypts a record's secrets. The result never leaves the backend.
func (s *Store) OpenSecrets(r *Record, contextID string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range r.SealedSecrets {
		if strings.TrimSpace(v) == "" {
			continue
		}
		plain, err := s.box.OpenString(v, contextID)
		if err != nil {
			return nil, fmt.Errorf("open %s secret %q: %w", r.ServiceType, k, err)
		}
		out[k] = plain
	}
	return out, nil
}

/* ---------- platform reads ---------- */

// GetPlatform returns the platform row for a service, or nil when absent.
func (s *Store) GetPlatform(ctx context.Context, service ServiceType) (*Record, error) {
	row, err := s.q.GetPlatformConfiguration(ctx, string(service))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get platform %s config: %w", service, err)
	}
	return recordFromPlatform(row), nil
}

// ListPlatform returns every platform row, keyed by service. Missing services
// are absent from the map.
func (s *Store) ListPlatform(ctx context.Context) (map[ServiceType]*Record, error) {
	rows, err := s.q.ListPlatformConfigurations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list platform configs: %w", err)
	}
	out := make(map[ServiceType]*Record, len(rows))
	for _, row := range rows {
		rec := recordFromPlatform(row)
		out[rec.ServiceType] = rec
	}
	return out, nil
}

/* ---------- platform writes ---------- */

// WriteRequest is a full save of one service's configuration.
type WriteRequest struct {
	ServiceType  ServiceType
	Provider     string
	Config       map[string]any
	Enabled      bool
	AllowTenants bool
}

// SavePlatform upserts a platform configuration.
func (s *Store) SavePlatform(ctx context.Context, req WriteRequest) (*Record, error) {
	existing, err := s.GetPlatform(ctx, req.ServiceType)
	if err != nil {
		return nil, err
	}
	priorSecrets := map[string]string{}
	if existing != nil {
		priorSecrets = existing.SealedSecrets
	}
	ctxID := platformContext(req.ServiceType)

	config := req.Config
	if config == nil {
		config = map[string]any{}
	}
	// Work on a copy: mergeSecrets deletes secret keys from the map it is given.
	merged := make(map[string]any, len(config))
	for k, v := range config {
		merged[k] = v
	}
	secrets, err := s.mergeSecrets(req.ServiceType, ctxID, merged, priorSecrets)
	if err != nil {
		return nil, err
	}
	merged["provider"] = req.Provider

	configRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode platform %s config: %w", req.ServiceType, err)
	}
	secretRaw, err := json.Marshal(secrets)
	if err != nil {
		return nil, fmt.Errorf("encode platform %s secrets: %w", req.ServiceType, err)
	}

	status := deriveStatus(req.Enabled, secrets, StatusUnconfigured)
	row, err := s.q.UpsertPlatformConfiguration(ctx, sqlc.UpsertPlatformConfigurationParams{
		ServiceType:  string(req.ServiceType),
		Provider:     req.Provider,
		Config:       configRaw,
		SecretConfig: secretRaw,
		Status:       string(status),
		Enabled:      req.Enabled,
		AllowTenants: req.AllowTenants,
	})
	if err != nil {
		return nil, fmt.Errorf("save platform %s config: %w", req.ServiceType, err)
	}
	return recordFromPlatform(row), nil
}

// SetPlatformAllowTenants flips whether tenants may borrow this configuration.
func (s *Store) SetPlatformAllowTenants(ctx context.Context, service ServiceType, allow bool) (*Record, error) {
	row, err := s.q.SetPlatformConfigurationAllowTenants(ctx, sqlc.SetPlatformConfigurationAllowTenantsParams{
		ServiceType:  string(service),
		AllowTenants: allow,
	})
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("set platform allow_tenants: %w", err)
	}
	return recordFromPlatform(row), nil
}

// RecordStatus persists the outcome of a connection test.
func (s *Store) RecordStatus(ctx context.Context, service ServiceType, status Status, lastErr string) error {
	_, err := s.q.SetPlatformConfigurationStatus(ctx, sqlc.SetPlatformConfigurationStatusParams{
		ServiceType: string(service),
		Status:      string(status),
		LastError:   truncate(lastErr, 500),
	})
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("record platform status: %w", err)
	}
	return nil
}

// RecordTenantStatus persists the outcome of a tenant connection test.
func (s *Store) RecordTenantStatus(ctx context.Context, tenantID uuid.UUID, service ServiceType, status Status, lastErr string) error {
	_, err := s.q.SetTenantConfigurationStatus(ctx, sqlc.SetTenantConfigurationStatusParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
		Status:      string(status),
		LastError:   truncate(lastErr, 500),
	})
	if err != nil && !isNoRows(err) {
		return fmt.Errorf("record tenant status: %w", err)
	}
	return nil
}

/* ---------- tenant reads ---------- */

// GetTenant returns one tenant's configuration row, or nil when absent.
func (s *Store) GetTenant(ctx context.Context, tenantID uuid.UUID, service ServiceType) (*Record, error) {
	row, err := s.q.GetTenantConfiguration(ctx, sqlc.GetTenantConfigurationParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
	})
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get tenant %s config: %w", service, err)
	}
	return recordFromTenant(row), nil
}

// ListTenant returns every configuration row for a tenant.
func (s *Store) ListTenant(ctx context.Context, tenantID uuid.UUID) (map[ServiceType]*Record, error) {
	rows, err := s.q.ListTenantConfigurations(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, fmt.Errorf("list tenant configs: %w", err)
	}
	out := make(map[ServiceType]*Record, len(rows))
	for _, row := range rows {
		rec := recordFromTenant(row)
		out[rec.ServiceType] = rec
	}
	return out, nil
}

/* ---------- tenant writes ---------- */

// SaveTenant upserts a tenant's own configuration.
func (s *Store) SaveTenant(ctx context.Context, tenantID uuid.UUID, req WriteRequest) (*Record, error) {
	existing, err := s.GetTenant(ctx, tenantID, req.ServiceType)
	if err != nil {
		return nil, err
	}
	priorSecrets := map[string]string{}
	if existing != nil {
		priorSecrets = existing.SealedSecrets
	}
	ctxID := tenantContext(tenantID, req.ServiceType)

	merged := map[string]any{}
	for k, v := range req.Config {
		merged[k] = v
	}
	secrets, err := s.mergeSecrets(req.ServiceType, ctxID, merged, priorSecrets)
	if err != nil {
		return nil, err
	}
	merged["provider"] = req.Provider

	configRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode tenant %s config: %w", req.ServiceType, err)
	}
	secretRaw, err := json.Marshal(secrets)
	if err != nil {
		return nil, fmt.Errorf("encode tenant %s secrets: %w", req.ServiceType, err)
	}

	status := deriveStatus(req.Enabled, secrets, StatusUnconfigured)
	row, err := s.q.UpsertTenantConfiguration(ctx, sqlc.UpsertTenantConfigurationParams{
		TenantID:     pgutil.UUID(tenantID),
		ServiceType:  string(req.ServiceType),
		Provider:     req.Provider,
		Config:       configRaw,
		SecretConfig: secretRaw,
		Status:       string(status),
		Enabled:      req.Enabled,
	})
	if err != nil {
		return nil, fmt.Errorf("save tenant %s config: %w", req.ServiceType, err)
	}
	return recordFromTenant(row), nil
}

// DeleteTenant removes a tenant's own configuration.
func (s *Store) DeleteTenant(ctx context.Context, tenantID uuid.UUID, service ServiceType) error {
	if err := s.q.DeleteTenantConfiguration(ctx, sqlc.DeleteTenantConfigurationParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
	}); err != nil {
		return fmt.Errorf("delete tenant %s config: %w", service, err)
	}
	return nil
}

/* ---------- preferences and access ---------- */

// Preference is a tenant's elected source for a service.
type Preference struct {
	ServiceType ServiceType
	Source      Source
	Set         bool
}

// GetPreference returns a tenant's elected source. When unset, the tenant
// defaults to its own configuration — never the platform's.
func (s *Store) GetPreference(ctx context.Context, tenantID uuid.UUID, service ServiceType) (Preference, error) {
	row, err := s.q.GetTenantServicePreference(ctx, sqlc.GetTenantServicePreferenceParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
	})
	if err != nil {
		if isNoRows(err) {
			return Preference{ServiceType: service, Source: SourceOrganization, Set: false}, nil
		}
		return Preference{}, fmt.Errorf("get preference: %w", err)
	}
	return Preference{ServiceType: service, Source: Source(row.Source), Set: true}, nil
}

// ListPreferences returns every preference for a tenant.
func (s *Store) ListPreferences(ctx context.Context, tenantID uuid.UUID) (map[ServiceType]Preference, error) {
	rows, err := s.q.ListTenantServicePreferences(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, fmt.Errorf("list preferences: %w", err)
	}
	out := map[ServiceType]Preference{}
	for _, row := range rows {
		out[ServiceType(row.ServiceType)] = Preference{
			ServiceType: ServiceType(row.ServiceType), Source: Source(row.Source), Set: true,
		}
	}
	return out, nil
}

// SetPreference records a tenant's elected source.
func (s *Store) SetPreference(ctx context.Context, tenantID uuid.UUID, service ServiceType, source Source) error {
	_, err := s.q.UpsertTenantServicePreference(ctx, sqlc.UpsertTenantServicePreferenceParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
		Source:      string(source),
	})
	if err != nil {
		return fmt.Errorf("set preference: %w", err)
	}
	return nil
}

// Access is the per-tenant permission to borrow a platform configuration.
type Access struct {
	ServiceType   ServiceType
	AllowPlatform bool
	Set           bool
}

// GetAccess returns whether a tenant may use the platform configuration.
func (s *Store) GetAccess(ctx context.Context, tenantID uuid.UUID, service ServiceType) (Access, error) {
	row, err := s.q.GetTenantServiceAccess(ctx, sqlc.GetTenantServiceAccessParams{
		TenantID:    pgutil.UUID(tenantID),
		ServiceType: string(service),
	})
	if err != nil {
		if isNoRows(err) {
			return Access{ServiceType: service, AllowPlatform: false, Set: false}, nil
		}
		return Access{}, fmt.Errorf("get access: %w", err)
	}
	return Access{ServiceType: service, AllowPlatform: row.AllowPlatform, Set: true}, nil
}

// ListAccess returns every access grant for a tenant.
func (s *Store) ListAccess(ctx context.Context, tenantID uuid.UUID) (map[ServiceType]Access, error) {
	rows, err := s.q.ListTenantServiceAccess(ctx, pgutil.UUID(tenantID))
	if err != nil {
		return nil, fmt.Errorf("list access: %w", err)
	}
	out := map[ServiceType]Access{}
	for _, row := range rows {
		out[ServiceType(row.ServiceType)] = Access{
			ServiceType: ServiceType(row.ServiceType), AllowPlatform: row.AllowPlatform, Set: true,
		}
	}
	return out, nil
}

// SetAccess grants or revokes a tenant's permission to use a platform config.
func (s *Store) SetAccess(ctx context.Context, tenantID uuid.UUID, service ServiceType, allow bool) error {
	_, err := s.q.UpsertTenantServiceAccess(ctx, sqlc.UpsertTenantServiceAccessParams{
		TenantID:      pgutil.UUID(tenantID),
		ServiceType:   string(service),
		AllowPlatform: allow,
	})
	if err != nil {
		return fmt.Errorf("set access: %w", err)
	}
	return nil
}

/* ---------- helpers ---------- */

// deriveStatus computes the status a freshly saved row should carry. Enabling
// without a stored secret would be a lie, so it is recorded as CONFIGURED.
func deriveStatus(enabled bool, secrets map[string]string, fallback Status) Status {
	if !enabled {
		if len(secrets) == 0 {
			return StatusUnconfigured
		}
		return StatusDisabled
	}
	for _, v := range secrets {
		if strings.TrimSpace(v) != "" {
			return StatusEnabled
		}
	}
	return StatusConfigured
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
