package local

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// resolverFixtureClient is a deterministic, in-memory ForgeClient used only
// by this file's pure-resolver tests. It never performs I/O and lets each
// test inject exact release graphs, version lists, and (optionally) errors
// for specific calls, while counting how many times each was invoked so
// tests can assert on the per-walk cache (D-04).
type resolverFixtureClient struct {
	releases     map[forgeCacheKey]*ForgeRelease
	releaseErr   map[forgeCacheKey]error
	versions     map[string][]string
	versionErr   map[string]error
	releaseCalls map[forgeCacheKey]int
	versionCalls map[string]int
}

func newResolverFixtureClient() *resolverFixtureClient {
	return &resolverFixtureClient{
		releases:     map[forgeCacheKey]*ForgeRelease{},
		releaseErr:   map[forgeCacheKey]error{},
		versions:     map[string][]string{},
		versionErr:   map[string]error{},
		releaseCalls: map[forgeCacheKey]int{},
		versionCalls: map[string]int{},
	}
}

func (f *resolverFixtureClient) addRelease(name, version string, deps ...ForgeDependency) {
	f.releases[forgeCacheKey{name, version}] = &ForgeRelease{Name: name, Version: version, Dependencies: deps}
}

func (f *resolverFixtureClient) Search(context.Context, ForgeEndpoint, string, string, *hostv1.Page) ([]*hostv1.ForgeSearchResult, *hostv1.PageInfo, error) {
	return nil, nil, status.Error(codes.Unimplemented, "resolverFixtureClient.Search is not used by resolver tests")
}

func (f *resolverFixtureClient) ListReleases(_ context.Context, _ ForgeEndpoint, name string) ([]string, error) {
	f.versionCalls[name]++
	if err, ok := f.versionErr[name]; ok {
		return nil, err
	}
	v, ok := f.versions[name]
	if !ok {
		return nil, ErrForgeNotFound
	}
	return v, nil
}

func (f *resolverFixtureClient) GetRelease(_ context.Context, _ ForgeEndpoint, name, version string) (*ForgeRelease, error) {
	key := forgeCacheKey{name, version}
	f.releaseCalls[key]++
	if err, ok := f.releaseErr[key]; ok {
		return nil, err
	}
	rel, ok := f.releases[key]
	if !ok {
		return nil, ErrForgeNotFound
	}
	return rel, nil
}

