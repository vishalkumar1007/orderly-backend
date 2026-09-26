package configsvc

import (
	"context"
	"fmt"
	"strings"
)

// NormalizeWriteRequest validates and canonicalises an incoming configuration
// save, returning the cleaned request to persist. Exported so the HTTP layer
// validates exactly once, with the same rules the store relies on.
func NormalizeWriteRequest(service ServiceType, provider string, config map[string]any) (WriteRequest, error) {
	return normalizeWriteRequest(service, provider, config)
}

// HasSecretInput reports whether a submitted config carries actual secret
// material, as opposed to leaving every secret field masked. Used to refuse a
// save when no encryption key is configured.
func HasSecretInput(service ServiceType, config map[string]any) bool {
	for _, key := range secretFields[service] {
		if v, ok := config[key]; ok {
			s := strings.TrimSpace(fmt.Sprintf("%v", v))
			if s != "" && s != MaskSentinel {
				return true
			}
		}
	}
	return false
}

// SendTestMessage delivers a test email over a built provider. It is exported so
// the HTTP layer can test a configuration it has not yet saved.
func SendTestMessage(ctx context.Context, provider EmailProvider, to, label string) TestOutcome {
	to = strings.TrimSpace(to)
	if !looksLikeEmail(to) {
		return TestOutcome{OK: false, Message: "A valid recipient address is required"}
	}
	if provider == nil {
		return TestOutcome{OK: false, Message: "No email provider is configured"}
	}
	msg := MailMessage{
		To:      []string{to},
		Subject: "Orderly SMTP test",
		Body: fmt.Sprintf("This is a test message sent by %s.\r\n\r\n"+
			"If you received it, outgoing email is configured correctly.\r\n", label),
	}
	if err := provider.Send(ctx, msg); err != nil {
		return TestOutcome{OK: false, Message: "The test message could not be sent", Detail: SafeErrorMessage(err)}
	}
	return TestOutcome{OK: true, Message: "Test message sent to " + to}
}

// UploadProbe writes and removes a small object, proving write access rather
// than only read access.
func UploadProbe(ctx context.Context, provider StorageProvider) TestOutcome {
	if provider == nil {
		return TestOutcome{OK: false, Message: "No storage provider is configured"}
	}
	key := fmt.Sprintf(".orderly-probe/%d.txt", nowUnixNano())
	obj, err := provider.Put(ctx, PutRequest{Key: key, Body: []byte("orderly storage probe"), ContentType: "text/plain"})
	if err != nil {
		return TestOutcome{OK: false, Message: "The probe object could not be written", Detail: SafeErrorMessage(err)}
	}
	// A failed cleanup leaves noise, not a security hole, so the write still
	// counts as a success.
	if err := provider.Delete(ctx, obj.Key); err != nil {
		return TestOutcome{
			OK:      true,
			Message: "Probe object written, but could not be removed",
			Detail:  "Remove " + obj.Key + " manually. " + SafeErrorMessage(err),
		}
	}
	return TestOutcome{OK: true, Message: "Probe object written and removed (" + obj.Key + ")"}
}

// TestModel issues a minimal completion, proving the configured model exists and
// the key can spend tokens. A model listing is not enough: a key can list models
// and still be unable to run them.
func TestModel(ctx context.Context, provider AIProvider) TestOutcome {
	if provider == nil {
		return TestOutcome{OK: false, Message: "No AI provider is configured"}
	}
	model := provider.DefaultModel()
	if model == "" {
		return TestOutcome{OK: false, Message: "No default model is configured"}
	}
	resp, err := provider.Complete(ctx, CompletionRequest{
		Model:     model,
		Prompt:    "Reply with the single word: ok",
		MaxTokens: 16,
	})
	if err != nil {
		return TestOutcome{OK: false, Message: "The model could not be called", Detail: SafeErrorMessage(err)}
	}
	if strings.TrimSpace(resp.Text) == "" {
		return TestOutcome{OK: false, Message: "The model returned an empty response"}
	}
	return TestOutcome{
		OK:      true,
		Message: "Model " + model + " responded",
		Detail:  fmt.Sprintf("%d input / %d output tokens", resp.InputTokens, resp.OutputTokens),
	}
}

// MaskedConfig returns a copy of a stored config with every secret field set to
// MaskSentinel, for building an editable form the console is allowed to render.
func MaskedConfig(service ServiceType, cfg map[string]any) map[string]any {
	return maskSecrets(service, cfg)
}
