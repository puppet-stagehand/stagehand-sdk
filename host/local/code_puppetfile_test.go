package local_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// puppetfileTextFromDocumentsRaw reads the code-puppetfiles document's
// "text" field directly through the always-available Documents facet —
// used by tests that must observe the stored bytes without going through a
// Puppetfile RPC that may not exist yet within this task's scope (e.g.
// RenderPuppetfile, which Task 2 of this plan adds).
func puppetfileTextFromDocumentsRaw(t *testing.T, h *host.Host, ctx context.Context, env string) string {
	t.Helper()
	doc, ok := getDoc(t, h, ctx, "code-puppetfiles", env)
	if !ok {
		return ""
	}
	text, _ := doc.Body.Value.AsMap()["text"].(string)
	return text
}

func TestCode_PuppetfileEmptyEnvironment(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if len(resp.Modules) != 0 {
		t.Fatalf("expected zero modules, got %d", len(resp.Modules))
	}
	if resp.Moduledir != "" {
		t.Fatalf("expected empty Moduledir, got %q", resp.Moduledir)
	}
	if resp.Page.GetNextCursor() != "" {
		t.Fatalf("expected empty NextCursor, got %q", resp.Page.GetNextCursor())
	}
}

func TestCode_PuppetfileForgeModuleLifecycle(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	bare, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/ntp",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}},
		},
	})
	if err != nil {
		t.Fatalf("PutPuppetfileModule(ntp): %v", err)
	}
	if bare.GetForge().GetVersion() != "" || bare.GetForge().GetLatest() {
		t.Fatalf("expected bare forge module, got %+v", bare.GetForge())
	}

	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/apache",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}},
		},
	}); err != nil {
		t.Fatalf("PutPuppetfileModule(apache): %v", err)
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/stdlib",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}},
		},
	}); err != nil {
		t.Fatalf("PutPuppetfileModule(stdlib): %v", err)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if len(resp.Modules) != 3 {
		t.Fatalf("expected 3 modules, got %d", len(resp.Modules))
	}
	wantNames := []string{"puppetlabs/ntp", "puppetlabs/apache", "puppetlabs/stdlib"}
	for i, name := range wantNames {
		if resp.Modules[i].GetName() != name {
			t.Fatalf("module %d: got name %q, want %q", i, resp.Modules[i].GetName(), name)
		}
	}

	// Edit apache in place. Replacing an existing module is an overwrite, so
	// it goes through the approval gate rather than a plain Put.
	overwriteViaApproval(t, h, "apache-edit", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	resp, err = h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules after edit: %v", err)
	}
	if len(resp.Modules) != 3 {
		t.Fatalf("expected 3 modules after edit, got %d", len(resp.Modules))
	}
	if resp.Modules[1].GetName() != "puppetlabs/apache" || resp.Modules[1].GetForge().GetVersion() != "6.1.0" {
		t.Fatalf("expected apache at index 1 with version 6.1.0, got %+v", resp.Modules[1])
	}
	if resp.Modules[0].GetName() != "puppetlabs/ntp" || resp.Modules[2].GetName() != "puppetlabs/stdlib" {
		t.Fatalf("neighbours disturbed: %+v", resp.Modules)
	}

	// Remove ntp, check the gap closes.
	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "puppetlabs/ntp"}); err != nil {
		t.Fatalf("RemovePuppetfileModule(ntp): %v", err)
	}
	resp, err = h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules after remove: %v", err)
	}
	if len(resp.Modules) != 2 {
		t.Fatalf("expected 2 modules after remove, got %d", len(resp.Modules))
	}
	if resp.Modules[0].GetName() != "puppetlabs/apache" || resp.Modules[1].GetName() != "puppetlabs/stdlib" {
		t.Fatalf("unexpected modules after remove: %+v", resp.Modules)
	}

	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "does-not-exist"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemovePuppetfileModule(unknown): got %v, want NotFound", err)
	}
}