func findChild(node *hostv1.DependencyNode, name string) *hostv1.DependencyNode {
	for _, c := range node.Dependencies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestForgeResolverDeepAcyclicGraph(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "acme/b", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/b"] = []string{"1.0.0", "1.2.0"}
	c.addRelease("acme/b", "1.2.0", ForgeDependency{Name: "acme/c", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/c"] = []string{"1.0.0"}
	c.addRelease("acme/c", "1.0.0", ForgeDependency{Name: "acme/d", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/d"] = []string{"1.0.0"}
	c.addRelease("acme/d", "1.0.0")

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.Unresolved || root.Cycle || root.Conflict {
		t.Fatalf("unexpected flags on root: %+v", root)
	}

	b := findChild(root, "acme/b")
	if b == nil || b.Version != "1.2.0" {
		t.Fatalf("expected acme/b@1.2.0 (highest satisfying >= 1.0.0), got %+v", b)
	}
	cc := findChild(b, "acme/c")
	if cc == nil || cc.Version != "1.0.0" {
		t.Fatalf("expected acme/c@1.0.0 nested under b, got %+v", cc)
	}
	d := findChild(cc, "acme/d")
	if d == nil || len(d.Dependencies) != 0 {
		t.Fatalf("expected acme/d as a 4-deep leaf with no further deps, got %+v", d)
	}
}

func TestForgeResolverCycleIsFlaggedAndStopsRecursion(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/a", "1.0.0", ForgeDependency{Name: "acme/b", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/b"] = []string{"1.0.0"}
	c.addRelease("acme/b", "1.0.0", ForgeDependency{Name: "acme/a", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/a"] = []string{"1.0.0"}

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/a", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b := findChild(root, "acme/b")
	if b == nil {
		t.Fatal("expected acme/b to be present")
	}
	cyclicA := findChild(b, "acme/a")
	if cyclicA == nil {
		t.Fatal("expected the cyclic acme/a edge to be present in the tree")
	}
	if !cyclicA.Cycle {
		t.Fatalf("expected the cyclic node to be flagged Cycle, got %+v", cyclicA)
	}
	if len(cyclicA.Dependencies) != 0 {
		t.Fatalf("expected recursion to stop at the cycle, got children %+v", cyclicA.Dependencies)
	}
	if len(cyclicA.Warnings) != 1 || cyclicA.Warnings[0].Code != "cycle" {
		t.Fatalf("expected exactly one cycle warning, got %+v", cyclicA.Warnings)
	}
	// The walk must still complete (no error) — a cycle is advisory, never
	// a whole-RPC failure.
	if root.Cycle {
		t.Fatalf("root itself should not be flagged Cycle: %+v", root)
	}
}

func TestForgeResolverUnresolvedDependencySiblingContinues(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0",
		ForgeDependency{Name: "acme/found", VersionRequirement: ">= 1.0.0"},
		ForgeDependency{Name: "acme/ghost", VersionRequirement: ">= 1.0.0"},
	)
	c.versions["acme/found"] = []string{"1.0.0"}
	c.addRelease("acme/found", "1.0.0")
	// acme/ghost deliberately has no versions entry at all -> ErrForgeNotFound.

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := findChild(root, "acme/found")
	if found == nil || found.Unresolved {
		t.Fatalf("expected acme/found to resolve cleanly, got %+v", found)
	}
	ghost := findChild(root, "acme/ghost")
	if ghost == nil || !ghost.Unresolved {
		t.Fatalf("expected acme/ghost to be an unresolved advisory node, got %+v", ghost)
	}
	if len(ghost.Warnings) != 1 || ghost.Warnings[0].Code != "unresolved" {
		t.Fatalf("expected exactly one unresolved warning, got %+v", ghost.Warnings)
	}
}

func TestForgeResolverRootNotFoundIsUnresolvedNotError(t *testing.T) {
	c := newResolverFixtureClient() // no releases registered at all

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/ghost", "9.9.9")
	if err != nil {
		t.Fatalf("expected a typed not-found root to be advisory data, not an error: %v", err)
	}
	if !root.Unresolved {
		t.Fatalf("expected the root itself to be marked Unresolved, got %+v", root)
	}
}

func TestForgeResolverInvalidRequirementIsAdvisoryWarning(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "acme/broken", VersionRequirement: "not a valid range"})

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	broken := findChild(root, "acme/broken")
	if broken == nil || !broken.Unresolved {
		t.Fatalf("expected acme/broken to be unresolved due to an invalid requirement, got %+v", broken)
	}
	if len(broken.Warnings) != 1 || broken.Warnings[0].Code != "invalid_range" {
		t.Fatalf("expected exactly one invalid_range warning, got %+v", broken.Warnings)
	}
	// listReleases must never be reached for an unparseable requirement.
	if n := c.versionCalls["acme/broken"]; n != 0 {
		t.Fatalf("expected no ListReleases call for an invalid requirement, got %d calls", n)
	}
}

func TestForgeResolverDisjointConstraintsProduceConflictOnBothPaths(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0",
		ForgeDependency{Name: "acme/m1", VersionRequirement: ">= 1.0.0"},
		ForgeDependency{Name: "acme/m2", VersionRequirement: ">= 1.0.0"},
	)
	c.versions["acme/m1"] = []string{"1.0.0"}
	c.addRelease("acme/m1", "1.0.0", ForgeDependency{Name: "acme/shared", VersionRequirement: ">= 2.0.0"})
	c.versions["acme/m2"] = []string{"1.0.0"}
	c.addRelease("acme/m2", "1.0.0", ForgeDependency{Name: "acme/shared", VersionRequirement: "< 1.0.0"})
	c.versions["acme/shared"] = []string{"0.5.0", "0.9.0", "2.0.0", "2.5.0"}
	c.addRelease("acme/shared", "0.9.0")
	c.addRelease("acme/shared", "2.5.0")

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m1 := findChild(root, "acme/m1")
	m2 := findChild(root, "acme/m2")
	sharedUnderM1 := findChild(m1, "acme/shared")
	sharedUnderM2 := findChild(m2, "acme/shared")
	if sharedUnderM1 == nil || sharedUnderM2 == nil {
		t.Fatalf("expected acme/shared under both m1 and m2, got m1=%+v m2=%+v", sharedUnderM1, sharedUnderM2)
	}

	// Each path still resolves to its OWN best-fit version — a conflict
	// warning must never change the tree topology (07-RESEARCH.md:
	// "retains all dependency paths").
	if sharedUnderM1.Version != "2.5.0" {
		t.Fatalf("expected acme/shared under m1 to resolve to 2.5.0 (>= 2.0.0), got %q", sharedUnderM1.Version)
	}
	if sharedUnderM2.Version != "0.9.0" {
		t.Fatalf("expected acme/shared under m2 to resolve to 0.9.0 (< 1.0.0), got %q", sharedUnderM2.Version)
	}

	if !sharedUnderM1.Conflict || !sharedUnderM2.Conflict {
		t.Fatalf("expected both acme/shared nodes to be flagged Conflict, got m1=%v m2=%v", sharedUnderM1.Conflict, sharedUnderM2.Conflict)
	}
	for _, n := range []*hostv1.DependencyNode{sharedUnderM1, sharedUnderM2} {
		found := false
		for _, w := range n.Warnings {
			if w.Code == "conflict" {
				found = true
				if len(w.Origins) != 2 {
					t.Fatalf("expected 2 origins on the conflict warning, got %+v", w.Origins)
				}
			}
		}
		if !found {
			t.Fatalf("expected a conflict warning on node %+v", n)
		}
	}

	// acme/shared's version list must be fetched exactly once despite two
	// dependency edges naming it (D-04 cache).
	if n := c.versionCalls["acme/shared"]; n != 1 {
		t.Fatalf("expected exactly 1 ListReleases call for acme/shared, got %d", n)
	}
}

func TestForgeResolverSameRequirementFromMultiplePathsIsNotAConflict(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0",
		ForgeDependency{Name: "acme/m1", VersionRequirement: ">= 1.0.0"},
		ForgeDependency{Name: "acme/m2", VersionRequirement: ">= 1.0.0"},
	)
	c.versions["acme/m1"] = []string{"1.0.0"}
	c.addRelease("acme/m1", "1.0.0", ForgeDependency{Name: "acme/common", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/m2"] = []string{"1.0.0"}
	c.addRelease("acme/m2", "1.0.0", ForgeDependency{Name: "acme/common", VersionRequirement: ">= 1.0.0"})
	c.versions["acme/common"] = []string{"1.0.0", "1.5.0"}
	c.addRelease("acme/common", "1.5.0")

	root, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m1 := findChild(root, "acme/m1")
	m2 := findChild(root, "acme/m2")
	commonUnderM1 := findChild(m1, "acme/common")
	commonUnderM2 := findChild(m2, "acme/common")
	if commonUnderM1 == nil || commonUnderM2 == nil {
		t.Fatalf("expected acme/common under both m1 and m2")
	}
	if commonUnderM1.Conflict || commonUnderM2.Conflict {
		t.Fatalf("identical requirements from two paths must not be flagged as a conflict: m1=%+v m2=%+v", commonUnderM1, commonUnderM2)
	}
	if commonUnderM1.Version != "1.5.0" || commonUnderM2.Version != "1.5.0" {
		t.Fatalf("expected both paths to resolve to 1.5.0, got m1=%q m2=%q", commonUnderM1.Version, commonUnderM2.Version)
	}
	// The repeated (name,version) pair must be fetched exactly once (D-04).
	if n := c.releaseCalls[forgeCacheKey{"acme/common", "1.5.0"}]; n != 1 {
		t.Fatalf("expected exactly 1 GetRelease call for acme/common@1.5.0, got %d", n)
	}
}

func TestForgeResolverTransportFailurePropagatesAsUnavailable(t *testing.T) {
	c := newResolverFixtureClient()
	c.addRelease("acme/root", "1.0.0", ForgeDependency{Name: "acme/flaky", VersionRequirement: ">= 1.0.0"})
	c.versionErr["acme/flaky"] = status.Error(codes.Unavailable, "forge: request failed (network error, timeout, or canceled context)")

	_, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected a transport failure deep in the tree to abort the whole walk as Unavailable, got %v", err)
	}
}

func TestForgeResolverMalformedPayloadPropagatesAsInternal(t *testing.T) {
	c := newResolverFixtureClient()
	c.releaseErr[forgeCacheKey{"acme/root", "1.0.0"}] = status.Error(codes.Internal, "forge: malformed response body")

	_, err := resolveDependencyTree(context.Background(), c, ForgeEndpoint{}, "puppet-forge", "acme/root", "1.0.0")
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected a malformed root payload to abort the walk as Internal, got %v", err)
	}
}
