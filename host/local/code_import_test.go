package local_test

// End-to-end tests for the Code facet's import flow: InspectImport reports and
// writes nothing, ProposeImport freezes the reviewed snapshot into one
// approval proposal, and ApplyImport materializes it. Every test runs against
// the in-memory GitFixture, so none needs a network or a git binary.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"sync"
	"testing"
	"time"

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

// ------------------------------------------------------------ Task 2 matrix

// mixedRemote is a remote with one importable branch and four whose names
// violate ^[a-z0-9_]+$.
func mixedRemote() map[string]tree {
	return map[string]tree{
		"production": canonicalTree(),
		"feature-x":  canonicalTree(),
		"Production": canonicalTree(),
		"prod.1":     canonicalTree(),
		"with space": canonicalTree(),
	}
}

func TestImport_Inspect(t *testing.T) {
	t.Run("a branch violating the name rule is reported, explained and never fetched", func(t *testing.T) {
		h, fx := newImportHost(t, mixedRemote())
		snap := mustInspect(t, h)
		if len(snap.Branches) != 5 {
			t.Fatalf("branches = %d, want 5", len(snap.Branches))
		}
		for _, name := range []string{"feature-x", "Production", "prod.1", "with space"} {
			b := branchByName(snap, name)
			if b == nil {
				t.Fatalf("branch %q missing from the report", name)
			}
			if b.Importable || b.WillOverwrite {
				t.Fatalf("%q must not be importable: %+v", name, b)
			}
			if len(b.Findings) != 1 || b.Findings[0].Kind != code.FindingBranchNameInvalid || b.Findings[0].Severity != hostv1.ImportFinding_ERROR {
				t.Fatalf("%q findings = %v, want one branch_name_invalid error", name, b.Findings)
			}
			msg := b.Findings[0].Message
			for _, want := range []string{"^[a-z0-9_]+$", "lowercase letters, digits and underscore", "r10k", "corrected name"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("%q message %q does not mention %q", name, msg, want)
				}
			}
			if b.Commit != "" || b.PuppetfileText != "" {
				t.Fatalf("%q was analysed: %+v", name, b)
			}
		}
		if opened := fx.OpenedBranches(); strings.Join(opened, ",") != "production" {
			t.Fatalf("opened branches = %v, want only [production]: a refused name must never be fetched", opened)
		}
		if b := branchByName(snap, "production"); b == nil || !b.Importable {
			t.Fatalf("production should be importable: %+v", b)
		}
	})

	t.Run("a remote of only invalid names fetches nothing at all", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"feature-x": canonicalTree()})
		mustInspect(t, h)
		if fx.Opens() != 0 {
			t.Fatal("Open was called although no branch was importable")
		}
	})

	t.Run("the git client's branch cap refusal passes through and nothing is fetched", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		fx.SetListErr(status.Error(codes.FailedPrecondition, "git remote has more branches than the limit of 100"))
		_, err := inspectImport(h)
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "more branches than the limit of 100") {
			t.Fatalf("error = %v, want the client's cap message unchanged", err)
		}
		if fx.Opens() != 0 {
			t.Fatal("a capped remote was fetched")
		}
	})

	t.Run("an unknown credential is NotFound naming the credential and no value", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		_, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Credential: "no-such-cred"})
		wantCode(t, err, codes.NotFound)
		if !strings.Contains(err.Error(), "no-such-cred") {
			t.Fatalf("error = %v, want it to name the credential", err)
		}
		if fx.Listed() != 0 {
			t.Fatal("the remote was contacted without a usable credential")
		}
		_, err = h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{ProposalId: "p", Url: importURL, Credential: "no-such-cred"})
		wantCode(t, err, codes.NotFound)
	})

	t.Run("a malformed credential is Internal naming the field and no value", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		storeCredential(t, h, "broken", `{"kind":"https_token","username":"u","token":""}`)
		_, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Credential: "broken"})
		wantCode(t, err, codes.Internal)
		if !strings.Contains(err.Error(), `"broken"`) || !strings.Contains(err.Error(), `"token"`) {
			t.Fatalf("error = %v, want the credential and the broken field named", err)
		}
	})

	t.Run("an unreachable remote is Unavailable without the URL", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		fx.SetListErr(status.Error(codes.Unavailable, "git remote could not be reached"))
		_, err := inspectImport(h)
		wantCode(t, err, codes.Unavailable)
		if strings.Contains(err.Error(), "git.example.test") {
			t.Fatalf("error %q echoes the URL", err)
		}
	})

	t.Run("a URL outside the scheme allowlist is InvalidArgument without the URL", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		for _, bad := range []string{"http://secret-host.example/x.git", "file:///etc/passwd", "ext::sh -c id", "git://secret-host.example/x"} {
			_, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: bad})
			wantCode(t, err, codes.InvalidArgument)
			if strings.Contains(err.Error(), "secret-host") || strings.Contains(err.Error(), "passwd") {
				t.Fatalf("error %q echoes the URL", err)
			}
		}
		if fx.Listed() != 0 {
			t.Fatal("a refused URL reached the git client")
		}
	})

	t.Run("a branch filter naming an unknown branch is InvalidArgument", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		_, err := inspectImport(h, "production", "ghost")
		wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("error = %v, want it to name the unknown branch", err)
		}
	})
}

