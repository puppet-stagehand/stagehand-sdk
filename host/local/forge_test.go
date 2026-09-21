package local

import (
	"context"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
