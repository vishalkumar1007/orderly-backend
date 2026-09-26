package configsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Factory builds providers from resolved configuration. Business services take
// a Factory, never a table reader, so no caller can accidentally bypass
// resolution.
type Factory struct{}

// NewFactory returns a Factory.
func NewFactory() *Factory { return &Factory{} }

// Email builds an EmailProvider. An unknown provider is a programming error
// rather than a user-facing condition, because the provider is validated on
// save and only ever comes from stored configuration.
func (f *Factory) Email(r *Resolved) (EmailProvider, error) {
	if r == nil {
		return nil, ErrUnavailable
	}
	switch r.Provider {
	case ProviderSMTP, "":
		return newSMTPProvider(r), nil
	}
	return nil, fmt.Errorf("unsupported email provider %q", r.Provider)
}

// Storage builds a StorageProvider. Every listed provider speaks the S3 API, so
// one implementation covers S3, R2 and MinIO.
func (f *Factory) Storage(r *Resolved) (StorageProvider, error) {
	if r == nil {
		return nil, ErrUnavailable
	}
	switch r.Provider {
	case ProviderS3, ProviderCloudflareR2, ProviderMinIO, "":
		return newS3Provider(r)
	}
	return nil, fmt.Errorf("unsupported storage provider %q", r.Provider)
}

// AI builds an AIProvider.
func (f *Factory) AI(r *Resolved) (AIProvider, error) {
	if r == nil {
		return nil, ErrUnavailable
	}
	switch r.Provider {
	case ProviderOpenAI, ProviderGemini, ProviderCustom, "":
		return newOpenAICompatibleProvider(r), nil
	case ProviderAnthropic:
		return newAnthropicProvider(r), nil
	}
	return nil, fmt.Errorf("unsupported ai provider %q", r.Provider)
}

/* ---------- validation ---------- */

// normalizeWriteRequest validates an incoming save for a service and returns the
// cleaned config plus the resolved provider. The provider defaults to the
// service's only choice when omitted, so a minimal SMTP form needs no provider
// picker.
func normalizeWriteRequest(service ServiceType, provider string, config map[string]any) (WriteRequest, error) {
	cfg := map[string]any{}
	for k, v := range config {
		cfg[k] = v
	}

	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		cfg["provider"] = p
	} else if existing, ok := cfg["provider"].(string); ok && existing != "" {
		cfg["provider"] = strings.ToLower(strings.TrimSpace(existing))
	}

	var resolvedProvider string
	switch service {
	case ServiceSMTP:
		resolvedProvider = ProviderSMTP
		cfg["provider"] = ProviderSMTP
		smtp := smtpFrom(cfg)
		smtp.applyDefaults()
		smtp.Provider = ProviderSMTP
		if err := smtp.Validate(); err != nil {
			return WriteRequest{}, err
		}
		// Re-emit the normalised values so the stored row is canonical.
		_ = mergeBack(cfg, smtp)

	case ServiceStorage:
		resolvedProvider = asString(cfg["provider"])
		storage := storageFrom(cfg)
		storage.applyDefaults()
		if resolvedProvider == "" {
			resolvedProvider = storage.Provider
		}
		if !IsKnownProvider(ServiceStorage, resolvedProvider) {
			return WriteRequest{}, fmt.Errorf("%w: unknown storage provider %q", ErrInvalidRequest, resolvedProvider)
		}
		storage.Provider = resolvedProvider
		if err := storage.Validate(); err != nil {
			return WriteRequest{}, err
		}
		_ = mergeBack(cfg, storage)

	case ServiceAI:
		resolvedProvider = asString(cfg["provider"])
		ai := aiFrom(cfg)
		ai.applyDefaults()
		if resolvedProvider == "" {
			resolvedProvider = ai.Provider
		}
		if !IsKnownProvider(ServiceAI, resolvedProvider) {
			return WriteRequest{}, fmt.Errorf("%w: unknown ai provider %q", ErrInvalidRequest, resolvedProvider)
		}
		ai.Provider = resolvedProvider
		if err := ai.Validate(); err != nil {
			return WriteRequest{}, err
		}
		_ = mergeBack(cfg, ai)

	default:
		return WriteRequest{}, fmt.Errorf("%w: unknown service %q", ErrInvalidRequest, service)
	}

	return WriteRequest{ServiceType: service, Provider: resolvedProvider, Config: cfg}, nil
}

