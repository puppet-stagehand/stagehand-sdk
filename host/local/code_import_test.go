package local_test

// End-to-end tests for the Code facet's import flow: InspectImport reports and
// writes nothing, ProposeImport freezes the reviewed snapshot into one
// approval proposal, and ApplyImport materializes it. Every test runs against
// the in-memory GitFixture, so none needs a network or a git binary.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/puppet-stagehand/stagehand-sdk/code"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

const importURL = "https://git.example.test/org/control-repo.git"

// allImportPerms is every permission the end-to-end tests need: the two Code
// grants, Secrets for a sealed credential, and tokens to mint an approver.
var allImportPerms = []string{"code:rw", "code:import", "documents:rw", "secrets:rw", "tokens:issue"}

// tree is a branch's files, path to content: the inline way a fixture remote
// is declared.
type tree = map[string]string

// canonicalTree is a small but complete control-repo branch: a Puppetfile, a
// version 5 hiera.yaml, one data file and an environment.conf.
func canonicalTree() tree {
	return tree{
		"Puppetfile":       "mod 'puppetlabs-stdlib', '9.0.0'\n",
		"hiera.yaml":       "---\nversion: 5\ndefaults:\n  datadir: data\n  data_hash: yaml_data\nhierarchy:\n  - name: \"Common\"\n    path: \"common.yaml\"\n",
		"data/common.yaml": "---\nmotd: hello\nntp::servers:\n  - 0.pool.ntp.org\n",
		"environment.conf": "modulepath = site-modules:modules:$basemodulepath\nenvironment_timeout = unlimited\n",
	}
}

// withFile returns a copy of t with one more file.
func withFile(t tree, path, content string) tree {
	out := tree{}
	for k, v := range t {
		out[k] = v
	}
	out[path] = content
	return out
}

func newImportHost(t *testing.T, trees map[string]tree, perms ...string) (*host.Host, *local.GitFixture) {
	t.Helper()
	conv := make(map[string]map[string]string, len(trees))
	for k, v := range trees {
		conv[k] = v
	}
	fx := local.NewGitFixture(conv)
	if len(perms) == 0 {
		perms = allImportPerms
	}
	return local.New(perms, "controlrepo", local.WithGitClient(fx)), fx
}

var codeCollections = []string{
	"code-environments", "code-puppetfiles", "code-hiera-hierarchy", "code-hiera-data",
	code.OverwriteCollection, "code-overwrite-applied",
}

// dumpStore reads every Code-owned collection, documents with their versions
// and timestamps, so two dumps can be compared for byte-identity.
func dumpStore(t *testing.T, h *host.Host) map[string][]*hostv1.Document {
	t.Helper()
	out := map[string][]*hostv1.Document{}
	for _, c := range codeCollections {
		resp, err := h.Documents.List(context.Background(), &hostv1.ListDocumentsRequest{Collection: c})
		if err != nil {
			t.Fatalf("List(%s): %v", c, err)
		}
		out[c] = resp.Documents
	}
	return out
}

func storesEqual(a, b map[string][]*hostv1.Document) bool {
	for _, c := range codeCollections {
		if len(a[c]) != len(b[c]) {
			return false
		}
		for i := range a[c] {
			if !proto.Equal(a[c][i], b[c][i]) {
				return false
			}
		}
	}
	return true
}

func inspectImport(h *host.Host, branches ...string) (*hostv1.ImportSnapshot, error) {
	resp, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Branches: branches})
	if err != nil {
		return nil, err
	}
	return resp.Snapshot, nil
}

func mustInspect(t *testing.T, h *host.Host, branches ...string) *hostv1.ImportSnapshot {
	t.Helper()
	s, err := inspectImport(h, branches...)
	if err != nil {
		t.Fatalf("InspectImport: %v", err)
	}
	return s
}

func proposeImport(h *host.Host, id string, branches ...string) (*hostv1.ProposeImportResponse, error) {
	return h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{ProposalId: id, Url: importURL, Branches: branches})
}

func mustPropose(t *testing.T, h *host.Host, id string, branches ...string) *hostv1.ProposeImportResponse {
	t.Helper()
	resp, err := proposeImport(h, id, branches...)
	if err != nil {
		t.Fatalf("ProposeImport(%s): %v", id, err)
	}
	return resp
}

