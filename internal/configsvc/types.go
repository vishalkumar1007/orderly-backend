// Package configsvc is the single place where provider configuration (SMTP,
// object storage, AI) is stored, resolved and consumed.
//
// The design has one rule that everything else follows: the *backend* decides
// which configuration is effective. A tenant records a preference — PLATFORM or
// ORGANIZATION — and the resolver turns that preference plus the stored rows
// into a concrete Resolved value. Business code never reads the configuration
// tables and never branches on the source itself.
//
// Two value types carry the security boundary:
//
//	Resolved   holds decrypted secrets. Backend-only. Never serialised.
//	PublicView holds display metadata with no secrets. The only shape that
//	           reaches an HTTP response.
package configsvc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ServiceType identifies a configurable integration.
type ServiceType string

const (
	ServiceSMTP    ServiceType = "SMTP"
	ServiceStorage ServiceType = "STORAGE"
	ServiceAI      ServiceType = "AI"
	ServiceSMS     ServiceType = "SMS"
)

// AllServices is the canonical ordering used by every listing endpoint.
var AllServices = []ServiceType{ServiceSMTP, ServiceStorage, ServiceAI, ServiceSMS}

// ParseServiceType validates a service segment from a URL.
func ParseServiceType(v string) (ServiceType, error) {
	switch ServiceType(strings.ToUpper(strings.TrimSpace(v))) {
	case ServiceSMTP:
		return ServiceSMTP, nil
	case ServiceStorage:
		return ServiceStorage, nil
	case ServiceAI:
		return ServiceAI, nil
	case ServiceSMS:
		return ServiceSMS, nil
	}
	return "", fmt.Errorf("%w: unknown service %q", ErrInvalidRequest, v)
}

// Label is the human name used in the UI.
func (s ServiceType) Label() string {
	switch s {
	case ServiceSMTP:
		return "Email / SMTP"
	case ServiceStorage:
		return "Storage"
	case ServiceAI:
		return "AI"
	case ServiceSMS:
		return "SMS"
	}
	return string(s)
}

// Source is the level a tenant has elected to use.
type Source string

const (
	SourcePlatform     Source = "PLATFORM"
	SourceOrganization Source = "ORGANIZATION"
)

// ParseSource validates a source value.
func ParseSource(v string) (Source, error) {
	switch Source(strings.ToUpper(strings.TrimSpace(v))) {
	case SourcePlatform:
		return SourcePlatform, nil
	case SourceOrganization:
		return SourceOrganization, nil
	}
	return "", fmt.Errorf("%w: source must be PLATFORM or ORGANIZATION", ErrInvalidRequest)
}

// Status is the lifecycle state of a stored configuration. It maps directly to
// the status badges the console renders.
type Status string

const (
	StatusUnconfigured     Status = "UNCONFIGURED"
	StatusConfigured       Status = "CONFIGURED"
	StatusEnabled          Status = "ENABLED"
	StatusDisabled         Status = "DISABLED"
	StatusConnectionFailed Status = "CONNECTION_FAILED"
	StatusTesting          Status = "TESTING"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusUnconfigured, StatusConfigured, StatusEnabled, StatusDisabled,
		StatusConnectionFailed, StatusTesting:
		return true
	}
	return false
}

// Sentinel errors. Handlers map these onto HTTP status codes.
var (
	ErrNotFound       = errors.New("configuration not found")
	ErrUnavailable    = errors.New("configuration unavailable")
	ErrNotPermitted   = errors.New("not permitted to use this configuration")
	ErrInvalidRequest = errors.New("invalid request")
	ErrDisabled       = errors.New("configuration is disabled")
)

// UnavailableError explains why an effective configuration could not be
// produced. It is safe to show a tenant: it never carries secret material.
type UnavailableError struct {
	Service ServiceType
	Source  Source
	Reason  string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s configuration unavailable: %s", e.Service, e.Reason)
}