func TestCode_PuppetfileGitModuleLifecycle(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	tagMod := &hostv1.PuppetfileModule{
		Name: "concat",
		Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url:     "https://github.com/puppetlabs/puppetlabs-concat",
			RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"},
		}},
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: tagMod}); err != nil {
		t.Fatalf("PutPuppetfileModule(concat/tag): %v", err)
	}

	controlBranchMod := &hostv1.PuppetfileModule{
		Name: "profiles",
		Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url:           "git@git.example.com:puppet/profiles.git",
			RefKind:       &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}},
			DefaultBranch: "main",
		}},
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: controlBranchMod}); err != nil {
		t.Fatalf("PutPuppetfileModule(profiles/control-branch): %v", err)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if len(resp.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(resp.Modules))
	}
	if !proto.Equal(resp.Modules[0], tagMod) {
		t.Fatalf("tag module round-trip mismatch: got %+v, want %+v", resp.Modules[0], tagMod)
	}
	if !proto.Equal(resp.Modules[1], controlBranchMod) {
		t.Fatalf("control-branch module round-trip mismatch: got %+v, want %+v", resp.Modules[1], controlBranchMod)
	}
}

func TestCode_PuppetfileRejectsInvalidModule(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	cases := []struct {
		name   string
		module *hostv1.PuppetfileModule
	}{
		{"nil module", nil},
		{"empty name", &hostv1.PuppetfileModule{Name: "", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}}},
		{"forge dual version+latest", &hostv1.PuppetfileModule{
			Name:   "puppetlabs/apache",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "1.0.0", Latest: true}},
		}},
		{"forge name without namespace", &hostv1.PuppetfileModule{
			Name:   "apache",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}},
		}},
		{"git empty url", &hostv1.PuppetfileModule{
			Name:   "apache",
			Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: ""}},
		}},
		{"git url with newline", &hostv1.PuppetfileModule{
			Name:   "apache",
			Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{Url: "https://example.com/apache.git\nmoduledir 'evil'"}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: tc.module})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("got %v, want InvalidArgument", err)
			}
		})
	}
}

func TestCode_PuppetfilePutOfExistingModuleIsRefusedAndTextStable(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	apache := &hostv1.PuppetfileModule{
		Name:   "puppetlabs/apache",
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}},
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/ntp",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}},
		},
	}); err != nil {
		t.Fatalf("PutPuppetfileModule(ntp): %v", err)
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: apache}); err != nil {
		t.Fatalf("PutPuppetfileModule(apache): %v", err)
	}

	// Read the stored text directly through the Documents facet, since
	// RenderPuppetfile is not implemented until Task 2 of this plan.
	firstText := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")

	// Putting the identical module again is still a replace of an existing
	// module, so it is refused rather than accepted as a no-op; what matters
	// here is that the refusal leaves the stored text byte-identical.
	if err := putModuleErr(h, "prod", apache); !local.IsCodeOverwriteRequiresApproval(err) {
		t.Fatalf("PutPuppetfileModule(apache second time): got %v, want requires-approval", err)
	}

	secondText := puppetfileTextFromDocumentsRaw(t, h, ctx, "prod")
	if firstText != secondText {
		t.Fatalf("expected byte-identical text after a refused put, got:\nfirst:  %q\nsecond: %q", firstText, secondText)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if len(resp.Modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(resp.Modules))
	}
}

func TestCode_PuppetfilePagination(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	for _, name := range []string{"puppetlabs/ntp", "puppetlabs/apache", "puppetlabs/stdlib"} {
		if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
			Environment: "prod",
			Module: &hostv1.PuppetfileModule{
				Name:   name,
				Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}},
			},
		}); err != nil {
			t.Fatalf("PutPuppetfileModule(%s): %v", name, err)
		}
	}

	page1, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod", Page: &hostv1.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("ListPuppetfileModules page1: %v", err)
	}
	if len(page1.Modules) != 2 {
		t.Fatalf("page1: expected 2 modules, got %d", len(page1.Modules))
	}
	if page1.Page.GetNextCursor() == "" {
		t.Fatalf("page1: expected non-empty NextCursor")
	}

	page2, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{
		Environment: "prod",
		Page:        &hostv1.Page{Limit: 2, Cursor: page1.Page.GetNextCursor()},
	})
	if err != nil {
		t.Fatalf("ListPuppetfileModules page2: %v", err)
	}
	if len(page2.Modules) != 1 {
		t.Fatalf("page2: expected 1 module, got %d", len(page2.Modules))
	}
	if page2.Page.GetNextCursor() != "" {
		t.Fatalf("page2: expected empty NextCursor, got %q", page2.Page.GetNextCursor())
	}

	if _, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{
		Environment: "prod",
		Page:        &hostv1.Page{Cursor: "abc"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad cursor: got %v, want InvalidArgument", err)
	}
}

