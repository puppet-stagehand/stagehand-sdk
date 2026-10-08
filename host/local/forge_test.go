package local

import (
	"context"
	"encoding/json"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// forgeFixture is a deterministic ForgeClient double used by every
// host.Local-level Forge test in this package — the real transport is
// exercised only in forge_client_test.go's httptest-backed tests.
type forgeFixture struct{}

func (forgeFixture) Search(_ context.Context, ep ForgeEndpoint, source, query string, _ *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	return []*hostv1.ForgeSearchResult{{
		Name: "puppetlabs/apache", Version: "12.0.0", Source: source,
		Endorsement: "Supported", QualityScore: 0.98, Deprecated: false,
	}}, &hostv1.PageInfo{}, nil
}

func (forgeFixture) ListReleases(context.Context, ForgeEndpoint, string) ([]string, error) {
	return []string{"12.0.0"}, nil
}

func (forgeFixture) GetRelease(_ context.Context, _ ForgeEndpoint, name, version string) (*ForgeRelease, error) {
	return &ForgeRelease{Name: name, Version: version}, nil
}

func TestForgeTracer(t *testing.T) {
	h := New([]string{"forge:rw"}, "fixture", WithForgeClient(forgeFixture{}))
	got, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{Query: "apache"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].Source != "puppet-forge" || got.Results[0].Endorsement != "Supported" {
		t.Fatalf("unexpected search result: %+v", got.Results)
	}
	res, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{Name: "puppetlabs/apache", Version: "12.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.AdvisoryOnly || res.Root == nil {
		t.Fatalf("unexpected resolve response: %+v", res)
	}
}

func TestForgePermission(t *testing.T) {
	h := New(nil, "fixture", WithForgeClient(forgeFixture{}))
	for _, call := range []func() error{
		func() error {
			_, e := h.Forge.Search(context.Background(), &hostv1.SearchRequest{Query: "apache"})
			return e
		},
		func() error {
			_, e := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{Name: "puppetlabs/apache", Version: "12.0.0"})
			return e
		},
	} {
		err := call()
		if status.Code(err) != codes.PermissionDenied || !hasFacetDetail(err) {
			t.Fatalf("expected facet permission denial, got %v", err)
		}
	}
}

func hasFacetDetail(err error) bool {
	for _, d := range status.Convert(err).Details() {
		if detail, ok := d.(*hostv1.ErrorDetail); ok && detail.Code == "facet_not_declared" {
			return true
		}
	}
	return false
}

// ------------------------------------------------------- multi-source config

// recordingForgeClient is a ForgeClient double that records the endpoint
// and source name of every Search call it receives, so tests can prove
// exactly which (and how many) source configs were actually revealed and
// used — not just that the returned result was tagged correctly.
type recordingForgeClient struct {
	calls []recordedForgeCall
}

type recordedForgeCall struct {
	ep     ForgeEndpoint
	source string
}

func (c *recordingForgeClient) Search(_ context.Context, ep ForgeEndpoint, source, query string, _ *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	c.calls = append(c.calls, recordedForgeCall{ep: ep, source: source})
	return []*hostv1.ForgeSearchResult{{Name: "acme/widget", Version: "1.0.0", Source: source}}, &hostv1.PageInfo{}, nil
}

func (c *recordingForgeClient) ListReleases(context.Context, ForgeEndpoint, string) ([]string, error) {
	return nil, nil
}

func (c *recordingForgeClient) GetRelease(context.Context, ForgeEndpoint, string, string) (*ForgeRelease, error) {
	return nil, nil
}

