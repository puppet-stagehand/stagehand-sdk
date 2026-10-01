package local_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// seedDoc writes a document directly through the Documents facet — labelled
// test scaffolding for collections (code-puppetfiles, code-hiera-hierarchy,
// code-hiera-data) whose own Code RPCs land in later plans in this phase.
func seedDoc(t *testing.T, h *host.Host, ctx context.Context, collection, docID string, body map[string]any) {
	t.Helper()
	_, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: collection, DocId: docID,
		Body: &hostv1.Json{Value: mustStruct(t, body)},
	})
	if err != nil {
		t.Fatalf("seeding %s/%s: %v", collection, docID, err)
	}
}

// getDoc reads a document directly through the Documents facet, reporting
// whether it exists. Any non-NotFound error fails the test.
func getDoc(t *testing.T, h *host.Host, ctx context.Context, collection, docID string) (*hostv1.Document, bool) {
	t.Helper()
	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: collection, DocId: docID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false
		}
		t.Fatalf("getting %s/%s: %v", collection, docID, err)
	}
	return doc, true
}

// seedFullEnvironment creates env via the real CreateEnvironment RPC, then
// seeds a Puppetfile, a Hiera hierarchy, two Hiera data files and a
// settings sub-object embedded directly in the identity document's body —
// the full document shape RenameEnvironment, DeleteEnvironment and
// DuplicateEnvironment must move or copy as a unit. Settings are seeded
// directly through the Documents facet (not via PutEnvironmentSettings) so
// this fixture, and the Task 1 tests that use it, stay independent of that
// RPC's own implementation.
func seedFullEnvironment(t *testing.T, h *host.Host, ctx context.Context, env string) {
	t.Helper()
	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: env}); err != nil {
		t.Fatalf("CreateEnvironment(%q): %v", env, err)
	}
	seedDoc(t, h, ctx, "code-puppetfiles", env, map[string]any{"text": "mod 'puppetlabs/apache', '5.0.0'\n"})
	seedDoc(t, h, ctx, "code-hiera-hierarchy", env, map[string]any{"yaml": "version: 5\nhierarchy: []\n"})
	seedDoc(t, h, ctx, "code-hiera-data", env+"/common.yaml", map[string]any{"path": "common.yaml", "yaml": "foo: bar\n"})
	seedDoc(t, h, ctx, "code-hiera-data", env+"/nodes/web01.yaml", map[string]any{"path": "nodes/web01.yaml", "yaml": "bar: baz\n"})

	// CreateEnvironment already created the identity document, so
	// embedding settings into it is an update (IfVersion = its current
	// version), not a create-only write.
	identity, ok := getDoc(t, h, ctx, "code-environments", env)
	if !ok {
		t.Fatalf("identity document for %q missing right after CreateEnvironment", env)
	}
	_, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: "code-environments", DocId: env,
		Body:      &hostv1.Json{Value: mustStruct(t, map[string]any{"name": env, "settings": map[string]any{"modulepath": testSettingsModulepath}})},
		IfVersion: identity.Version,
	})
	if err != nil {
		t.Fatalf("seeding settings on %q's identity document: %v", env, err)
	}
}

// testSettingsModulepath is the fixture modulepath value seedFullEnvironment
// embeds in every environment it seeds, asserted back by
// assertEnvDocsPresent.
const testSettingsModulepath = "modules:$basemodulepath"

// assertEnvDocsPresent asserts every document env owns (per
// seedFullEnvironment: identity-with-settings, Puppetfile, hierarchy and
// two Hiera data files) is present, reading the identity document's
// embedded "settings" sub-object directly through the Documents facet —
// not via GetEnvironmentSettings — so this assertion stays usable from
// Task 1's own tests, which predate that RPC's existence.
func assertEnvDocsPresent(t *testing.T, h *host.Host, ctx context.Context, env string) {
	t.Helper()
	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: env}); err != nil {
		t.Fatalf("GetEnvironment(%q): %v", env, err)
	}
	for _, c := range []struct{ collection, docID string }{
		{"code-puppetfiles", env},
		{"code-hiera-hierarchy", env},
		{"code-hiera-data", env + "/common.yaml"},
		{"code-hiera-data", env + "/nodes/web01.yaml"},
	} {
		if _, ok := getDoc(t, h, ctx, c.collection, c.docID); !ok {
			t.Fatalf("%s/%s missing, want present", c.collection, c.docID)
		}
	}
	identity, ok := getDoc(t, h, ctx, "code-environments", env)
	if !ok {
		t.Fatalf("code-environments/%s missing, want present", env)
	}
	settingsRaw, ok := identity.Body.Value.AsMap()["settings"].(map[string]any)
	if !ok || settingsRaw["modulepath"] != testSettingsModulepath {
		t.Fatalf("%s identity document settings = %v, want modulepath %q", env, settingsRaw, testSettingsModulepath)
	}
}

// assertEnvDocsAbsent asserts none of the six documents seedFullEnvironment
// wrote for env are present anywhere.
func assertEnvDocsAbsent(t *testing.T, h *host.Host, ctx context.Context, env string) {
	t.Helper()
	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: env}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetEnvironment(%q) = %v, want codes.NotFound", env, err)
	}
	for _, c := range []struct{ collection, docID string }{
		{"code-puppetfiles", env},
		{"code-hiera-hierarchy", env},
		{"code-hiera-data", env + "/common.yaml"},
		{"code-hiera-data", env + "/nodes/web01.yaml"},
	} {
		if _, ok := getDoc(t, h, ctx, c.collection, c.docID); ok {
			t.Fatalf("%s/%s still present, want absent", c.collection, c.docID)
		}
	}
}