func TestImport_Propose(t *testing.T) {
	proposals := func(t *testing.T, h *host.Host) int {
		t.Helper()
		docs, err := h.Documents.List(context.Background(), &hostv1.ListDocumentsRequest{Collection: code.OverwriteCollection})
		if err != nil {
			t.Fatal(err)
		}
		return len(docs.Documents)
	}

	t.Run("an explicit branch the remote does not have is InvalidArgument", func(t *testing.T) {
		h, _ := newImportHost(t, mixedRemote())
		_, err := proposeImport(h, "p1", "production", "ghost")
		wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("error = %v, want it to name the branch", err)
		}
		if proposals(t, h) != 0 {
			t.Fatal("a refused ProposeImport filed a proposal")
		}
	})

	t.Run("an explicit branch that is not importable is InvalidArgument", func(t *testing.T) {
		h, fx := newImportHost(t, mixedRemote())
		_, err := proposeImport(h, "p1", "production", "feature-x")
		wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(err.Error(), "feature-x") {
			t.Fatalf("error = %v, want it to name the branch", err)
		}
		if proposals(t, h) != 0 {
			t.Fatal("a refused ProposeImport filed a proposal")
		}
		if strings.Contains(strings.Join(fx.OpenedBranches(), ","), "feature-x") {
			t.Fatal("a refused name was fetched")
		}
	})

	t.Run("a remote whose every branch is unimportable is FailedPrecondition", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"feature-x": canonicalTree(), "Prod": canonicalTree()})
		_, err := proposeImport(h, "p1")
		wantCode(t, err, codes.FailedPrecondition)
		if proposals(t, h) != 0 {
			t.Fatal("a refused ProposeImport filed a proposal")
		}
	})

	t.Run("an empty list selects every importable branch and keeps the refused ones' findings", func(t *testing.T) {
		trees := mixedRemote()
		trees["staging"] = canonicalTree()
		h, _ := newImportHost(t, trees)
		resp := mustPropose(t, h, "p1")
		for _, name := range []string{"production", "staging"} {
			if b := branchByName(resp.Snapshot, name); b == nil || !b.Importable {
				t.Fatalf("%s should be in the frozen snapshot as importable: %+v", name, b)
			}
		}
		fx := branchByName(resp.Snapshot, "feature-x")
		if fx == nil || fx.Importable || len(fx.Findings) != 1 {
			t.Fatalf("feature-x should be carried as not importable with its finding: %+v", fx)
		}
		approveOverwrite(t, h, "p1")
		applied := mustApply(t, h, "p1")
		if strings.Join(envNames(applied), ",") != "production,staging" {
			t.Fatalf("applied environments = %v, want only the importable ones", envNames(applied))
		}
		if _, err := h.Code.GetEnvironment(context.Background(), &hostv1.GetEnvironmentRequest{Name: "feature-x"}); status.Code(err) != codes.NotFound {
			t.Fatalf("a refused branch became an environment: %v", err)
		}
	})

	t.Run("an explicit subset is the only thing fetched and frozen", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
		resp := mustPropose(t, h, "p1", "staging")
		if len(resp.Snapshot.Branches) != 1 || resp.Snapshot.Branches[0].Branch != "staging" {
			t.Fatalf("frozen branches = %v, want only staging", resp.Snapshot.Branches)
		}
		if strings.Join(fx.OpenedBranches(), ",") != "staging" {
			t.Fatalf("opened = %v, want only staging", fx.OpenedBranches())
		}
	})

	t.Run("an expected_commits mismatch is import_branch_moved and files nothing", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		report := mustInspect(t, h)
		fx.SetCommit("production", strings.Repeat("a", 40))
		_, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{
			ProposalId: "p1", Url: importURL,
			ExpectedCommits: map[string]string{"production": report.Branches[0].Commit},
		})
		wantCode(t, err, codes.FailedPrecondition)
		if !local.IsCodeImportBranchMoved(err) {
			t.Fatalf("error = %v, want IsCodeImportBranchMoved", err)
		}
		if d := errorDetail(err); d == nil || d.Code != "import_branch_moved" {
			t.Fatalf("detail = %+v, want import_branch_moved", d)
		}
		if strings.Contains(err.Error(), "git.example.test") {
			t.Fatalf("error %q echoes the URL", err)
		}
		if proposals(t, h) != 0 {
			t.Fatal("a moved branch still filed a proposal")
		}
	})

	t.Run("an expected_commits map matching every selected branch succeeds", func(t *testing.T) {
		h, _ := newImportHost(t, mixedRemote())
		report := mustInspect(t, h)
		resp, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{
			ProposalId: "p1", Url: importURL,
			ExpectedCommits: map[string]string{"production": branchByName(report, "production").Commit},
		})
		if err != nil {
			t.Fatalf("ProposeImport: %v", err)
		}
		if resp.ProposalId != "p1" {
			t.Fatalf("proposal id = %q", resp.ProposalId)
		}
	})

	t.Run("an expected_commits map that omits a selected branch is InvalidArgument", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
		report := mustInspect(t, h)
		_, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{
			ProposalId: "p1", Url: importURL,
			ExpectedCommits: map[string]string{"production": branchByName(report, "production").Commit},
		})
		wantCode(t, err, codes.InvalidArgument)
		if !strings.Contains(err.Error(), "staging") {
			t.Fatalf("error = %v, want it to name the unpinned branch", err)
		}
	})

	t.Run("a missing proposal id is InvalidArgument", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		_, err := proposeImport(h, "")
		wantCode(t, err, codes.InvalidArgument)
	})
}