// mustConfigureForgeSource performs the same two-call sequence a pack
// itself would: seal {base_url, auth} as one Secrets value (D-06), then
// write a name/ref-only index Document (D-07). It is deliberately NOT a
// Forge RPC — Forge declares no configuration RPC of its own, per
// forge.go's package doc.
func mustConfigureForgeSource(t *testing.T, h *host.Host, name, label, baseURL, auth string) {
	t.Helper()
	secretJSON, err := json.Marshal(forgeSourceSecret{BaseURL: baseURL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{
		Name: "forge-source-" + name, Plaintext: secretJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := structpb.NewStruct(map[string]any{"name": name, "label": label, "secret_ref": ref.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: forgeSourceCollection, DocId: name,
		Body: &hostv1.Json{Value: body}, IfVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestForgeSearchPublicAndPrivateSourcesAreSeparate(t *testing.T) {
	client := &recordingForgeClient{}
	h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(client))
	mustConfigureForgeSource(t, h, "internal-a", "Internal A", "https://internal-a.example.test", "Bearer token-a")

	pub, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{Query: "widget"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Results) != 1 || pub.Results[0].Source != "puppet-forge" {
		t.Fatalf("expected a public-tagged result, got %+v", pub.Results)
	}

	priv, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{
		Query: "widget", Source: &hostv1.ForgeSourceSelection{Name: "internal-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(priv.Results) != 1 || priv.Results[0].Source != "internal-a" {
		t.Fatalf("expected an internal-a-tagged result, got %+v", priv.Results)
	}

	if len(client.calls) != 2 {
		t.Fatalf("expected exactly 2 client calls, got %d", len(client.calls))
	}
	if client.calls[0].ep.BaseURL != DefaultForgeBaseURL || client.calls[0].ep.Auth != "" {
		t.Fatalf("expected the public call to use the default endpoint with no auth, got %+v", client.calls[0].ep)
	}
	if client.calls[1].ep.BaseURL != "https://internal-a.example.test" || client.calls[1].ep.Auth != "Bearer token-a" {
		t.Fatalf("expected the private call to use internal-a's sealed endpoint/auth, got %+v", client.calls[1].ep)
	}
}

func TestForgeSearchSourcesRevealOnlySelectedSecret(t *testing.T) {
	client := &recordingForgeClient{}
	h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(client))
	mustConfigureForgeSource(t, h, "internal-a", "Internal A", "https://internal-a.example.test", "Bearer token-a")
	mustConfigureForgeSource(t, h, "internal-b", "Internal B", "https://internal-b.example.test", "Bearer token-b")

	if _, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{
		Query: "widget", Source: &hostv1.ForgeSourceSelection{Name: "internal-a"},
	}); err != nil {
		t.Fatal(err)
	}

	if len(client.calls) != 1 {
		t.Fatalf("expected exactly 1 client call (only the selected source), got %d", len(client.calls))
	}
	got := client.calls[0]
	if got.source != "internal-a" || got.ep.BaseURL != "https://internal-a.example.test" || got.ep.Auth != "Bearer token-a" {
		t.Fatalf("expected only internal-a's config to be revealed and used, got %+v", got)
	}
	if got.ep.BaseURL == "https://internal-b.example.test" || got.ep.Auth == "Bearer token-b" {
		t.Fatalf("internal-b's config leaked into an internal-a search: %+v", got)
	}
}

func TestForgeSourcesIndexDocumentExposesOnlyNameLabelRef(t *testing.T) {
	h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(&recordingForgeClient{}))
	mustConfigureForgeSource(t, h, "internal-a", "Internal A", "https://internal-a.example.test", "Bearer token-a")

	doc, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{
		Collection: forgeSourceCollection, DocId: "internal-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := doc.Body.Value.AsMap()
	if len(m) != 3 {
		t.Fatalf("expected exactly 3 keys (name, label, secret_ref), got %v", m)
	}
	for _, forbidden := range []string{"base_url", "auth", "baseurl", "token"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("forge-sources index document leaked a sealed field %q: %v", forbidden, m)
		}
	}
	if m["name"] != "internal-a" || m["label"] != "Internal A" {
		t.Fatalf("unexpected index document contents: %v", m)
	}
	if ref, ok := m["secret_ref"].(string); !ok || ref == "" {
		t.Fatalf("expected a non-empty secret_ref, got %v", m["secret_ref"])
	}
}

func TestForgeSearchUnconfiguredSourceIsNotFound(t *testing.T) {
	h := New([]string{"forge:rw"}, "pkg", WithForgeClient(&recordingForgeClient{}))
	_, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{
		Query: "widget", Source: &hostv1.ForgeSourceSelection{Name: "ghost"},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for an unconfigured source, got %v", err)
	}
}

func TestForgeSearchRejectsInvalidSealedBaseURL(t *testing.T) {
	h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(&recordingForgeClient{}))
	// A source whose sealed config carries a non-https base URL — proves
	// T-07-05's validation applies to configured sources, not just the
	// public default.
	mustConfigureForgeSource(t, h, "insecure", "Insecure", "http://internal.example.test", "token")

	_, err := h.Forge.Search(context.Background(), &hostv1.SearchRequest{
		Query: "widget", Source: &hostv1.ForgeSourceSelection{Name: "insecure"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for an invalid sealed base URL, got %v", err)
	}
}

func TestForgeSearchCorruptSecretRefIsInternal(t *testing.T) {
	h := New([]string{"forge:rw", "secrets:rw"}, "pkg", WithForgeClient(&recordingForgeClient{}))
	// Write the index document directly, pointing at a secret_ref that was
	// never actually Stored — simulates the index and secret store
	// disagreeing.
	body, err := structpb.NewStruct(map[string]any{"name": "broken", "label": "Broken", "secret_ref": "does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: forgeSourceCollection, DocId: "broken",
		Body: &hostv1.Json{Value: body}, IfVersion: 0,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = h.Forge.Search(context.Background(), &hostv1.SearchRequest{
		Query: "widget", Source: &hostv1.ForgeSourceSelection{Name: "broken"},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal for an unresolvable secret_ref, got %v", err)
	}
}

// --------------------------------------- Resolve: Puppetfile integration

// mustCreateEnvironmentWithPuppetfile creates env via the real Code facet
// and writes each given module into its Puppetfile via the real
// PutPuppetfileModule RPC — Task 2's tests integrate against Code's actual
// RPCs, not a Documents shortcut, so a passing test proves the real
// cross-facet seam (D-15) works, not just this package's own conventions.
func mustCreateEnvironmentWithPuppetfile(t *testing.T, h *host.Host, env string, modules ...string) {
	t.Helper()
	if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: env}); err != nil {
		t.Fatal(err)
	}
	for _, name := range modules {
		if _, err := h.Code.PutPuppetfileModule(context.Background(), &hostv1.PutPuppetfileModuleRequest{
			Environment: env,
			Module: &hostv1.PuppetfileModule{
				Name:   name,
				Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "1.0.0"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// mustCreateEnvironmentWithPuppetfileText creates env through the real Code
// facet and then writes the literal Puppetfile text into the code-puppetfiles
// collection through Documents, bypassing PutPuppetfileModule. It exists
// because mustCreateEnvironmentWithPuppetfile goes through
// PutPuppetfileModule, which Plan 04 makes rewrite the stored module name to
// its canonical form; any test whose point is the stored spelling must bypass
// the write path exactly as an import does.
func mustCreateEnvironmentWithPuppetfileText(t *testing.T, h *host.Host, env, text string) {
	t.Helper()
	if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: env}); err != nil {
		t.Fatal(err)
	}
	if err := SeedDocument(h, puppetfileCollection, env, map[string]any{"text": text}); err != nil {
		t.Fatalf("seeding puppetfile text for %q: %v", env, err)
	}
}

func TestForgeResolvePuppetfileStatusMarksExistingAndNewModules(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0",
		ForgeDependency{Name: "puppetlabs/stdlib", VersionRequirement: ">= 1.0.0"},
		ForgeDependency{Name: "acme/new", VersionRequirement: ">= 1.0.0"},
	)
	c.versions["puppetlabs/stdlib"] = []string{"9.0.0"}
	c.addRelease("puppetlabs/stdlib", "9.0.0")
	c.versions["acme/new"] = []string{"1.0.0"}
	c.addRelease("acme/new", "1.0.0")

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	mustCreateEnvironmentWithPuppetfile(t, h, "production", "puppetlabs/stdlib")

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Root.AlreadyInPuppetfile {
		t.Fatalf("root itself was never added to the Puppetfile, expected AlreadyInPuppetfile=false, got true")
	}
	stdlib := findChild(resp.Root, "puppetlabs/stdlib")
	if stdlib == nil || !stdlib.AlreadyInPuppetfile {
		t.Fatalf("expected puppetlabs/stdlib to be marked already_in_puppetfile, got %+v", stdlib)
	}
	fresh := findChild(resp.Root, "acme/new")
	if fresh == nil || fresh.AlreadyInPuppetfile {
		t.Fatalf("expected acme/new to be marked net-new (already_in_puppetfile=false), got %+v", fresh)
	}
}

func TestForgeResolvePuppetfileStatusRecognizesHyphenSlugForm(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "puppetlabs/stdlib", VersionRequirement: ">= 1.0.0"})
	c.versions["puppetlabs/stdlib"] = []string{"9.0.0"}
	c.addRelease("puppetlabs/stdlib", "9.0.0")

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	// Written with the ns-name hyphen slug form, matching how a human or
	// an imported Puppetfile might spell it (code/puppetfile.go's
	// forgeSlugOK accepts both) — normalizeModuleName must still match it
	// against this facet's ns/name result.
	mustCreateEnvironmentWithPuppetfile(t, h, "production", "puppetlabs-stdlib")

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	stdlib := findChild(resp.Root, "puppetlabs/stdlib")
	if stdlib == nil || !stdlib.AlreadyInPuppetfile {
		t.Fatalf("expected the hyphen-slug Puppetfile entry to match the ns/name result, got %+v", stdlib)
	}
}

// NEW-1: the Puppetfile grammar allows an uppercase owner (reForgeSlug), so an
// entry written 'Puppetlabs-stdlib' is the same module as this facet's
// lowercase puppetlabs/stdlib result. The wire spelling of the result must not
// move while the comparison key folds case.
func TestForgeResolvePuppetfileStatusRecognizesUppercaseOwner(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "puppetlabs/stdlib", VersionRequirement: ">= 1.0.0"})
	c.versions["puppetlabs/stdlib"] = []string{"9.0.0"}
	c.addRelease("puppetlabs/stdlib", "9.0.0")

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	mustCreateEnvironmentWithPuppetfileText(t, h, "production", "mod 'Puppetlabs-stdlib', '9.0.0'\n")

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	stdlib := findChild(resp.Root, "puppetlabs/stdlib")
	if stdlib == nil {
		t.Fatal("expected puppetlabs/stdlib in the resolved tree")
	}
	if !stdlib.AlreadyInPuppetfile {
		t.Fatal("an uppercase-owner Puppetfile entry must be recognised as already present (NEW-1)")
	}
	if stdlib.Name != "puppetlabs/stdlib" {
		t.Fatalf("wire Name must stay the ns/name spelling, got %q", stdlib.Name)
	}
}

// IN-02 regression: a namespace-less Git module whose bare name contains a
// hyphen (mod 'my-module', :git => ...) must not be rewritten to my/module,
// which would falsely mark a Forge my/module as already_in_puppetfile.
func TestForgeResolvePuppetfileStatusIgnoresHyphenatedGitModuleName(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "my/module", VersionRequirement: ">= 1.0.0"})
	c.versions["my/module"] = []string{"1.0.0"}
	c.addRelease("my/module", "1.0.0")

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Code.PutPuppetfileModule(context.Background(), &hostv1.PutPuppetfileModuleRequest{
		Environment: "production",
		Module: &hostv1.PuppetfileModule{
			Name: "my-module",
			Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
				Url:     "https://example.com/my-module.git",
				RefKind: &hostv1.GitSource_Tag{Tag: "v1.0.0"},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	dep := findChild(resp.Root, "my/module")
	if dep == nil {
		t.Fatal("expected my/module in the resolved tree")
	}
	if dep.AlreadyInPuppetfile {
		t.Fatal("Git module my-module must not mark Forge my/module as already_in_puppetfile")
	}
}

// FORGE-01 empty: a Puppetfile that is absent, empty or unparseable describes
// no modules, so every node is net-new and the annotation pass never fails the
// Resolve call.
func TestForgeResolvePuppetfileStatusEmptyAbsentOrUnparseable(t *testing.T) {
	cases := []struct {
		name string
		// text is the literal Puppetfile body; nil means no document at all.
		text *string
	}{
		{"absent_document", nil},
		{"empty_text", strPtrForge("")},
		{"unparseable_text", strPtrForge("mod 'puppetlabs-stdlib', {{{ not a puppetfile")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newResolverFixtureClient()
			c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "puppetlabs/stdlib", VersionRequirement: ">= 1.0.0"})
			c.versions["puppetlabs/stdlib"] = []string{"9.0.0"}
			c.addRelease("puppetlabs/stdlib", "9.0.0")

			h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
			if tc.text == nil {
				if _, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
					t.Fatal(err)
				}
			} else {
				mustCreateEnvironmentWithPuppetfileText(t, h, "production", *tc.text)
			}

			resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
				Name: "acme/root", Version: "1.0.0", Environment: "production",
			})
			if err != nil {
				t.Fatalf("Resolve must not fail on a %s Puppetfile: %v", tc.name, err)
			}
			if resp.Root.AlreadyInPuppetfile {
				t.Fatal("root must be net-new")
			}
			stdlib := findChild(resp.Root, "puppetlabs/stdlib")
			if stdlib == nil || stdlib.AlreadyInPuppetfile {
				t.Fatalf("expected puppetlabs/stdlib to be net-new, got %+v", stdlib)
			}
		})
	}
}

