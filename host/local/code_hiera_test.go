package local_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// hieraHierarchyTextRaw reads the code-hiera-hierarchy document's "yaml"
// field directly through the always-available Documents facet, so tests
// can assert on the stored bytes without going through a Hiera RPC.
func hieraHierarchyTextRaw(t *testing.T, h *host.Host, ctx context.Context, env string) string {
	t.Helper()
	doc, ok := getDoc(t, h, ctx, "code-hiera-hierarchy", env)
	if !ok {
		return ""
	}
	text, _ := doc.Body.Value.AsMap()["yaml"].(string)
	return text
}

// hieraDataTextRaw reads the code-hiera-data document's "yaml" field
// directly through the always-available Documents facet.
func hieraDataTextRaw(t *testing.T, h *host.Host, ctx context.Context, env, path string) string {
	t.Helper()
	doc, ok := getDoc(t, h, ctx, "code-hiera-data", env+"/"+path)
	if !ok {
		return ""
	}
	text, _ := doc.Body.Value.AsMap()["yaml"].(string)
	return text
}

func TestCode_HieraEmptyEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	hierarchy, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("GetHieraHierarchy: %v", err)
	}
	if len(hierarchy.Levels) != 0 {
		t.Fatalf("expected zero levels, got %d", len(hierarchy.Levels))
	}

	if _, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetHieraHierarchy(ghost): got %v, want NotFound", err)
	}
}

func TestCode_HieraLevelLifecycle(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// First PutHieraLevel against an unauthored environment creates the
	// document from the minimal v5 skeleton.
	resp, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "common", Path: "common.yaml"},
		Insert:      true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(common, insert): %v", err)
	}
	if resp.Hierarchy.Version != 5 {
		t.Fatalf("expected Version 5, got %d", resp.Hierarchy.Version)
	}
	if len(resp.Hierarchy.Levels) != 1 || resp.Hierarchy.Levels[0].Name != "common" {
		t.Fatalf("expected one level named common, got %+v", resp.Hierarchy.Levels)
	}

	// Insert a second level after common.
	resp, err = h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "role", Path: "roles/%{facts.role}.yaml"},
		Index:       0,
		Insert:      true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(role, insert at 0): %v", err)
	}
	if len(resp.Hierarchy.Levels) != 2 || resp.Hierarchy.Levels[0].Name != "role" || resp.Hierarchy.Levels[1].Name != "common" {
		t.Fatalf("expected [role, common], got %+v", resp.Hierarchy.Levels)
	}

	// Insert a third level at index 1, between role and common.
	resp, err = h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "os", Path: "os/%{facts.os.name}.yaml"},
		Index:       1,
		Insert:      true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(os, insert at 1): %v", err)
	}
	if len(resp.Hierarchy.Levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(resp.Hierarchy.Levels))
	}
	wantOrder := []string{"role", "os", "common"}
	for i, name := range wantOrder {
		if resp.Hierarchy.Levels[i].Name != name {
			t.Fatalf("level %d: got %q, want %q (full: %+v)", i, resp.Hierarchy.Levels[i].Name, name, resp.Hierarchy.Levels)
		}
	}

	// Replace common in place (insert=false).
	resp, err = h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "common", Path: "common.yaml", DataHash: "yaml_data"},
		Insert:      false,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(common, replace): %v", err)
	}
	if len(resp.Hierarchy.Levels) != 3 || resp.Hierarchy.Levels[2].DataHash != "yaml_data" {
		t.Fatalf("expected common replaced in place at index 2, got %+v", resp.Hierarchy.Levels)
	}

	// Replace an unknown name.
	if _, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "does-not-exist"},
		Insert:      false,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("PutHieraLevel(unknown, replace): got %v, want InvalidArgument", err)
	}

	// Remove os, closing the gap.
	hierarchy, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "os"})
	if err != nil {
		t.Fatalf("RemoveHieraLevel(os): %v", err)
	}
	if len(hierarchy.Levels) != 2 || hierarchy.Levels[0].Name != "role" || hierarchy.Levels[1].Name != "common" {
		t.Fatalf("expected [role, common] after remove, got %+v", hierarchy.Levels)
	}

	// Remove an unknown name.
	if _, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "does-not-exist"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RemoveHieraLevel(unknown): got %v, want InvalidArgument", err)
	}

	// Reorder.
	hierarchy, err = h.Code.ReorderHieraLevels(ctx, &hostv1.ReorderHieraLevelsRequest{Environment: "prod", Names: []string{"common", "role"}})
	if err != nil {
		t.Fatalf("ReorderHieraLevels: %v", err)
	}
	if len(hierarchy.Levels) != 2 || hierarchy.Levels[0].Name != "common" || hierarchy.Levels[1].Name != "role" {
		t.Fatalf("expected [common, role] after reorder, got %+v", hierarchy.Levels)
	}

	// A non-permutation reorder is refused and leaves the document
	// byte-identical.
	before := hieraHierarchyTextRaw(t, h, ctx, "prod")
	if _, err := h.Code.ReorderHieraLevels(ctx, &hostv1.ReorderHieraLevelsRequest{Environment: "prod", Names: []string{"common", "role", "extra"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ReorderHieraLevels(non-permutation): got %v, want InvalidArgument", err)
	}
	after := hieraHierarchyTextRaw(t, h, ctx, "prod")
	if before != after {
		t.Fatalf("expected document unchanged after refused reorder:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestCode_HieraCommentsSurviveFacetRoundTrip is the load-bearing test for
// HIERA-02's comment-preservation guarantee, asserted end to end against
// the stored document rather than only through the format package's own
// tests.
func TestCode_HieraCommentsSurviveFacetRoundTrip(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// Seed a hiera.yaml carrying a document head comment, a comment above
	// a level, and a trailing comment on a key, through the Documents
	// facet as clearly-labelled test scaffolding.
	seeded := `# head-of-document comment
version: 5
hierarchy:
  # comment above the common level
  - name: common
    path: common.yaml
  - name: common_extra
    path: common_extra.yaml # trailing comment on common_extra's path
`
	seedDoc(t, h, ctx, "code-hiera-hierarchy", "prod", map[string]any{"yaml": seeded})

	assertComments := func(t *testing.T, step string) {
		t.Helper()
		text := hieraHierarchyTextRaw(t, h, ctx, "prod")
		for _, want := range []string{
			"# head-of-document comment",
			"# comment above the common level",
			"# trailing comment on common_extra's path",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s: expected comment %q to survive, stored text:\n%s", step, want, text)
			}
		}
	}
	assertComments(t, "after seed")

	// Insert.
	if _, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "role", Path: "roles/%{facts.role}.yaml"},
		Index:       0,
		Insert:      true,
	}); err != nil {
		t.Fatalf("PutHieraLevel(insert): %v", err)
	}
	assertComments(t, "after insert")

	// Replace.
	if _, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "role", Path: "roles/%{facts.role}.yaml", DataHash: "yaml_data"},
		Insert:      false,
	}); err != nil {
		t.Fatalf("PutHieraLevel(replace): %v", err)
	}
	assertComments(t, "after replace")

	// Remove.
	if _, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "role"}); err != nil {
		t.Fatalf("RemoveHieraLevel: %v", err)
	}
	assertComments(t, "after remove")

	// Reorder.
	if _, err := h.Code.ReorderHieraLevels(ctx, &hostv1.ReorderHieraLevelsRequest{Environment: "prod", Names: []string{"common_extra", "common"}}); err != nil {
		t.Fatalf("ReorderHieraLevels: %v", err)
	}
	assertComments(t, "after reorder")
}