// seedOldEnvironment gives env a Puppetfile, hierarchy, two data files and
// settings through the Documents facet, plus a data file the import's snapshot
// will not contain.
func seedOldEnvironment(t *testing.T, h *host.Host, env string) {
	t.Helper()
	seedFullEnvironment(t, h, context.Background(), env)
}

func TestImport_Overwrite(t *testing.T) {
	ctx := context.Background()

	t.Run("a flagged overwrite replaces every owned document and leaves no stale data file", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		seedOldEnvironment(t, h, "production") // owns common.yaml, nodes/web01.yaml, apache module, settings
		report := mustInspect(t, h)
		if b := branchByName(report, "production"); !b.WillOverwrite {
			t.Fatal("the report did not flag the existing environment as an overwrite")
		}
		resp := mustPropose(t, h, "ow-1")
		if !branchByName(resp.Snapshot, "production").WillOverwrite {
			t.Fatal("the frozen snapshot did not record the overwrite")
		}
		approveOverwrite(t, h, "ow-1")
		mustApply(t, h, "ow-1")

		// Read back only through the facet's own RPCs: the old nodes/web01.yaml
		// and the old apache module must be gone, the imported content present.
		assertReadable(t, h, "production", 1, []string{"common.yaml"})
		mods, _ := h.Code.ListPuppetfileModules(ctx, &hostv1.ListPuppetfileModulesRequest{Environment: "production"})
		if mods.Modules[0].Name != "stdlib" && mods.Modules[0].Name != "puppetlabs-stdlib" {
			t.Fatalf("module = %q, want the imported stdlib", mods.Modules[0].Name)
		}
		if _, err := h.Code.GetHieraDataFile(ctx, &hostv1.GetHieraDataFileRequest{Environment: "production", Path: "nodes/web01.yaml"}); status.Code(err) != codes.NotFound {
			t.Fatalf("a data file only the old environment had is still readable: %v", err)
		}
		s, err := h.Code.GetEnvironmentSettings(ctx, &hostv1.GetEnvironmentSettingsRequest{Environment: "production"})
		if err != nil || s.GetModulepath() != "site-modules:modules:$basemodulepath" {
			t.Fatalf("settings = %v, %v; want the imported modulepath", s, err)
		}
	})

	t.Run("a collision on a branch the snapshot did not flag refuses the whole import", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
		mustPropose(t, h, "ow-2")
		approveOverwrite(t, h, "ow-2")
		mustCreateEnv(t, h, "staging") // appears after the proposal was filed
		before := dumpStore(t, h)

		_, err := applyImport(h, "ow-2")
		wantCode(t, err, codes.FailedPrecondition)
		if !local.IsCodeImportUnflaggedCollision(err) {
			t.Fatalf("error = %v, want IsCodeImportUnflaggedCollision", err)
		}
		if d := errorDetail(err); d == nil || d.Code != "import_unflagged_collision" {
			t.Fatalf("detail = %+v, want import_unflagged_collision", d)
		}
		if !storesEqual(before, dumpStore(t, h)) {
			t.Fatal("a refused ApplyImport changed the store")
		}
		if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"}); status.Code(err) != codes.NotFound {
			t.Fatalf("production was created although the whole import must be refused: %v", err)
		}
	})

	t.Run("a flagged overwrite whose environment has since been deleted is simply created", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		seedOldEnvironment(t, h, "production")
		mustPropose(t, h, "ow-3")
		approveOverwrite(t, h, "ow-3")
		if _, err := h.Code.DeleteEnvironment(ctx, &hostv1.DeleteEnvironmentRequest{Name: "production"}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, h, "ow-3")
		assertReadable(t, h, "production", 1, []string{"common.yaml"})
	})
}