func strPtrForge(s string) *string { return &s }

// The positive half of the IN-02 Git carve-out: the same spelling written as a
// Forge entry IS the Forge module. Together with the Git-name test above this
// proves the discriminator is the entry's source and not its spelling — a
// source-blind key that always answered false would pass the Git half alone.
func TestForgeResolvePuppetfileStatusRecognizesHyphenatedForgeEntry(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "my/module", VersionRequirement: ">= 1.0.0"})
	c.versions["my/module"] = []string{"1.0.0"}
	c.addRelease("my/module", "1.0.0")

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	mustCreateEnvironmentWithPuppetfileText(t, h, "production", "mod 'my-module', '1.0.0'\n")

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	dep := findChild(resp.Root, "my/module")
	if dep == nil || !dep.AlreadyInPuppetfile {
		t.Fatalf("a Forge entry my-module must mark Forge my/module as already present, got %+v", dep)
	}
}

// normalizeModuleName is the display/wire form: it returns the ns/name
// spelling and preserves case. code.CanonicalModuleName is the identity key
// (hyphen form, lowercase). The two are deliberately different; a change that
// unifies them fails here rather than in a distant example test.
func TestNormalizeModuleNameIsTheSlashWireForm(t *testing.T) {
	for in, want := range map[string]string{
		"puppetlabs-stdlib": "puppetlabs/stdlib",
		"puppetlabs/stdlib": "puppetlabs/stdlib",
		"PuppetLabs/stdlib": "PuppetLabs/stdlib",
		"PuppetLabs-stdlib": "PuppetLabs/stdlib",
	} {
		if got := normalizeModuleName(in); got != want {
			t.Fatalf("normalizeModuleName(%q) = %q, want the wire form %q", in, got, want)
		}
	}
}