func TestCode_PuppetfileUnknownEnvironmentAndModule(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("ListPuppetfileModules(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "ghost",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/ntp",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}},
		},
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("PutPuppetfileModule(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "ghost", Name: "puppetlabs/ntp"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemovePuppetfileModule(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "ghost", Moduledir: "thirdparty"}); status.Code(err) != codes.NotFound {
		t.Fatalf("SetModuledir(ghost): got %v, want NotFound", err)
	}
	if _, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RenderPuppetfile(ghost): got %v, want NotFound", err)
	}

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "does-not-exist"}); status.Code(err) != codes.NotFound {
		t.Fatalf("RemovePuppetfileModule(unknown module): got %v, want NotFound", err)
	}
}

func TestCode_ModuledirSetAndClear(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	// RenderPuppetfile for an environment with no Puppetfile document
	// returns the empty string and a nil error.
	unauthored, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile (unauthored): %v", err)
	}
	if unauthored.Text != "" {
		t.Fatalf("expected empty text for an unauthored Puppetfile, got %q", unauthored.Text)
	}

	pf, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: "thirdparty"})
	if err != nil {
		t.Fatalf("SetModuledir(thirdparty): %v", err)
	}
	if pf.Moduledir != "thirdparty" {
		t.Fatalf("expected returned Puppetfile.Moduledir == thirdparty, got %q", pf.Moduledir)
	}

	resp, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("ListPuppetfileModules: %v", err)
	}
	if resp.Moduledir != "thirdparty" {
		t.Fatalf("expected Moduledir == thirdparty, got %q", resp.Moduledir)
	}

	rendered, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile: %v", err)
	}
	if rendered.Text != "moduledir 'thirdparty'\n" {
		t.Fatalf("expected moduledir-only render, got %q", rendered.Text)
	}

	// Setting the same value twice leaves the stored text byte-identical.
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: "thirdparty"}); err != nil {
		t.Fatalf("SetModuledir(thirdparty again): %v", err)
	}
	rendered2, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile (2nd): %v", err)
	}
	if rendered.Text != rendered2.Text {
		t.Fatalf("expected byte-identical text after idempotent SetModuledir, got:\nfirst:  %q\nsecond: %q", rendered.Text, rendered2.Text)
	}

	// Clearing removes the moduledir line entirely.
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: ""}); err != nil {
		t.Fatalf("SetModuledir(clear): %v", err)
	}
	cleared, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile after clear: %v", err)
	}
	if cleared.Text != "" {
		t.Fatalf("expected empty text after clearing moduledir with no modules, got %q", cleared.Text)
	}
}

