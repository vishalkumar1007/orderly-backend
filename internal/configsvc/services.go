package configsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service is the facade business code depends on. It owns the resolver and the
// provider factory, so nothing outside this package reads configuration tables
// or constructs a provider directly.
//
//	resolver decides WHICH configuration applies
//	factory  decides HOW to talk to it
type Service struct {
	store    *Store
	resolver *Resolver
	factory  *Factory
	log      Logger
}

// NewService wires the service together.
func NewService(store *Store, resolver *Resolver, log Logger) *Service {
	return &Service{store: store, resolver: resolver, factory: NewFactory(), log: log}
}

// Store exposes the store for the HTTP layer, which needs it to render views.
func (s *Service) Store() *Store { return s.store }

// Resolver exposes the resolver for the HTTP layer's option/permission views.
func (s *Service) Resolver() *Resolver { return s.resolver }

// Factory exposes the provider factory for connection tests.
func (s *Service) Factory() *Factory { return s.factory }

/* ------------------------------------------------------------------ *
 * Notifications
 * ------------------------------------------------------------------ */

// Notifications sends tenant mail through whichever configuration resolves for
// that tenant.
type Notifications struct {
	svc *Service
}

// Notifications returns the notification facade.
func (s *Service) Notifications() *Notifications { return &Notifications{svc: s} }

// Send delivers a message using the tenant's effective SMTP configuration.
func (n *Notifications) Send(ctx context.Context, tenantID *uuid.UUID, msg MailMessage) error {
	provider, err := n.svc.emailProvider(ctx, tenantID, msg)
	if err != nil {
		return err
	}
	if err := provider.Send(ctx, msg); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}
	return nil
}

// SendPlatform delivers a platform-scoped message, such as a tenant invitation.
// It deliberately does not consider any tenant's configuration.
func (n *Notifications) SendPlatform(ctx context.Context, msg MailMessage) error {
	provider, err := n.svc.emailProvider(ctx, nil, msg)
	if err != nil {
		return err
	}
	if err := provider.Send(ctx, msg); err != nil {
		return fmt.Errorf("send platform mail: %w", err)
	}
	return nil
}

// SendTest delivers a test message and reports whether it went out. The
// recipient must be supplied explicitly so a test cannot be aimed at an
// arbitrary address.
func (n *Notifications) SendTest(ctx context.Context, tenantID *uuid.UUID, to string) (TestOutcome, error) {
	to = strings.TrimSpace(to)
	if !looksLikeEmail(to) {
		return TestOutcome{}, fmt.Errorf("%w: a valid recipient address is required", ErrInvalidRequest)
	}
	provider, err := n.svc.emailProvider(ctx, tenantID, MailMessage{To: []string{to}})
	if err != nil {
		return TestOutcome{}, err
	}

	label := "your organization"
	if tenantID == nil {
		label = "the platform"
	}
	msg := MailMessage{
		To:      []string{to},
		Subject: "Orderly SMTP test",
		Body: fmt.Sprintf("This is a test message sent by %s.\r\n\r\n"+
			"If you received it, outgoing email is configured correctly.\r\n", label),
	}
	if err := provider.Send(ctx, msg); err != nil {
		return TestOutcome{OK: false, Message: "The test message could not be sent", Detail: SafeErrorMessage(err)}, nil
	}
	return TestOutcome{OK: true, Message: "Test message sent to " + to}, nil
}

// emailProvider resolves configuration and builds a provider. A test message
// forces the connection open even when the configuration is saved as disabled,
// so an operator can verify a server before switching it on.
func (s *Service) emailProvider(ctx context.Context, tenantID *uuid.UUID, probe MailMessage) (EmailProvider, error) {
	resolved, err := s.resolveForProbe(ctx, tenantID, ServiceSMTP, len(probe.To) > 0)
	if err != nil {
		return nil, err
	}
	return s.factory.Email(resolved)
}

// scopeFor maps an optional tenant onto the matching resolution scope.
func scopeFor(tenantID *uuid.UUID) Scope {
	if tenantID == nil {
		return ScopePlatform
	}
	return ScopeTenant
}

// resolveForProbe resolves, optionally tolerating a disabled configuration.
func (s *Service) resolveForProbe(ctx context.Context, tenantID *uuid.UUID, service ServiceType, probe bool) (*Resolved, error) {
	resolved, err := s.resolver.Resolve(ctx, scopeFor(tenantID), tenantID, service)
	if err == nil {
		return resolved, nil
	}
	if !probe {
		return nil, err
	}
	// A probe may run against a saved-but-disabled configuration, but never
	// against a level the caller is not permitted to use.
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) && strings.Contains(unavailable.Reason, "disabled") {
		return s.resolveIgnoringEnabled(ctx, tenantID, service)
	}
	return nil, err
}

func (s *Service) resolveIgnoringEnabled(ctx context.Context, tenantID *uuid.UUID, service ServiceType) (*Resolved, error) {
	if tenantID == nil {
		rec, err := s.store.GetPlatform(ctx, service)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, &UnavailableError{Service: service, Source: SourcePlatform, Reason: "no platform configuration has been saved yet"}
		}
		return s.resolver.hydrate(ctx, rec, SourcePlatform, nil, platformContext(service))
	}

	pref, err := s.store.GetPreference(ctx, *tenantID, service)
	if err != nil {
		return nil, err
	}
	if pref.Source == SourcePlatform {
		perm, err := s.resolver.PlatformPermitted(ctx, *tenantID, service)
		if err != nil {
			return nil, err
		}
		if !perm.Allowed {
			return nil, &UnavailableError{Service: service, Source: SourcePlatform, Reason: perm.Reason}
		}
		rec, err := s.store.GetPlatform(ctx, service)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, &UnavailableError{Service: service, Source: SourcePlatform, Reason: "no platform configuration has been saved yet"}
		}
		return s.resolver.hydrate(ctx, rec, SourcePlatform, tenantID, platformContext(service))
	}

	rec, err := s.store.GetTenant(ctx, *tenantID, service)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, &UnavailableError{Service: service, Source: SourceOrganization, Reason: "no organization configuration has been saved yet"}
	}
	return s.resolver.hydrate(ctx, rec, SourceOrganization, tenantID, tenantContext(*tenantID, service))
}