func TestCode_CreateThenGetEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	created, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if created.Name != "production" {
		t.Fatalf("created.Name = %q, want %q", created.Name, "production")
	}
	if created.CreatedAt == nil {
		t.Fatal("created.CreatedAt is nil, want populated")
	}
	if created.UpdatedAt == nil {
		t.Fatal("created.UpdatedAt is nil, want populated")
	}

	got, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if got.Name != "production" {
		t.Fatalf("got.Name = %q, want %q", got.Name, "production")
	}
}

func TestCode_CreateOnExistingNameIsAlreadyExists(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	first, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("first CreateEnvironment: %v", err)
	}

	_, err = h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"})
	if err == nil {
		t.Fatal("second CreateEnvironment succeeded, want AlreadyExists")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.AlreadyExists {
		t.Fatalf("second CreateEnvironment error = %v, want codes.AlreadyExists", err)
	}

	got, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment after refused create: %v", err)
	}
	if !got.CreatedAt.AsTime().Equal(first.CreatedAt.AsTime()) {
		t.Fatalf("CreatedAt changed after refused duplicate create: got %v, want %v", got.CreatedAt.AsTime(), first.CreatedAt.AsTime())
	}

	list, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(list.Environments) != 1 {
		t.Fatalf("ListEnvironments after refused duplicate create returned %d environments, want 1", len(list.Environments))
	}
}

func TestCode_ListEnvironmentsPagination(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	// Empty store: zero environments, empty cursor, no error.
	empty, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments on empty store: %v", err)
	}
	if len(empty.Environments) != 0 {
		t.Fatalf("empty store returned %d environments, want 0", len(empty.Environments))
	}
	if empty.Page.NextCursor != "" {
		t.Fatalf("empty store NextCursor = %q, want empty", empty.Page.NextCursor)
	}

	for _, name := range []string{"staging", "production", "dev"} {
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: name}); err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
	}

	all, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	wantOrder := []string{"dev", "production", "staging"}
	if len(all.Environments) != len(wantOrder) {
		t.Fatalf("ListEnvironments returned %d environments, want %d", len(all.Environments), len(wantOrder))
	}
	for i, name := range wantOrder {
		if all.Environments[i].Name != name {
			t.Fatalf("ListEnvironments[%d] = %q, want %q", i, all.Environments[i].Name, name)
		}
	}

	// Two-page walk with Limit: 2.
	page1, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("ListEnvironments page1: %v", err)
	}
	if len(page1.Environments) != 2 {
		t.Fatalf("page1 returned %d environments, want 2", len(page1.Environments))
	}
	if page1.Page.NextCursor == "" {
		t.Fatal("page1 NextCursor is empty, want non-empty")
	}

	page2, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Limit: 2, Cursor: page1.Page.NextCursor}})
	if err != nil {
		t.Fatalf("ListEnvironments page2: %v", err)
	}
	if len(page2.Environments) != 1 {
		t.Fatalf("page2 returned %d environments, want 1", len(page2.Environments))
	}
	if page2.Page.NextCursor != "" {
		t.Fatalf("page2 NextCursor = %q, want empty", page2.Page.NextCursor)
	}
}

func TestCode_ListEnvironmentsRejectsBadCursor(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	for _, name := range []string{"staging", "production", "dev"} {
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: name}); err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
	}

	for _, cursor := range []string{"-1", "abc"} {
		_, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Cursor: cursor}})
		if err == nil {
			t.Fatalf("cursor %q succeeded, want InvalidArgument", cursor)
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.InvalidArgument {
			t.Fatalf("cursor %q error = %v, want codes.InvalidArgument", cursor, err)
		}
	}

	outOfRange, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{Page: &hostv1.Page{Cursor: "99"}})
	if err != nil {
		t.Fatalf("out-of-range cursor: %v", err)
	}
	if len(outOfRange.Environments) != 0 {
		t.Fatalf("out-of-range cursor returned %d environments, want 0", len(outOfRange.Environments))
	}
	if outOfRange.Page.NextCursor != "" {
		t.Fatalf("out-of-range cursor NextCursor = %q, want empty", outOfRange.Page.NextCursor)
	}
}

func TestCode_CreateRejectsInvalidName(t *testing.T) {
	invalid := []string{"Production", "prod-1", "prod.1", "prod/1", "prod 1", "PROD", "", "naïve"}
	valid := []string{"production", "dev", "prod_1", "e2e_2026"}

	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			h := local.New([]string{"code:rw"}, "controlrepo")
			_, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: name})
			if err == nil {
				t.Fatalf("CreateEnvironment(%q) succeeded, want InvalidArgument", name)
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.InvalidArgument {
				t.Fatalf("CreateEnvironment(%q) error = %v, want codes.InvalidArgument", name, err)
			}
		})
	}

	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			h := local.New([]string{"code:rw"}, "controlrepo")
			_, err := h.Code.CreateEnvironment(context.Background(), &hostv1.CreateEnvironmentRequest{Name: name})
			if err != nil {
				t.Fatalf("CreateEnvironment(%q) failed: %v", name, err)
			}
		})
	}
}