func TestCode_PuppetfileNoSpuriousDiff(t *testing.T) {
	h := newOverwriteHost()
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	assertFixedPoint := func(t *testing.T, step string) {
		t.Helper()
		rendered, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
		if err != nil {
			t.Fatalf("%s: RenderPuppetfile: %v", step, err)
		}
		// This assertion is deliberately NOT a comparison against some
		// original hand-written file's bytes — PF-05 is model round-trip
		// idempotency (the writer being a fixed point of its own parser),
		// not preservation of an imported file's comments or quoting
		// style. The ecosystem's own tooling discards those on parse.
		model, err := code.ParsePuppetfile(rendered.Text)
		if err != nil {
			t.Fatalf("%s: ParsePuppetfile(%q): %v", step, rendered.Text, err)
		}
		rerendered, err := code.RenderPuppetfile(model)
		if err != nil {
			t.Fatalf("%s: RenderPuppetfile(model): %v", step, err)
		}
		if rendered.Text != rerendered {
			t.Fatalf("%s: not a fixed point:\nstored:     %q\nre-rendered: %q", step, rendered.Text, rerendered)
		}
	}

	// 1. Add a Forge module.
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name:   "puppetlabs/apache",
			Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}},
		},
	}); err != nil {
		t.Fatalf("add forge module: %v", err)
	}
	assertFixedPoint(t, "after add Forge module")

	// 2. Add a Git module.
	if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{
		Environment: "prod",
		Module: &hostv1.PuppetfileModule{
			Name: "concat",
			Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
				Url:     "https://github.com/puppetlabs/puppetlabs-concat",
				RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"},
			}},
		},
	}); err != nil {
		t.Fatalf("add git module: %v", err)
	}
	assertFixedPoint(t, "after add Git module")

	// 3. Edit the Forge module's version.
	overwriteViaApproval(t, h, "apache-edit", "prod", forgeModule("puppetlabs/apache", "6.1.0"))
	assertFixedPoint(t, "after edit Forge module")

	// 4. Set moduledir.
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: "thirdparty"}); err != nil {
		t.Fatalf("set moduledir: %v", err)
	}
	assertFixedPoint(t, "after set moduledir")

	// 5. Remove the Git module.
	if _, err := h.Code.RemovePuppetfileModule(ctx, &hostv1.RemovePuppetfileModuleRequest{Environment: "prod", Name: "concat"}); err != nil {
		t.Fatalf("remove git module: %v", err)
	}
	assertFixedPoint(t, "after remove Git module")

	// 6. Clear moduledir.
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: ""}); err != nil {
		t.Fatalf("clear moduledir: %v", err)
	}
	assertFixedPoint(t, "after clear moduledir")

	// Calling RenderPuppetfile twice with no intervening write returns
	// identical text both times.
	first, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile (first, no write): %v", err)
	}
	second, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile (second, no write): %v", err)
	}
	if first.Text != second.Text {
		t.Fatalf("RenderPuppetfile not stable across repeated calls: %q vs %q", first.Text, second.Text)
	}
}

func TestCode_PuppetfileRenderMatchesRenderContract(t *testing.T) {
	h := local.New([]string{"code:rw"}, "controlrepo")
	ctx := context.Background()

	if _, err := h.Code.CreateEnvironment(ctx, &hostv1.CreateEnvironmentRequest{Name: "prod"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if _, err := h.Code.SetModuledir(ctx, &hostv1.SetModuledirRequest{Environment: "prod", Moduledir: "thirdparty"}); err != nil {
		t.Fatalf("SetModuledir: %v", err)
	}

	modules := []*hostv1.PuppetfileModule{
		{Name: "puppetlabs/ntp", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{}}},
		{Name: "puppetlabs/apache", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "0.10.0"}}},
		{Name: "puppetlabs/stdlib", Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Latest: true}}},
		{Name: "apache", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url: "https://github.com/puppetlabs/puppetlabs-apache",
		}}},
		{Name: "concat", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url:     "https://github.com/puppetlabs/puppetlabs-concat",
			RefKind: &hostv1.GitSource_Tag{Tag: "0.9.0"},
		}}},
		{Name: "profiles", Source: &hostv1.PuppetfileModule_Git{Git: &hostv1.GitSource{
			Url:           "git@git.example.com:puppet/profiles.git",
			RefKind:       &hostv1.GitSource_ControlBranch{ControlBranch: &hostv1.ControlBranch{}},
			DefaultBranch: "main",
		}}},
	}
	for _, m := range modules {
		if _, err := h.Code.PutPuppetfileModule(ctx, &hostv1.PutPuppetfileModuleRequest{Environment: "prod", Module: m}); err != nil {
			t.Fatalf("PutPuppetfileModule(%s): %v", m.GetName(), err)
		}
	}

	want := `moduledir 'thirdparty'

mod 'puppetlabs/ntp'
mod 'puppetlabs/apache', '0.10.0'
mod 'puppetlabs/stdlib', :latest
mod 'apache',
  :git => 'https://github.com/puppetlabs/puppetlabs-apache'
mod 'concat',
  :git => 'https://github.com/puppetlabs/puppetlabs-concat',
  :tag => '0.9.0'
mod 'profiles',
  :git => 'git@git.example.com:puppet/profiles.git',
  :branch => :control_branch,
  :default_branch => 'main'
`

	rendered, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: "prod"})
	if err != nil {
		t.Fatalf("RenderPuppetfile: %v", err)
	}
	if rendered.Text != want {
		t.Fatalf("render mismatch:\ngot:\n%s\nwant:\n%s", rendered.Text, want)
	}
}
