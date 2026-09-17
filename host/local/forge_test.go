package local

import (
	"context"
	"testing"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type forgeFixture struct{}

func (forgeFixture) Search(context.Context, *hostv1.SearchRequest) (*hostv1.SearchResponse, error) {
	return &hostv1.SearchResponse{Results: []*hostv1.ForgeSearchResult{{
		Name: "puppetlabs/apache", Version: "12.0.0", Source: "puppet-forge",
		Endorsement: "Supported", QualityScore: 0.98, Deprecated: false,
	}}}, nil
}
func (forgeFixture) Resolve(context.Context, *hostv1.ResolveRequest) (*hostv1.ResolveResponse, error) {
	return &hostv1.ResolveResponse{AdvisoryOnly: true, AdvisoryMessage: "authoring advisory; does not emulate r10k deployment", Root: &hostv1.DependencyNode{Name: "puppetlabs/apache", Version: "12.0.0"}}, nil
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
