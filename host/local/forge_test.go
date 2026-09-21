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
