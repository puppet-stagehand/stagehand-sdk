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

	// An environment with no hiera.yaml and no data files returns zero
	// levels and zero paths with no error from both GetHieraHierarchy
	// (above) and ListHieraDataFiles (Task 2 of this plan).
	files, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListHieraDataFiles: %v", err)
	}
	if len(files.Paths) != 0 {
		t.Fatalf("expected zero paths, got %d", len(files.Paths))
	}
	if files.Page.GetNextCursor() != "" {
		t.Fatalf("expected empty NextCursor, got %q", files.Page.GetNextCursor())
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

// TestCode_HieraDataFileLifecycle carries the structure-preservation
// assertion end to end: seed a data file with a head comment, a trailing
// key comment, a nested mapping, and a sequence value; write one
// unrelated key; read the stored document back and assert everything
// seeded, plus the new key, is intact. It also proves PutHieraDataKey
// refuses the reserved key "lookup_options" (HIERA-03's write-side half
// on the data-key path), stores nothing, ListHieraDataFiles pagination,
// and the "prod"/"production" isolation.
func TestCode_HieraDataFileLifecycle(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	seeded := `# head-of-document comment
ntp::servers:
  - 0.pool.ntp.org
  - 1.pool.ntp.org
apache::config:
  listen: 80
  docroot: /var/www
port: 8080 # trailing comment on port
`
	seedDoc(t, h, ctx, "code-hiera-data", "prod/common.yaml", map[string]any{"path": "common.yaml", "yaml": seeded})
	seedDoc(t, h, ctx, "code-hiera-data", "prod/nodes/web01.yaml", map[string]any{"path": "nodes/web01.yaml", "yaml": "role: web\n"})

	// PutHieraDataKey against an unrelated key.
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "prod", Path: "common.yaml", Key: "newkey",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": "newvalue"})},
	}); err != nil {
		t.Fatalf("PutHieraDataKey(newkey): %v", err)
	}

	stored := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
	for _, want := range []string{
		"# head-of-document comment",
		"0.pool.ntp.org",
		"1.pool.ntp.org",
		"listen: 80",
		"docroot: /var/www",
		"port: 8080 # trailing comment on port",
		"newkey: newvalue",
	} {
		if !strings.Contains(stored, want) {
			t.Fatalf("expected %q to survive, stored text:\n%s", want, stored)
		}
	}

	// GetHieraDataFile excludes lookup_options from values.
	df, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "prod", Path: "common.yaml"})
	if err != nil {
		t.Fatalf("GetHieraDataFile: %v", err)
	}
	if _, ok := df.Values["lookup_options"]; ok {
		t.Fatalf("expected lookup_options excluded from Values")
	}
	if _, ok := df.Values["newkey"]; !ok {
		t.Fatalf("expected newkey present in Values, got %+v", df.Values)
	}

	// PutHieraDataKey("lookup_options", ...) is refused and stores
	// nothing (HIERA-03's write-side half on the data-key path).
	before := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "prod", Path: "common.yaml", Key: "lookup_options",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"foo": map[string]any{"merge": "first"}})},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("PutHieraDataKey(lookup_options): got %v, want InvalidArgument", err)
	}
	after := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
	if before != after {
		t.Fatalf("expected document unchanged after refused lookup_options write:\nbefore: %q\nafter:  %q", before, after)
	}

	// ListHieraDataFiles returns both paths ascending, prefix stripped.
	files, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListHieraDataFiles: %v", err)
	}
	if len(files.Paths) != 2 || files.Paths[0] != "common.yaml" || files.Paths[1] != "nodes/web01.yaml" {
		t.Fatalf("expected [common.yaml, nodes/web01.yaml], got %+v", files.Paths)
	}

	// RemoveHieraDataKey removes the named key.
	if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "newkey"}); err != nil {
		t.Fatalf("RemoveHieraDataKey(newkey): %v", err)
	}
	afterRemove, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "prod", Path: "common.yaml"})
	if err != nil {
		t.Fatalf("GetHieraDataFile after remove: %v", err)
	}
	if _, ok := afterRemove.Values["newkey"]; ok {
		t.Fatalf("expected newkey removed, got %+v", afterRemove.Values)
	}

	// RemoveHieraDataKey with an unknown key.
	if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: "common.yaml", Key: "does-not-exist"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RemoveHieraDataKey(unknown key): got %v, want InvalidArgument", err)
	}

	// DeleteHieraDataFile removes the whole document.
	if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "prod", Path: "nodes/web01.yaml"}); err != nil {
		t.Fatalf("DeleteHieraDataFile: %v", err)
	}
	if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "prod", Path: "nodes/web01.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetHieraDataFile after delete: got %v, want NotFound", err)
	}
	if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "prod", Path: "nodes/web01.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("DeleteHieraDataFile(already deleted): got %v, want NotFound", err)
	}

	// ListHieraDataFiles for "prod" never returns a path belonging to
	// "production".
	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment(production): %v", err)
	}
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "production", Path: "common.yaml", Key: "k",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": "v"})},
	}); err != nil {
		t.Fatalf("PutHieraDataKey(production): %v", err)
	}
	prodFiles, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListHieraDataFiles(prod): %v", err)
	}
	if len(prodFiles.Paths) != 1 || prodFiles.Paths[0] != "common.yaml" {
		t.Fatalf("expected prod's listing to carry only its own common.yaml, got %+v", prodFiles.Paths)
	}

	// Pagination: 2 then 1 over three modules ... here, exercise a
	// non-numeric cursor's InvalidArgument.
	if _, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{
		Environment: "prod",
		Page:        &hostv1.Page{Cursor: "abc"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ListHieraDataFiles(bad cursor): got %v, want InvalidArgument", err)
	}

	// Add two more prod files so pagination has three entries total, and
	// page through them 2 then 1.
	for _, p := range []string{"a.yaml", "b.yaml"} {
		if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
			Environment: "prod", Path: p, Key: "k",
			Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": "v"})},
		}); err != nil {
			t.Fatalf("PutHieraDataKey(%s): %v", p, err)
		}
	}
	page1, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "prod", Page: &hostv1.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("ListHieraDataFiles page1: %v", err)
	}
	if len(page1.Paths) != 2 {
		t.Fatalf("page1: expected 2 paths, got %d", len(page1.Paths))
	}
	if page1.Page.GetNextCursor() == "" {
		t.Fatalf("page1: expected non-empty NextCursor")
	}
	page2, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{
		Environment: "prod",
		Page:        &hostv1.Page{Limit: 2, Cursor: page1.Page.GetNextCursor()},
	})
	if err != nil {
		t.Fatalf("ListHieraDataFiles page2: %v", err)
	}
	if len(page2.Paths) != 1 {
		t.Fatalf("page2: expected 1 path, got %d", len(page2.Paths))
	}
	if page2.Page.GetNextCursor() != "" {
		t.Fatalf("page2: expected empty NextCursor, got %q", page2.Page.GetNextCursor())
	}
}

