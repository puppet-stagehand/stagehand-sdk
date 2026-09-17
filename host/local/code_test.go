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