// ------------------------------------------------------------ Task 3 proofs

// frozenPuppetfile reads production's stored Puppetfile text straight through
// the facet.
func renderedPuppetfile(t *testing.T, h *host.Host, env string) string {
	t.Helper()
	r, err := h.Code.RenderPuppetfile(context.Background(), &hostv1.RenderPuppetfileRequest{Environment: env})
	if err != nil {
		t.Fatalf("RenderPuppetfile(%s): %v", env, err)
	}
	return r.Text
}

func proposalDoc(t *testing.T, h *host.Host, id string) *hostv1.Document {
	t.Helper()
	d, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: code.OverwriteCollection, DocId: id})
	if err != nil {
		t.Fatalf("reading proposal %s: %v", id, err)
	}
	return d
}

// rewriteProposal replaces a pending proposal's snapshot with mutate(snapshot),
// keeping it pending. A frozen snapshot is the only place an invalid branch can
// be injected once the parsers refuse to produce one.
func rewriteProposal(t *testing.T, h *host.Host, id string, mutate func(*hostv1.ImportSnapshot), extra map[string]any) {
	t.Helper()
	d := proposalDoc(t, h, id)
	snap, err := code.OverwritePayloadImport(d.Body.Value.AsMap())
	if err != nil {
		t.Fatalf("OverwritePayloadImport: %v", err)
	}
	mutate(snap)
	body, err := code.OverwriteBodyForImport(snap)
	if err != nil {
		t.Fatalf("OverwriteBodyForImport: %v", err)
	}
	body["status"] = "pending"
	for k, v := range extra {
		body[k] = v
	}
	if _, err := h.Documents.Put(context.Background(), &hostv1.PutDocumentRequest{
		Collection: code.OverwriteCollection, DocId: id,
		Body: &hostv1.Json{Value: mustStruct(t, body)}, IfVersion: d.Version,
	}); err != nil {
		t.Fatalf("rewriting proposal %s: %v", id, err)
	}
}

// seedProposalStatus forges a proposal's status through the operator seeding
// path: it reads the stored body, sets status, and writes it back with
// local.SeedDocument. The pack-facing Documents facet refuses this write
// (FND-03), so a test that needs a status-only record with no approval
// provenance has to build it where the console would, not where a pack can.
func seedProposalStatus(t *testing.T, h *host.Host, id, status string) {
	t.Helper()
	body := proposalDoc(t, h, id).Body.Value.AsMap()
	body["status"] = status
	if err := local.SeedDocument(h, code.OverwriteCollection, id, body); err != nil {
		t.Fatalf("seeding proposal %s as %s: %v", id, status, err)
	}
}

func TestImport_ApplyIsFrozen(t *testing.T) {
	t.Run("a push to the remote after Propose does not change what Apply writes", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		resp := mustPropose(t, h, "fz-1")
		frozenText := branchByName(resp.Snapshot, "production").PuppetfileText
		fx.SetBranch("production", withFile(canonicalTree(), "Puppetfile", "mod 'puppetlabs-apache', '12.0.0'\n"))
		approveOverwrite(t, h, "fz-1")
		mustApply(t, h, "fz-1")
		if got := renderedPuppetfile(t, h, "production"); got != frozenText || strings.Contains(got, "apache") {
			t.Fatalf("applied Puppetfile = %q, want exactly the frozen %q", got, frozenText)
		}
	})

	t.Run("deleting the branch from the remote does not prevent Apply", func(t *testing.T) {
		h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
		mustPropose(t, h, "fz-2")
		fx.DeleteBranch("production")
		approveOverwrite(t, h, "fz-2")
		mustApply(t, h, "fz-2")
		assertReadable(t, h, "production", 1, []string{"common.yaml"})
	})
}

