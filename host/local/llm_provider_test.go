package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// llmResolveFixture is a forgeServer wired straight to real Documents and
// Secrets servers, so resolveLLMProvider can be driven (and its reveals
// observed) without going through the permission gate.
type llmResolveFixture struct {
	t       *testing.T
	srv     *forgeServer
	secrets *secretsServer

	mu       sync.Mutex
	revealed []string
}

func newLLMResolveFixture(t *testing.T) *llmResolveFixture {
	t.Helper()
	docs := newDocumentsServer("pack")
	secrets := newSecretsServer("pack")
	f := &llmResolveFixture{t: t, secrets: secrets}
	secrets.revealHook = func(ref string) {
		f.mu.Lock()
		f.revealed = append(f.revealed, ref)
		f.mu.Unlock()
	}
	f.srv = newForgeServer("pack", docs, secrets, &recordingForgeClient{}, nil)
	return f
}

func (f *llmResolveFixture) reveals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revealed...)
}

// seal stores plaintext as a secret and writes the name/label/ref index
// document for it, the same two-call sequence a pack performs.
func (f *llmResolveFixture) seal(name string, plaintext string) string {
	f.t.Helper()
	ref, err := f.secrets.Store(context.Background(), &hostv1.StoreSecretRequest{Name: "llm-provider-" + name, Plaintext: []byte(plaintext)})
	if err != nil {
		f.t.Fatal(err)
	}
	f.index(name, ref.Ref)
	return ref.Ref
}

// index writes an index document pointing at ref (which need not exist).
func (f *llmResolveFixture) index(name, ref string) {
	f.t.Helper()
	body, err := structpb.NewStruct(map[string]any{"name": name, "label": name, "secret_ref": ref})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.srv.docs.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: llmProviderCollection, DocId: name, Body: &hostv1.Json{Value: body},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *llmResolveFixture) resolve(name string) (LLMProvider, error) {
	return f.srv.resolveLLMProvider(context.Background(), name)
}

// ------------------------------------------------------------- index shape

func TestLLMProviderIndexExposesOnlyNameLabelRef(t *testing.T) {
	h := New([]string{"secrets:rw"}, "pkg")
	mustConfigureLLMProvider(t, h, "primary", "Primary", llmKindAnthropic, "https://api.example.test", "claude-test", "KEY-SENTINEL-index")

	doc, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: llmProviderCollection, DocId: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	m := doc.Body.Value.AsMap()
	if len(m) != 3 {
		t.Fatalf("expected exactly 3 keys (name, label, secret_ref), got %v", m)
	}
	for _, forbidden := range []string{
		"kind", "base_url", "baseurl", "baseURL", "model", "api_key", "apikey", "apiKey", "key", "max_tokens_field", "maxTokensField", "token",
	} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("llm-providers index document leaked a sealed field %q: %v", forbidden, m)
		}
	}
	if m["name"] != "primary" || m["label"] != "Primary" {
		t.Fatalf("unexpected index document contents: %v", m)
	}
	if ref, ok := m["secret_ref"].(string); !ok || ref == "" {
		t.Fatalf("expected a non-empty secret_ref, got %v", m["secret_ref"])
	}
	if strings.Contains(fmt.Sprint(m), "KEY-SENTINEL-index") {
		t.Fatalf("the API key reached the index document: %v", m)
	}
}

// ---------------------------------------------------------- ladder

