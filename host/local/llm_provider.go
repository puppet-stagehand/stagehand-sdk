package local

// This file reads a configured LLM provider back out of Documents + Secrets.
// It mirrors forge.go's forge-sources convention: a pack seals the provider's
// config with Secrets.Store, then writes a name-only index document into
// llmProviderCollection pointing at the resulting ref. The index document
// never carries the kind, base URL, model or API key.

import (
	"context"
	"encoding/json"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// llmProviderCollection is the Documents collection holding the name-only
// llm-providers index: one document per configured provider, keyed by name,
// body {name, label, secret_ref}.
const llmProviderCollection = "llm-providers"

// defaultAnthropicBaseURL is the vendor's public API base, used when an
// anthropic-kind provider seals no base URL of its own.
const defaultAnthropicBaseURL = "https://api.anthropic.com"

// llmProviderSecret is the sealed JSON shape an llm-providers index entry's
// secret_ref resolves to via Secrets.Reveal. It is an internal host.Local
// convention a pack must follow when configuring a provider, not part of the
// public wire contract.
type llmProviderSecret struct {
	Kind           string `json:"kind"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKey         string `json:"api_key"`
	MaxTokensField string `json:"max_tokens_field"`
}

// resolveLLMProvider turns an explicit provider name into its decoded
// configuration. There is no default provider and no only-one-configured
// fallback (D-03). The error ladder matches resolveSource: index miss
// NotFound; empty secret_ref, reveal failure, malformed JSON Internal.
// docs.mu is held for the index read only and released before Reveal, so no
// lock is ever held across network I/O.
func (s *forgeServer) resolveLLMProvider(ctx context.Context, name string) (LLMProvider, error) {
	if name == "" {
		return LLMProvider{}, status.Error(codes.InvalidArgument, "llm_provider is required")
	}

	s.docs.mu.Lock()
	doc, ok := s.docs.getLocked(llmProviderCollection, name)
	var secretRef string
	if ok && doc.Body != nil && doc.Body.Value != nil {
		secretRef, _ = doc.Body.Value.AsMap()["secret_ref"].(string)
	}
	s.docs.mu.Unlock()
	if !ok {
		return LLMProvider{}, status.Errorf(codes.NotFound, "llm provider %q is not configured", name)
	}
	if secretRef == "" {
		return LLMProvider{}, status.Errorf(codes.Internal, "llm provider %q index entry has no secret_ref", name)
	}

	revealed, err := s.secrets.Reveal(ctx, &hostv1.SecretRef{Ref: secretRef})
	if err != nil {
		return LLMProvider{}, status.Errorf(codes.Internal, "llm provider %q secret could not be revealed", name)
	}
	var sealed llmProviderSecret
	if err := json.Unmarshal(revealed.Plaintext, &sealed); err != nil {
		return LLMProvider{}, status.Errorf(codes.Internal, "llm provider %q secret is malformed", name)
	}
	return LLMProvider{
		Kind:           sealed.Kind,
		BaseURL:        sealed.BaseURL,
		Model:          sealed.Model,
		APIKey:         sealed.APIKey,
		MaxTokensField: sealed.MaxTokensField,
	}, nil
}