func TestForgeResolveWithoutEnvironmentLeavesStatusFalse(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "acme/child", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/child"] = []string{"1.0.0"}
	c.addRelease("acme/child", "1.0.0")

	h := New([]string{"forge:rw"}, "pkg", WithForgeClient(c))
	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{Name: "acme/root", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Root.AlreadyInPuppetfile {
		t.Fatal("expected AlreadyInPuppetfile=false when no environment is given")
	}
	if child := findChild(resp.Root, "acme/child"); child == nil || child.AlreadyInPuppetfile {
		t.Fatalf("expected no already_in_puppetfile status without an environment, got %+v", child)
	}
}

// AR-07-01: a malformed Environment is rejected up front, not treated as an
// unknown environment that silently yields already_in_puppetfile=false.
func TestForgeResolveRejectsMalformedEnvironment(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0")
	h := New([]string{"forge:rw"}, "pkg", WithForgeClient(c))

	for _, env := range []string{"Production", "prod-uction", "../etc", "has space", "prod\nuction"} {
		_, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
			Name: "acme/root", Version: "1.0.0", Environment: env,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("environment %q: expected InvalidArgument, got %v", env, err)
		}
	}

	// A well-formed name that has no stored Puppetfile is still advisory-only
	// success, and an empty environment stays optional.
	for _, env := range []string{"", "staging_2"} {
		if _, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
			Name: "acme/root", Version: "1.0.0", Environment: env,
		}); err != nil {
			t.Fatalf("environment %q: unexpected error %v", env, err)
		}
	}
}