func applyImport(h *host.Host, id string) (*hostv1.ApplyImportResponse, error) {
	return h.Code.ApplyImport(context.Background(), &hostv1.ApplyImportRequest{ProposalId: id})
}

func mustApply(t *testing.T, h *host.Host, id string) *hostv1.ApplyImportResponse {
	t.Helper()
	resp, err := applyImport(h, id)
	if err != nil {
		t.Fatalf("ApplyImport(%s): %v", id, err)
	}
	return resp
}

func envNames(resp *hostv1.ApplyImportResponse) []string {
	var out []string
	for _, e := range resp.Environments {
		out = append(out, e.Name)
	}
	return out
}

func branchByName(s *hostv1.ImportSnapshot, name string) *hostv1.ImportBranchSnapshot {
	for _, b := range s.GetBranches() {
		if b.Branch == name {
			return b
		}
	}
	return nil
}

func errorSeverity(findings []*hostv1.ImportFinding) []*hostv1.ImportFinding {
	var out []*hostv1.ImportFinding
	for _, f := range findings {
		if f.Severity == hostv1.ImportFinding_ERROR {
			out = append(out, f)
		}
	}
	return out
}

func storeCredential(t *testing.T, h *host.Host, name, payload string) {
	t.Helper()
	if _, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{Name: name, Plaintext: []byte(payload)}); err != nil {
		t.Fatalf("Secrets.Store(%s): %v", name, err)
	}
}

// assertReadable asserts that every Code read RPC works for env and that the
// Puppetfile re-renders and re-parses to an equal model.
func assertReadable(t *testing.T, h *host.Host, env string, wantModules int, wantDataPaths []string) {
	t.Helper()
	ctx := context.Background()
	mods, err := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: env})
	if err != nil {
		t.Fatalf("ListPuppetfileModules(%s): %v", env, err)
	}
	if len(mods.Modules) != wantModules {
		t.Fatalf("ListPuppetfileModules(%s) = %d modules, want %d", env, len(mods.Modules), wantModules)
	}
	if _, err := h.Code.GetHieraHierarchy(ctx, &hostv1.GetHieraHierarchyRequest{Environment: env}); err != nil {
		t.Fatalf("GetHieraHierarchy(%s): %v", env, err)
	}
	list, err := h.Code.ListHieraDataFiles(ctx, &hostv1.ListHieraDataFilesRequest{Environment: env})
	if err != nil {
		t.Fatalf("ListHieraDataFiles(%s): %v", env, err)
	}
	if strings.Join(list.Paths, ",") != strings.Join(wantDataPaths, ",") {
		t.Fatalf("ListHieraDataFiles(%s) = %v, want %v", env, list.Paths, wantDataPaths)
	}
	for _, p := range list.Paths {
		if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: env, Path: p}); err != nil {
			t.Fatalf("GetHieraDataFile(%s, %s): %v", env, p, err)
		}
	}
	if _, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: env}); err != nil {
		t.Fatalf("GetEnvironmentSettings(%s): %v", env, err)
	}
	rendered, err := h.Code.RenderPuppetfile(ctx, &hostv1.RenderPuppetfileRequest{Environment: env})
	if err != nil {
		t.Fatalf("RenderPuppetfile(%s): %v", env, err)
	}
	pf, err := code.ParsePuppetfile(rendered.Text)
	if err != nil {
		t.Fatalf("ParsePuppetfile(rendered %s): %v", env, err)
	}
	again, err := code.RenderPuppetfile(pf)
	if err != nil {
		t.Fatalf("RenderPuppetfile(model %s): %v", env, err)
	}
	pf2, err := code.ParsePuppetfile(again)
	if err != nil {
		t.Fatalf("ParsePuppetfile(re-rendered %s): %v", env, err)
	}
	if !proto.Equal(pf, pf2) {
		t.Fatalf("Puppetfile for %s does not re-parse equal after a re-render", env)
	}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %v (%v), want %v", got, err, want)
	}
}

// ------------------------------------------------------------ Task 1 tracer

