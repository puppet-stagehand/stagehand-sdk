package local

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// canonicalNeed is the free-text input every Recommend test starts from.
const canonicalNeed = "I need to manage security settings on my Windows servers"

// scriptedLLM is an LLMClient double. replies[i] answers the i-th call; an
// unscripted call returns Internal. Every request it receives is recorded so
// tests can assert on what would have left the process.
type scriptedLLM struct {
	mu        sync.Mutex
	replies   []string
	calls     []LLMRequest
	providers []LLMProvider
}

func (f *scriptedLLM) Complete(_ context.Context, p LLMProvider, r LLMRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	f.providers = append(f.providers, p)
	i := len(f.calls) - 1
	if i >= len(f.replies) {
		return "", status.Error(codes.Internal, "unscripted call")
	}
	return f.replies[i], nil
}

func (f *scriptedLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// mustConfigureLLMProvider performs the same two-call sequence a pack itself
// would: seal the provider config as one Secrets value, then write a
// name/label/ref-only index document into llmProviderCollection.
func mustConfigureLLMProvider(t *testing.T, h *host.Host, name, label, kind, baseURL, model, apiKey string) {
	t.Helper()
	secretJSON, err := json.Marshal(llmProviderSecret{Kind: kind, BaseURL: baseURL, Model: model, APIKey: apiKey})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{
		Name: "llm-provider-" + name, Plaintext: secretJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := structpb.NewStruct(map[string]any{"name": name, "label": label, "secret_ref": ref.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: llmProviderCollection, DocId: name,
		Body: &hostv1.Json{Value: body}, IfVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}
}

// extractionReply and rankingReply build the two scripted LLM answers.
func extractionReply(queries ...string) string {
	b, _ := json.Marshal(map[string]any{"queries": queries})
	return string(b)
}

type scriptedRank struct{ Name, Source, Reasoning string }

func rankingReply(entries ...scriptedRank) string {
	out := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]string{"name": e.Name, "source": e.Source, "reasoning": e.Reasoning})
	}
	b, _ := json.Marshal(map[string]any{"suggestions": out})
	return string(b)
}

func TestRecommendTracer(t *testing.T) {
	scripted := &scriptedLLM{replies: []string{
		extractionReply("windows security"),
		rankingReply(scriptedRank{"puppetlabs/apache", "puppet-forge", "Manages web server hardening settings."}),
	}}
	h := New([]string{"forge:recommend", "secrets:rw"}, "pkg", WithForgeClient(forgeFixture{}), WithLLMClient(scripted))
	mustConfigureLLMProvider(t, h, "primary", "Primary", llmKindAnthropic, "", "test-model", "sk-test-key")

	resp, err := h.Forge.Recommend(context.Background(), &hostv1.RecommendRequest{
		Text: canonicalNeed, LlmProvider: "primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := scripted.callCount(); got != 2 {
		t.Fatalf("expected exactly 2 LLM calls, got %d", got)
	}
	if len(resp.Suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %+v", resp.Suggestions)
	}
	s := resp.Suggestions[0]
	if s.Rank != 1 || s.Reasoning == "" {
		t.Fatalf("expected rank 1 with reasoning, got %+v", s)
	}
	// Module facts equal forgeFixture's own values, not anything the ranker
	// supplied.
	if s.Module.Name != "puppetlabs/apache" || s.Module.Version != "12.0.0" || s.Module.Endorsement != "Supported" {
		t.Fatalf("module facts do not match the fixture's search result: %+v", s.Module)
	}
	if len(resp.Queries) != 1 || resp.Queries[0] != "windows security" {
		t.Fatalf("expected the searched query to be echoed, got %v", resp.Queries)
	}
	if scripted.providers[0].APIKey != "sk-test-key" || scripted.providers[0].Model != "test-model" {
		t.Fatalf("expected the resolved provider to be handed to the LLM client, got %v", scripted.providers[0])
	}
}
