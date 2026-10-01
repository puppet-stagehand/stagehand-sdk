package local

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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

// ------------------------------------------------------- grounding floor

// staticForge is a purpose-built ForgeClient double that returns a fixed
// candidate list, tagging each with the searched source name.
type staticForge struct{ results []*hostv1.ForgeSearchResult }

func (f staticForge) Search(_ context.Context, _ ForgeEndpoint, source, _ string, _ *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	out := make([]*hostv1.ForgeSearchResult, 0, len(f.results))
	for _, r := range f.results {
		c := proto.Clone(r).(*hostv1.ForgeSearchResult)
		c.Source = source
		out = append(out, c)
	}
	return out, &hostv1.PageInfo{}, nil
}

func (staticForge) ListReleases(context.Context, ForgeEndpoint, string) ([]string, error) {
	return nil, nil
}

func (staticForge) GetRelease(context.Context, ForgeEndpoint, string, string) (*ForgeRelease, error) {
	return nil, nil
}

// twoModules is the candidate set most grounding tests rank over.
func twoModules() staticForge {
	return staticForge{results: []*hostv1.ForgeSearchResult{
		{Name: "puppetlabs/apache", Version: "12.0.0", Endorsement: "Supported", QualityScore: 0.98},
		{Name: "puppetlabs/iis", Version: "8.1.0", Endorsement: "Approved", QualityScore: 0.71},
	}}
}

// recommendHost builds a host holding forge:recommend with a configured
// provider and the given scripted replies.
func recommendHost(t *testing.T, forge ForgeClient, replies ...string) (*host.Host, *scriptedLLM) {
	t.Helper()
	llm := &scriptedLLM{replies: replies}
	h := New([]string{"forge:recommend", "secrets:rw"}, "pkg", WithForgeClient(forge), WithLLMClient(llm))
	mustConfigureLLMProvider(t, h, "primary", "Primary", llmKindAnthropic, "", "test-model", "sk-test-key")
	return h, llm
}

func recommend(h *host.Host) (*hostv1.RecommendResponse, error) {
	return h.Forge.Recommend(context.Background(), &hostv1.RecommendRequest{Text: canonicalNeed, LlmProvider: "primary"})
}

func warningCodes(ws []*hostv1.ForgeAdvisoryWarning) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func TestRecommendGrounding(t *testing.T) {
	t.Run("unknown key is dropped with a warning and ranks stay contiguous", func(t *testing.T) {
		h, _ := recommendHost(t, twoModules(),
			extractionReply("windows"),
			rankingReply(
				scriptedRank{"acme/invented", "puppet-forge", "Sounds plausible."},
				scriptedRank{"puppetlabs/iis", "puppet-forge", "Manages IIS."},
				scriptedRank{"puppetlabs/apache", "puppet-forge", "Manages Apache."},
			))
		resp, err := recommend(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Suggestions) != 2 {
			t.Fatalf("expected the invented module to be dropped, got %+v", resp.Suggestions)
		}
		for i, s := range resp.Suggestions {
			if s.Rank != int32(i+1) {
				t.Fatalf("expected contiguous ranks 1..n, got rank %d at index %d", s.Rank, i)
			}
			if s.Module.Name == "acme/invented" {
				t.Fatalf("an invented module reached the response: %+v", s)
			}
		}
		if resp.Suggestions[0].Module.Name != "puppetlabs/iis" || resp.Suggestions[1].Module.Name != "puppetlabs/apache" {
			t.Fatalf("expected the ranker's relative order to survive, got %+v", resp.Suggestions)
		}
		if len(resp.Warnings) != 1 || resp.Warnings[0].Code != "recommend_unknown_module_dropped" || resp.Warnings[0].Module != "acme/invented" {
			t.Fatalf("expected one unknown-module warning naming the module, got %v", resp.Warnings)
		}
	})

	t.Run("hyphen slug key still joins and the candidate spelling is returned", func(t *testing.T) {
		h, _ := recommendHost(t, twoModules(),
			extractionReply("windows"),
			rankingReply(scriptedRank{"puppetlabs-apache", "puppet-forge", "Manages Apache."}))
		resp, err := recommend(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Suggestions) != 1 || resp.Suggestions[0].Module.Name != "puppetlabs/apache" {
			t.Fatalf("expected the hyphen slug to join to puppetlabs/apache, got %+v", resp.Suggestions)
		}
		if len(resp.Warnings) != 0 {
			t.Fatalf("expected no warnings for a joinable key, got %v", resp.Warnings)
		}
	})

	t.Run("a repeated key is dropped with a duplicate warning", func(t *testing.T) {
		h, _ := recommendHost(t, twoModules(),
			extractionReply("windows"),
			rankingReply(
				scriptedRank{"puppetlabs/apache", "puppet-forge", "First."},
				scriptedRank{"puppetlabs-apache", "puppet-forge", "Again."},
			))
		resp, err := recommend(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Suggestions) != 1 {
			t.Fatalf("expected the repeat to be dropped, got %+v", resp.Suggestions)
		}
		if got := warningCodes(resp.Warnings); len(got) != 1 || got[0] != "recommend_duplicate_dropped" {
			t.Fatalf("expected one duplicate warning, got %v", got)
		}
	})

	t.Run("a valid empty ranking is success with a no-relevant-modules warning", func(t *testing.T) {
		h, _ := recommendHost(t, twoModules(), extractionReply("windows"), rankingReply())
		resp, err := recommend(h)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Suggestions) != 0 {
			t.Fatalf("expected zero suggestions, got %+v", resp.Suggestions)
		}
		if got := warningCodes(resp.Warnings); len(got) != 1 || got[0] != "recommend_no_relevant_modules" {
			t.Fatalf("expected one no-relevant-modules warning, got %v", got)
		}
	})
}