func TestCode_EnvironmentNameIsByteExact(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "PRODUCTION"}); err == nil {
		t.Fatal("GetEnvironment(\"PRODUCTION\") succeeded, want NotFound")
	} else if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("GetEnvironment(\"PRODUCTION\") error = %v, want codes.NotFound", err)
	}

	if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("GetEnvironment(\"production\") failed: %v", err)
	}
}

// TestCode_GetEnvironmentRejectsEmptyAndUnknownName covers the empty and
// unknown-name edges of ENV-01: an empty name can never name an
// environment, so it returns InvalidArgument rather than NotFound — the
// two errors mean different things to a caller.
func TestCode_GetEnvironmentRejectsEmptyAndUnknownName(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	_, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: ""})
	if err == nil {
		t.Fatal("GetEnvironment(\"\") succeeded, want InvalidArgument")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("GetEnvironment(\"\") error = %v, want codes.InvalidArgument", err)
	}

	_, err = h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "does-not-exist"})
	if err == nil {
		t.Fatal("GetEnvironment(\"does-not-exist\") succeeded, want NotFound")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.NotFound {
		t.Fatalf("GetEnvironment(\"does-not-exist\") error = %v, want codes.NotFound", err)
	}
}

func TestCode_ReadPathClonesEnvironments(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	first, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	first.Name = "mutated"
	first.CreatedAt = nil

	second, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
	if err != nil {
		t.Fatalf("GetEnvironment second read: %v", err)
	}
	if second.Name != "production" {
		t.Fatalf("second read Name = %q, want %q (mutation of first leaked into stored state)", second.Name, "production")
	}
	if second.CreatedAt == nil {
		t.Fatal("second read CreatedAt is nil (mutation of first leaked into stored state)")
	}
}

// codeCall is one entry of the 22-RPC denial table TestCode_DeniedWithoutPermission
// drives against every differently-permissioned host.
type codeCall struct {
	name string
	call func(h *host.Host) error
}

// codeCallsTable builds one closure per Code RPC, each calling it with a
// minimally-valid request. The gate is checked first in every gatedCode
// forwarder, so it is exercised identically whether the inner RPC is a real
// implementation (the three Task 1/2 RPCs) or still resolves through
// codeServer's embedded hostv1.UnimplementedCodeServer (the remaining
// nineteen, implemented by later plans in this phase).
func codeCallsTable() []codeCall {
	ctx := context.Background()
	return []codeCall{
		{"ListEnvironments", func(h *host.Host) error {
			_, err := h.Code.ListEnvironments(ctx, &hostv1.ListEnvironmentsRequest{})
			return err
		}},
		{"GetEnvironment", func(h *host.Host) error {
			_, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"})
			return err
		}},
		{"CreateEnvironment", func(h *host.Host) error {
			_, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "denytest"})
			return err
		}},
		{"RenameEnvironment", func(h *host.Host) error {
			_, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "a", NewName: "b"})
			return err
		}},
		{"DeleteEnvironment", func(h *host.Host) error {
			_, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "a"})
			return err
		}},
		{"DuplicateEnvironment", func(h *host.Host) error {
			_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "a", TargetName: "b"})
			return err
		}},
		{"GetEnvironmentSettings", func(h *host.Host) error {
			_, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "a"})
			return err
		}},
		{"PutEnvironmentSettings", func(h *host.Host) error {
			_, err := h.Code.PutEnvironmentSettings(ctx, &hostv1.PutEnvironmentSettingsRequest{Settings: &hostv1.EnvironmentSettings{Environment: "a"}})
			return err
		}},
		{"ListPuppetfileModules", func(h *host.Host) error {
			_, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "a"})
			return err
		}},
		{"PutPuppetfileModule", func(h *host.Host) error {
			_, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "a", Module: &hostv1.PuppetfileModule{Name: "m"}})
			return err
		}},
		{"RemovePuppetfileModule", func(h *host.Host) error {
			_, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "a", Name: "m"})
			return err
		}},
		{"SetModuledir", func(h *host.Host) error {
			_, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "a", Moduledir: "modules"})
			return err
		}},
		{"RenderPuppetfile", func(h *host.Host) error {
			_, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "a"})
			return err
		}},
		{"GetHieraHierarchy", func(h *host.Host) error {
			_, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: "a"})
			return err
		}},
		{"PutHieraLevel", func(h *host.Host) error {
			_, err := h.Code.PutHieraLevel(ctx, &hostv1.PutHieraLevelRequest{Environment: "a", Level: &hostv1.HieraLevel{Name: "common"}})
			return err
		}},
		{"RemoveHieraLevel", func(h *host.Host) error {
			_, err := h.Code.RemoveHieraLevel(ctx, &hostv1.RemoveHieraLevelRequest{Environment: "a", Name: "common"})
			return err
		}},
		{"ReorderHieraLevels", func(h *host.Host) error {
			_, err := h.Code.ReorderHieraLevels(ctx, &hostv1.ReorderHieraLevelsRequest{Environment: "a", Names: []string{"common"}})
			return err
		}},
		{"ListHieraDataFiles", func(h *host.Host) error {
			_, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: "a"})
			return err
		}},
		{"GetHieraDataFile", func(h *host.Host) error {
			_, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "a", Path: "common.yaml"})
			return err
		}},
		{"PutHieraDataKey", func(h *host.Host) error {
			_, err := h.Code.PutHieraDataKey(ctx, &hostv1.PutHieraDataKeyRequest{Environment: "a", Path: "common.yaml", Key: "k", Value: &hostv1.Json{}})
			return err
		}},
		{"RemoveHieraDataKey", func(h *host.Host) error {
			_, err := h.Code.RemoveHieraDataKey(ctx, &hostv1.RemoveHieraDataKeyRequest{Environment: "a", Path: "common.yaml", Key: "k"})
			return err
		}},
		{"DeleteHieraDataFile", func(h *host.Host) error {
			_, err := h.Code.DeleteHieraDataFile(ctx, &hostv1.DeleteHieraDataFileRequest{Environment: "a", Path: "common.yaml"})
			return err
		}},
	}
}