// TestImport_Tracer_AdoptOneBranch walks the whole sanctioned path for one
// branch: inspect (nothing written), propose (one frozen proposal), apply
// refused before approval, approve, apply, read it back through the Code
// facet's own RPCs. The credential is deleted before Apply to prove Apply
// needs none.
func TestImport_Tracer_AdoptOneBranch(t *testing.T) {
	h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
	storeCredential(t, h, "deploy", `{"kind":"https_token","username":"deploy","token":"tok-secret"}`)

	var snap *hostv1.ImportSnapshot
	t.Run("inspect reports the branch with its parsed content", func(t *testing.T) {
		resp, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Credential: "deploy"})
		if err != nil {
			t.Fatalf("InspectImport: %v", err)
		}
		snap = resp.Snapshot
		if len(snap.Branches) != 1 {
			t.Fatalf("branches = %d, want 1", len(snap.Branches))
		}
		b := snap.Branches[0]
		if b.Branch != "production" || !b.Importable || b.WillOverwrite {
			t.Fatalf("branch = %+v, want importable, no overwrite", b)
		}
		if len(b.Commit) != 40 {
			t.Fatalf("commit = %q, want a 40-hex SHA", b.Commit)
		}
		if !strings.Contains(b.PuppetfileText, "puppetlabs-stdlib") || b.HieraYaml == "" ||
			len(b.DataFiles) != 1 || b.Settings == nil || b.Settings.GetModulepath() == "" {
			t.Fatalf("parsed content missing: %+v", b)
		}
		if e := errorSeverity(b.Findings); len(e) != 0 {
			t.Fatalf("unexpected error findings: %v", e)
		}
		remote := fx.LastRemote()
		if remote.URL != importURL || remote.Credential == nil || remote.Credential.Token != "tok-secret" {
			t.Fatalf("the fixture was not handed the resolved credential: %+v", remote)
		}
	})

	t.Run("inspect writes nothing", func(t *testing.T) {
		before := dumpStore(t, h)
		mustInspect(t, h)
		if !storesEqual(before, dumpStore(t, h)) {
			t.Fatal("InspectImport changed the Documents store")
		}
	})

	var proposed *hostv1.ProposeImportResponse
	t.Run("propose files one pending frozen proposal", func(t *testing.T) {
		resp, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{ProposalId: "imp-1", Url: importURL, Credential: "deploy"})
		if err != nil {
			t.Fatalf("ProposeImport: %v", err)
		}
		proposed = resp
		docs, err := h.Documents.List(context.Background(), &hostv1.ListDocumentsRequest{Collection: code.OverwriteCollection})
		if err != nil {
			t.Fatal(err)
		}
		if len(docs.Documents) != 1 {
			t.Fatalf("proposals = %d, want 1", len(docs.Documents))
		}
		body := docs.Documents[0].Body.Value.AsMap()
		if body["status"] != "pending" {
			t.Fatalf("status = %v, want pending", body["status"])
		}
		target, err := code.ParseOverwriteTarget(body)
		if err != nil {
			t.Fatalf("ParseOverwriteTarget: %v", err)
		}
		if target.Resource != code.OverwriteResourceImport || target.Source != importURL || target.Name != "production" {
			t.Fatalf("target = %+v", target)
		}
		got, err := code.OverwritePayloadImport(body)
		if err != nil {
			t.Fatalf("OverwritePayloadImport: %v", err)
		}
		if !proto.Equal(got, resp.Snapshot) {
			t.Fatalf("stored payload differs from the returned snapshot:\n got %v\nwant %v", got, resp.Snapshot)
		}
		if resp.ProposalId != "imp-1" {
			t.Fatalf("proposal id = %q", resp.ProposalId)
		}
	})

	t.Run("apply before approval is refused and writes nothing", func(t *testing.T) {
		before := dumpStore(t, h)
		_, err := applyImport(h, "imp-1")
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "not approved") {
			t.Fatalf("error = %v, want it to say the proposal is not approved", err)
		}
		if !storesEqual(before, dumpStore(t, h)) {
			t.Fatal("a refused ApplyImport changed the store")
		}
		if _, err := h.Code.GetEnvironment(context.Background(), &hostv1.GetEnvironmentRequest{Name: "production"}); status.Code(err) != codes.NotFound {
			t.Fatalf("environment exists before approval: %v", err)
		}
	})

	t.Run("apply after approval needs no network and no credential", func(t *testing.T) {
		approveOverwrite(t, h, "imp-1")
		if _, err := h.Secrets.Delete(context.Background(), &hostv1.SecretRef{Ref: "expansion/controlrepo/deploy"}); err != nil {
			t.Fatalf("Secrets.Delete: %v", err)
		}
		opensBefore, listedBefore := fx.Opens(), fx.Listed()
		resp := mustApply(t, h, "imp-1")
		if strings.Join(envNames(resp), ",") != "production" {
			t.Fatalf("environments = %v, want [production]", envNames(resp))
		}
		if fx.Opens() != opensBefore || fx.Listed() != listedBefore {
			t.Fatalf("ApplyImport touched the git client (opens %d->%d, lists %d->%d)", opensBefore, fx.Opens(), listedBefore, fx.Listed())
		}
		_ = proposed
	})

	t.Run("every Code read RPC works for the imported environment", func(t *testing.T) {
		assertReadable(t, h, "production", 1, []string{"common.yaml"})
		s, err := h.Code.GetEnvironmentSettings(context.Background(), &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
		if err != nil {
			t.Fatal(err)
		}
		if s.GetModulepath() != "site-modules:modules:$basemodulepath" || s.GetEnvironmentTimeout() != "unlimited" {
			t.Fatalf("settings = %v", s)
		}
	})
}

