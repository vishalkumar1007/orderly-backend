package configsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

/* ------------------------------------------------------------------ *
 * Redaction — the security boundary
 * ------------------------------------------------------------------ */

// TestPublicViewNeverCarriesSecrets is the guardrail for the whole redaction
// design: whatever a stored record contains, a PublicView must not serialise
// any secret-shaped key.
func TestPublicViewNeverCarriesSecrets(t *testing.T) {
	for _, service := range AllServices {
		t.Run(string(service), func(t *testing.T) {
			rec := &Record{
				ID:           mustUUID("11111111-1111-1111-1111-111111111111"),
				ServiceType:  service,
				Provider:     "test",
				Status:       StatusEnabled,
				Enabled:      true,
				AllowTenants: true,
				UpdatedAt:    time.Now(),
				Config: map[string]any{
					"host": "smtp.test.local", "port": 587, "region": "us-east-1",
					"bucket": "media", "access_key": "AKIAEXAMPLE", "base_url": "https://x",
					// A secret smuggled into the non-secret column, as a bug or a
					// legacy row might.
					"password": "leaked", "secret_key": "leaked", "api_key": "leaked",
				},
				SealedSecrets: map[string]string{
					"password": "v1:AAAA", "secret_key": "v1:BBBB", "api_key": "v1:CCCC",
				},
			}
			view := NewPublicView(rec, SourcePlatform)
			raw, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, forbidden := range []string{"v1:AAAA", "v1:BBBB", "v1:CCCC", "leaked", "sealed"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("PublicView leaked %q: %s", forbidden, body)
				}
			}
			// Non-secret fields must survive redaction, or the UI is useless.
			if !strings.Contains(body, "smtp.test.local") && !strings.Contains(body, "AKIAEXAMPLE") {
				t.Errorf("redaction stripped non-secret fields too: %s", body)
			}
		})
	}
}

func TestPublicViewReportsSecretPresence(t *testing.T) {
	rec := &Record{
		ServiceType:   ServiceSMTP,
		SealedSecrets: map[string]string{"password": "v1:AAAA"},
	}
	view := NewPublicView(rec, SourcePlatform)
	if !view.HasSecret {
		t.Error("HasSecret must be true when the primary secret is stored")
	}
	if !view.HasAnySecret {
		t.Error("HasAnySecret must be true when any secret is stored")
	}
	// The UI needs the field names to know what to mask.
	if len(view.SecretFields) == 0 || view.SecretFields[0] != "password" {
		t.Errorf("SecretFields = %v, want [password]", view.SecretFields)
	}

	empty := NewPublicView(&Record{ServiceType: ServiceSMTP}, SourcePlatform)
	if empty.HasSecret || empty.HasAnySecret {
		t.Error("a record with no secrets must report none")
	}
}

func TestMaskedConfigMarksEverySecret(t *testing.T) {
	cfg := map[string]any{"host": "smtp.test.local"}
	masked := MaskedConfig(ServiceSMTP, cfg)
	if masked["password"] != MaskSentinel {
		t.Errorf("password = %v, want the mask sentinel", masked["password"])
	}
	if masked["host"] != "smtp.test.local" {
		t.Error("masking must not touch non-secret fields")
	}
	// The original must be untouched.
	if _, mutated := cfg["password"]; mutated {
		t.Error("MaskedConfig mutated its input")
	}
}