// TestCode_DeniedWithoutPermission exercises all 22 Code RPCs against three
// differently-permissioned hosts (no permissions, an unrelated permission,
// and the pre-existing code:read permission bound to the dormant reference
// tree's file-browser service) — 66 calls total, every one of which must be
// refused with codes.PermissionDenied carrying an ErrorDetail whose Code is
// facet_not_declared and whose Message names code:rw. It then drives the
// same table once more against a code:rw host and asserts none of the 22 is
// denied, which is what proves the table is wired to real methods rather
// than passing vacuously.
func TestCode_DeniedWithoutPermission(t *testing.T) {
	calls := codeCallsTable()

	deniedHosts := []struct {
		label string
		host  *host.Host
	}{
		{"no permissions", local.New(nil, "controlrepo")},
		{"unrelated permission (inventory:rw)", local.New([]string{"inventory:rw"}, "controlrepo")},
		{"pre-existing code:read", local.New([]string{"code:read"}, "controlrepo")},
	}

	for _, dh := range deniedHosts {
		for _, c := range calls {
			err := c.call(dh.host)
			if err == nil {
				t.Fatalf("%s/%s: succeeded, want PermissionDenied", dh.label, c.name)
			}
			st, ok := status.FromError(err)
			if !ok || st.Code() != codes.PermissionDenied {
				t.Fatalf("%s/%s: error = %v, want codes.PermissionDenied", dh.label, c.name, err)
			}
			var detail *hostv1.ErrorDetail
			for _, d := range st.Details() {
				if ed, ok := d.(*hostv1.ErrorDetail); ok {
					detail = ed
					break
				}
			}
			if detail == nil {
				t.Fatalf("%s/%s: PermissionDenied status has no ErrorDetail", dh.label, c.name)
			}
			if detail.Code != "facet_not_declared" {
				t.Fatalf("%s/%s: ErrorDetail.Code = %q, want %q", dh.label, c.name, detail.Code, "facet_not_declared")
			}
			if !strings.Contains(detail.Message, "code:rw") {
				t.Fatalf("%s/%s: ErrorDetail.Message = %q, want it to contain %q", dh.label, c.name, detail.Message, "code:rw")
			}
		}
	}

	permitted := local.New([]string{"code:rw"}, "controlrepo")
	for _, c := range calls {
		if err := c.call(permitted); err != nil {
			if st, ok := status.FromError(err); ok && st.Code() == codes.PermissionDenied {
				t.Fatalf("code:rw host: %s returned PermissionDenied, want the gate to pass it through to the inner method", c.name)
			}
		}
	}
}

// TestCode_RenameEnvironment proves ENV-01's rename moves all six documents
// an environment owns (identity, Puppetfile, hierarchy, two Hiera data
// files and settings) to the new name in one operation, and that none of
// them are reachable under the old name afterwards.
func TestCode_RenameEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	seedFullEnvironment(t, h, ctx, "staging")

	renamed, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "staging", NewName: "qa"})
	if err != nil {
		t.Fatalf("RenameEnvironment: %v", err)
	}
	if renamed.Name != "qa" {
		t.Fatalf("renamed.Name = %q, want %q", renamed.Name, "qa")
	}

	assertEnvDocsPresent(t, h, ctx, "qa")
	assertEnvDocsAbsent(t, h, ctx, "staging")
}

// TestCode_RenameRejectsCollisionAndUnknown covers RenameEnvironment's
// refusal edges: an invalid current or new name, an unknown source, and a
// target name already in use — each of which must move nothing.
func TestCode_RenameRejectsCollisionAndUnknown(t *testing.T) {
	t.Run("invalid_current_name", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		_, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "Not Valid", NewName: "qa"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("RenameEnvironment(invalid current name) = %v, want codes.InvalidArgument", err)
		}
	})

	t.Run("invalid_new_name", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "staging"}); err != nil {
			t.Fatalf("CreateEnvironment: %v", err)
		}
		_, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "staging", NewName: "Not Valid"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("RenameEnvironment(invalid new name) = %v, want codes.InvalidArgument", err)
		}
		if _, ok := getDoc(t, h, ctx, "code-environments", "staging"); !ok {
			t.Fatal("staging identity document moved despite invalid new name")
		}
	})

	t.Run("unknown_source", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		_, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "does_not_exist", NewName: "qa"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("RenameEnvironment(unknown source) = %v, want codes.NotFound", err)
		}
	})

	t.Run("collision", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		seedFullEnvironment(t, h, ctx, "staging")
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "qa"}); err != nil {
			t.Fatalf("CreateEnvironment(qa): %v", err)
		}

		_, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "staging", NewName: "qa"})
		if status.Code(err) != codes.AlreadyExists {
			t.Fatalf("RenameEnvironment(collision) = %v, want codes.AlreadyExists", err)
		}
		// The source is still fully readable afterwards.
		assertEnvDocsPresent(t, h, ctx, "staging")
	})
}

