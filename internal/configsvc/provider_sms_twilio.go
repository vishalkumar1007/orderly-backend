package configsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// twilioProvider talks to the Twilio REST API over plain net/http — no SDK
// dependency, matching this package's existing providers (net/smtp for
// email, the AWS v4-signed S3 API for storage).
type twilioProvider struct {
	cfg        SMSConfig
	authToken  string
	httpClient *http.Client
}

const twilioBaseURL = "https://api.twilio.com/2010-04-01"

func newTwilioProvider(r *Resolved) *twilioProvider {
	cfg := smsFrom(r.Config)
	cfg.applyDefaults()
	return &twilioProvider{
		cfg:        cfg,
		authToken:  r.Secret("auth_token"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *twilioProvider) Provider() string   { return ProviderTwilio }
func (p *twilioProvider) FromNumber() string { return p.cfg.FromNumber }

// TestConnection fetches the account resource. It sends no message — the
// same "verify, don't act" contract as the SMTP and storage providers.
func (p *twilioProvider) TestConnection(ctx context.Context) error {
	endpoint := fmt.Sprintf("%s/Accounts/%s.json", twilioBaseURL, url.PathEscape(p.cfg.AccountSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(p.cfg.AccountSID, p.authToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to twilio failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("twilio rejected the credentials: %s", twilioErrorDetail(resp))
	}
	return nil
}

// Send delivers one SMS message and returns Twilio's message SID.
func (p *twilioProvider) Send(ctx context.Context, to, body string) (string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", fmt.Errorf("a recipient number is required")
	}
	endpoint := fmt.Sprintf("%s/Accounts/%s/Messages.json", twilioBaseURL, url.PathEscape(p.cfg.AccountSID))
	form := url.Values{"To": {to}, "From": {p.cfg.FromNumber}, "Body": {body}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(p.cfg.AccountSID, p.authToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send sms failed: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		SID          string `json:"sid"`
		ErrorCode    *int   `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)

	if resp.StatusCode != http.StatusCreated {
		if out.ErrorMessage != "" {
			return "", fmt.Errorf("twilio rejected the message: %s", out.ErrorMessage)
		}
		return "", fmt.Errorf("twilio returned status %d", resp.StatusCode)
	}
	return out.SID, nil
}

// twilioErrorDetail extracts Twilio's error message from a failed response
// body, falling back to the raw status when the body is not JSON.
func twilioErrorDetail(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var out struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &out) == nil && out.Message != "" {
		return out.Message
	}
	return resp.Status
}
