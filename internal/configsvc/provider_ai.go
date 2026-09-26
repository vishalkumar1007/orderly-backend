package configsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAICompatibleProvider covers OpenAI and every endpoint that speaks its
// schema, which includes Gemini's OpenAI compatibility layer and any
// self-hosted or third-party gateway configured as "custom".
type openAICompatibleProvider struct {
	cfg    AIConfig
	apiKey string
	client *http.Client
}

func newOpenAICompatibleProvider(r *Resolved) *openAICompatibleProvider {
	cfg := aiFrom(r.Config)
	cfg.applyDefaults()
	return &openAICompatibleProvider{
		cfg:    cfg,
		apiKey: r.Secret("api_key"),
		client: &http.Client{Timeout: time.Duration(cfg.RequestTimeoutS) * time.Second},
	}
}

func (p *openAICompatibleProvider) Provider() string       { return p.cfg.Provider }
func (p *openAICompatibleProvider) DefaultModel() string   { return p.cfg.DefaultModel }
func (p *openAICompatibleProvider) EmbeddingModel() string { return p.cfg.EmbeddingModel }

// TestConnection asks for the model catalogue, which is the cheapest call that
// proves both reachability and that the key is accepted.
func (p *openAICompatibleProvider) TestConnection(ctx context.Context) error {
	req, err := p.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", p.cfg.BaseURL, err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the API key was rejected (%s)", resp.Status)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}

// ListModels returns the advertised model ids.
func (p *openAICompatibleProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", p.cfg.BaseURL, err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("listing models returned %s", resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("could not parse model list: %w", err)
	}
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Complete generates text.
func (p *openAICompatibleProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultModel
	}
	if model == "" {
		return CompletionResponse{}, fmt.Errorf("%w: no model configured", ErrInvalidRequest)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxOutputTokens
	}

	messages := make([]map[string]string, 0, 2)
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	messages = append(messages, map[string]string{"role": "user", "content": req.Prompt})

	payload := map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	if req.Temp != nil {
		payload["temperature"] = *req.Temp
	}

	httpReq, err := p.newRequest(ctx, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return CompletionResponse{}, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return CompletionResponse{}, fmt.Errorf("completion returned %s: %s",
			resp.Status, snippet(resp.Body))
	}

	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return CompletionResponse{}, fmt.Errorf("could not parse completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return CompletionResponse{}, fmt.Errorf("the provider returned no choices")
	}
	return CompletionResponse{
		Text:         out.Choices[0].Message.Content,
		Model:        out.Model,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}, nil
}

// Embed turns text into vectors.
func (p *openAICompatibleProvider) Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.EmbeddingModel
	}
	if model == "" {
		return EmbeddingResponse{}, fmt.Errorf("%w: no embedding model configured", ErrInvalidRequest)
	}
	if len(req.Input) == 0 {
		return EmbeddingResponse{}, fmt.Errorf("%w: embedding input is required", ErrInvalidRequest)
	}
	payload := map[string]any{"model": model, "input": req.Input}

	httpReq, err := p.newRequest(ctx, http.MethodPost, "/embeddings", payload)
	if err != nil {
		return EmbeddingResponse{}, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return EmbeddingResponse{}, fmt.Errorf("embedding returned %s: %s", resp.Status, snippet(resp.Body))
	}

	var out struct {
		Model string `json:"model"`
		Data  []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return EmbeddingResponse{}, fmt.Errorf("could not parse embeddings: %w", err)
	}
	values := make([][]float32, 0, len(out.Data))
	dims := 0
	for _, d := range out.Data {
		values = append(values, d.Embedding)
		if dims == 0 {
			dims = len(d.Embedding)
		}
	}
	return EmbeddingResponse{Model: out.Model, Dimensions: dims, Values: values}, nil
}

func (p *openAICompatibleProvider) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	if p.cfg.OrganizationID != "" {
		req.Header.Set("OpenAI-Organization", p.cfg.OrganizationID)
	}
	return req, nil
}

/* ------------------------------------------------------------------ *
 * Anthropic
 * ------------------------------------------------------------------ */

// anthropicProvider speaks the Messages API, which differs from OpenAI's in
// both endpoint shape and system-prompt handling.
type anthropicProvider struct {
	cfg    AIConfig
	apiKey string
	client *http.Client
}

func newAnthropicProvider(r *Resolved) *anthropicProvider {
	cfg := aiFrom(r.Config)
	cfg.applyDefaults()
	return &anthropicProvider{
		cfg:    cfg,
		apiKey: r.Secret("api_key"),
		client: &http.Client{Timeout: time.Duration(cfg.RequestTimeoutS) * time.Second},
	}
}

func (p *anthropicProvider) Provider() string       { return ProviderAnthropic }
func (p *anthropicProvider) DefaultModel() string   { return p.cfg.DefaultModel }
func (p *anthropicProvider) EmbeddingModel() string { return p.cfg.EmbeddingModel }

func (p *anthropicProvider) TestConnection(ctx context.Context) error {
	req, err := p.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", p.cfg.BaseURL, err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the API key was rejected (%s)", resp.Status)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned %s", resp.Status)
	}
	return nil
}

// ListModels returns Anthropic model ids.
func (p *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", p.cfg.BaseURL, err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("listing models returned %s", resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("could not parse model list: %w", err)
	}
	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Complete calls the Messages API.
func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultModel
	}
	if model == "" {
		return CompletionResponse{}, fmt.Errorf("%w: no model configured", ErrInvalidRequest)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxOutputTokens
	}
	// The Messages API requires max_tokens, and 4096 is its minimum.
	if maxTokens < 1024 {
		maxTokens = 1024
	}

	payload := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": req.Prompt},
		},
	}
	if req.System != "" {
		payload["system"] = req.System
	}
	if req.Temp != nil {
		payload["temperature"] = *req.Temp
	}

	httpReq, err := p.newRequest(ctx, http.MethodPost, "/messages", payload)
	if err != nil {
		return CompletionResponse{}, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		return CompletionResponse{}, fmt.Errorf("completion returned %s: %s", resp.Status, snippet(resp.Body))
	}

	var out struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return CompletionResponse{}, fmt.Errorf("could not parse completion: %w", err)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return CompletionResponse{
		Text:         b.String(),
		Model:        out.Model,
		InputTokens:  out.Usage.InputTokens,
		OutputTokens: out.Usage.OutputTokens,
	}, nil
}

// Embed is unsupported on Anthropic, which has no public embedding endpoint.
func (p *anthropicProvider) Embed(context.Context, EmbeddingRequest) (EmbeddingResponse, error) {
	return EmbeddingResponse{}, fmt.Errorf("%w: Anthropic does not provide an embedding endpoint; configure a separate provider", ErrInvalidRequest)
}

func (p *anthropicProvider) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if p.apiKey != "" {
		req.Header.Set("x-api-key", p.apiKey)
	}
	return req, nil
}

/* ---------- shared helpers ---------- */

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 4<<20)).Decode(v)
}

// snippet returns a short, safe excerpt of an error body for diagnostics.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