func TestForgeResolveNeverMutatesThePuppetfile(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0",
		ForgeDependency{Name: "acme/a", VersionRequirement: ">= 1.0.0"},
		ForgeDependency{Name: "acme/cyclic", VersionRequirement: ">= 1.0.0"},
	)
	c.versions["acme/a"] = []string{"1.0.0"}
	c.addRelease("acme/a", "1.0.0", ForgeDependency{Name: "acme/cyclic", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/cyclic"] = []string{"1.0.0"}
	c.addRelease("acme/cyclic", "1.0.0", ForgeDependency{Name: "acme/root", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/root"] = []string{"1.0.0"}

	h := New([]string{"forge:rw", "code:rw"}, "pkg", WithForgeClient(c))
	mustCreateEnvironmentWithPuppetfile(t, h, "production", "acme/a")

	before, err := h.Code.RenderPuppetfile(context.Background(), &hostv1.RenderPuppetfileRequest{Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	modsBefore, err := h.Code.ListPuppetfileModules(context.Background(), &hostv1.ListPuppetfileModulesRequest{Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Environment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Root == nil || len(resp.Root.Dependencies) == 0 {
		t.Fatalf("expected a non-trivial resolved tree to exercise the read path, got %+v", resp.Root)
	}

	after, err := h.Code.RenderPuppetfile(context.Background(), &hostv1.RenderPuppetfileRequest{Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if before.Text != after.Text {
		t.Fatalf("Resolve must never mutate the Puppetfile: before=%q after=%q", before.Text, after.Text)
	}
	modsAfter, err := h.Code.ListPuppetfileModules(context.Background(), &hostv1.ListPuppetfileModulesRequest{Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	if len(modsBefore.Modules) != len(modsAfter.Modules) {
		t.Fatalf("Resolve must never change the Puppetfile module count: before=%d after=%d", len(modsBefore.Modules), len(modsAfter.Modules))
	}
}

func TestForgeResolveUnconfiguredSourceIsNotFound(t *testing.T) {
	h := New([]string{"forge:rw"}, "pkg", WithForgeClient(newResolverFixtureClient()))
	_, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{
		Name: "acme/root", Version: "1.0.0", Source: &hostv1.ForgeSourceSelection{Name: "ghost"},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for an unconfigured source, got %v", err)
	}
}

func TestForgeResolveTopLevelWarningsAggregateTree(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "acme/ghost", VersionRequirement: ">= 1.0.0"})
	// acme/ghost is intentionally unregistered -> ErrForgeNotFound -> one
	// "unresolved" warning, which must surface in the response-level
	// Warnings rollup as well as on the node itself.

	h := New([]string{"forge:rw"}, "pkg", WithForgeClient(c))
	resp, err := h.Forge.Resolve(context.Background(), &hostv1.ResolveRequest{Name: "acme/root", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0].Code != "unresolved" {
		t.Fatalf("expected exactly one aggregated unresolved warning, got %+v", resp.Warnings)
	}
	if !resp.AdvisoryOnly || resp.AdvisoryMessage == "" {
		t.Fatalf("expected an explicit advisory-only marker and message, got %+v", resp)
	}
}