func TestImport_Apply(t *testing.T) {
	ctx := context.Background()

	t.Run("a repeat Apply with nothing changed is idempotent and writes nothing", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
		mustPropose(t, h, "ap-1")
		approveOverwrite(t, h, "ap-1")
		first := mustApply(t, h, "ap-1")
		after := dumpStore(t, h)
		second := mustApply(t, h, "ap-1")
		if strings.Join(envNames(first), ",") != strings.Join(envNames(second), ",") {
			t.Fatalf("environments differ between calls: %v vs %v", envNames(first), envNames(second))
		}
		if !storesEqual(after, dumpStore(t, h)) {
			t.Fatal("an idempotent repeat Apply changed the stored documents or their versions")
		}
	})

	t.Run("after the content drifts a repeat Apply is refused as already applied", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		mustPropose(t, h, "ap-2")
		approveOverwrite(t, h, "ap-2")
		mustApply(t, h, "ap-2")
		// A new module is an ungated create, so this is a legitimate later edit.
		mustPutModule(t, h, "production", forgeModule("puppetlabs-apache", "12.0.0"))
		_, err := applyImport(h, "ap-2")
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "already applied") {
			t.Fatalf("error = %v, want the already-applied refusal", err)
		}
		if got := listModuleNames(t, h, "production"); len(got) != 2 {
			t.Fatalf("modules = %v: the edit must survive the refused Apply", got)
		}
	})

	t.Run("Apply never writes into the proposal", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		mustPropose(t, h, "ap-3")
		approveOverwrite(t, h, "ap-3")
		before := proposalDoc(t, h, "ap-3")
		mustApply(t, h, "ap-3")
		mustApply(t, h, "ap-3")
		after := proposalDoc(t, h, "ap-3")
		if !proto.Equal(before, after) {
			t.Fatalf("the proposal document changed:\n before %v\n after  %v", before, after)
		}
		if after.Body.Value.AsMap()["status"] != "approved" {
			t.Fatalf("status = %v, want approved", after.Body.Value.AsMap()["status"])
		}
	})

	t.Run("a pending proposal and a rejected one write nothing", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		mustPropose(t, h, "ap-4")
		before := dumpStore(t, h)
		_, err := applyImport(h, "ap-4")
		wantCode(t, err, codes.FailedPrecondition)
		rejectOverwrite(t, h, "ap-4")
		before = dumpStore(t, h)
		_, err = applyImport(h, "ap-4")
		wantCode(t, err, codes.FailedPrecondition)
		if !storesEqual(before, dumpStore(t, h)) {
			t.Fatal("a refused ApplyImport changed the store")
		}
		if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"}); status.Code(err) != codes.NotFound {
			t.Fatalf("an unapproved proposal created an environment: %v", err)
		}
	})

	t.Run("an approved status with no recorded provenance is refused", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		mustPropose(t, h, "ap-5")
		// A pack with documents access cannot forge the status: the guard refuses
		// the write and the stored proposal is still pending.
		d := proposalDoc(t, h, "ap-5")
		forged := d.Body.Value.AsMap()
		forged["status"] = "approved"
		_, perr := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: code.OverwriteCollection, DocId: "ap-5",
			Body: &hostv1.Json{Value: mustStruct(t, forged)}, IfVersion: d.Version,
		})
		if !local.IsApprovalTransitionRefused(perr) {
			t.Fatalf("a direct Put of status approved: want IsApprovalTransitionRefused, got %v", perr)
		}
		if got := proposalDoc(t, h, "ap-5").Body.Value.AsMap()["status"]; got != "pending" {
			t.Fatalf("a refused forgery changed the stored status to %v", got)
		}
		// The operator path can still write a status-only record, with no
		// approved_scope or decided_by, and ApplyImport judges it by provenance.
		seedProposalStatus(t, h, "ap-5", "approved")
		_, err := applyImport(h, "ap-5")
		wantCode(t, err, codes.FailedPrecondition)
		if _, err := h.Code.GetEnvironment(ctx, &hostv1.GetEnvironmentRequest{Name: "production"}); status.Code(err) != codes.NotFound {
			t.Fatalf("a forged approval created an environment: %v", err)
		}
	})

	t.Run("an environment whose name is a prefix of another is not disturbed", func(t *testing.T) {
		h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
		seedOldEnvironment(t, h, "prod")
		seedOldEnvironment(t, h, "production")
		mustPropose(t, h, "ap-6")
		approveOverwrite(t, h, "ap-6")
		mustApply(t, h, "ap-6")
		assertEnvDocsPresent(t, h, ctx, "prod") // untouched, nested data file included
		assertReadable(t, h, "production", 1, []string{"common.yaml"})
	})
}