func TestImport_InspectWritesNothing(t *testing.T) {
	h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
	mustCreateEnv(t, h, "staging")
	before := dumpStore(t, h)
	snap := mustInspect(t, h)
	if len(snap.Branches) != 2 {
		t.Fatalf("branches = %d, want 2", len(snap.Branches))
	}
	if !storesEqual(before, dumpStore(t, h)) {
		t.Fatal("InspectImport changed the Documents store")
	}
	if b := branchByName(snap, "staging"); b == nil || !b.WillOverwrite {
		t.Fatalf("staging should be marked will_overwrite: %+v", b)
	}
	if b := branchByName(snap, "production"); b == nil || b.WillOverwrite {
		t.Fatalf("production should not be marked will_overwrite: %+v", b)
	}
}

func TestImport_ImportedIsReadable(t *testing.T) {
	prod := canonicalTree()
	staging := withFile(canonicalTree(), "data/staging_only.yaml", "---\nonly: staging\n")
	staging["Puppetfile"] = "mod 'puppetlabs-stdlib', '9.0.0'\nmod 'puppetlabs-apache', '12.0.0'\n"
	h, _ := newImportHost(t, map[string]tree{"production": prod, "staging": staging})
	mustPropose(t, h, "imp-2")
	approveOverwrite(t, h, "imp-2")
	resp := mustApply(t, h, "imp-2")
	if strings.Join(envNames(resp), ",") != "production,staging" {
		t.Fatalf("environments = %v", envNames(resp))
	}
	assertReadable(t, h, "production", 1, []string{"common.yaml"})
	assertReadable(t, h, "staging", 2, []string{"common.yaml", "staging_only.yaml"})
}

// TestImport_Tracer_Permissions covers the two permission directions for each
// import RPC and that neither refusal reaches the git client. The full table,
// with the fully-permitted row, is TestImport_Permissions in code_test.go.
func TestImport_Tracer_Permissions(t *testing.T) {
	calls := map[string]func(h *host.Host) error{
		"InspectImport": func(h *host.Host) error { _, err := inspectImport(h); return err },
		"ProposeImport": func(h *host.Host) error { _, err := proposeImport(h, "p"); return err },
		"ApplyImport":   func(h *host.Host) error { _, err := applyImport(h, "p"); return err },
	}
	cases := []struct {
		name  string
		perms []string
		names string
	}{
		{"no code:rw", []string{"code:import"}, "code:rw"},
		{"no code:import", []string{"code:rw"}, "code:import"},
	}
	for rpc, call := range calls {
		for _, c := range cases {
			t.Run(rpc+"/"+c.name, func(t *testing.T) {
				h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()}, c.perms...)
				err := call(h)
				wantCode(t, err, codes.PermissionDenied)
				d := errorDetail(err)
				if d == nil || d.Code != "facet_not_declared" || !strings.Contains(d.Message, fmt.Sprintf("%q", c.names)) {
					t.Fatalf("detail = %+v, want facet_not_declared naming %s", d, c.names)
				}
				if fx.Listed() != 0 || fx.Opens() != 0 {
					t.Fatal("a refused call reached the git client")
				}
			})
		}
	}
}