func TestCode_HieraLevelAdjacentNames(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	seedDoc(t, h, ctx, "code-hiera-hierarchy", "prod", map[string]any{"yaml": "version: 5\nhierarchy:\n  - name: common\n    path: common.yaml\n  - name: common_extra\n    path: common_extra.yaml\n"})

	// Replacing "common" leaves "common_extra" untouched.
	hierarchy, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "common", DataHash: "yaml_data"},
		Insert:      false,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(common, replace): %v", err)
	}
	if len(hierarchy.Hierarchy.Levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(hierarchy.Hierarchy.Levels))
	}
	var extra *hostv1.HieraLevel
	for _, lvl := range hierarchy.Hierarchy.Levels {
		if lvl.Name == "common_extra" {
			extra = lvl
		}
	}
	if extra == nil || extra.Path != "common_extra.yaml" || extra.DataHash != "" {
		t.Fatalf("expected common_extra untouched, got %+v", extra)
	}

	// Removing "common" leaves "common_extra" in place.
	after, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "prod", Name: "common"})
	if err != nil {
		t.Fatalf("RemoveHieraLevel(common): %v", err)
	}
	if len(after.Levels) != 1 || after.Levels[0].Name != "common_extra" {
		t.Fatalf("expected only common_extra to remain, got %+v", after.Levels)
	}
}

