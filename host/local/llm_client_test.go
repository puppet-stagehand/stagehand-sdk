package local

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// llmSeen is one request a fixture provider received.
type llmSeen struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// llmRecorder records every request a fixture provider handler sees. Handlers
// run on the server's goroutines, so captures go through the mutex.
type llmRecorder struct {
	mu   sync.Mutex
	seen []llmSeen
}

func (r *llmRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, llmSeen{method: req.Method, path: req.URL.Path, header: req.Header.Clone(), body: body})
}

func (r *llmRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *llmRecorder) only(t *testing.T) llmSeen {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) != 1 {
		t.Fatalf("expected exactly 1 provider request, got %d", len(r.seen))
	}
	return r.seen[0]
}

// bodyMap decodes the recorded request body into a generic map.
func (s llmSeen) bodyMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.body, &m); err != nil {
		t.Fatalf("recorded request body is not JSON: %v (%s)", err, s.body)
	}
	return m
}

// newLLMTestServer starts a TLS provider fixture and returns the server, a
// dispatcher whose HTTP client trusts it, and the request recorder. The
// handler is called after the request is recorded.
func newLLMTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *defaultLLMClient, *llmRecorder) {
	t.Helper()
	rec := &llmRecorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, newDefaultLLMClient(srv.Client()), rec
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requireCode asserts err carries the given gRPC code.
func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %v error, got nil", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("expected code %v, got %v (%v)", want, got, err)
	}
}

// ------------------------------------------------------------- anthropic

func TestLLMAnthropicCompleteHappyPath(t *testing.T) {
	const reply = `{"queries":["windows security"]}`
	srv, client, rec := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"content": []map[string]any{
				{"type": "thinking", "thinking": "ignored block"},
				{"type": "text", "text": reply},
			},
			"stop_reason": "end_turn",
		})
	})
	schema := map[string]any{"type": "object", "properties": map[string]any{"queries": map[string]any{"type": "array"}}}

	got, err := client.Complete(context.Background(), LLMProvider{
		Kind: llmKindAnthropic, BaseURL: srv.URL, Model: "claude-test-model", APIKey: "KEY-SENTINEL-anthropic",
	}, LLMRequest{System: "sys rules", User: "user text", MaxOutputTokens: 1234, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if got != reply {
		t.Fatalf("expected the text block verbatim, got %q", got)
	}

	seen := rec.only(t)
	if seen.method != http.MethodPost || seen.path != "/v1/messages" {
		t.Fatalf("unexpected request line: %s %s", seen.method, seen.path)
	}
	if seen.header.Get("x-api-key") != "KEY-SENTINEL-anthropic" {
		t.Fatalf("x-api-key header missing or wrong: %q", seen.header.Get("x-api-key"))
	}
	if seen.header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic-version header missing or wrong: %q", seen.header.Get("anthropic-version"))
	}
	if ct := seen.header.Get("content-type"); ct != "application/json" {
		t.Fatalf("content-type header wrong: %q", ct)
	}

	body := seen.bodyMap(t)
	if body["model"] != "claude-test-model" {
		t.Fatalf("model not carried: %v", body["model"])
	}
	if body["max_tokens"] != float64(1234) {
		t.Fatalf("output budget not carried: %v", body["max_tokens"])
	}
	if body["system"] != "sys rules" {
		t.Fatalf("system not carried: %v", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one message, got %v", body["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "user text" {
		t.Fatalf("unexpected user message: %v", m0)
	}
	oc, _ := body["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config json_schema block missing: %v", body["output_config"])
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Fatalf("output_config schema missing: %v", format)
	}
	for _, forbidden := range []string{"temperature", "top_p", "top_k", "thinking", "effort", "tools", "tool_choice"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("request carries %q, which current models reject or the plan forbids: %s", forbidden, seen.body)
		}
	}
	if strings.Contains(string(seen.body), "KEY-SENTINEL-anthropic") {
		t.Fatalf("API key leaked into the request body")
	}
}

func TestLLMAnthropicOmitsOutputConfigWithoutSchema(t *testing.T) {
	srv, client, rec := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "stop_reason": "end_turn"})
	})
	if _, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k"},
		LLMRequest{User: "u"}); err != nil {
		t.Fatal(err)
	}
	body := rec.only(t).bodyMap(t)
	if _, ok := body["output_config"]; ok {
		t.Fatalf("output_config sent without a schema: %v", body)
	}
	if body["max_tokens"] != float64(llmDefaultMaxOutputTokens) {
		t.Fatalf("expected the default budget when none is requested, got %v", body["max_tokens"])
	}
}

func TestLLMAnthropicBadReplyIsInternal(t *testing.T) {
	cases := map[string]map[string]any{
		"max_tokens": {"content": []map[string]any{{"type": "text", "text": "{\"cut"}}, "stop_reason": "max_tokens"},
		"refusal":    {"content": []map[string]any{{"type": "text", "text": "no"}}, "stop_reason": "refusal"},
		"empty":      {"content": []map[string]any{{"type": "thinking", "thinking": "only thoughts"}}, "stop_reason": "end_turn"},
		"no content": {"content": []map[string]any{}, "stop_reason": "end_turn"},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, reply) })
			_, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
			requireCode(t, err, codes.Internal)
		})
	}
}