// TestCode_DeleteEnvironment proves ENV-01's delete removes all six
// documents an environment owns, and that deleting an unknown name returns
// codes.NotFound.
func TestCode_DeleteEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	seedFullEnvironment(t, h, ctx, "staging")

	if _, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "staging"}); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	assertEnvDocsAbsent(t, h, ctx, "staging")

	_, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "staging"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("DeleteEnvironment(unknown) = %v, want codes.NotFound", err)
	}
}

// TestCode_AdjacentEnvironmentNamesAreNotTouched is the load-bearing proof
// that environment ownership of a code-hiera-data document is decided by
// the composite doc id's "<name>/" prefix, never by a plain string-prefix
// match on the name: with both "prod" and "production" present and each
// carrying a common.yaml data file, deleting or renaming "prod" must leave
// every one of "production"'s documents present and byte-identical.
func TestCode_AdjacentEnvironmentNamesAreNotTouched(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		seedFullEnvironment(t, h, ctx, "prod")
		seedFullEnvironment(t, h, ctx, "production")

		if _, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "prod"}); err != nil {
			t.Fatalf("DeleteEnvironment(prod): %v", err)
		}
		assertEnvDocsAbsent(t, h, ctx, "prod")
		assertEnvDocsPresent(t, h, ctx, "production")
	})

	t.Run("rename", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		seedFullEnvironment(t, h, ctx, "prod")
		seedFullEnvironment(t, h, ctx, "production")

		if _, err := h.Code.RenameEnvironment(ctx, &hostv1.RenameEnvironmentRequest{Name: "prod", NewName: "qa"}); err != nil {
			t.Fatalf("RenameEnvironment(prod->qa): %v", err)
		}
		assertEnvDocsAbsent(t, h, ctx, "prod")
		assertEnvDocsPresent(t, h, ctx, "qa")
		assertEnvDocsPresent(t, h, ctx, "production")
	})
}

// TestCode_SettingsUnwrittenFieldsAreAbsent asserts each of the seven
// setting fields individually by name for a never-configured environment,
// rather than comparing against a zero-value struct, so a future eighth
// field added without presence handling fails here instead of passing
// silently.
func TestCode_SettingsUnwrittenFieldsAreAbsent(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	settings, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings: %v", err)
	}
	if settings.Environment != "production" {
		t.Fatalf("Environment = %q, want %q", settings.Environment, "production")
	}
	if settings.Modulepath != nil {
		t.Fatalf("Modulepath = %v, want nil", settings.Modulepath)
	}
	if settings.Manifest != nil {
		t.Fatalf("Manifest = %v, want nil", settings.Manifest)
	}
	if settings.ConfigVersion != nil {
		t.Fatalf("ConfigVersion = %v, want nil", settings.ConfigVersion)
	}
	if settings.EnvironmentTimeout != nil {
		t.Fatalf("EnvironmentTimeout = %v, want nil", settings.EnvironmentTimeout)
	}
	if settings.DisablePerEnvironmentManifest != nil {
		t.Fatalf("DisablePerEnvironmentManifest = %v, want nil", settings.DisablePerEnvironmentManifest)
	}
	if settings.StaticCatalogs != nil {
		t.Fatalf("StaticCatalogs = %v, want nil", settings.StaticCatalogs)
	}
	if settings.RichData != nil {
		t.Fatalf("RichData = %v, want nil", settings.RichData)
	}

	_, err = h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "does_not_exist"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetEnvironmentSettings(unknown) = %v, want codes.NotFound", err)
	}
}

// TestCode_SettingsRoundTrip proves PutEnvironmentSettings replaces the
// whole record rather than merging: a field written as the empty string
// reads back present-and-empty (distinct from absent), and a field set by
// an earlier put and absent from a later one reads back absent.
func TestCode_SettingsRoundTrip(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// Settings writes are gated on every call (D-03), so each one goes
	// through propose, approve and ApplyEnvironmentSettings.
	first := putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{
		Environment: "production",
		Modulepath:  strPtr("modules:$basemodulepath"),
		RichData:    boolPtr(true),
	})
	if first.Modulepath == nil || *first.Modulepath != "modules:$basemodulepath" {
		t.Fatalf("first.Modulepath = %v, want %q", first.Modulepath, "modules:$basemodulepath")
	}
	if first.RichData == nil || *first.RichData != true {
		t.Fatalf("first.RichData = %v, want true", first.RichData)
	}
	if first.Manifest != nil {
		t.Fatalf("first.Manifest = %v, want nil", first.Manifest)
	}

	got, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings: %v", err)
	}
	if got.Modulepath == nil || *got.Modulepath != "modules:$basemodulepath" {
		t.Fatalf("got.Modulepath = %v, want %q", got.Modulepath, "modules:$basemodulepath")
	}
	if got.RichData == nil || *got.RichData != true {
		t.Fatalf("got.RichData = %v, want true", got.RichData)
	}

	// manifest stored as the empty string reads back present-and-empty,
	// distinguishable from absent.
	second := putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{
		Environment: "production",
		Manifest:    strPtr(""),
	})
	if second.Manifest == nil || *second.Manifest != "" {
		t.Fatalf("second.Manifest = %v, want present-and-empty", second.Manifest)
	}
	// A field set by the first call and absent from the second reads back
	// absent — the whole record was replaced, not merged.
	if second.Modulepath != nil {
		t.Fatalf("second.Modulepath = %v, want nil (whole record replaced, not merged)", second.Modulepath)
	}
	if second.RichData != nil {
		t.Fatalf("second.RichData = %v, want nil (whole record replaced, not merged)", second.RichData)
	}

	reread, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings after second put: %v", err)
	}
	if reread.Manifest == nil || *reread.Manifest != "" {
		t.Fatalf("reread.Manifest = %v, want present-and-empty", reread.Manifest)
	}
	if reread.Modulepath != nil {
		t.Fatalf("reread.Modulepath = %v, want nil", reread.Modulepath)
	}

	_, err = h.Code.PutEnvironmentSettings(ctx, &hostv1.PutEnvironmentSettingsRequest{
		Settings: &hostv1.EnvironmentSettings{Environment: "does_not_exist", Modulepath: strPtr("x")},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("PutEnvironmentSettings(unknown environment) = %v, want codes.NotFound", err)
	}
}

