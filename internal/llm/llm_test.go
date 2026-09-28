package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type request struct {
	path   string
	header http.Header
	body   map[string]any
}

// providerStub stands in for one of the three APIs and records what it was sent.
type providerStub struct {
	client   *Client
	url      string
	mu       sync.Mutex
	requests []request
	response string
	status   int
	delay    time.Duration
}

func newStub(t *testing.T, status int, response string) *providerStub {
	t.Helper()
	s := &providerStub{status: status, response: response}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delay, status, response := s.delay, s.status, s.response
		s.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		raw, _ := io.ReadAll(r.Body)
		rec := request{path: r.URL.Path, header: r.Header.Clone()}
		if err := json.Unmarshal(raw, &rec.body); err != nil {
			t.Errorf("request body is not json: %v (%s)", err, raw)
		}
		s.mu.Lock()
		s.requests = append(s.requests, rec)
		s.mu.Unlock()

		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)

	s.url = srv.URL
	// No client timeout: the per-configuration deadline is what is under test.
	s.client = New(&http.Client{})
	return s
}

func (s *providerStub) last(t *testing.T) request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the provider saw no request")
	}
	return s.requests[len(s.requests)-1]
}

func (s *providerStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// slow makes the stub answer late. It goes through the same lock the handler
// reads, so a test can set it without racing the server goroutine.
func (s *providerStub) slow(d time.Duration) {
	s.mu.Lock()
	s.delay = d
	s.mu.Unlock()
}

func openAIConfig(url string) Config {
	return Config{
		Provider:    "openai",
		BaseURL:     url,
		APIKey:      "sk-test",
		Model:       "gpt-test",
		Persona:     "你是群助手",
		Temperature: 0.5,
		MaxTokens:   64,
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"role":"assistant","content":"  答案  "}}]}`)

	got, err := stub.client.Complete(context.Background(), openAIConfig(stub.url), "怎么注册")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "答案" {
		t.Errorf("answer = %q, want the trimmed content", got)
	}

	rec := stub.last(t)
	if rec.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", rec.path)
	}
	if auth := rec.header.Get("authorization"); auth != "Bearer sk-test" {
		t.Errorf("authorization = %q, want the bearer key", auth)
	}
	if rec.body["model"] != "gpt-test" {
		t.Errorf("model = %v, want the configured model", rec.body["model"])
	}
	if n, _ := rec.body["max_tokens"].(float64); n != 64 {
		t.Errorf("max_tokens = %v, want 64", rec.body["max_tokens"])
	}
	if f, _ := rec.body["temperature"].(float64); f != 0.5 {
		t.Errorf("temperature = %v, want 0.5", rec.body["temperature"])
	}

	msgs, _ := rec.body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want the persona plus the question", rec.body["messages"])
	}
	system, _ := msgs[0].(map[string]any)
	user, _ := msgs[1].(map[string]any)
	if system["role"] != "system" || system["content"] != "你是群助手" {
		t.Errorf("messages[0] = %v, want the persona as system text", msgs[0])
	}
	if user["role"] != "user" || user["content"] != "怎么注册" {
		t.Errorf("messages[1] = %v, want the group message as the user turn", msgs[1])
	}
}

// With no persona there is no system turn, only the question.
func TestOpenAIWithoutPersona(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)
	cfg := openAIConfig(stub.url)
	cfg.Persona = "   "

	if _, err := stub.client.Complete(context.Background(), cfg, "问题"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, _ := stub.last(t).body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages = %v, want only the user turn", msgs)
	}
}

func TestAnthropicRequestShape(t *testing.T) {
	stub := newStub(t, 200, `{"content":[{"type":"text","text":"部"},{"type":"text","text":"分"},{"type":"thinking","text":"skip"}]}`)
	cfg := Config{
		Provider:  "anthropic",
		BaseURL:   stub.url,
		APIKey:    "sk-ant",
		Model:     "claude-test",
		Persona:   "人设",
		MaxTokens: 256,
	}

	got, err := stub.client.Complete(context.Background(), cfg, "问题")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "部分" {
		t.Errorf("answer = %q, want only the text blocks joined", got)
	}

	rec := stub.last(t)
	if rec.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", rec.path)
	}
	if key := rec.header.Get("x-api-key"); key != "sk-ant" {
		t.Errorf("x-api-key = %q, want the key in its own header", key)
	}
	if v := rec.header.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want the dated header the api requires", v)
	}
	if rec.body["system"] != "人设" {
		t.Errorf("system = %v, want the persona in its own field, not in messages", rec.body["system"])
	}
	if n, _ := rec.body["max_tokens"].(float64); n != 256 {
		t.Errorf("max_tokens = %v, want 256: anthropic rejects a missing one", rec.body["max_tokens"])
	}
}

func TestGeminiRequestShape(t *testing.T) {
	stub := newStub(t, 200, `{"candidates":[{"content":{"parts":[{"text":"答"},{"text":"案"}]}}]}`)
	cfg := Config{
		Provider:  "gemini",
		BaseURL:   stub.url,
		APIKey:    "gk-test",
		Model:     "gemini-test",
		Persona:   "人设",
		MaxTokens: 128,
	}

	got, err := stub.client.Complete(context.Background(), cfg, "问题")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "答案" {
		t.Errorf("answer = %q, want the parts joined", got)
	}

	rec := stub.last(t)
	if rec.path != "/v1beta/models/gemini-test:generateContent" {
		t.Errorf("path = %q, want the model name in the path", rec.path)
	}
	if key := rec.header.Get("x-goog-api-key"); key != "gk-test" {
		t.Errorf("x-goog-api-key = %q, want the key in a header", key)
	}
	if strings.Contains(rec.path, "key=") || strings.Contains(rec.header.Get("authorization"), "gk-test") {
		t.Error("the gemini key must not travel in the url")
	}

	system, _ := rec.body["systemInstruction"].(map[string]any)
	if system == nil {
		t.Fatalf("systemInstruction missing from %v", rec.body)
	}
	parts, _ := system["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("systemInstruction.parts = %v, want the persona", parts)
	}
	gen, _ := rec.body["generationConfig"].(map[string]any)
	if n, _ := gen["maxOutputTokens"].(float64); n != 128 {
		t.Errorf("maxOutputTokens = %v, want 128", gen["maxOutputTokens"])
	}
	contents, _ := rec.body["contents"].([]any)
	if len(contents) != 1 {
		t.Errorf("contents = %v, want a single turn", contents)
	}
}

// A base that already carries its version segment must not gain a second one,
// which is how a gateway mounted under /v1 would break.
func TestVersionSegmentIsNotDoubled(t *testing.T) {
	anthropic := newStub(t, 200, `{"content":[{"type":"text","text":"ok"}]}`)
	cfg := Config{Provider: "anthropic", BaseURL: anthropic.url + "/v1", APIKey: "k", Model: "m", MaxTokens: 8}
	if _, err := anthropic.client.Complete(context.Background(), cfg, "问题"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := anthropic.last(t).path; got != "/v1/messages" {
		t.Errorf("path = %q, want no doubled version segment", got)
	}

	gemini := newStub(t, 200, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	cfg = Config{Provider: "gemini", BaseURL: gemini.url + "/v1beta", APIKey: "k", Model: "m"}
	if _, err := gemini.client.Complete(context.Background(), cfg, "问题"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := gemini.last(t).path; got != "/v1beta/models/m:generateContent" {
		t.Errorf("path = %q, want no doubled version segment", got)
	}

	// A trailing slash in the saved value is the operator's, not a path to keep.
	openai := newStub(t, 200, `{"choices":[{"message":{"content":"ok"}}]}`)
	cfg = Config{Provider: "openai", BaseURL: openai.url + "/v1/", APIKey: "k", Model: "m"}
	if _, err := openai.client.Complete(context.Background(), cfg, "问题"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := openai.last(t).path; got != "/v1/chat/completions" {
		t.Errorf("path = %q, want the trailing slash collapsed", got)
	}
}

// A refused call must say so, because the group would otherwise just go quiet.
func TestProviderErrorsSurface(t *testing.T) {
	http401 := newStub(t, 401, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
	if _, err := http401.client.Complete(context.Background(), openAIConfig(http401.url), "问题"); err == nil {
		t.Fatal("Complete accepted http 401")
	} else if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %v, want the status and the provider message", err)
	}

	// Some compatible gateways answer 200 and put the failure in the body.
	quiet200 := newStub(t, 200, `{"error":{"message":"quota exhausted"}}`)
	if _, err := quiet200.client.Complete(context.Background(), openAIConfig(quiet200.url), "问题"); err == nil {
		t.Error("Complete accepted an error body carried by http 200")
	} else if !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("error = %v, want the body message", err)
	}

	blocked := newStub(t, 200, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	cfg := Config{Provider: "gemini", BaseURL: blocked.url, APIKey: "k", Model: "m"}
	if _, err := blocked.client.Complete(context.Background(), cfg, "问题"); err == nil {
		t.Error("Complete accepted a blocked prompt")
	} else if !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("error = %v, want the block reason", err)
	}

	noChoices := newStub(t, 200, `{"choices":[]}`)
	if _, err := noChoices.client.Complete(context.Background(), openAIConfig(noChoices.url), "问题"); err == nil {
		t.Error("Complete accepted a response with no choices")
	}

	notJSON := newStub(t, 200, `<html>gateway said no</html>`)
	if _, err := notJSON.client.Complete(context.Background(), openAIConfig(notJSON.url), "问题"); err == nil {
		t.Error("Complete accepted a non-json body")
	}
}

// The guards run before anything leaves the process: no key means no request.
func TestGuardsSendNothing(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"content":"x"}}]}`)

	cases := map[string]Config{
		"no key":         {Provider: "openai", BaseURL: stub.url, Model: "m"},
		"no base":        {Provider: "openai", APIKey: "k", Model: "m"},
		"blank base":     {Provider: "openai", BaseURL: "   ", APIKey: "k", Model: "m"},
		"unknown":        {Provider: "mistral", BaseURL: stub.url, APIKey: "k", Model: "m"},
		"empty provider": {BaseURL: stub.url, APIKey: "k", Model: "m"},
	}
	for name, cfg := range cases {
		if _, err := stub.client.Complete(context.Background(), cfg, "问题"); err == nil {
			t.Errorf("%s: Complete succeeded, want an error", name)
		}
	}
	if got := stub.count(); got != 0 {
		t.Errorf("requests = %d, want the guards to stop before any call", got)
	}
}