// mergeBack writes the typed struct's normalised values back into the generic
// config map, so what we validated is exactly what we persist.
func mergeBack(cfg map[string]any, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var normalised map[string]any
	if err := json.Unmarshal(raw, &normalised); err != nil {
		return err
	}
	for k, val := range normalised {
		cfg[k] = val
	}
	return nil
}

func smtpFrom(cfg map[string]any) SMTPConfig {
	return SMTPConfig{
		Provider:   asString(cfg["provider"]),
		Host:       asString(cfg["host"]),
		Port:       asInt(cfg["port"]),
		Encryption: asString(cfg["encryption"]),
		Username:   asString(cfg["username"]),
		FromName:   asString(cfg["from_name"]),
		FromEmail:  asString(cfg["from_email"]),
		ReplyTo:    asString(cfg["reply_to"]),
	}
}

func storageFrom(cfg map[string]any) StorageConfig {
	return StorageConfig{
		Provider:       asString(cfg["provider"]),
		Endpoint:       asString(cfg["endpoint"]),
		Region:         asString(cfg["region"]),
		Bucket:         asString(cfg["bucket"]),
		AccessKey:      asString(cfg["access_key"]),
		PublicURL:      asString(cfg["public_url"]),
		PathPrefix:     asString(cfg["path_prefix"]),
		ForcePathStyle: asBool(cfg["force_path_style"]),
	}
}

func aiFrom(cfg map[string]any) AIConfig {
	return AIConfig{
		Provider:        asString(cfg["provider"]),
		BaseURL:         asString(cfg["base_url"]),
		OrganizationID:  asString(cfg["organization_id"]),
		DefaultModel:    asString(cfg["default_model"]),
		EmbeddingModel:  asString(cfg["embedding_model"]),
		RequestTimeoutS: asInt(cfg["request_timeout_seconds"]),
		MaxOutputTokens: asInt(cfg["max_output_tokens"]),
		RateLimitRPM:    asInt(cfg["rate_limit_per_minute"]),
	}
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return n
		}
	}
	return 0
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

/* ---------- shared test plumbing ---------- */

// TestRequest asks a provider to verify itself.
type TestRequest struct {
	// Service identifies which provider to build.
	Service ServiceType
	// Action is provider-specific: "connection" always, plus "send_email" for
	// SMTP and "upload" for storage.
	Action string
	// To is the recipient for a send_email action.
	To string
	// Key is the object name for an upload action.
	Key string
	// Content is the payload for an upload action.
	Content []byte
}

// TestOutcome reports the result of a provider test.
type TestOutcome struct {
	OK      bool
	Message string
	Detail  string
}

// runConnectionTest builds the provider and asks it to verify itself.
func (f *Factory) runConnectionTest(ctx context.Context, r *Resolved) (TestOutcome, error) {
	switch r.Service {
	case ServiceSMTP:
		p, err := f.Email(r)
		if err != nil {
			return TestOutcome{}, err
		}
		if err := p.TestConnection(ctx); err != nil {
			return TestOutcome{OK: false, Message: "Could not reach the SMTP server", Detail: SafeErrorMessage(err)}, nil
		}
		return TestOutcome{OK: true, Message: "SMTP connection succeeded"}, nil

	case ServiceStorage:
		p, err := f.Storage(r)
		if err != nil {
			return TestOutcome{}, err
		}
		if err := p.TestConnection(ctx); err != nil {
			return TestOutcome{OK: false, Message: "Could not reach the bucket", Detail: SafeErrorMessage(err)}, nil
		}
		return TestOutcome{OK: true, Message: "Bucket is reachable"}, nil

	case ServiceAI:
		p, err := f.AI(r)
		if err != nil {
			return TestOutcome{}, err
		}
		if err := p.TestConnection(ctx); err != nil {
			return TestOutcome{OK: false, Message: "Could not reach the AI endpoint", Detail: SafeErrorMessage(err)}, nil
		}
		return TestOutcome{OK: true, Message: "AI endpoint responded"}, nil
	}
	return TestOutcome{}, fmt.Errorf("%w: unknown service %q", ErrInvalidRequest, r.Service)
}