func TestCode_HieraDataKeyIsIdempotent(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// PutHieraDataKey against a path with no document yet creates it
	// with one key.
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "prod", Path: "common.yaml", Key: "port",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": float64(8080)})},
	}); err != nil {
		t.Fatalf("PutHieraDataKey(port, create): %v", err)
	}
	first := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")

	// Writing the identical key and value twice leaves the stored YAML
	// byte-identical.
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "prod", Path: "common.yaml", Key: "port",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": float64(8080)})},
	}); err != nil {
		t.Fatalf("PutHieraDataKey(port, repeat): %v", err)
	}
	second := hieraDataTextRaw(t, h, ctx, "prod", "common.yaml")
	if first != second {
		t.Fatalf("expected byte-identical text after idempotent put, got:\nfirst:  %q\nsecond: %q", first, second)
	}
}

// TestCode_HieraDataPathIsRefused drives all six refused paths through
// every path-taking RPC (Get, Put, Remove, Delete), before any storage
// key is constructed.
func TestCode_HieraDataPathIsRefused(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	badPaths := []string{
		"",
		"/etc/passwd",
		"../secrets.yaml",
		"nodes/../../etc/x.yaml",
		"nodes\\web01.yaml",
		"nodes/web01\n.yaml",
	}

	for _, p := range badPaths {
		if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "prod", Path: p}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("GetHieraDataFile(%q): got %v, want InvalidArgument", p, err)
		}
		if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
			Environment: "prod", Path: p, Key: "k",
			Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": "v"})},
		}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("PutHieraDataKey(%q): got %v, want InvalidArgument", p, err)
		}
		if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: p, Key: "k"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("RemoveHieraDataKey(%q): got %v, want InvalidArgument", p, err)
		}
		if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "prod", Path: p}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("DeleteHieraDataFile(%q): got %v, want InvalidArgument", p, err)
		}
	}
}

// TestCode_HieraUnknownEnvironmentAndPath drives every Hiera RPC against
// an unknown environment, then a subset of the data-file RPCs against a
// known environment with a path it does not carry.
func TestCode_HieraUnknownEnvironmentAndPath(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetHieraHierarchy(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{
		Environment: "ghost",
		Level:       &hostv1.HieraLevel{Name: "common"},
		Insert:      true,
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("PutHieraLevel(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "ghost", Name: "common"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemoveHieraLevel(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.ReorderHieraLevels(ctx, &hostv1.ReorderHieraLevelsRequest{Environment: "ghost", Names: []string{"common"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("ReorderHieraLevels(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("ListHieraDataFiles(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "ghost", Path: "common.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetHieraDataFile(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{
		Environment: "ghost", Path: "common.yaml", Key: "k",
		Value: &hostv1.Json{Value: mustStruct(t, map[string]any{"v": "v"})},
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("PutHieraDataKey(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "ghost", Path: "common.yaml", Key: "k"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemoveHieraDataKey(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "ghost", Path: "common.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("DeleteHieraDataFile(ghost): got %v, want NotFound", err)
	}

	// A path the environment does not carry.
	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "prod", Path: "absent.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetHieraDataFile(prod, absent path): got %v, want NotFound", err)
	}
	if _, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "prod", Path: "absent.yaml", Key: "k"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemoveHieraDataKey(prod, absent path): got %v, want NotFound", err)
	}
	if _, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "prod", Path: "absent.yaml"}); status.Code(err) != codes.NotFound {
		t.Fatalf("DeleteHieraDataFile(prod, absent path): got %v, want NotFound", err)
	}
}