// A provider that returns only whitespace is answered with "", which the caller
// treats as nothing to post.
func TestBlankAnswerIsTrimmedToEmpty(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"content":"   \n"}}]}`)
	got, err := stub.client.Complete(context.Background(), openAIConfig(stub.url), "问题")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "" {
		t.Errorf("answer = %q, want the whitespace trimmed away", got)
	}
}

func TestPerCallTimeoutApplies(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"content":"too late"}}]}`)
	stub.slow(300 * time.Millisecond)

	cfg := openAIConfig(stub.url)
	cfg.Timeout = 30 * time.Millisecond

	start := time.Now()
	if _, err := stub.client.Complete(context.Background(), cfg, "问题"); err == nil {
		t.Fatal("Complete succeeded past its timeout")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("took %v, want the configured timeout to cut the call short", elapsed)
	}
}

// MaxTokens is required by two of the three protocols, so a zero from an older
// row is given a working default instead of being sent as 0.
func TestMissingMaxTokensGetsADefault(t *testing.T) {
	stub := newStub(t, 200, `{"choices":[{"message":{"content":"ok"}}]}`)
	cfg := openAIConfig(stub.url)
	cfg.MaxTokens = 0

	if _, err := stub.client.Complete(context.Background(), cfg, "问题"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if n, _ := stub.last(t).body["max_tokens"].(float64); n != 1024 {
		t.Errorf("max_tokens = %v, want the 1024 default", stub.last(t).body["max_tokens"])
	}
}
