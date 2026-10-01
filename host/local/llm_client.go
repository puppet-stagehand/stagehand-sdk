package local

// This file declares the LLM seam Forge.Recommend talks through and the
// default implementation behind it: a kind dispatcher over two plain net/http
// protocol clients (llm_anthropic.go, llm_openai.go) that share one bounded,
// redirect-refusing transport posture defined here. Tests and pack examples
// inject a fixture with WithLLMClient, exactly as they inject a ForgeClient
// with WithForgeClient.
//
// Posture every protocol inherits (change it here, never per client):
//   - a 3xx is never followed and is itself an error, so a redirect can never
//     carry the provider's API key to another host (T-08-30);
//   - the response body is capped at llmMaxResponseBytes with explicit
//     overflow detection, and each call has its own client-side timeout
//     independent of the caller's context, with no retries (T-08-34);
//   - no returned message ever contains the transport error text, the request
//     URL or any part of the provider's response body: Go wraps the URL
//     (userinfo included) into transport errors and a provider can quote the
//     prompt back in an error body (T-08-31).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The two provider kinds a sealed llm-providers config may name.
const (
	llmKindAnthropic        = "anthropic"
	llmKindOpenAICompatible = "openai_compatible"
)

const (
	// llmHTTPTimeout bounds every provider call's own client-side timeout,
	// independent of any shorter deadline the caller's context carries.
	llmHTTPTimeout = 45 * time.Second

	// llmMaxResponseBytes bounds how much of a provider response body is read.
	// One byte past it is read so an oversized body is detected, not silently
	// truncated and then misreported as malformed JSON.
	llmMaxResponseBytes = 1 << 20 // 1 MiB

	// llmDefaultMaxOutputTokens is used only when a request names no budget;
	// the Messages API requires max_tokens on every call.
	llmDefaultMaxOutputTokens = 2048
)

// LLMProvider is one resolved provider's configuration. APIKey must never
// reach a log, a status message, or a formatted struct dump, so the type
// redacts it from every fmt verb.
type LLMProvider struct {
	Kind           string
	BaseURL        string
	Model          string
	APIKey         string
	MaxTokensField string
}

// String implements fmt.Stringer without the API key.
func (p LLMProvider) String() string {
	return fmt.Sprintf("LLMProvider{Kind:%q BaseURL:%q Model:%q APIKey:[redacted]}", p.Kind, p.BaseURL, p.Model)
}

// GoString implements fmt.GoStringer without the API key.
func (p LLMProvider) GoString() string { return p.String() }

// Format implements fmt.Formatter so %v, %+v and %#v all stay redacted.
func (p LLMProvider) Format(f fmt.State, _ rune) { _, _ = fmt.Fprint(f, p.String()) }

// LLMRequest is one provider-agnostic completion request. System carries the
// host's rules; User carries the caller's text and candidate metadata inside
// a labelled data block. Schema, when set, is the JSON Schema the reply must
// satisfy (providers that support structured output use it).
type LLMRequest struct {
	System          string
	User            string
	MaxOutputTokens int
	Schema          map[string]any
}

// LLMClient is the injectable seam. Complete returns the model's plain-text
// reply; JSON parsing and validation are host-side and identical for every
// provider.
type LLMClient interface {
	Complete(ctx context.Context, p LLMProvider, req LLMRequest) (string, error)
}

// ------------------------------------------------------------- dispatcher

// defaultLLMClient routes a call to the protocol client for the provider's
// kind over one shared HTTP client.
type defaultLLMClient struct {
	hc *http.Client
}

// newDefaultLLMClient builds the dispatcher. A nil hc gets the production
// client; tests pass httptest's server.Client() so the TLS trust root matches
// the test server's self-signed certificate. Either way the redirect and
// timeout posture is applied (see newLLMHTTPClient).
func newDefaultLLMClient(hc *http.Client) *defaultLLMClient {
	return &defaultLLMClient{hc: newLLMHTTPClient(hc)}
}