// TestCode_HieraEnvironmentLintIsAdvisoryOnly asserts three things
// together, because any one alone would pass against a blocking
// implementation: the call returns a nil error, the stored document
// contains the offending level, and the response's warnings list is
// non-empty.
func TestCode_HieraEnvironmentLintIsAdvisoryOnly(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	resp, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "per_env", Path: "environments/%{environment}.yaml"},
		Insert:      true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(environment-interpolated path): expected nil error, got %v", err)
	}
	if len(resp.Hierarchy.Levels) != 1 || resp.Hierarchy.Levels[0].Path != "environments/%{environment}.yaml" {
		t.Fatalf("expected the level to be written, got %+v", resp.Hierarchy.Levels)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected a non-empty warnings list")
	}
	found := false
	for _, w := range resp.Warnings {
		if w.Field == "path" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming field \"path\", got %+v", resp.Warnings)
	}

	text := hieraHierarchyTextRaw(t, h, ctx, "prod")
	if !strings.Contains(text, "%{environment}") {
		t.Fatalf("expected the stored document to contain the offending level, got:\n%s", text)
	}

	// A paths list with three entries, two of which interpolate the
	// environment, produces exactly two warnings naming the offending
	// indices.
	resp2, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level: &hostv1.HieraLevel{
			Name:  "multi",
			Paths: []string{"safe.yaml", "environments/%{environment}/a.yaml", "environments/%{::environment}/b.yaml"},
		},
		Insert: true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(multi paths): %v", err)
	}
	if len(resp2.Warnings) != 2 {
		t.Fatalf("expected exactly 2 warnings, got %d: %+v", len(resp2.Warnings), resp2.Warnings)
	}
	wantFields := map[string]bool{"paths[1]": true, "paths[2]": true}
	for _, w := range resp2.Warnings {
		if !wantFields[w.Field] {
			t.Fatalf("unexpected warning field %q", w.Field)
		}
	}

	// A level with only datadir/data_hash produces zero warnings.
	resp3, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "plain", Datadir: "data", DataHash: "yaml_data"},
		Insert:      true,
	})
	if err != nil {
		t.Fatalf("PutHieraLevel(plain): %v", err)
	}
	if len(resp3.Warnings) != 0 {
		t.Fatalf("expected zero warnings for a level with no path-shaped field, got %+v", resp3.Warnings)
	}

	// A level with a non-empty lookup_options is refused and stores
	// nothing.
	before := hieraHierarchyTextRaw(t, h, ctx, "prod")
	if _, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "prod",
		Level:       &hostv1.HieraLevel{Name: "denied", Path: "denied.yaml", LookupOptions: map[string]string{"foo": "first"}},
		Insert:      true,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("PutHieraLevel(non-empty lookup_options): got %v, want InvalidArgument", err)
	}
	after := hieraHierarchyTextRaw(t, h, ctx, "prod")
	if before != after {
		t.Fatalf("expected document unchanged after refused lookup_options write:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestCode_HieraLookupOptionsAreReadOnly proves HIERA-03's read half: a
// level whose path literally names a data file carrying lookup_options
// has that map mirrored onto it, while a level whose path is interpolated
// gets an empty map — no fact substitution or merge evaluation. The
// write-side refusal on the data-key path (PutHieraDataKey("lookup_options"))
// is asserted separately in TestCode_HieraDataFileLifecycle (Task 2 of
// this plan), since PutHieraDataKey is a Task 2 RPC.
func TestCode_HieraLookupOptionsAreReadOnly(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	seedDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml", map[string]any{
		"path": "common.yaml",
		"yaml": "lookup_options:\n  foo:\n    merge: unique\nfoo: bar\n",
	})
	seedDoc(t, h, ctx, "code-hiera-hierarchy", "prod", map[string]any{
		"yaml": "version: 5\nhierarchy:\n  - name: common\n    path: common.yaml\n  - name: nodes\n    path: \"nodes/%{trusted.certname}.yaml\"\n",
	})

	hierarchy, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("GetHieraHierarchy: %v", err)
	}
	if len(hierarchy.Levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(hierarchy.Levels))
	}
	var common, nodes *hostv1.HieraLevel
	for _, lvl := range hierarchy.Levels {
		switch lvl.Name {
		case "common":
			common = lvl
		case "nodes":
			nodes = lvl
		}
	}
	if common == nil || len(common.LookupOptions) != 1 || common.LookupOptions["foo"] != "unique" {
		t.Fatalf("expected common's lookup_options to mirror the data file, got %+v", common)
	}
	if nodes == nil || len(nodes.LookupOptions) != 0 {
		t.Fatalf("expected nodes' lookup_options to be empty (interpolated path), got %+v", nodes)
	}
}