func TestImport_ApplyIsAtomic(t *testing.T) {
	h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
	mustPropose(t, h, "at-1")
	rewriteProposal(t, h, "at-1", func(s *hostv1.ImportSnapshot) {
		branchByName(s, "staging").PuppetfileText = "mod 'puppetlabs-stdlib', :git => \n((( not a puppetfile\n"
	}, nil)
	approveOverwrite(t, h, "at-1")
	before := dumpStore(t, h)

	_, err := applyImport(h, "at-1")
	if err == nil {
		t.Fatal("ApplyImport accepted a snapshot whose second branch is invalid")
	}
	wantCode(t, err, codes.FailedPrecondition)
	for _, env := range []string{"production", "staging"} {
		if _, err := h.Code.GetEnvironment(context.Background(), &hostv1.GetEnvironmentRequest{Name: env}); status.Code(err) != codes.NotFound {
			t.Fatalf("environment %s exists although one branch failed: all-or-none broken (%v)", env, err)
		}
	}
	if !storesEqual(before, dumpStore(t, h)) {
		t.Fatal("a refused ApplyImport changed the store")
	}
}

func TestImport_ApplyConcurrent(t *testing.T) {
	h, _ := newImportHost(t, map[string]tree{"production": canonicalTree(), "staging": canonicalTree()})
	mustPropose(t, h, "cc-1")
	approveOverwrite(t, h, "cc-1")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	resps := make([]*hostv1.ApplyImportResponse, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resps[i], errs[i] = applyImport(h, "cc-1")
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent ApplyImport #%d: %v", i, errs[i])
		}
		if strings.Join(envNames(resps[i]), ",") != "production,staging" {
			t.Fatalf("call #%d environments = %v", i, envNames(resps[i]))
		}
	}
	assertReadable(t, h, "production", 1, []string{"common.yaml"})
	assertReadable(t, h, "staging", 1, []string{"common.yaml"})
	ids, err := h.Documents.List(context.Background(), &hostv1.ListDocumentsRequest{Collection: "code-hiera-data"})
	if err != nil || len(ids.Documents) != 2 {
		t.Fatalf("hiera data documents = %v, %v; want exactly 2 (no duplicate)", len(ids.GetDocuments()), err)
	}
}