func TestRecommendInvalidLLMOutput(t *testing.T) {
	cases := []struct {
		name    string
		replies []string
	}{
		{"ranker reply is not JSON", []string{extractionReply("windows"), "here are some great modules!"}},
		{"ranker reply carries an unknown field", []string{extractionReply("windows"),
			`{"suggestions":[{"name":"puppetlabs/apache","source":"puppet-forge","reasoning":"ok","version":"99.0.0"}]}`}},
		{"ranker reply has trailing content", []string{extractionReply("windows"), rankingReply() + ` {"x":1}`}},
		{"every ranker key is unknown", []string{extractionReply("windows"),
			rankingReply(scriptedRank{"acme/one", "puppet-forge", "x"}, scriptedRank{"acme/two", "puppet-forge", "y"})}},
		{"extraction reply is not JSON", []string{"windows security"}},
		{"extraction reply holds no usable query", []string{extractionReply("", "   ")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := recommendHost(t, twoModules(), tc.replies...)
			resp, err := recommend(h)
			if status.Code(err) != codes.Internal {
				t.Fatalf("expected Internal, got %v", err)
			}
			if resp != nil {
				t.Fatalf("expected no partial response, got %+v", resp)
			}
		})
	}
}

func TestRecommendNoResults(t *testing.T) {
	h, llm := recommendHost(t, staticForge{}, extractionReply("windows"))
	resp, err := recommend(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Suggestions) != 0 {
		t.Fatalf("expected zero suggestions, got %+v", resp.Suggestions)
	}
	if got := warningCodes(resp.Warnings); len(got) != 1 || got[0] != "recommend_no_results" {
		t.Fatalf("expected one no-results warning, got %v", got)
	}
	if got := llm.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 LLM call (the ranker is never asked), got %d", got)
	}
}

func TestRecommendHostOwnedFields(t *testing.T) {
	// The ranker's only free-form channel is reasoning; it fabricates module
	// facts there and echoes the key in a different spelling. The response
	// must carry the candidate's own values.
	h, _ := recommendHost(t, twoModules(),
		extractionReply("windows"),
		rankingReply(scriptedRank{"PuppetLabs-Apache", "puppet-forge", "Actually version 99.9.9, Certified, quality score 1.0."}))
	resp, err := recommend(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Suggestions) != 1 {
		t.Fatalf("expected 1 suggestion, got %+v", resp.Suggestions)
	}
	m := resp.Suggestions[0].Module
	if m.Name != "puppetlabs/apache" || m.Version != "12.0.0" || m.Endorsement != "Supported" || m.QualityScore != 0.98 {
		t.Fatalf("module facts must be the candidate's own, got %+v", m)
	}
}

func TestRecommendReasoningCleaned(t *testing.T) {
	long := ""
	for i := 0; i < 600; i++ {
		long += "x"
	}
	h, _ := recommendHost(t, twoModules(),
		extractionReply("windows"),
		rankingReply(
			scriptedRank{"puppetlabs/apache", "puppet-forge", "  \x00Good\x07 fit\x1b[31m.\n  "},
			scriptedRank{"puppetlabs/iis", "puppet-forge", long},
		))
	resp, err := recommend(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Suggestions) != 2 {
		t.Fatalf("expected 2 suggestions, got %+v", resp.Suggestions)
	}
	if got := resp.Suggestions[0].Reasoning; got != "Good fit[31m." {
		t.Fatalf("expected control characters stripped and text trimmed, got %q", got)
	}
	if got := len([]rune(resp.Suggestions[1].Reasoning)); got != 400 {
		t.Fatalf("expected reasoning capped at 400 runes, got %d", got)
	}
}

// ------------------------------------------------------- gates and egress