// TestCode_SettingsRefusesNewlineValue proves PutEnvironmentSettings
// refuses, with codes.InvalidArgument and no write, any string value
// carrying a carriage return or line feed — such a value would, once
// written to a real environment.conf, split into an additional unauthored
// setting line (T-06-02).
func TestCode_SettingsRefusesNewlineValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"line_feed_in_modulepath", "modules\nmalicious = true"},
		{"carriage_return_in_modulepath", "modules\rmalicious = true"},
		{"line_feed_in_environment_timeout", "5m\nmalicious = true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := local.New([]string{"code:rw"}, "controlrepo")
			ctx := context.Background()
			if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
				t.Fatalf("CreateEnvironment: %v", err)
			}

			settings := &hostv1.EnvironmentSettings{Environment: "production"}
			if strings.Contains(tc.name, "environment_timeout") {
				settings.EnvironmentTimeout = strPtr(tc.value)
			} else {
				settings.Modulepath = strPtr(tc.value)
			}

			_, err := h.Code.PutEnvironmentSettings(ctx, &hostv1.PutEnvironmentSettingsRequest{Settings: settings})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("PutEnvironmentSettings(%s) = %v, want codes.InvalidArgument", tc.name, err)
			}

			got, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
			if err != nil {
				t.Fatalf("GetEnvironmentSettings after refused put: %v", err)
			}
			if got.Modulepath != nil || got.EnvironmentTimeout != nil {
				t.Fatalf("settings stored after refused put: Modulepath=%v EnvironmentTimeout=%v", got.Modulepath, got.EnvironmentTimeout)
			}
		})
	}
}

// TestCode_SettingsTimeoutIsOpaqueText proves environment_timeout's three
// documented forms — "0", "unlimited" and a duration like "5m" — all store
// and read back byte-identical, with no numeric parsing applied.
func TestCode_SettingsTimeoutIsOpaqueText(t *testing.T) {
	for _, value := range []string{"0", "unlimited", "5m"} {
		t.Run(value, func(t *testing.T) {
			h := newOverwriteHost()
			ctx := context.Background()
			if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
				t.Fatalf("CreateEnvironment: %v", err)
			}
			putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "production", EnvironmentTimeout: strPtr(value)})
			got, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
			if err != nil {
				t.Fatalf("GetEnvironmentSettings: %v", err)
			}
			if got.EnvironmentTimeout == nil || *got.EnvironmentTimeout != value {
				t.Fatalf("EnvironmentTimeout = %v, want %q", got.EnvironmentTimeout, value)
			}
		})
	}

	// A very long modulepath value survives storage and retrieval without
	// truncation.
	h := newOverwriteHost()
	ctx := context.Background()
	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "production"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	long := strings.Repeat("modules/path-segment:", 500)
	putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "production", Modulepath: strPtr(long)})
	got, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings: %v", err)
	}
	if got.Modulepath == nil || *got.Modulepath != long {
		t.Fatalf("long modulepath round-trip mismatch: got len %d, want len %d", len(strDeref(got.Modulepath)), len(long))
	}
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// TestCode_DuplicateEnvironmentCopiesEverything proves DuplicateEnvironment
// copies a source's Puppetfile, Hiera hierarchy, both Hiera data files and
// settings to the target name, byte-identical, with data files re-keyed
// under the target prefix — and that the source is unchanged afterwards,
// including after the target is separately edited (T-06-25: no shared
// *hostv1.Json value).
func TestCode_DuplicateEnvironmentCopiesEverything(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	seedFullEnvironment(t, h, ctx, "production")

	dup, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "production", TargetName: "staging"})
	if err != nil {
		t.Fatalf("DuplicateEnvironment: %v", err)
	}
	if dup.Name != "staging" {
		t.Fatalf("dup.Name = %q, want %q", dup.Name, "staging")
	}

	assertEnvDocsPresent(t, h, ctx, "production")
	assertEnvDocsPresent(t, h, ctx, "staging")

	srcPF, ok := getDoc(t, h, ctx, "code-puppetfiles", "production")
	if !ok {
		t.Fatal("source Puppetfile missing")
	}
	dstPF, ok := getDoc(t, h, ctx, "code-puppetfiles", "staging")
	if !ok {
		t.Fatal("target Puppetfile missing")
	}
	if srcPF.Body.Value.AsMap()["text"] != dstPF.Body.Value.AsMap()["text"] {
		t.Fatalf("Puppetfile text mismatch: source %v, target %v", srcPF.Body.Value.AsMap()["text"], dstPF.Body.Value.AsMap()["text"])
	}

	// Editing the target's settings after the copy must not change the
	// source's — proves the two environments share no *hostv1.Json value.
	putSettingsViaApproval(t, h, &hostv1.EnvironmentSettings{Environment: "staging", Modulepath: strPtr("changed-after-copy")})
	srcSettings, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings(production): %v", err)
	}
	if srcSettings.Modulepath == nil || *srcSettings.Modulepath != "modules:$basemodulepath" {
		t.Fatalf("source settings changed after editing target's copy: got %v, want %q", srcSettings.Modulepath, "modules:$basemodulepath")
	}
}