func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// Hint returns tenant-facing guidance for the failure.
func (e *UnavailableError) Hint() string {
	switch e.Source {
	case SourcePlatform:
		return "Platform " + string(e.Service) + " configuration is currently unavailable. " +
			"Please configure organization " + string(e.Service) + " or contact your platform administrator."
	default:
		return "Your organization has not configured " + string(e.Service) + " yet. " +
			"Complete the setup or contact your platform administrator."
	}
}

/* ------------------------------------------------------------------ *
 * Provider interfaces
 * ------------------------------------------------------------------ */

// MailMessage is a single outbound email.
type MailMessage struct {
	To          []string
	Subject     string
	Body        string
	ReplyTo     string
	ContentType string // defaults to text/plain
}

// EmailProvider sends mail over a concrete transport.
type EmailProvider interface {
	// Provider is the provider identifier this instance was built from.
	Provider() string
	// TestConnection verifies credentials and reachability without sending.
	TestConnection(ctx context.Context) error
	// Send delivers one message.
	Send(ctx context.Context, msg MailMessage) error
	// FromAddress is the default sender, for display in the UI.
	FromAddress() string
}

// SMSProvider sends text messages over a concrete transport.
type SMSProvider interface {
	// Provider is the provider identifier this instance was built from.
	Provider() string
	// TestConnection verifies credentials without sending a message.
	TestConnection(ctx context.Context) error
	// Send delivers one message and returns the provider's message id.
	Send(ctx context.Context, to, body string) (messageID string, err error)
	// FromNumber is the default sender, for display in the UI.
	FromNumber() string
}

// StoredObject describes an object written to storage.
type StoredObject struct {
	Key         string
	Size        int64
	ContentType string
	URL         string
}

// PutRequest is a single storage write.
type PutRequest struct {
	Key         string
	Body        []byte
	ContentType string
}

// StorageProvider reads and writes objects in a bucket.
type StorageProvider interface {
	Provider() string
	// TestConnection verifies the bucket is reachable and credentials work.
	TestConnection(ctx context.Context) error
	// Put writes an object and returns its descriptor.
	Put(ctx context.Context, req PutRequest) (StoredObject, error)
	// Delete removes an object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// PresignGet returns a time-limited download URL, so private buckets still
	// work without making objects world-readable.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PublicURL is the CDN/public URL for a key, or "" when the bucket is
	// private and only presigned links are available.
	PublicURL(key string) string
}

// CompletionRequest is a chat/completion call.
type CompletionRequest struct {
	Model     string
	System    string
	Prompt    string
	MaxTokens int
	Temp      *float64
}

// CompletionResponse is a normalised completion result, so callers are not
// coupled to any one provider's response shape.
type CompletionResponse struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
}

// EmbeddingRequest is a vectorisation call.
type EmbeddingRequest struct {
	Model string
	Input []string
}

// EmbeddingResponse is a normalised embedding result.
type EmbeddingResponse struct {
	Model      string
	Dimensions int
	Values     [][]float32
}

// AIProvider performs completions and embeddings.
type AIProvider interface {
	Provider() string
	// TestConnection checks the endpoint answers and the key is accepted.
	TestConnection(ctx context.Context) error
	// ListModels returns the model ids the endpoint advertises, when supported.
	ListModels(ctx context.Context) ([]string, error)
	// Complete generates text.
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	// Embed turns text into vectors.
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
	// DefaultModel is the configured default completion model.
	DefaultModel() string
	// EmbeddingModel is the configured default embedding model.
	EmbeddingModel() string
}

// SortServices orders service types canonically, tolerating unknown values by
// placing them last so a typo cannot scramble the UI ordering.
func SortServices(in []ServiceType) {
	rank := map[ServiceType]int{ServiceSMTP: 0, ServiceStorage: 1, ServiceAI: 2, ServiceSMS: 3}
	sort.SliceStable(in, func(i, j int) bool {
		ri, oki := rank[in[i]]
		rj, okj := rank[in[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return in[i] < in[j]
		}
	})
}