// TestLLMStatusMapping pins the shared status ladder both protocols inherit.
func TestLLMStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   codes.Code
	}{
		{"request timeout", http.StatusRequestTimeout, codes.Unavailable},
		{"rate limited", http.StatusTooManyRequests, codes.Unavailable},
		{"server error", http.StatusInternalServerError, codes.Unavailable},
		{"overloaded 529", 529, codes.Unavailable},
		{"unauthorized", http.StatusUnauthorized, codes.FailedPrecondition},
		{"forbidden", http.StatusForbidden, codes.FailedPrecondition},
		{"not found", http.StatusNotFound, codes.FailedPrecondition},
		{"redirect", http.StatusFound, codes.FailedPrecondition},
		{"bad request", http.StatusBadRequest, codes.Internal},
		{"unprocessable", http.StatusUnprocessableEntity, codes.Internal},
	}
	for _, kind := range []string{llmKindAnthropic, llmKindOpenAICompatible} {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				const quoted = "PROMPT-QUOTED-BY-PROVIDER-SENTINEL"
				srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					if tc.status == http.StatusFound {
						w.Header().Set("Location", "https://example.invalid/elsewhere")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":"`+quoted+`"}`)
				})
				_, err := client.Complete(context.Background(), LLMProvider{
					Kind: kind, BaseURL: srv.URL, Model: "m", APIKey: "KEY-SENTINEL-status",
				}, LLMRequest{User: "u"})
				requireCode(t, err, tc.want)
				msg := err.Error()
				for _, leaked := range []string{quoted, "KEY-SENTINEL-status", srv.URL, "127.0.0.1"} {
					if strings.Contains(msg, leaked) {
						t.Fatalf("status message leaks %q: %s", leaked, msg)
					}
				}
			})
		}
	}
}

func TestLLMUnreachableProviderIsUnavailable(t *testing.T) {
	srv, client, _ := newLLMTestServer(t, func(http.ResponseWriter, *http.Request) {})
	url := srv.URL
	srv.Close() // nothing listens any more

	_, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindAnthropic, BaseURL: url, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
	requireCode(t, err, codes.Unavailable)
	if strings.Contains(err.Error(), url) {
		t.Fatalf("transport error message leaks the request URL: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Complete(ctx, LLMProvider{Kind: llmKindAnthropic, BaseURL: url, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
	requireCode(t, err, codes.Unavailable)
}

func TestLLMOversizedResponseIsInternal(t *testing.T) {
	srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"`)
		_, _ = io.WriteString(w, strings.Repeat("a", llmMaxResponseBytes+10))
		_, _ = io.WriteString(w, `"}],"stop_reason":"end_turn"}`)
	})
	_, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
	requireCode(t, err, codes.Internal)
	if !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected an explicit size-limit message, got %v", err)
	}
}

func TestLLMNonJSONReplyIsInternal(t *testing.T) {
	srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>gateway</html>") })
	_, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindAnthropic, BaseURL: srv.URL, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
	requireCode(t, err, codes.Internal)
}

// ------------------------------------------------- openai-compatible

// openAIReply builds a chat-completions reply with one choice.
func openAIReply(content string, refusal any, finish string) map[string]any {
	return map[string]any{"choices": []map[string]any{{
		"message":       map[string]any{"role": "assistant", "content": content, "refusal": refusal},
		"finish_reason": finish,
	}}}
}