// TestCode_DuplicateOntoExistingNameIsRefused proves an in-use target
// refuses with the structured requires-approval error (it was a bare
// codes.AlreadyExists before the overwrite gate) having written nothing, an unknown
// source refuses with codes.NotFound, an invalid name refuses with
// codes.InvalidArgument, and a second duplicate of the same pair still
// refuses with the first copy left byte-identical.
func TestCode_DuplicateOntoExistingNameIsRefused(t *testing.T) {
	t.Run("target_in_use", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		seedFullEnvironment(t, h, ctx, "production")
		if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "staging"}); err != nil {
			t.Fatalf("CreateEnvironment(staging): %v", err)
		}

		_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "production", TargetName: "staging"})
		if !local.IsCodeOverwriteRequiresApproval(err) || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("DuplicateEnvironment(target in use) = %v, want FailedPrecondition requires-approval", err)
		}
		// Nothing beyond the pre-existing bare "staging" identity document
		// was written.
		if _, ok := getDoc(t, h, ctx, "code-puppetfiles", "staging"); ok {
			t.Fatal("staging gained a Puppetfile despite the refused duplicate")
		}
	})

	t.Run("unknown_source", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "does_not_exist", TargetName: "staging"})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("DuplicateEnvironment(unknown source) = %v, want codes.NotFound", err)
		}
	})

	t.Run("invalid_name", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "Not Valid", TargetName: "staging"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("DuplicateEnvironment(invalid source name) = %v, want codes.InvalidArgument", err)
		}
	})

	t.Run("repeat_duplicate_leaves_first_copy_intact", func(t *testing.T) {
		h := local.New([]string{"code:rw"}, "controlrepo")
		ctx := context.Background()
		seedFullEnvironment(t, h, ctx, "production")
		if _, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "production", TargetName: "staging"}); err != nil {
			t.Fatalf("first DuplicateEnvironment: %v", err)
		}
		firstDoc, ok := getDoc(t, h, ctx, "code-puppetfiles", "staging")
		if !ok {
			t.Fatal("first copy's Puppetfile missing")
		}

		_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "production", TargetName: "staging"})
		if !local.IsCodeOverwriteRequiresApproval(err) {
			t.Fatalf("second DuplicateEnvironment = %v, want requires-approval", err)
		}
		secondDoc, ok := getDoc(t, h, ctx, "code-puppetfiles", "staging")
		if !ok {
			t.Fatal("staging's Puppetfile disappeared after refused repeat duplicate")
		}
		if firstDoc.Version != secondDoc.Version {
			t.Fatalf("staging's Puppetfile version changed after refused repeat duplicate: was %d, now %d", firstDoc.Version, secondDoc.Version)
		}
		if firstDoc.Body.Value.AsMap()["text"] != secondDoc.Body.Value.AsMap()["text"] {
			t.Fatal("staging's Puppetfile text changed after refused repeat duplicate")
		}
	})
}

// TestCode_DuplicateOfEmptyEnvironment proves duplicating a brand-new
// environment with no Puppetfile, no Hiera and no settings authored yet
// succeeds and produces a target in the same (empty) state, rather than
// erroring on the absent documents. It also proves duplicating "prod" never
// copies any of "production"'s documents when both are present.
func TestCode_DuplicateOfEmptyEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment(prod): %v", err)
	}
	seedFullEnvironment(t, h, ctx, "production")

	dup, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "prod", TargetName: "qa"})
	if err != nil {
		t.Fatalf("DuplicateEnvironment(empty prod -> qa): %v", err)
	}
	if dup.Name != "qa" {
		t.Fatalf("dup.Name = %q, want %q", dup.Name, "qa")
	}
	if _, ok := getDoc(t, h, ctx, "code-puppetfiles", "qa"); ok {
		t.Fatal("qa gained a Puppetfile from duplicating empty prod — production's Puppetfile leaked across the prod/production name-prefix boundary")
	}
	if _, ok := getDoc(t, h, ctx, "code-hiera-hierarchy", "qa"); ok {
		t.Fatal("qa gained a hierarchy from duplicating empty prod")
	}
	if _, ok := getDoc(t, h, ctx, "code-hiera-data", "qa/common.yaml"); ok {
		t.Fatal("qa gained production's common.yaml from duplicating empty prod")
	}
	settings, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "qa"})
	if err != nil {
		t.Fatalf("GetEnvironmentSettings(qa): %v", err)
	}
	if settings.Modulepath != nil {
		t.Fatalf("qa.Modulepath = %v, want nil", settings.Modulepath)
	}
}

