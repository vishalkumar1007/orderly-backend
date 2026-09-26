package configsvc

// Storage provider identifiers. Every one of these speaks the S3 API, so a
// single implementation serves all of them; the provider only changes defaults
// and the addressing style.
const (
	ProviderS3           = "s3"    // Amazon S3
	ProviderCloudflareR2 = "r2"    // Cloudflare R2
	ProviderMinIO        = "minio" // MinIO and self-hosted gateways
)

// AI provider identifiers.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
	ProviderCustom    = "custom" // any OpenAI-compatible endpoint
)

// Email provider identifiers.
const (
	ProviderSMTP = "smtp"
)

// StorageProviders is the catalogue the Super Admin UI renders.
var StorageProviders = []ProviderInfo{
	{Value: ProviderS3, Label: "Amazon S3", NeedsEndpoint: false, DefaultRegion: "us-east-1"},
	{Value: ProviderCloudflareR2, Label: "Cloudflare R2", NeedsEndpoint: true, DefaultRegion: "auto"},
	{Value: ProviderMinIO, Label: "MinIO / self-hosted", NeedsEndpoint: true, DefaultRegion: "us-east-1"},
}

// AIProviders is the catalogue the Super Admin UI renders.
var AIProviders = []ProviderInfo{
	{Value: ProviderOpenAI, Label: "OpenAI", BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-4o-mini", DefaultEmbeddingModel: "text-embedding-3-small"},
	{Value: ProviderAnthropic, Label: "Anthropic", BaseURL: "https://api.anthropic.com/v1", DefaultModel: "claude-3-5-sonnet-latest", DefaultEmbeddingModel: ""},
	{Value: ProviderGemini, Label: "Google Gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", DefaultModel: "gemini-1.5-flash", DefaultEmbeddingModel: "text-embedding-004"},
	{Value: ProviderCustom, Label: "OpenAI-compatible endpoint", BaseURL: "", DefaultModel: "", DefaultEmbeddingModel: ""},
}

// EmailProviders is the catalogue the Super Admin UI renders. SMTP is the only
// transport today; the list exists so adding a transactional API provider later
// is a data change rather than a code change.
var EmailProviders = []ProviderInfo{
	{Value: ProviderSMTP, Label: "SMTP", NeedsEndpoint: false},
}

// knownAIProviders is the validation allowlist.
var knownAIProviders = func() map[string]struct{} {
	m := map[string]struct{}{}
	for _, p := range AIProviders {
		m[p.Value] = struct{}{}
	}
	return m
}()

// ProviderInfo describes a provider choice for the UI.
type ProviderInfo struct {
	Value                 string `json:"value"`
	Label                 string `json:"label"`
	BaseURL               string `json:"base_url"`
	NeedsEndpoint         bool   `json:"needs_endpoint"`
	DefaultRegion         string `json:"default_region"`
	DefaultModel          string `json:"default_model"`
	DefaultEmbeddingModel string `json:"default_embedding_model"`
}

// ProvidersFor returns the catalogue for a service.
func ProvidersFor(service ServiceType) []ProviderInfo {
	switch service {
	case ServiceSMTP:
		return EmailProviders
	case ServiceStorage:
		return StorageProviders
	case ServiceAI:
		return AIProviders
	}
	return nil
}

// IsKnownProvider reports whether provider is valid for the service.
func IsKnownProvider(service ServiceType, provider string) bool {
	for _, p := range ProvidersFor(service) {
		if p.Value == provider {
			return true
		}
	}
	return false
}
