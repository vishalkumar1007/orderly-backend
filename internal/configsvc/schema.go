package configsvc

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Sentinel the UI sends instead of a real secret when a field is left untouched.
// A masked submit means "keep whatever is already stored", which is what lets
// the console render a form containing a secret it is not allowed to read.
const MaskSentinel = "__KEEP__"

// SecretFields lists, per service, the config keys whose values are secret.
// Everything named here is sealed before it reaches the database and is never
// included in a PublicView.
var secretFields = map[ServiceType][]string{
	ServiceSMTP:    {"password"},
	ServiceStorage: {"secret_key"},
	ServiceAI:      {"api_key"},
}

// SecretKeys returns the secret field names for a service.
func SecretKeys(service ServiceType) []string {
	fields := secretFields[service]
	out := make([]string, len(fields))
	copy(out, fields)
	return out
}

// IsSecretField reports whether key holds secret material for this service.
func IsSecretField(service ServiceType, key string) bool {
	for _, k := range secretFields[service] {
		if k == key {
			return true
		}
	}
	return false
}

/* ------------------------------------------------------------------ *
 * SMTP
 * ------------------------------------------------------------------ */

// Encryption selects the transport security mode for SMTP.
type Encryption string

const (
	EncryptionTLS      Encryption = "TLS"
	EncryptionSTARTTLS Encryption = "STARTTLS"
	EncryptionNone     Encryption = "NONE"
)

// ParseEncryption validates and normalises the encryption mode.
func ParseEncryption(v string) (Encryption, error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "TLS":
		return EncryptionTLS, nil
	case "STARTTLS", "":
		return EncryptionSTARTTLS, nil
	case "NONE":
		return EncryptionNone, nil
	}
	return "", fmt.Errorf("%w: encryption must be TLS, STARTTLS or NONE", ErrInvalidRequest)
}

// SMTPConfig is the non-secret half of an SMTP configuration.
type SMTPConfig struct {
	Provider   string `json:"provider"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Encryption string `json:"encryption"`
	Username   string `json:"username"`
	FromName   string `json:"from_name"`
	FromEmail  string `json:"from_email"`
	ReplyTo    string `json:"reply_to"`
}

// Defaults applied when a field is omitted.
func (c *SMTPConfig) applyDefaults() {
	if c.Port == 0 {
		c.Port = 587
	}
	if strings.TrimSpace(c.Encryption) == "" {
		c.Encryption = string(EncryptionSTARTTLS)
	}
	c.Host = strings.TrimSpace(c.Host)
	c.Username = strings.TrimSpace(c.Username)
	c.FromName = strings.TrimSpace(c.FromName)
	c.FromEmail = strings.ToLower(strings.TrimSpace(c.FromEmail))
	c.ReplyTo = strings.ToLower(strings.TrimSpace(c.ReplyTo))
}

// Validate checks an SMTP configuration is usable.
func (c SMTPConfig) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("%w: smtp host is required", ErrInvalidRequest)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("%w: smtp port must be between 1 and 65535", ErrInvalidRequest)
	}
	if _, err := ParseEncryption(c.Encryption); err != nil {
		return err
	}
	if !looksLikeEmail(c.FromEmail) {
		return fmt.Errorf("%w: a valid from_email is required", ErrInvalidRequest)
	}
	if c.ReplyTo != "" && !looksLikeEmail(c.ReplyTo) {
		return fmt.Errorf("%w: reply_to must be a valid email address", ErrInvalidRequest)
	}
	// Implicit TLS is the only mode that does not advertise STARTTLS support.
	return nil
}

/* ------------------------------------------------------------------ *
 * Storage
 * ------------------------------------------------------------------ */

// StorageConfig is the non-secret half of a storage configuration. One shape
// covers every S3-compatible backend; the provider field selects the defaults
// and the signature variant.
type StorageConfig struct {
	Provider       string `json:"provider"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	AccessKey      string `json:"access_key"`
	PublicURL      string `json:"public_url"`
	PathPrefix     string `json:"path_prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
}

func (c *StorageConfig) applyDefaults() {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.Endpoint = strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	c.Region = strings.TrimSpace(c.Region)
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.AccessKey = strings.TrimSpace(c.AccessKey)
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	c.PathPrefix = strings.Trim(strings.TrimSpace(c.PathPrefix), "/")
	if c.Provider == "" {
		c.Provider = ProviderS3
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	// MinIO and most self-hosted gateways only support path-style addressing.
	if c.Provider == ProviderMinIO {
		c.ForcePathStyle = true
	}
}

// Validate checks a storage configuration is usable.
func (c StorageConfig) Validate() error {
	if strings.TrimSpace(c.Bucket) == "" {
		return fmt.Errorf("%w: bucket is required", ErrInvalidRequest)
	}
	if c.AccessKey == "" {
		return fmt.Errorf("%w: access_key is required", ErrInvalidRequest)
	}
	if c.Endpoint != "" && !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("%w: endpoint must start with http:// or https://", ErrInvalidRequest)
	}
	// A bucket name must be DNS-safe; catching it here beats a 400 from the API.
	if strings.ContainsAny(c.Bucket, " /\\") {
		return fmt.Errorf("%w: bucket name contains invalid characters", ErrInvalidRequest)
	}
	return nil
}

/* ------------------------------------------------------------------ *
 * AI
 * ------------------------------------------------------------------ */

// AIConfig is the non-secret half of an AI configuration.
type AIConfig struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	OrganizationID  string `json:"organization_id"`
	DefaultModel    string `json:"default_model"`
	EmbeddingModel  string `json:"embedding_model"`
	RequestTimeoutS int    `json:"request_timeout_seconds"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	RateLimitRPM    int    `json:"rate_limit_per_minute"`
}