func TestLLMProviderResolutionLadder(t *testing.T) {
	const okAnthropic = `{"kind":"anthropic","model":"m","api_key":"k"}`
	cases := []struct {
		name  string
		setup func(f *llmResolveFixture) string // returns the provider name to resolve
		want  codes.Code
	}{
		{"empty name", func(*llmResolveFixture) string { return "" }, codes.InvalidArgument},
		{"unknown name", func(*llmResolveFixture) string { return "ghost" }, codes.NotFound},
		{"index entry without secret_ref", func(f *llmResolveFixture) string { f.index("p", ""); return "p" }, codes.Internal},
		{"reveal failure", func(f *llmResolveFixture) string { f.index("p", "expansion/pack/never-stored"); return "p" }, codes.Internal},
		{"sealed payload is not JSON", func(f *llmResolveFixture) string { f.seal("p", "not json at all"); return "p" }, codes.Internal},
		{"unknown kind", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"bedrock","base_url":"https://x.example.test","model":"m","api_key":"k"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"empty kind", func(f *llmResolveFixture) string {
			f.seal("p", `{"base_url":"https://x.example.test","model":"m","api_key":"k"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"empty model", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"anthropic","api_key":"k"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"base url over plain http on a remote host", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"http://models.example.test/v1","model":"m"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"base url with userinfo", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"https://u:pw@models.example.test/v1","model":"m"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"base url without a host", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"https:///v1","model":"m"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"max_tokens_field that is not a plain name", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"https://x.example.test/v1","model":"m","max_tokens_field":"a b\"c"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"max_tokens_field that would displace the model", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"https://x.example.test/v1","model":"m","max_tokens_field":"model"}`)
			return "p"
		}, codes.FailedPrecondition},
		{"valid anthropic payload resolves", func(f *llmResolveFixture) string { f.seal("p", okAnthropic); return "p" }, codes.OK},
		{"valid loopback http compatible payload resolves", func(f *llmResolveFixture) string {
			f.seal("p", `{"kind":"openai_compatible","base_url":"http://localhost:11434/v1","model":"llama"}`)
			return "p"
		}, codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLLMResolveFixture(t)
			name := tc.setup(f)
			_, err := f.resolve(name)
			if tc.want == codes.OK {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			requireCode(t, err, tc.want)
			if strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "u:pw") {
				t.Fatalf("resolution error echoes embedded credentials: %v", err)
			}
		})
	}
}

func TestLLMProviderPerKindDefaults(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		want        codes.Code
		wantBase    string
		wantKeyKept string
	}{
		{"anthropic: no base, no key", `{"kind":"anthropic","model":"m"}`, codes.FailedPrecondition, "", ""},
		{"anthropic: no base, key", `{"kind":"anthropic","model":"m","api_key":"k"}`, codes.OK, defaultAnthropicBaseURL, "k"},
		{"anthropic: explicit base, no key", `{"kind":"anthropic","base_url":"https://proxy.example.test","model":"m"}`, codes.FailedPrecondition, "", ""},
		{"anthropic: explicit base, key", `{"kind":"anthropic","base_url":"https://proxy.example.test","model":"m","api_key":"k"}`, codes.OK, "https://proxy.example.test", "k"},
		{"compatible: no base, no key", `{"kind":"openai_compatible","model":"m"}`, codes.FailedPrecondition, "", ""},
		{"compatible: no base, key", `{"kind":"openai_compatible","model":"m","api_key":"k"}`, codes.FailedPrecondition, "", ""},
		{"compatible: base, no key", `{"kind":"openai_compatible","base_url":"https://x.example.test/v1","model":"m"}`, codes.OK, "https://x.example.test/v1", ""},
		{"compatible: base, key", `{"kind":"openai_compatible","base_url":"https://x.example.test/v1","model":"m","api_key":"k"}`, codes.OK, "https://x.example.test/v1", "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLLMResolveFixture(t)
			f.seal("p", tc.payload)
			got, err := f.resolve("p")
			if tc.want != codes.OK {
				requireCode(t, err, tc.want)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.BaseURL != tc.wantBase || got.APIKey != tc.wantKeyKept {
				t.Fatalf("unexpected resolution: base %q key %q", got.BaseURL, got.APIKey)
			}
		})
	}
}

func TestLLMProviderIgnoresUnknownSealedField(t *testing.T) {
	f := newLLMResolveFixture(t)
	f.seal("p", `{"kind":"anthropic","model":"m","api_key":"k","future_option":{"nested":true},"another":1}`)
	got, err := f.resolve("p")
	if err != nil {
		t.Fatalf("an unknown sealed field must be ignored so later options are additive, got %v", err)
	}
	if got.Kind != llmKindAnthropic || got.Model != "m" || got.APIKey != "k" {
		t.Fatalf("known fields not resolved: %v", got)
	}
}

