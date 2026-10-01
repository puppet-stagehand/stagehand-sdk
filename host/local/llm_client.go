package local

// This file declares the LLM seam Forge.Recommend talks through. It is a
// declaration only: the real wire clients (anthropic, openai_compatible) and
// the default dispatcher land in a later plan against this interface. Tests
// and pack examples inject a fixture with WithLLMClient, exactly as they
// inject a ForgeClient with WithForgeClient.

import (
	"context"
	"fmt"
)

// The two provider kinds a sealed llm-providers config may name.
const (
	llmKindAnthropic        = "anthropic"
	llmKindOpenAICompatible = "openai_compatible"
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