func (c *AIConfig) applyDefaults() {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.OrganizationID = strings.TrimSpace(c.OrganizationID)
	c.DefaultModel = strings.TrimSpace(c.DefaultModel)
	c.EmbeddingModel = strings.TrimSpace(c.EmbeddingModel)
	if c.Provider == "" {
		c.Provider = ProviderOpenAI
	}
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL(c.Provider)
	}
	if c.RequestTimeoutS <= 0 {
		c.RequestTimeoutS = 60
	}
	if c.RequestTimeoutS > 600 {
		c.RequestTimeoutS = 600
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = 1024
	}
}

// Validate checks an AI configuration is usable.
func (c AIConfig) Validate() error {
	if _, ok := knownAIProviders[c.Provider]; !ok {
		return fmt.Errorf("%w: unknown ai provider %q", ErrInvalidRequest, c.Provider)
	}
	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("%w: base_url must start with http:// or https://", ErrInvalidRequest)
	}
	if c.RequestTimeoutS < 1 || c.RequestTimeoutS > 600 {
		return fmt.Errorf("%w: request_timeout_seconds must be between 1 and 600", ErrInvalidRequest)
	}
	return nil
}

func defaultBaseURL(provider string) string {
	switch provider {
	case ProviderAnthropic:
		return "https://api.anthropic.com/v1"
	case ProviderGemini:
		return "https://generativelanguage.googleapis.com/v1beta/openai"
	case ProviderOpenAI:
		return "https://api.openai.com/v1"
	}
	return ""
}

/* ------------------------------------------------------------------ *
 * Helpers
 * ------------------------------------------------------------------ */

// looksLikeEmail is a deliberately permissive check. Deliverability is the
// server's problem; we only reject things that are obviously not addresses.
func looksLikeEmail(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || !strings.Contains(v, "@") {
		return false
	}
	at := strings.LastIndex(v, "@")
	if at == 0 || at == len(v)-1 {
		return false
	}
	domain := v[at+1:]
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") &&
		!strings.HasSuffix(domain, ".")
}

// decodeConfig unmarshals the non-secret config blob into a typed struct.
func decodeConfig(service ServiceType, raw []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: stored %s config is not valid json", ErrInvalidRequest, service)
	}
	return out, nil
}