func TestLLMOpenAICompatibleCompleteHappyPath(t *testing.T) {
	const reply = `{"ranked":[]}`
	srv, client, rec := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAIReply(reply, nil, "stop"))
	})
	schema := map[string]any{"type": "object"}

	got, err := client.Complete(context.Background(), LLMProvider{
		Kind: llmKindOpenAICompatible, BaseURL: srv.URL + "/v1", Model: "gpt-test", APIKey: "KEY-SENTINEL-openai",
	}, LLMRequest{System: "sys rules", User: "user text", MaxOutputTokens: 777, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if got != reply {
		t.Fatalf("expected the message content verbatim, got %q", got)
	}

	seen := rec.only(t)
	if seen.method != http.MethodPost || seen.path != "/v1/chat/completions" {
		t.Fatalf("unexpected request line: %s %s", seen.method, seen.path)
	}
	if seen.header.Get("Authorization") != "Bearer KEY-SENTINEL-openai" {
		t.Fatalf("bearer header missing or wrong: %q", seen.header.Get("Authorization"))
	}
	body := seen.bodyMap(t)
	if body["model"] != "gpt-test" {
		t.Fatalf("model not carried: %v", body["model"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected a system then a user message, got %v", body["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	m1, _ := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "sys rules" || m1["role"] != "user" || m1["content"] != "user text" {
		t.Fatalf("unexpected message order or content: %v", msgs)
	}
	if body["max_completion_tokens"] != float64(777) {
		t.Fatalf("expected the newer output-token field by default: %s", seen.body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("legacy max_tokens sent by default (reasoning models reject it): %s", seen.body)
	}
	if _, ok := body["response_format"]; ok {
		t.Fatalf("response_format must not be sent to arbitrary gateways: %s", seen.body)
	}
	if strings.Contains(string(seen.body), "KEY-SENTINEL-openai") {
		t.Fatalf("API key leaked into the request body")
	}
}

func TestLLMOpenAICompatibleLegacyTokenFieldOverride(t *testing.T) {
	srv, client, rec := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAIReply("ok", nil, "stop"))
	})
	if _, err := client.Complete(context.Background(), LLMProvider{
		Kind: llmKindOpenAICompatible, BaseURL: srv.URL, Model: "m", APIKey: "k", MaxTokensField: "max_tokens",
	}, LLMRequest{User: "u", MaxOutputTokens: 321}); err != nil {
		t.Fatal(err)
	}
	seen := rec.only(t)
	if seen.path != "/chat/completions" {
		t.Fatalf("a base without /v1 must post to /chat/completions, got %s", seen.path)
	}
	body := seen.bodyMap(t)
	if body["max_tokens"] != float64(321) {
		t.Fatalf("expected the configured legacy field to carry the budget: %s", seen.body)
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("newer field sent alongside the override: %s", seen.body)
	}
}

func TestLLMOpenAICompatibleOmitsAuthWhenKeyEmpty(t *testing.T) {
	srv, client, rec := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAIReply("ok", nil, "stop"))
	})
	if _, err := client.Complete(context.Background(), LLMProvider{
		Kind: llmKindOpenAICompatible, BaseURL: srv.URL, Model: "m", APIKey: "",
	}, LLMRequest{User: "u"}); err != nil {
		t.Fatal(err)
	}
	seen := rec.only(t)
	if _, present := seen.header["Authorization"]; present {
		t.Fatalf("an empty key must omit Authorization entirely, got %q", seen.header.Get("Authorization"))
	}
}

func TestLLMOpenAICompatibleBadReplyIsInternal(t *testing.T) {
	cases := map[string]map[string]any{
		"cut short":  openAIReply("{\"cut", nil, "length"),
		"refusal":    openAIReply("", "I cannot help with that", "stop"),
		"no choices": {"choices": []map[string]any{}},
		"empty text": openAIReply("", nil, "stop"),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, reply) })
			_, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindOpenAICompatible, BaseURL: srv.URL, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
			requireCode(t, err, codes.Internal)
			if strings.Contains(err.Error(), "I cannot help with that") {
				t.Fatalf("provider refusal text echoed into the status message: %v", err)
			}
		})
	}
}

func TestLLMBaseURLLoopbackHTTPException(t *testing.T) {
	// Loopback plain http is accepted: a local model server with no TLS.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAIReply("local ok", nil, "stop"))
	}))
	t.Cleanup(plain.Close)
	if !strings.HasPrefix(plain.URL, "http://127.0.0.1") {
		t.Fatalf("fixture is not a loopback http server: %s", plain.URL)
	}
	got, err := DefaultLLMClient().Complete(context.Background(), LLMProvider{
		Kind: llmKindOpenAICompatible, BaseURL: plain.URL, Model: "m",
	}, LLMRequest{User: "u"})
	if err != nil || got != "local ok" {
		t.Fatalf("loopback plain http must be accepted, got %q, %v", got, err)
	}

	// https is accepted.
	srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAIReply("tls ok", nil, "stop"))
	})
	if got, err := client.Complete(context.Background(), LLMProvider{Kind: llmKindOpenAICompatible, BaseURL: srv.URL, Model: "m"}, LLMRequest{User: "u"}); err != nil || got != "tls ok" {
		t.Fatalf("https must be accepted, got %q, %v", got, err)
	}

	// Everything else is FailedPrecondition before any network I/O.
	for name, raw := range map[string]string{
		"non-loopback http":      "http://models.example.test/v1",
		"http lookalike host":    "http://localhost.example.test/v1",
		"userinfo over https":    "https://user:pass@models.example.test/v1",
		"userinfo over loopback": "http://user:pass@127.0.0.1:1/v1",
		"no host":                "https:///v1",
		"other scheme":           "ftp://models.example.test/v1",
		"empty":                  "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DefaultLLMClient().Complete(context.Background(), LLMProvider{Kind: llmKindOpenAICompatible, BaseURL: raw, Model: "m", APIKey: "k"}, LLMRequest{User: "u"})
			requireCode(t, err, codes.FailedPrecondition)
			if strings.Contains(err.Error(), "pass@") || strings.Contains(err.Error(), "user:pass") {
				t.Fatalf("validation message echoes embedded credentials: %v", err)
			}
		})
	}
}