func TestImport_RevealsOnlyTheNamedCredential(t *testing.T) {
	h, _ := newImportHost(t, map[string]tree{"production": canonicalTree()})
	ref, err := h.Secrets.Store(context.Background(), &hostv1.StoreSecretRequest{
		Name: "deploy", Plaintext: []byte(`{"kind":"https_token","username":"u","token":"t"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second secret that must never be touched.
	storeCredential(t, h, "other", `{"kind":"https_token","username":"x","token":"y"}`)

	var mu sync.Mutex
	var revealed []string
	local.SetRevealHook(h, func(r string) { mu.Lock(); revealed = append(revealed, r); mu.Unlock() })

	if _, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Credential: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if len(revealed) != 1 || revealed[0] != ref.Ref {
		t.Fatalf("revealed = %v, want exactly [%s]", revealed, ref.Ref)
	}
	revealed = nil
	if _, err := inspectImport(h); err != nil {
		t.Fatal(err)
	}
	if len(revealed) != 0 {
		t.Fatalf("an anonymous inspect revealed %v", revealed)
	}
}

func TestImport_NoLockHeldAcrossNetwork(t *testing.T) {
	h, fx := newImportHost(t, map[string]tree{"production": canonicalTree()})
	storeCredential(t, h, "deploy", `{"kind":"https_token","username":"u","token":"t"}`)

	var probes int
	probe := func() {
		// Any Documents call takes docs.mu; if the import held it across the
		// network method this would block forever.
		_, _ = h.Documents.List(context.Background(), &hostv1.ListDocumentsRequest{Collection: "code-environments"})
		probes++
	}
	fx.OnNetwork(probe)
	local.SetRevealHook(h, func(string) { probe() })

	done := make(chan error, 1)
	go func() {
		if _, err := h.Code.InspectImport(context.Background(), &hostv1.InspectImportRequest{Url: importURL, Credential: "deploy"}); err != nil {
			done <- err
			return
		}
		_, err := h.Code.ProposeImport(context.Background(), &hostv1.ProposeImportRequest{ProposalId: "nl-1", Url: importURL, Credential: "deploy"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: docs.mu is held across a clone, fetch, reveal or proposal write")
	}
	if probes < 6 { // reveal + list + open, twice
		t.Fatalf("only %d network-time probes ran; the hooks are not wired", probes)
	}
}

// ------------------------------------------------------------ structure

// parseGoFile parses one source file of this package for the structural tests.
func parseGoFile(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return fset, f
}

func funcDecl(t *testing.T, f *ast.File, recv, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != name || fd.Body == nil {
			continue
		}
		if recv == "" && fd.Recv == nil {
			return fd
		}
		if recv != "" && fd.Recv != nil && len(fd.Recv.List) == 1 {
			if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok && id.Name == recv {
					return fd
				}
			}
		}
	}
	t.Fatalf("function %s.%s not found", recv, name)
	return nil
}

// selectors returns every selector expression under n, with its position.
func selectors(n ast.Node) []*ast.SelectorExpr {
	var out []*ast.SelectorExpr
	ast.Inspect(n, func(x ast.Node) bool {
		if se, ok := x.(*ast.SelectorExpr); ok {
			out = append(out, se)
		}
		return true
	})
	return out
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	}
	return ""
}

// callPositions returns the position of every call whose selector ends in sel.
func callPositions(n ast.Node, sel string) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(x ast.Node) bool {
		if c, ok := x.(*ast.CallExpr); ok {
			if se, ok := c.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == sel {
				out = append(out, c.Pos())
			}
		}
		return true
	})
	return out
}

// TestImport_CannotDecide is a structural test, not a behavioural one, because
// the property it asserts is the absence of a capability, and absence cannot be
// demonstrated by exercising behaviour. It parses the import controller and
// the git client files and fails on any selector naming the approval package's
// decide functions or the Auth facet's token verification, and on any reference
// to the governance keys a decision writes. Phase 9 asserts the approval
// scope's provenance the same way (approval's TestApprovalScopeSourcedFromKindOnly).
func TestImport_CannotDecide(t *testing.T) {
	files := []string{"code_import.go", "git_client.go", "git_exec.go", "git_exec_unix.go", "git_exec_other.go"}
	forbiddenIdents := map[string]bool{
		"overwriteStatusKey": true, "overwriteStatusApproved": true, "overwriteScopeKey": true, "overwriteDecidedByKey": true,
	}
	forbiddenStrings := map[string]bool{
		`"status"`: true, `"approved_scope"`: true, `"decided_by"`: true, `"decided_at"`: true, `"reason"`: true,
	}
	sawProposeBody := false
	for _, name := range files {
		fset, f := parseGoFile(t, name)
		for _, se := range selectors(f) {
			if id, ok := se.X.(*ast.Ident); ok && id.Name == "approval" {
				switch se.Sel.Name {
				case "Approve", "Reject":
					t.Errorf("%s: %s references approval.%s, which decides a proposal", name, fset.Position(se.Pos()), se.Sel.Name)
				case "ProposeBody":
					sawProposeBody = true
				}
			}
			if se.Sel.Name == "Verify" {
				t.Errorf("%s: %s references .Verify, the Auth facet's token verification", name, fset.Position(se.Pos()))
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if forbiddenIdents[v.Name] {
					t.Errorf("%s: %s references governance key %s", name, fset.Position(v.Pos()), v.Name)
				}
			case *ast.BasicLit:
				if v.Kind == token.STRING && forbiddenStrings[v.Value] {
					t.Errorf("%s: %s uses governance literal %s", name, fset.Position(v.Pos()), v.Value)
				}
			}
			return true
		})
	}
	if !sawProposeBody {
		t.Fatal("code_import.go never references approval.ProposeBody; the test is not looking at the right file")
	}
}

// TestImport_SourceDiscipline asserts, over the parsed source, the properties
// whose violation no behavioural test can reliably catch: lock scope, the
// infallible write pass, the approval Kind's provenance and gate order.
func TestImport_SourceDiscipline(t *testing.T) {
	_, f := parseGoFile(t, "code_import.go")

	t.Run("ApplyImport takes the lock once and touches Documents only through *Locked helpers", func(t *testing.T) {
		fd := funcDecl(t, f, "codeServer", "ApplyImport")
		if n := len(callPositions(fd.Body, "Lock")); n != 1 {
			t.Fatalf("ApplyImport calls Lock %d times, want exactly 1", n)
		}
		if n := len(callPositions(fd.Body, "Unlock")); n != 1 {
			t.Fatalf("ApplyImport calls Unlock %d times, want exactly 1 (deferred)", n)
		}
		deferred := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if d, ok := n.(*ast.DeferStmt); ok {
				if se, ok := d.Call.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Unlock" {
					deferred = true
				}
			}
			return true
		})
		if !deferred {
			t.Fatal("ApplyImport's Unlock is not deferred")
		}
		for _, m := range []string{"Get", "Put", "Delete", "List", "Query"} {
			for _, se := range selectors(fd.Body) {
				if se.Sel.Name == m && exprString(se.X) == "s.docs" {
					t.Fatalf("ApplyImport calls the Documents gRPC method %s inside the lock", m)
				}
			}
		}
		for _, se := range selectors(fd.Body) {
			switch {
			case exprString(se) == "s.git", exprString(se) == "s.secrets":
				t.Fatalf("ApplyImport references %s: Apply makes no network call and needs no credential", exprString(se))
			case se.Sel.Name == "Reveal", se.Sel.Name == "ListBranches", se.Sel.Name == "Open":
				t.Fatalf("ApplyImport references %s", se.Sel.Name)
			}
		}
	})

	t.Run("ApplyImport's write pass cannot return an error before the applied marker", func(t *testing.T) {
		fd := funcDecl(t, f, "codeServer", "ApplyImport")
		deletes := callPositions(fd.Body, "deleteLocked")
		marks := callPositions(fd.Body, "markOverwriteAppliedLocked")
		if len(deletes) == 0 || len(marks) == 0 {
			t.Fatal("ApplyImport has no deleteLocked or no markOverwriteAppliedLocked call")
		}
		first, lastMark := deletes[0], marks[len(marks)-1]
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if r, ok := n.(*ast.ReturnStmt); ok && r.Pos() > first && r.Pos() < lastMark {
				t.Errorf("a return statement sits between the first deleteLocked and the applied marker")
			}
			return true
		})
	})

	t.Run("analyzeImport holds docs.mu only for the existence loop", func(t *testing.T) {
		fd := funcDecl(t, f, "codeServer", "analyzeImport")
		locks := callPositions(fd.Body, "Lock")
		if len(locks) != 1 {
			t.Fatalf("analyzeImport calls Lock %d times, want exactly 1", len(locks))
		}
		for _, m := range []string{"ListBranches", "Open", "AnalyzeBranch", "resolveGitCredential", "Reveal"} {
			for _, p := range callPositions(fd.Body, m) {
				if p > locks[0] {
					t.Errorf("%s is called after the lock is taken", m)
				}
			}
		}
		if len(callPositions(fd.Body, "targetInUseLocked")) != 1 {
			t.Fatal("analyzeImport should call targetInUseLocked exactly once")
		}
	})

	t.Run("resolveGitCredential never takes the lock and reveals once", func(t *testing.T) {
		fd := funcDecl(t, f, "codeServer", "resolveGitCredential")
		if len(callPositions(fd.Body, "Lock")) != 0 {
			t.Fatal("resolveGitCredential takes docs.mu")
		}
		if len(callPositions(fd.Body, "Reveal")) != 1 {
			t.Fatal("resolveGitCredential must call Reveal exactly once")
		}
	})

	t.Run("ProposeImport composes the approval Kind from the code constants and holds no lock", func(t *testing.T) {
		fd := funcDecl(t, f, "codeServer", "ProposeImport")
		if len(callPositions(fd.Body, "Lock")) != 0 {
			t.Fatal("ProposeImport takes a lock; approval.ProposeBody reaches Documents.Put, which takes the same one")
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok || exprString(cl.Type) != "approval.Kind" {
				return true
			}
			found = true
			for _, el := range cl.Elts {
				kv := el.(*ast.KeyValueExpr)
				se, ok := kv.Value.(*ast.SelectorExpr)
				if !ok || exprString(se.X) != "code" {
					t.Errorf("approval.Kind field %s is not a code package constant", exprString(kv.Key))
				}
			}
			if len(cl.Elts) != 2 {
				t.Errorf("approval.Kind has %d fields set, want exactly Collection and ApproveScope", len(cl.Elts))
			}
			return true
		})
		if !found {
			t.Fatal("ProposeImport builds no approval.Kind literal")
		}
		for _, se := range selectors(fd.Body) {
			if exprString(se.X) == "req" && (se.Sel.Name == "Collection" || se.Sel.Name == "ApproveScope") {
				t.Errorf("a request field reaches the approval Kind")
			}
		}
	})

	t.Run("each gatedCode import forwarder checks code:rw before code:import", func(t *testing.T) {
		_, cf := parseGoFile(t, "code.go")
		for _, name := range []string{"InspectImport", "ProposeImport", "ApplyImport"} {
			fd := funcDecl(t, cf, "gatedCode", name)
			check, imp, inner := callPositions(fd.Body, "check"), callPositions(fd.Body, "checkImport"), callPositions(fd.Body, name)
			if len(check) != 1 || len(imp) != 1 || len(inner) != 1 {
				t.Fatalf("%s: want one check, one checkImport and one delegating call, got %d/%d/%d", name, len(check), len(imp), len(inner))
			}
			if !(check[0] < imp[0] && imp[0] < inner[0]) {
				t.Errorf("%s: the order must be check, checkImport, delegate", name)
			}
		}
	})
}