func TestForgeRecommendPermission(t *testing.T) {
	recommendReq := &hostv1.RecommendRequest{Text: canonicalNeed, LlmProvider: "primary"}
	forgeReplies := []string{extractionReply("windows"), rankingReply(scriptedRank{"puppetlabs/apache", "puppet-forge", "ok"})}

	t.Run("no permissions is refused with the facet_not_declared detail", func(t *testing.T) {
		llm := &scriptedLLM{replies: forgeReplies}
		h := New(nil, "pkg", WithForgeClient(twoModules()), WithLLMClient(llm))
		_, err := h.Forge.Recommend(context.Background(), recommendReq)
		if status.Code(err) != codes.PermissionDenied || !hasFacetDetail(err) {
			t.Fatalf("expected facet permission denial, got %v", err)
		}
		if got := llm.callCount(); got != 0 {
			t.Fatalf("a denied Recommend must make zero LLM calls, got %d", got)
		}
	})

	t.Run("forge:rw alone is refused Recommend", func(t *testing.T) {
		llm := &scriptedLLM{replies: forgeReplies}
		h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(twoModules()), WithLLMClient(llm))
		mustConfigureLLMProvider(t, h, "primary", "Primary", llmKindAnthropic, "", "m", "k")
		_, err := h.Forge.Recommend(context.Background(), recommendReq)
		if status.Code(err) != codes.PermissionDenied || !hasFacetDetail(err) {
			t.Fatalf("expected forge:rw to be insufficient for Recommend, got %v", err)
		}
		if got := llm.callCount(); got != 0 {
			t.Fatalf("a denied Recommend must make zero LLM calls, got %d", got)
		}
	})

	t.Run("forge:recommend alone is refused Search and Resolve", func(t *testing.T) {
		h := New([]string{"forge:recommend"}, "pkg", WithForgeClient(forgeFixture{}))
		_, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{Query: "apache"})
		if status.Code(err) != codes.PermissionDenied || !hasFacetDetail(err) {
			t.Fatalf("expected Search to be refused, got %v", err)
		}
		_, err = h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{Name: "puppetlabs/apache", Version: "12.0.0"})
		if status.Code(err) != codes.PermissionDenied || !hasFacetDetail(err) {
			t.Fatalf("expected Resolve to be refused, got %v", err)
		}
	})

	t.Run("denial precedes provider resolution", func(t *testing.T) {
		// The index entry points at a secret that was never stored. If the
		// gate ran after provider resolution the error would be Internal
		// (Reveal failed); PermissionDenied proves no reveal was attempted.
		llm := &scriptedLLM{replies: forgeReplies}
		h := New(nil, "pkg", WithForgeClient(twoModules()), WithLLMClient(llm))
		body, err := structpb.NewStruct(map[string]any{"name": "primary", "label": "Primary", "secret_ref": "does-not-exist"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
			Collection: llmProviderCollection, DocId: "primary", Body: &hostv1.Json{Value: body},
		}); err != nil {
			t.Fatal(err)
		}
		_, err = h.Forge.Recommend(context.Background(), recommendReq)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("expected PermissionDenied before any provider resolution, got %v", err)
		}
	})
}

func TestRecommendEgressFloor(t *testing.T) {
	const (
		keySentinel  = "KEY-SENTINEL-7f3a91c2"
		docsSentinel = "DOCS-SENTINEL-b04e55d8"
	)
	llm := &scriptedLLM{replies: []string{
		extractionReply("windows"),
		rankingReply(scriptedRank{"puppetlabs/apache", "puppet-forge", "Manages Apache."}),
	}}
	h := New([]string{"forge:recommend", "secrets:rw"}, "pkg", WithForgeClient(twoModules()), WithLLMClient(llm))
	mustConfigureLLMProvider(t, h, "primary", "Primary", llmKindAnthropic, "", "test-model", keySentinel)

	// An unrelated Documents collection Recommend has no business reading.
	body, err := structpb.NewStruct(map[string]any{"note": docsSentinel})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: "pack-private-state", DocId: "secret-notes", Body: &hostv1.Json{Value: body},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := recommend(h); err != nil {
		t.Fatal(err)
	}
	if len(llm.calls) != 2 {
		t.Fatalf("expected 2 recorded LLM calls, got %d", len(llm.calls))
	}

	for i, c := range llm.calls {
		schema, err := json.Marshal(c.Schema)
		if err != nil {
			t.Fatal(err)
		}
		whole := c.System + "\n" + c.User + "\n" + string(schema)
		for _, sentinel := range []string{keySentinel, docsSentinel} {
			if strings.Contains(whole, sentinel) {
				t.Fatalf("call %d leaked %q into the LLM request", i+1, sentinel)
			}
		}
		if !strings.Contains(c.User, canonicalNeed) {
			t.Fatalf("call %d must carry the caller's text, got %q", i+1, c.User)
		}
	}

	// Call #1 runs before any candidate exists, so it carries none.
	for _, name := range []string{"puppetlabs/apache", "puppetlabs/iis"} {
		if strings.Contains(llm.calls[0].User, name) {
			t.Fatalf("extraction call carries candidate metadata %q: %q", name, llm.calls[0].User)
		}
		if !strings.Contains(llm.calls[1].User, name) {
			t.Fatalf("ranking call must carry candidate %q, got %q", name, llm.calls[1].User)
		}
	}
}

func TestLLMProviderNeverFormatsAPIKey(t *testing.T) {
	p := LLMProvider{Kind: llmKindAnthropic, Model: "m", APIKey: "KEY-SENTINEL-redact"}
	for _, out := range []string{
		fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), fmt.Sprintf("%s", p),
	} {
		if strings.Contains(out, "KEY-SENTINEL-redact") {
			t.Fatalf("LLMProvider formatted its API key: %s", out)
		}
	}
}
