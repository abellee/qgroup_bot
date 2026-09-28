// Package llm calls one chat completion on a provider's own protocol. Only the
// three the admin panel offers are supported, and each keeps its native request
// shape: the persona goes where that provider puts system text, and the answer
// is read back the way that provider returns it.
package llm

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

const anthropicVersion = "2023-06-01"

// Config is one model_configs row reduced to what a call needs.
type Config struct {
	Provider    string
	BaseURL     string
	APIKey      string
	Model       string
	Persona     string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
}

// Client is safe for concurrent use; the HTTP client carries the shared pool.
type Client struct{ http *http.Client }

func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{http: httpClient}
}

// Complete sends a single turn: persona plus this one message, no history. The
// reply text is what belongs in the group.
func (c *Client) Complete(ctx context.Context, cfg Config, prompt string) (string, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return "", fmt.Errorf("no api base url configured")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return "", fmt.Errorf("no api key configured")
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 1024
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	switch strings.ToLower(cfg.Provider) {
	case "openai":
		return c.openai(ctx, cfg, prompt)
	case "anthropic":
		return c.anthropic(ctx, cfg, prompt)
	case "gemini":
		return c.gemini(ctx, cfg, prompt)
	default:
		return "", fmt.Errorf("unsupported provider %q", cfg.Provider)
	}
}

// message is the OpenAI chat item; content is a plain string because a
// completion of this shape is all a group reply needs.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *Client) openai(ctx context.Context, cfg Config, prompt string) (string, error) {
	msgs := make([]message, 0, 2)
	if p := strings.TrimSpace(cfg.Persona); p != "" {
		msgs = append(msgs, message{Role: "system", Content: p})
	}
	msgs = append(msgs, message{Role: "user", Content: prompt})

	body := map[string]any{
		"model":       cfg.Model,
		"messages":    msgs,
		"temperature": cfg.Temperature,
		"max_tokens":  cfg.MaxTokens,
	}
	var out struct {
		Choices []struct {
			Message      message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	url := joinBase(cfg.BaseURL, "/v1") + "/chat/completions"
	if err := c.post(ctx, url, map[string]string{"Authorization": "Bearer " + cfg.APIKey}, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s: %s", firstNonEmpty(out.Error.Type, "api error"), out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func (c *Client) anthropic(ctx context.Context, cfg Config, prompt string) (string, error) {
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    []message{{Role: "user", Content: prompt}},
		"temperature": cfg.Temperature,
		"max_tokens":  cfg.MaxTokens,
	}
	if p := strings.TrimSpace(cfg.Persona); p != "" {
		body["system"] = p
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	url := joinBase(cfg.BaseURL, "/v1") + "/messages"
	headers := map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": anthropicVersion,
	}
	if err := c.post(ctx, url, headers, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s: %s", firstNonEmpty(out.Error.Type, "api error"), out.Error.Message)
	}
	var sb strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

func (c *Client) gemini(ctx context.Context, cfg Config, prompt string) (string, error) {
	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]any{
			"temperature":     cfg.Temperature,
			"maxOutputTokens": cfg.MaxTokens,
		},
	}
	if p := strings.TrimSpace(cfg.Persona); p != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]string{{"text": p}},
		}
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finish_reason"`
		} `json:"candidates"`
		PromptFeedback *struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Error *struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	// The key rides in a header rather than the documented ?key= query so it
	// cannot end up in an access log on whatever proxy the base url points at.
	url := joinBase(cfg.BaseURL, "/v1beta") + "/models/" + cfg.Model + ":generateContent"
	headers := map[string]string{"x-goog-api-key": cfg.APIKey}
	if err := c.post(ctx, url, headers, body, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s: %s", firstNonEmpty(out.Error.Status, "api error"), out.Error.Message)
	}
	if out.PromptFeedback != nil && out.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("prompt blocked: %s", out.PromptFeedback.BlockReason)
	}
	if len(out.Candidates) == 0 {
		return "", fmt.Errorf("no candidates returned")
	}
	var sb strings.Builder
	for _, part := range out.Candidates[0].Content.Parts {
		sb.WriteString(part.Text)
	}
	return strings.TrimSpace(sb.String()), nil
}

// post sends the body and decodes a JSON answer. A provider that answers 200
// with an error object is handled by the callers, who read the typed error field
// their protocol defines.
func (c *Client) post(ctx context.Context, url string, headers map[string]string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: http %d %s", url, resp.StatusCode, snippet(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response: %w (%s)", err, snippet(raw))
	}
	return nil
}

// joinBase appends the provider's version segment unless the configured url
// already carries it, so both `https://api.openai.com` and
// `https://gw.example.com/openai/v1` work.
func joinBase(base, version string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, version) {
		return base
	}
	return base + version
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "api error"
}

// snippet keeps a provider's error text in the log without pasting a whole
// HTML error page into it.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