/* ------------------------------------------------------------------ *
 * Storage
 * ------------------------------------------------------------------ */

// Storage is the tenant-scoped object storage facade.
type Storage struct {
	svc *Service
}

// StorageService returns the storage facade.
func (s *Service) StorageService() *Storage { return &Storage{svc: s} }

// Put writes an object using the tenant's effective storage configuration.
func (st *Storage) Put(ctx context.Context, tenantID uuid.UUID, req PutRequest) (StoredObject, error) {
	provider, err := st.svc.storageProvider(ctx, &tenantID)
	if err != nil {
		return StoredObject{}, err
	}
	return provider.Put(ctx, req)
}

// PutPlatform writes an object using the platform's storage configuration.
func (st *Storage) PutPlatform(ctx context.Context, req PutRequest) (StoredObject, error) {
	provider, err := st.svc.storageProvider(ctx, nil)
	if err != nil {
		return StoredObject{}, err
	}
	return provider.Put(ctx, req)
}

// PresignGetPlatform returns a time-limited download URL for platform-scoped objects.
func (st *Storage) PresignGetPlatform(ctx context.Context, key string, ttl time.Duration) (string, error) {
	provider, err := st.svc.storageProvider(ctx, nil)
	if err != nil {
		return "", err
	}
	return provider.PresignGet(ctx, key, ttl)
}

// Delete removes an object.
func (st *Storage) Delete(ctx context.Context, tenantID uuid.UUID, key string) error {
	provider, err := st.svc.storageProvider(ctx, &tenantID)
	if err != nil {
		return err
	}
	return provider.Delete(ctx, key)
}

// PresignGet returns a time-limited download URL.
func (st *Storage) PresignGet(ctx context.Context, tenantID uuid.UUID, key string, ttl time.Duration) (string, error) {
	provider, err := st.svc.storageProvider(ctx, &tenantID)
	if err != nil {
		return "", err
	}
	return provider.PresignGet(ctx, key, ttl)
}

// UploadProbe writes and removes a small object, proving write access rather
// than only read access.
func (st *Storage) UploadProbe(ctx context.Context, tenantID uuid.UUID) (TestOutcome, error) {
	provider, err := st.svc.storageProvider(ctx, &tenantID)
	if err != nil {
		return TestOutcome{}, err
	}
	key := fmt.Sprintf(".orderly-probe/%d.txt", time.Now().UnixNano())
	content := []byte("orderly storage probe")

	obj, err := provider.Put(ctx, PutRequest{Key: key, Body: content, ContentType: "text/plain"})
	if err != nil {
		return TestOutcome{OK: false, Message: "The probe object could not be written", Detail: SafeErrorMessage(err)}, nil
	}
	// Always clean up, even if the delete fails: a stray probe is noise but not
	// a security issue, so the write is still reported as a success.
	if err := provider.Delete(ctx, obj.Key); err != nil && st.svc.log != nil {
		st.svc.log.Warn("storage probe cleanup failed", "error", SafeErrorMessage(err), "key", obj.Key)
	}
	return TestOutcome{OK: true, Message: "Probe object written and removed (" + obj.Key + ")"}, nil
}

func (s *Service) storageProvider(ctx context.Context, tenantID *uuid.UUID) (StorageProvider, error) {
	resolved, err := s.resolver.Resolve(ctx, ScopeTenant, tenantID, ServiceStorage)
	if err != nil {
		return nil, err
	}
	return s.factory.Storage(resolved)
}

/* ------------------------------------------------------------------ *
 * AI
 * ------------------------------------------------------------------ */

// AI is the tenant-scoped AI facade.
type AI struct {
	svc *Service
}

// AIService returns the AI facade.
func (s *Service) AIService() *AI { return &AI{svc: s} }

// Complete generates text using the tenant's effective AI configuration.
func (a *AI) Complete(ctx context.Context, tenantID uuid.UUID, req CompletionRequest) (CompletionResponse, error) {
	provider, err := a.svc.aiProvider(ctx, &tenantID)
	if err != nil {
		return CompletionResponse{}, err
	}
	return provider.Complete(ctx, req)
}

// Embed turns text into vectors using the tenant's effective AI configuration.
func (a *AI) Embed(ctx context.Context, tenantID uuid.UUID, req EmbeddingRequest) (EmbeddingResponse, error) {
	provider, err := a.svc.aiProvider(ctx, &tenantID)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	return provider.Embed(ctx, req)
}

func (s *Service) aiProvider(ctx context.Context, tenantID *uuid.UUID) (AIProvider, error) {
	resolved, err := s.resolver.Resolve(ctx, ScopeTenant, tenantID, ServiceAI)
	if err != nil {
		return nil, err
	}
	return s.factory.AI(resolved)
}

/* ------------------------------------------------------------------ *
 * Connection tests
 * ------------------------------------------------------------------ */

// TestConnection verifies a configuration without saving it, so an operator
// can check a server before committing. resolved is supplied by the caller,
// which has already validated the request.
func (s *Service) TestConnection(ctx context.Context, resolved *Resolved) (TestOutcome, error) {
	return s.factory.runConnectionTest(ctx, resolved)
}