func TestLLMProviderResolvesLegacyTokensField(t *testing.T) {
	f := newLLMResolveFixture(t)
	f.seal("p", `{"kind":"openai_compatible","base_url":"https://x.example.test/v1","model":"m","max_tokens_field":"max_tokens"}`)
	got, err := f.resolve("p")
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTokensField != "max_tokens" {
		t.Fatalf("legacy token field not carried: %q", got.MaxTokensField)
	}
}

// ---------------------------------------------------- selective reveal

func TestLLMProviderRevealsOnlySelectedProvider(t *testing.T) {
	f := newLLMResolveFixture(t)
	refA := f.seal("alpha", `{"kind":"anthropic","model":"m-a","api_key":"KEY-A"}`)
	refB := f.seal("beta", `{"kind":"openai_compatible","base_url":"https://b.example.test/v1","model":"m-b","api_key":"KEY-B"}`)

	got, err := f.resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "KEY-A" || got.Model != "m-a" {
		t.Fatalf("resolved the wrong provider: %v", got)
	}
	revealed := f.reveals()
	if len(revealed) != 1 || revealed[0] != refA {
		t.Fatalf("expected exactly one reveal of alpha's secret, got %v", revealed)
	}
	for _, r := range revealed {
		if r == refB {
			t.Fatalf("beta's secret was revealed while resolving alpha")
		}
	}
}

// TestLLMProviderDoesNotHoldDocumentsLockAcrossReveal proves the Documents
// lock is released before Reveal (T-08-37): Recommend runs for seconds across
// several network calls and docs.mu is shared with Documents, Code and
// Inventory.
func TestLLMProviderDoesNotHoldDocumentsLockAcrossReveal(t *testing.T) {
	f := newLLMResolveFixture(t)
	f.seal("p", `{"kind":"anthropic","model":"m","api_key":"k"}`)

	var lockFreeDuringReveal bool
	f.secrets.revealHook = func(string) {
		if f.srv.docs.mu.TryLock() {
			lockFreeDuringReveal = true
			f.srv.docs.mu.Unlock()
		}
	}
	if _, err := f.resolve("p"); err != nil {
		t.Fatal(err)
	}
	if !lockFreeDuringReveal {
		t.Fatalf("docs.mu was held across Secrets.Reveal")
	}
}

// ------------------------------------------------------------ key safety

func TestLLMProviderNeverFormatsTheKey(t *testing.T) {
	const sentinel = "KEY-SENTINEL-format-proof"
	p := LLMProvider{Kind: llmKindOpenAICompatible, BaseURL: "https://x.example.test/v1", Model: "m", APIKey: sentinel, MaxTokensField: "max_tokens"}
	wrapper := struct {
		Provider  LLMProvider
		PtrToProv *LLMProvider
	}{p, &p}

	outputs := map[string]string{
		"%v":        fmt.Sprintf("%v", p),
		"%+v":       fmt.Sprintf("%+v", p),
		"%#v":       fmt.Sprintf("%#v", p),
		"%s":        fmt.Sprintf("%s", p),
		"%v ptr":    fmt.Sprintf("%v", &p),
		"%+v ptr":   fmt.Sprintf("%+v", &p),
		"wrapper":   fmt.Sprintf("%+v", wrapper),
		"wrapper#":  fmt.Sprintf("%#v", wrapper),
		"Sprint":    fmt.Sprint(p),
		"Errorf %w": fmt.Errorf("provider failed: %v", p).Error(),
	}
	for verb, out := range outputs {
		if strings.Contains(out, sentinel) {
			t.Fatalf("formatting a provider with %s leaked the API key: %s", verb, out)
		}
	}
	// The non-secret fields stay visible: they are diagnostics.
	if !strings.Contains(outputs["%+v"], "https://x.example.test/v1") || !strings.Contains(outputs["%+v"], `"m"`) {
		t.Fatalf("diagnostic fields should remain visible, got %s", outputs["%+v"])
	}
}

