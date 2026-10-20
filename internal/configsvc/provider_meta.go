package configsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// metaProvider talks to the Meta Graph API over plain net/http: WhatsApp
// Business Platform (Cloud API) messages, Facebook Page posts and Instagram
// professional-account publishing, all with one access token. Which of the
// three a tenant can use depends on which ids they filled in; the marketing
// module checks those before offering a channel.
type metaProvider struct {
	cfg         MetaConfig
	accessToken string
	httpClient  *http.Client
}

const metaGraphBaseURL = "https://graph.facebook.com"

func newMetaProvider(r *Resolved) *metaProvider {
	cfg := metaFrom(r.Config)
	cfg.applyDefaults()
	return &metaProvider{
		cfg:         cfg,
		accessToken: r.Secret("access_token"),
		httpClient:  &http.Client{Timeout: 20 * time.Second},
	}
}

// MetaProvider is what the marketing module needs from Meta.
type MetaProvider interface {
	Provider() string
	TestConnection(ctx context.Context) error
	// CanWhatsApp / CanFacebook / CanInstagram report which ids are configured.
	CanWhatsApp() bool
	CanFacebook() bool
	CanInstagram() bool
	// SendWhatsAppTemplate sends an approved template; bodyParams fill {{1}}..{{n}}.
	SendWhatsAppTemplate(ctx context.Context, to, template, lang string, bodyParams []string) (messageID string, err error)
	// PublishFacebook posts to the Page: a photo post when imageURL is set, else a link/text post.
	PublishFacebook(ctx context.Context, message, link, imageURL string) (postID string, err error)
	// PublishInstagram publishes one image with a caption (Instagram requires an image).
	PublishInstagram(ctx context.Context, imageURL, caption string) (mediaID string, err error)
}

func (p *metaProvider) Provider() string   { return ProviderMeta }
func (p *metaProvider) CanWhatsApp() bool  { return p.cfg.PhoneNumberID != "" }
func (p *metaProvider) CanFacebook() bool  { return p.cfg.PageID != "" }
func (p *metaProvider) CanInstagram() bool { return p.cfg.InstagramUserID != "" }

func (p *metaProvider) endpoint(path string) string {
	return metaGraphBaseURL + "/" + p.cfg.APIVersion + "/" + strings.TrimPrefix(path, "/")
}

// call performs one Graph API request and decodes the JSON reply.
func (p *metaProvider) call(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint(path), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to meta failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta rejected the request: %s", metaErrorDetail(resp.StatusCode, out))
	}
	return out, nil
}

func metaErrorDetail(status int, body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		if msg != "" {
			return fmt.Sprintf("HTTP %d: %s", status, msg)
		}
	}
	return fmt.Sprintf("HTTP %d", status)
}

// TestConnection reads the token's own identity, then the WhatsApp number
// when one is configured. Nothing is sent or published.
func (p *metaProvider) TestConnection(ctx context.Context) error {
	if p.accessToken == "" {
		return fmt.Errorf("access token is missing")
	}
	if _, err := p.call(ctx, http.MethodGet, "me?fields=id,name", nil); err != nil {
		return err
	}
	if p.cfg.PhoneNumberID != "" {
		if _, err := p.call(ctx, http.MethodGet, url.PathEscape(p.cfg.PhoneNumberID)+"?fields=display_phone_number", nil); err != nil {
			return fmt.Errorf("whatsapp number: %w", err)
		}
	}
	return nil
}

func (p *metaProvider) SendWhatsAppTemplate(ctx context.Context, to, template, lang string, bodyParams []string) (string, error) {
	if !p.CanWhatsApp() {
		return "", fmt.Errorf("whatsapp phone_number_id is not configured")
	}
	to = strings.TrimPrefix(strings.TrimSpace(to), "+")
	params := make([]map[string]any, 0, len(bodyParams))
	for _, v := range bodyParams {
		params = append(params, map[string]any{"type": "text", "text": v})
	}
	tmpl := map[string]any{"name": template, "language": map[string]any{"code": lang}}
	if len(params) > 0 {
		tmpl["components"] = []map[string]any{{"type": "body", "parameters": params}}
	}
	out, err := p.call(ctx, http.MethodPost, url.PathEscape(p.cfg.PhoneNumberID)+"/messages", map[string]any{
		"messaging_product": "whatsapp", "to": to, "type": "template", "template": tmpl,
	})
	if err != nil {
		return "", err
	}
	if msgs, ok := out["messages"].([]any); ok && len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				return id, nil
			}
		}
	}
	return "", nil
}

func (p *metaProvider) PublishFacebook(ctx context.Context, message, link, imageURL string) (string, error) {
	if !p.CanFacebook() {
		return "", fmt.Errorf("facebook page_id is not configured")
	}
	var out map[string]any
	var err error
	if imageURL != "" {
		caption := message
		if link != "" {
			caption = strings.TrimSpace(caption + "\n" + link)
		}
		out, err = p.call(ctx, http.MethodPost, url.PathEscape(p.cfg.PageID)+"/photos", map[string]any{"url": imageURL, "caption": caption})
	} else {
		body := map[string]any{"message": message}
		if link != "" {
			body["link"] = link
		}
		out, err = p.call(ctx, http.MethodPost, url.PathEscape(p.cfg.PageID)+"/feed", body)
	}
	if err != nil {
		return "", err
	}
	for _, k := range []string{"post_id", "id"} {
		if id, ok := out[k].(string); ok && id != "" {
			return id, nil
		}
	}
	return "", nil
}

func (p *metaProvider) PublishInstagram(ctx context.Context, imageURL, caption string) (string, error) {
	if !p.CanInstagram() {
		return "", fmt.Errorf("instagram account id is not configured")
	}
	if imageURL == "" {
		return "", fmt.Errorf("instagram needs an image")
	}
	container, err := p.call(ctx, http.MethodPost, url.PathEscape(p.cfg.InstagramUserID)+"/media", map[string]any{
		"image_url": imageURL, "caption": caption,
	})
	if err != nil {
		return "", err
	}
	creationID, _ := container["id"].(string)
	if creationID == "" {
		return "", fmt.Errorf("instagram did not return a media container")
	}
	published, err := p.call(ctx, http.MethodPost, url.PathEscape(p.cfg.InstagramUserID)+"/media_publish", map[string]any{
		"creation_id": creationID,
	})
	if err != nil {
		return "", err
	}
	id, _ := published["id"].(string)
	return id, nil
}