func TestSafeErrorMessageRedactsCredentials(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"dial tcp: connection refused", ""},
		{"failed with api_key=sk-live-abc123", "sk-live-abc123"},
		{"auth failed for password hunter2", "hunter2"},
		{"Authorization: Bearer eyJhbGciOi", "eyJhbGciOi"},
	}
	for _, tc := range cases {
		got := SafeErrorMessage(errString(tc.in))
		if tc.mustNotContain != "" && strings.Contains(got, tc.mustNotContain) {
			t.Errorf("SafeErrorMessage(%q) = %q, still contains the secret", tc.in, got)
		}
	}
	if SafeErrorMessage(nil) != "" {
		t.Error("SafeErrorMessage(nil) should be empty")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

/* ------------------------------------------------------------------ *
 * Normalisation and validation
 * ------------------------------------------------------------------ */

func TestNormalizeWriteRequestAppliesDefaults(t *testing.T) {
	write, err := NormalizeWriteRequest(ServiceSMTP, "", map[string]any{
		"host": "smtp.test.local", "from_email": "A@B.CO", "password": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if write.Provider != ProviderSMTP {
		t.Errorf("provider = %q, want smtp", write.Provider)
	}
	if got := write.Config["port"]; got != float64(587) && got != 587 {
		t.Errorf("port = %v, want the 587 default", got)
	}
	if got := write.Config["encryption"]; got != "STARTTLS" {
		t.Errorf("encryption = %v, want the STARTTLS default", got)
	}
	if got := write.Config["from_email"]; got != "a@b.co" {
		t.Errorf("from_email = %v, want it lower-cased", got)
	}
}

func TestNormalizeWriteRequestProviderDefaults(t *testing.T) {
	// Storage without a provider defaults to S3.
	write, err := NormalizeWriteRequest(ServiceStorage, "", map[string]any{
		"bucket": "b", "access_key": "k", "secret_key": "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if write.Provider != ProviderS3 {
		t.Errorf("provider = %q, want s3", write.Provider)
	}
	// MinIO implies path-style addressing.
	write, err = NormalizeWriteRequest(ServiceStorage, ProviderMinIO, map[string]any{
		"endpoint": "https://minio.local", "bucket": "b", "access_key": "k", "secret_key": "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if write.Config["force_path_style"] != true {
		t.Error("minio must default to path-style addressing")
	}
	if write.Config["region"] != "us-east-1" {
		t.Errorf("region = %v, want the us-east-1 default", write.Config["region"])
	}
}

func TestNormalizeWriteRequestRejectsUnknownProvider(t *testing.T) {
	for _, tc := range []struct {
		service  ServiceType
		provider string
	}{
		{ServiceStorage, "dropbox"},
		{ServiceAI, "llama"},
	} {
		if _, err := NormalizeWriteRequest(tc.service, tc.provider, map[string]any{}); err == nil {
			t.Errorf("%s accepted provider %q", tc.service, tc.provider)
		}
	}
}

func TestHasSecretInput(t *testing.T) {
	if HasSecretInput(ServiceSMTP, map[string]any{"host": "x"}) {
		t.Error("no secret present should be false")
	}
	if HasSecretInput(ServiceSMTP, map[string]any{"password": MaskSentinel}) {
		t.Error("a masked field is not a new secret")
	}
	if HasSecretInput(ServiceSMTP, map[string]any{"password": ""}) {
		t.Error("an empty field is not a new secret")
	}
	if !HasSecretInput(ServiceSMTP, map[string]any{"password": "real"}) {
		t.Error("a supplied password must be detected")
	}
	if !HasSecretInput(ServiceAI, map[string]any{"api_key": "sk-real"}) {
		t.Error("a supplied api key must be detected")
	}
}

func TestParseServiceAndSource(t *testing.T) {
	for _, in := range []string{"smtp", "SMTP", " Smtp "} {
		if got, err := ParseServiceType(in); err != nil || got != ServiceSMTP {
			t.Errorf("ParseServiceType(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"whatsapp", "email", "", "smtps"} {
		if _, err := ParseServiceType(in); err == nil {
			t.Errorf("ParseServiceType(%q) should fail", in)
		}
	}
	for _, in := range []string{"platform", "PLATFORM", " Organization "} {
		if _, err := ParseSource(in); err != nil {
			t.Errorf("ParseSource(%q) failed: %v", in, err)
		}
	}
	if _, err := ParseSource("SYSTEM"); err == nil {
		t.Error("ParseSource(SYSTEM) should fail")
	}
}

func TestParseEncryption(t *testing.T) {
	for in, want := range map[string]Encryption{
		"tls": EncryptionTLS, "TLS": EncryptionTLS,
		"starttls": EncryptionSTARTTLS, "": EncryptionSTARTTLS,
		"none": EncryptionNone, "NONE": EncryptionNone,
	} {
		got, err := ParseEncryption(in)
		if err != nil || got != want {
			t.Errorf("ParseEncryption(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	if _, err := ParseEncryption("rot13"); err == nil {
		t.Error("ParseEncryption(rot13) should fail")
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{
		StatusUnconfigured, StatusConfigured, StatusEnabled,
		StatusDisabled, StatusConnectionFailed, StatusTesting,
	} {
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if Status("MADE_UP").Valid() {
		t.Error("an unknown status must not be valid")
	}
}

func TestUnavailableErrorHint(t *testing.T) {
	platform := &UnavailableError{Service: ServiceSMTP, Source: SourcePlatform, Reason: "disabled"}
	org := &UnavailableError{Service: ServiceAI, Source: SourceOrganization, Reason: "missing"}

	if !strings.Contains(platform.Hint(), "platform administrator") {
		t.Errorf("platform hint should point at the administrator: %q", platform.Hint())
	}
	if !strings.Contains(org.Hint(), "organization") {
		t.Errorf("organization hint should mention the organization: %q", org.Hint())
	}
	if platform.Error() == "" || org.Error() == "" {
		t.Error("an unavailable error must describe itself")
	}
}

func TestSortServicesIsCanonical(t *testing.T) {
	in := []ServiceType{ServiceAI, "ZZZ", ServiceSMTP, ServiceStorage}
	SortServices(in)
	if in[0] != ServiceSMTP || in[1] != ServiceStorage || in[2] != ServiceAI {
		t.Errorf("order = %v, want SMTP, STORAGE, AI first", in)
	}
	if in[3] != "ZZZ" {
		t.Errorf("an unknown service should sort last, got %v", in[3])
	}
}

/* ------------------------------------------------------------------ *
 * Provider factory
 * ------------------------------------------------------------------ */

func TestFactoryBuildsProviders(t *testing.T) {
	f := NewFactory()

	smtpResolved := &Resolved{Service: ServiceSMTP, Provider: ProviderSMTP,
		Config: map[string]any{"host": "smtp.test.local", "port": 587,
			"from_email": "a@b.co", "encryption": "STARTTLS"}}
	if p, err := f.Email(smtpResolved); err != nil || p.Provider() != ProviderSMTP {
		t.Errorf("Email() = %v, %v", p, err)
	}

	// Every storage provider is S3-compatible and must build.
	for _, prov := range []string{ProviderS3, ProviderCloudflareR2, ProviderMinIO} {
		resolved := &Resolved{Service: ServiceStorage, Provider: prov, Config: map[string]any{
			"provider": prov, "bucket": "b", "access_key": "k", "region": "us-east-1",
			"endpoint": "https://storage.test.local", "secret_key": "s",
		}}
		if p, err := f.Storage(resolved); err != nil {
			t.Errorf("Storage(%s) failed: %v", prov, err)
		} else if p.Provider() != prov {
			t.Errorf("Storage(%s) reported provider %q", prov, p.Provider())
		}
	}

	// S3 with no endpoint gets an implied AWS host; the others must not.
	resolved := &Resolved{Service: ServiceStorage, Provider: ProviderS3, Config: map[string]any{
		"provider": ProviderS3, "bucket": "b", "access_key": "k", "region": "eu-west-1",
		"secret_key": "s",
	}}
	p, err := f.Storage(resolved)
	if err != nil {
		t.Fatalf("bare S3 failed: %v", err)
	}
	if got := p.PublicURL("k.png"); got != "" {
		t.Errorf("a private bucket must have no public URL, got %q", got)
	}

	for _, prov := range []string{ProviderOpenAI, ProviderGemini, ProviderCustom} {
		resolved := &Resolved{Service: ServiceAI, Provider: prov, Config: map[string]any{
			"provider": prov, "base_url": "https://ai.test.local/v1", "default_model": "m", "api_key": "k",
		}}
		if _, err := f.AI(resolved); err != nil {
			t.Errorf("AI(%s) failed: %v", prov, err)
		}
	}
	resolved = &Resolved{Service: ServiceAI, Provider: ProviderAnthropic, Config: map[string]any{
		"provider": ProviderAnthropic, "base_url": "https://api.anthropic.com/v1", "default_model": "m", "api_key": "k",
	}}
	if _, err := f.AI(resolved); err != nil {
		t.Errorf("AI(anthropic) failed: %v", err)
	}

	// A nil resolution must be an error, not a nil-provider panic.
	if _, err := f.Email(nil); err == nil {
		t.Error("Email(nil) should fail")
	}
	if _, err := f.Storage(nil); err == nil {
		t.Error("Storage(nil) should fail")
	}
	if _, err := f.AI(nil); err == nil {
		t.Error("AI(nil) should fail")
	}
}

/* ------------------------------------------------------------------ *
 * S3 signing
 * ------------------------------------------------------------------ */

func TestS3SigningIsDeterministicAndPathAware(t *testing.T) {
	resolved := &Resolved{Service: ServiceStorage, Provider: ProviderMinIO, Config: map[string]any{
		"provider": ProviderMinIO, "endpoint": "https://minio.test.local", "region": "us-east-1",
		"bucket": "media", "access_key": "AKIATEST", "secret_key": "topsecret", "path_prefix": "tenant-1",
	}}
	p, err := NewFactory().Storage(resolved)
	if err != nil {
		t.Fatal(err)
	}
	s3, ok := p.(*s3Provider)
	if !ok {
		t.Fatalf("expected *s3Provider, got %T", p)
	}

	// Put and PresignGet pass an already-prefixed key; resolveTarget then adds
	// the bucket for path-style addressing.
	host, uri, query := s3.resolveTarget(s3.objectKey("logo.png"), nil)
	if !strings.HasPrefix(host, "minio.test.local") {
		t.Errorf("host = %q, want the endpoint host (path style)", host)
	}
	if uri != "/media/tenant-1/logo.png" {
		t.Errorf("uri = %q, want /media/tenant-1/logo.png", uri)
	}
	if query != "" {
		t.Errorf("query = %q, want empty", query)
	}

	// The path prefix must be applied to returned keys.
	if got := s3.objectKey("logo.png"); got != "tenant-1/logo.png" {
		t.Errorf("objectKey = %q, want tenant-1/logo.png", got)
	}

	// A presigned URL must be signed, time-limited and carry the bucket.
	signed, err := s3.PresignGet(context.Background(), "logo.png", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Signature=", "X-Amz-Expires=600", "media"} {
		if !strings.Contains(signed, part) {
			t.Errorf("presigned url missing %q: %s", part, signed)
		}
	}
	// The secret must not appear in the URL.
	if strings.Contains(signed, "topsecret") {
		t.Error("presigned url leaked the secret key")
	}
}

func TestS3PublicURLHonoursCDN(t *testing.T) {
	resolved := &Resolved{Service: ServiceStorage, Provider: ProviderS3, Config: map[string]any{
		"provider": ProviderS3, "region": "us-east-1", "bucket": "media",
		"access_key": "k", "secret_key": "s", "public_url": "https://cdn.test.local/assets/",
	}}
	p, _ := NewFactory().Storage(resolved)
	if got := p.PublicURL("logo.png"); got != "https://cdn.test.local/assets/logo.png" {
		t.Errorf("PublicURL = %q", got)
	}
}

func TestS3RequiresEndpointForNonAWS(t *testing.T) {
	resolved := &Resolved{Service: ServiceStorage, Provider: ProviderCloudflareR2, Config: map[string]any{
		"provider": ProviderCloudflareR2, "bucket": "media", "access_key": "k", "secret_key": "s",
	}}
	if _, err := NewFactory().Storage(resolved); err == nil {
		t.Error("R2 without an endpoint must fail rather than guess a host")
	}
}

/* ------------------------------------------------------------------ *
 * AI providers against a stub server
 * ------------------------------------------------------------------ */

func TestOpenAIProviderCompletesAndEmbeds(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"model":"gpt-test","choices":[{"message":{"content":"pong"}}],
				"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
		case "/v1/embeddings":
			_, _ = w.Write([]byte(`{"model":"emb","data":[{"embedding":[0.1,0.2,0.3]}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-test"},{"id":"gpt-other"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolved := &Resolved{Service: ServiceAI, Provider: ProviderOpenAI,
		Config: map[string]any{
			"provider": ProviderOpenAI, "base_url": srv.URL + "/v1", "default_model": "gpt-test",
			"embedding_model": "emb",
		},
		Secrets: map[string]string{"api_key": "sk-test"}}
	provider, err := NewFactory().AI(resolved)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := provider.Complete(ctx, CompletionRequest{Prompt: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "pong" || out.InputTokens != 7 || out.OutputTokens != 2 {
		t.Errorf("completion = %+v", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}

	emb, err := provider.Embed(ctx, EmbeddingRequest{Input: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if emb.Dimensions != 3 || len(emb.Values) != 1 {
		t.Errorf("embedding = %+v", emb)
	}

	models, err := provider.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want 2", models)
	}
}

func TestOpenAIProviderReportsRejectedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	resolved := &Resolved{Service: ServiceAI, Provider: ProviderOpenAI,
		Config:  map[string]any{"provider": ProviderOpenAI, "base_url": srv.URL + "/v1", "default_model": "m"},
		Secrets: map[string]string{"api_key": "bad"}}
	provider, _ := NewFactory().AI(resolved)
	err := provider.TestConnection(context.Background())
	if err == nil {
		t.Fatal("a rejected key must fail the connection test")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %q, want it to name the problem", err)
	}
}

func TestAnthropicProviderUsesMessagesAPI(t *testing.T) {
	var gotKey, gotVersion, gotPath string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_, _ = w.Write([]byte(`{"model":"claude-test","content":[{"type":"text","text":"hi"}],
			"usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	defer srv.Close()

	resolved := &Resolved{Service: ServiceAI, Provider: ProviderAnthropic,
		Config:  map[string]any{"provider": ProviderAnthropic, "base_url": srv.URL + "/v1", "default_model": "claude-test"},
		Secrets: map[string]string{"api_key": "ak-test"}}
	provider, _ := NewFactory().AI(resolved)
	out, err := provider.Complete(context.Background(), CompletionRequest{System: "sys", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "hi" {
		t.Errorf("text = %q", out.Text)
	}
	if gotKey != "ak-test" || gotVersion == "" || gotPath != "/v1/messages" {
		t.Errorf("anthropic request: key=%q version=%q path=%q", gotKey, gotVersion, gotPath)
	}
	if payload["system"] != "sys" {
		t.Errorf("system prompt was not sent: %v", payload)
	}
	// The Messages API requires a minimum max_tokens.
	if mt, _ := payload["max_tokens"].(float64); mt < 1024 {
		t.Errorf("max_tokens = %v, want at least the API minimum of 1024", payload["max_tokens"])
	}

	// Embeddings are not available on this provider.
	if _, err := provider.Embed(context.Background(), EmbeddingRequest{Input: []string{"x"}}); err == nil {
		t.Error("anthropic embedding should report that it is unsupported")
	}
}

func TestSMTPProviderRefusesPlaintextAuth(t *testing.T) {
	// A server that advertises AUTH PLAIN and accepts EHLO, so the failure comes
	// from the encryption policy rather than the network.
	server := startFakeSMTP(t)
	defer server.close()

	resolved := &Resolved{Service: ServiceSMTP, Provider: ProviderSMTP, Config: map[string]any{
		"host": server.host, "port": server.port, "encryption": "NONE",
		"username": "u", "from_email": "a@b.co",
	}, Secrets: map[string]string{"password": "p"}}
	provider, _ := NewFactory().Email(resolved)
	err := provider.TestConnection(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unencrypted") {
		t.Errorf("error = %q, want it to explain the encryption policy", err)
	}
}

func TestSMTPProviderSendsMessage(t *testing.T) {
	smtp := startFakeSMTP(t)
	defer smtp.close()

	resolved := &Resolved{Service: ServiceSMTP, Provider: ProviderSMTP, Config: map[string]any{
		"host": smtp.host, "port": smtp.port, "encryption": "NONE",
		"from_name": "Orderly", "from_email": "noreply@test.local",
	}}
	provider, _ := NewFactory().Email(resolved)
	err := provider.Send(context.Background(), MailMessage{
		To: []string{"rcpt@test.local"}, Subject: "Hello", Body: "Body line",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	got := smtp.message()
	if !strings.Contains(got, "Subject: Hello") {
		t.Errorf("message missing subject:\n%s", got)
	}
	if !strings.Contains(got, "From: Orderly <noreply@test.local>") {
		t.Errorf("message missing from:\n%s", got)
	}
	if !strings.Contains(got, "Body line") {
		t.Errorf("message missing body:\n%s", got)
	}
}

func TestSMTPHeaderInjectionIsStripped(t *testing.T) {
	cfg := SMTPConfig{FromEmail: "a@b.co", FromName: "Evil\r\nBcc: attacker@evil.test"}
	body := buildMIME(cfg, MailMessage{To: []string{"x@y.co"}, Subject: "Hi\r\nBcc: bad@test"})

	// The real property: no line in the header block may be a header the caller
	// injected. Split on CRLF and check that "Bcc" never starts a line.
	headerBlock := body[:strings.Index(body, "\r\n\r\n")]
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Errorf("injected a Bcc header:\n%s", body)
		}
	}
	if !strings.Contains(body, "Subject: Hi") {
		t.Errorf("the legitimate part of the subject was lost:\n%s", body)
	}
	if strings.Contains(body, "attacker@evil.test") {
		t.Errorf("injected text survived in a header:\n%s", body)
	}
}

func TestSendTestMessageRequiresRecipient(t *testing.T) {
	outcome := SendTestMessage(context.Background(), nil, "", "label")
	if outcome.OK {
		t.Error("an empty recipient must be rejected")
	}
	if outcome.Message == "" {
		t.Error("a rejected test must explain itself")
	}
}

func TestUploadProbeWithNilProvider(t *testing.T) {
	if outcome := UploadProbe(context.Background(), nil); outcome.OK {
		t.Error("a nil provider must not report success")
	}
}

func TestTestModelWithoutModel(t *testing.T) {
	resolved := &Resolved{Service: ServiceAI, Provider: ProviderOpenAI, Config: map[string]any{
		"provider": ProviderOpenAI, "base_url": "https://x/v1", "api_key": "k",
	}}
	provider, _ := NewFactory().AI(resolved)
	outcome := TestModel(context.Background(), provider)
	if outcome.OK {
		t.Error("no default model must not report success")
	}
	if !strings.Contains(outcome.Message, "model") {
		t.Errorf("message = %q, want it to mention the missing model", outcome.Message)
	}
}

// mustUUID parses a fixed test id.
func mustUUID(s string) uuid.UUID {
	t, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}