// TestLLMProviderKeyNeverAppearsInAnyReturnedMessage drives every failure
// branch of the real client with a sentinel key and a hostile provider that
// quotes the key back, and checks no returned message carries it.
func TestLLMProviderKeyNeverAppearsInAnyReturnedMessage(t *testing.T) {
	const sentinel = "KEY-SENTINEL-message-proof"
	for _, code := range []int{400, 401, 404, 429, 500, 529} {
		srv, client, _ := newLLMTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"error":"you sent %s"}`, r.Header.Get("x-api-key")+r.Header.Get("Authorization"))
		})
		for _, kind := range []string{llmKindAnthropic, llmKindOpenAICompatible} {
			_, err := client.Complete(context.Background(), LLMProvider{Kind: kind, BaseURL: srv.URL, Model: "m", APIKey: sentinel}, LLMRequest{User: "u"})
			if err == nil {
				t.Fatalf("expected an error for status %d", code)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("status %d / %s message leaks the key: %v", code, kind, err)
			}
		}
	}
}

func TestLLMClientDoesNotFollowRedirectWithKey(t *testing.T) {
	second := &llmRecorder{}
	secondSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.record(r)
		writeJSON(w, map[string]any{"content": []map[string]any{{"type": "text", "text": "stolen"}}, "stop_reason": "end_turn"})
	}))
	t.Cleanup(secondSrv.Close)

	// httptest TLS servers share one certificate, so a redirect to the second
	// server WOULD succeed if it were followed: a zero count is a real proof,
	// not a handshake failure in disguise.
	for _, redirectCode := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, kind := range []string{llmKindAnthropic, llmKindOpenAICompatible} {
			t.Run(fmt.Sprintf("%d/%s", redirectCode, kind), func(t *testing.T) {
				first, client, firstRec := newLLMTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, secondSrv.URL+r.URL.Path, redirectCode)
				})
				before := second.count()
				got, err := client.Complete(context.Background(), LLMProvider{
					Kind: kind, BaseURL: first.URL, Model: "m", APIKey: "KEY-SENTINEL-redirect",
				}, LLMRequest{User: "u"})
				if err == nil {
					t.Fatalf("a redirect must be an error, got reply %q", got)
				}
				requireCode(t, err, codes.FailedPrecondition)
				if firstRec.count() != 1 {
					t.Fatalf("expected the first server to be asked exactly once, got %d", firstRec.count())
				}
				if n := second.count() - before; n != 0 {
					t.Fatalf("the redirect target received %d request(s); the key may have been forwarded", n)
				}
				if strings.Contains(err.Error(), secondSrv.URL) {
					t.Fatalf("error message names the redirect target: %v", err)
				}
			})
		}
	}
}

// ------------------------------------------------------- default client

func TestHostWithoutWithLLMClientHasADefaultClient(t *testing.T) {
	srv := newForgeServer("pack", newDocumentsServer("pack"), newSecretsServer("pack"), &recordingForgeClient{}, nil)
	if srv.llm == nil {
		t.Fatal("a forge server built without WithLLMClient must fall back to DefaultLLMClient, got nil")
	}
	_, err := srv.llm.Complete(context.Background(), LLMProvider{Kind: "bogus", BaseURL: "https://x.example.test", Model: "m"}, LLMRequest{User: "u"})
	requireCode(t, err, codes.FailedPrecondition)
	if strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected a dispatch error for an unknown kind, got a nil-client style error: %v", err)
	}

	// An explicitly injected client is kept as given.
	fixture := &scriptedLLM{}
	srv = newForgeServer("pack", newDocumentsServer("pack"), newSecretsServer("pack"), &recordingForgeClient{}, fixture)
	if srv.llm != LLMClient(fixture) {
		t.Fatal("an injected LLM client must not be replaced by the default")
	}
}
