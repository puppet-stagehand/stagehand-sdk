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