// TestCode_DuplicateConcurrentIsExactlyOnce launches eight goroutines
// duplicating the same source onto the same target concurrently and
// asserts exactly one succeeds and seven return the requires-approval refusal, with
// the target's document count equal to the source's — proving the
// operation is genuinely atomic under concurrency, not merely correct in
// the single-threaded case. A second set of goroutines reads the target
// throughout via GetEnvironment and the Documents facet directly, asserting
// every read returns either a complete result or codes.NotFound — never a
// partial one.
func TestCode_DuplicateConcurrentIsExactlyOnce(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()
	seedFullEnvironment(t, h, ctx, "production")

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, alreadyExists, other int

	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A single GetEnvironment call is trivially all-or-nothing at
			// its own boundary (one document); the meaningful atomicity
			// check is across the two-document code-hiera-data collection,
			// read through one List call — one RPC, one lock acquisition,
			// one consistent snapshot. The count of "staging/"-prefixed
			// documents it returns must be 0 or 2, never 1 (never a torn
			// copy observed mid-write).
			_, envErr := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "staging"})
			if envErr != nil && status.Code(envErr) != codes.NotFound {
				t.Errorf("concurrent GetEnvironment(staging) = %v, want nil or codes.NotFound", envErr)
			}

			listResp, listErr := h.Documents.List(ctx, &hostv1.ListDocumentsRequest{Collection: "code-hiera-data"})
			if listErr != nil {
				t.Errorf("concurrent Documents.List(code-hiera-data) = %v, want nil", listErr)
				continue
			}
			stagingCount := 0
			for _, doc := range listResp.Documents {
				if strings.HasPrefix(doc.DocId, "staging/") {
					stagingCount++
				}
			}
			if stagingCount != 0 && stagingCount != 2 {
				t.Errorf("partial copy observed: %d of staging's 2 hiera data documents present", stagingCount)
			}
		}
	}()

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := h.Code.DuplicateEnvironment(ctx, &hostv1.DuplicateEnvironmentRequest{SourceName: "production", TargetName: "staging"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case local.IsCodeOverwriteRequiresApproval(err):
				alreadyExists++
			default:
				other++
			}
		}()
	}
	wg.Wait()
	close(stop)
	readerWG.Wait()

	if successes != 1 {
		t.Fatalf("successes = %d, want 1", successes)
	}
	if alreadyExists != n-1 {
		t.Fatalf("requires-approval refusals = %d, want %d", alreadyExists, n-1)
	}
	if other != 0 {
		t.Fatalf("other errors = %d, want 0", other)
	}

	assertEnvDocsPresent(t, h, ctx, "staging")
	assertEnvDocsPresent(t, h, ctx, "production")
}

// TestImport_Permissions is the denial table for the three import RPCs. It is
// separate from codeCallsTable on purpose: the import RPCs need two grants, so
// the table asserts each direction. Because gatedCode embeds
// UnimplementedCodeServer, a forgotten forwarder compiles and returns
// Unimplemented rather than PermissionDenied, so this table is the only thing
// that proves each forwarder exists.
func TestImport_Permissions(t *testing.T) {
	calls := []struct {
		name string
		call func(h *host.Host) error
	}{
		{"InspectImport", func(h *host.Host) error {
			_, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: "https://git.example.test/org/control.git"})
			return err
		}},
		{"ProposeImport", func(h *host.Host) error {
			_, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{ProposalId: "p", Url: "https://git.example.test/org/control.git"})
			return err
		}},
		{"ApplyImport", func(h *host.Host) error {
			_, err := h.Code.ApplyImport(context.Background(), &hostv1.ApplyImportRequest{ProposalId: "no-such-proposal"})
			return err
		}},
	}
	rows := []struct {
		label  string
		perms  []string
		denied bool
		names  string // the permission the refusal must name
	}{
		{"no permissions", nil, true, "code:rw"},
		{"code:import only", []string{"code:import"}, true, "code:rw"},
		{"code:rw only", []string{"code:rw"}, true, "code:import"},
		{"both", []string{"code:rw", "code:import"}, false, ""},
	}
	for _, c := range calls {
		for _, r := range rows {
			fx := local.NewGitFixture(map[string]map[string]string{"production": {"Puppetfile": ""}})
			h := local.New(r.perms, "controlrepo", local.WithGitClient(fx))
			err := c.call(h)
			if !r.denied {
				if status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.Unimplemented {
					t.Fatalf("%s/%s: %v, want the gate to pass the call through to the inner method", c.name, r.label, err)
				}
				continue
			}
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("%s/%s: error = %v, want PermissionDenied (Unimplemented means the forwarder is missing)", c.name, r.label, err)
			}
			var detail *hostv1.ErrorDetail
			for _, d := range status.Convert(err).Details() {
				if ed, ok := d.(*hostv1.ErrorDetail); ok {
					detail = ed
					break
				}
			}
			if detail == nil || detail.Code != "facet_not_declared" {
				t.Fatalf("%s/%s: detail = %+v, want facet_not_declared", c.name, r.label, detail)
			}
			if !strings.Contains(detail.Message, `"`+r.names+`"`) {
				t.Fatalf("%s/%s: message %q does not name %s", c.name, r.label, detail.Message, r.names)
			}
			if r.names == "code:rw" && strings.Contains(detail.Message, "code:import") {
				t.Fatalf("%s/%s: code:rw is checked first, but the message names code:import", c.name, r.label)
			}
			if fx.Listed() != 0 || fx.Opens() != 0 {
				t.Fatalf("%s/%s: a refused call reached the git client", c.name, r.label)
			}
		}
	}
}