// DefaultLLMClient returns the real net/http-backed LLMClient host.Local wires
// in when WithLLMClient was never applied. It speaks the Anthropic Messages
// API and OpenAI-compatible chat completions.
func DefaultLLMClient() LLMClient { return newDefaultLLMClient(nil) }

// Complete validates the provider's base URL, then dispatches on its kind.
func (c *defaultLLMClient) Complete(ctx context.Context, p LLMProvider, req LLMRequest) (string, error) {
	base, err := validateLLMBaseURL(p.BaseURL)
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition, "llm provider base URL is not usable: %v", err)
	}
	switch p.Kind {
	case llmKindAnthropic:
		return completeAnthropic(ctx, c.hc, base, p, req)
	default:
		return "", status.Error(codes.FailedPrecondition, "llm provider kind is not supported")
	}
}

// ------------------------------------------------------------- transport

// newLLMHTTPClient returns an HTTP client that never follows a redirect and
// carries llmHTTPTimeout. A nil hc builds a fresh client; a supplied one (a
// test's httptest client) is shallow-copied so the caller's value is not
// mutated, keeping its transport and trust roots.
func newLLMHTTPClient(hc *http.Client) *http.Client {
	var c http.Client
	if hc != nil {
		c = *hc
	}
	if c.Timeout == 0 || c.Timeout > llmHTTPTimeout {
		c.Timeout = llmHTTPTimeout
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// postJSON POSTs reqBody as JSON to rawURL with the given headers (empty
// values are skipped) and decodes a 2xx reply into out. The status ladder is
// shared by every protocol:
//
//	network error, timeout, canceled context, 408, 429, 5xx (529 included) -> Unavailable
//	401, 403, 404                                                           -> FailedPrecondition
//	any 3xx (never followed)                                                -> FailedPrecondition
//	any other non-2xx, an oversized body, a body that does not decode       -> Internal
func postJSON(ctx context.Context, hc *http.Client, rawURL string, headers map[string]string, reqBody, out any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return status.Error(codes.Internal, "llm request could not be encoded")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload))
	if err != nil {
		return status.Error(codes.Internal, "llm request could not be built")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		if v != "" {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := hc.Do(httpReq)
	if err != nil {
		// Never surface err.Error(): net/http wraps it around the full
		// request URL, userinfo and all.
		return status.Error(codes.Unavailable, "llm provider request failed (network error, timeout, or canceled context)")
	}
	defer resp.Body.Close()

	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		// fall through to body handling
	case code >= 300 && code < 400:
		return status.Error(codes.FailedPrecondition, "llm provider answered with a redirect, which is never followed; fix the provider base URL")
	case code == http.StatusUnauthorized, code == http.StatusForbidden, code == http.StatusNotFound:
		return status.Errorf(codes.FailedPrecondition, "llm provider rejected the request (%d); fix the provider configuration (API key, model or base URL)", code)
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		return status.Errorf(codes.Unavailable, "llm provider is unavailable or rate limited (%d)", code)
	default:
		return status.Errorf(codes.Internal, "llm provider answered with unexpected status %d", code)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, llmMaxResponseBytes+1))
	if err != nil {
		return status.Error(codes.Unavailable, "llm provider response could not be read")
	}
	if len(body) > llmMaxResponseBytes {
		return status.Error(codes.Internal, "llm provider response exceeds the size limit")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return status.Error(codes.Internal, "llm provider response was not valid JSON")
	}
	return nil
}

// ------------------------------------------------------------- validation

// validateLLMBaseURL enforces T-08-32: an absolute URL with a host, no
// embedded userinfo, and https — except that plain http is permitted when and
// only when the host is the localhost name or a loopback address, which is
// what makes a local model server (Ollama, vLLM) usable. Messages never echo
// the raw input: url.Parse errors quote it, userinfo included.
func validateLLMBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("base URL is not a valid URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("base URL must include a host")
	}
	if u.User != nil {
		return nil, errors.New("base URL must not embed credentials")
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return u, nil
		}
		return nil, errors.New("base URL must use https (plain http is allowed only for a loopback host)")
	default:
		return nil, errors.New("base URL must use https")
	}
}

// isLoopbackHost reports whether host is the localhost name or a loopback IP.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
