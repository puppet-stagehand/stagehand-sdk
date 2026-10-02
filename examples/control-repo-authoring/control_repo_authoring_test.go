package controlrepoauthoring_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	controlrepoauthoring "github.com/puppet-stagehand/stagehand-sdk/examples/control-repo-authoring"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// newHost loads manifest.json from this example's own directory, refuses any
// parse or validation finding, and builds one *host.Host scoped to exactly the
// permissions that manifest declares. The host's grant therefore comes from
// the manifest and nowhere else.
func newHost(t *testing.T) *host.Host {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}
	return local.New(m.Permissions, m.ID)
}

// approverToken mints a CodeKind.ApproveScope-scoped token and returns only
// its secret — standing in for an operator obtaining a token out of band.
// Token minting lives in this test file and nowhere else: it is the entire
// structural claim of this example that no function in
// control_repo_authoring.go can reach it.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      controlrepoauthoring.CodeKind.ApproveScope,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

// ptr returns a pointer to v, for the optional proto3 fields of
// EnvironmentSettings.
func ptr[T any](v T) *T { return &v }

// TestControlRepoAuthoring_EndToEnd walks one blank host through the whole
// slice: create environments, author a module, then author settings through
// the approval gate, and read every byte back. Run with -v to read it as a
// transcript.
func TestControlRepoAuthoring_EndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)
	proposer := controlrepoauthoring.NewProposer(h)
	approver := controlrepoauthoring.NewApprover(h)

	// Step 1: two blank environments.
	for _, name := range []string{"authored", "canary"} {
		env, err := proposer.CreateEnvironment(ctx, name)
		if err != nil {
			t.Fatalf("CreateEnvironment(%q): %v", name, err)
		}
		if env.Name != name {
			t.Fatalf("CreateEnvironment: expected name %q, got %q", name, env.Name)
		}
		t.Logf("step 1: created blank environment %q", env.Name)
	}

	// Step 2: author one Puppetfile module (an ungated, additive write).
	mod, err := proposer.AddModule(ctx, "authored", &hostv1.PuppetfileModule{
		Name:   "puppetlabs/ntp",
		Source: &hostv1.PuppetfileModule_Forge{Forge: &hostv1.ForgeSource{Version: "13.2.1"}},
	})
	if err != nil {
		t.Fatalf("AddModule: %v", err)
	}
	if mod.Name != "puppetlabs/ntp" || mod.GetForge().GetVersion() != "13.2.1" {
		t.Fatalf("AddModule: expected puppetlabs/ntp at 13.2.1, got %+v", mod)
	}
	t.Logf("step 2: added module %s at %s to authored", mod.Name, mod.GetForge().GetVersion())

	// Step 3: propose the environment settings through the gate.
	const proposalID = "settings-authored-1"
	proposal, err := proposer.ProposeSettings(ctx, proposalID, &hostv1.EnvironmentSettings{
		Environment:        "authored",
		ConfigVersion:      ptr("scripts/config_version.sh"),
		EnvironmentTimeout: ptr("5m"),
	})
	if err != nil {
		t.Fatalf("ProposeSettings: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("ProposeSettings: expected status %q, got %q", approval.StatusPending, proposal.Status)
	}
	t.Logf("step 3: proposed settings for authored as %q (status %s)", proposalID, proposal.Status)

	// Step 4: a second persona, holding a token the proposer never saw, approves.
	secret := approverToken(t, h, "operator-ada")
	approved, err := approver.Approve(ctx, proposalID, secret)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("Approve: expected status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-ada" {
		t.Fatalf("Approve: expected DecidedBy %q, got %q", "operator-ada", approved.DecidedBy)
	}
	t.Logf("step 4: %s approved %q", approved.DecidedBy, proposalID)

	// Step 5: apply immediately after Approve, with no intervening read.
	applied, err := proposer.ApplySettings(ctx, proposalID)
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	assertAuthoredSettings(t, "ApplySettings", applied)
	t.Logf("step 5: applied settings: config_version=%s environment_timeout=%s", applied.GetConfigVersion(), applied.GetEnvironmentTimeout())

	// Step 6: read everything back.
	got, err := proposer.Settings(ctx, "authored")
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	assertAuthoredSettings(t, "Settings", got)

	text, err := proposer.RenderPuppetfile(ctx, "authored")
	if err != nil {
		t.Fatalf("RenderPuppetfile: %v", err)
	}
	if want := "mod 'puppetlabs/ntp', '13.2.1'\n"; text != want {
		t.Fatalf("RenderPuppetfile: expected %q, got %q", want, text)
	}

	envs, err := proposer.ListEnvironments(ctx)
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(envs) != 2 || envs[0].Name != "authored" || envs[1].Name != "canary" {
		t.Fatalf("ListEnvironments: expected [authored canary], got %+v", envs)
	}
	t.Logf("step 6: read back settings, Puppetfile %q and environments [%s %s]", text, envs[0].Name, envs[1].Name)
}

// assertAuthoredSettings checks the two written fields and that the five
// unwritten ones are still unset — presence, not zero value: a setting nobody
// wrote stays absent, it does not become a Puppet default.
func assertAuthoredSettings(t *testing.T, label string, s *hostv1.EnvironmentSettings) {
	t.Helper()
	if s.GetEnvironment() != "authored" {
		t.Fatalf("%s: expected environment %q, got %q", label, "authored", s.GetEnvironment())
	}
	if s.ConfigVersion == nil || *s.ConfigVersion != "scripts/config_version.sh" {
		t.Fatalf("%s: expected ConfigVersion %q, got %v", label, "scripts/config_version.sh", s.ConfigVersion)
	}
	if s.EnvironmentTimeout == nil || *s.EnvironmentTimeout != "5m" {
		t.Fatalf("%s: expected EnvironmentTimeout %q, got %v", label, "5m", s.EnvironmentTimeout)
	}
	if s.Modulepath != nil || s.Manifest != nil || s.DisablePerEnvironmentManifest != nil || s.StaticCatalogs != nil || s.RichData != nil {
		t.Fatalf("%s: expected Modulepath, Manifest, DisablePerEnvironmentManifest, StaticCatalogs and RichData to stay unset, got %+v", label, s)
	}
}

// TestControlRepoAuthoring_ManifestDeclaresExactly pins the manifest's grant
// by equality, so an added headroom permission, a renamed route or an
// approval scope promoted into permissions fails here.
func TestControlRepoAuthoring_ManifestDeclaresExactly(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, findings := manifest.Parse(raw)
	if len(findings) > 0 {
		t.Fatalf("manifest.json failed to parse: %v", findings)
	}
	if findings := manifest.Validate(m); len(findings) > 0 {
		t.Fatalf("manifest.json failed validation: %v", findings)
	}

	// The registry read permission for the manifest content rule is absent on
	// purpose: no host/local facet checks it, and the only rule that requires
	// it fires inside the Content != nil branch, which this manifest's null
	// content never enters. Do not add it back.
	wantPerms := []string{"code:rw", "code:import", "forge:rw", "forge:recommend", "secrets:rw", "tokens:issue"}
	if !reflect.DeepEqual(m.Permissions, wantPerms) {
		t.Fatalf("permissions: expected exactly %v, got %v", wantPerms, m.Permissions)
	}
	for _, p := range m.Permissions {
		if p == controlrepoauthoring.CodeKind.ApproveScope {
			t.Fatalf("permissions must never contain %q: the approval scope is a per-decision token, not a standing install-time grant", p)
		}
	}

	type wantRoute struct{ opID, scope string }
	want := []wantRoute{
		{"proposeImport", ""},
		{"proposeOverwrite", ""},
		{"approveProposal", "code:approve"},
		{"rejectProposal", "code:approve"},
	}
	if len(m.Routes) != len(want) {
		t.Fatalf("routes: expected %d, got %d: %+v", len(want), len(m.Routes), m.Routes)
	}
	for i, w := range want {
		if m.Routes[i].OperationID != w.opID {
			t.Errorf("route %d: expected operation_id %q, got %q", i, w.opID, m.Routes[i].OperationID)
		}
		if m.Routes[i].Access.Scope != w.scope {
			t.Errorf("route %q: expected access scope %q, got %q", w.opID, w.scope, m.Routes[i].Access.Scope)
		}
	}

	if m.Content != nil {
		t.Errorf("content: expected nil, got %+v", m.Content)
	}
	if m.OpenAPIPath == "" {
		t.Errorf("openapi_path: required because the manifest declares routes")
	}
}

// TestControlRepoAuthoring_CodeKindIsPinned keeps the two literals the README
// and both guides quote from drifting silently: CodeKind is built from the
// code package constants, and this test is what ties it to the strings.
func TestControlRepoAuthoring_CodeKindIsPinned(t *testing.T) {
	want := approval.Kind{Collection: "code-overwrites", ApproveScope: "code:approve"}
	if controlrepoauthoring.CodeKind != want {
		t.Fatalf("CodeKind: expected %+v, got %+v", want, controlrepoauthoring.CodeKind)
	}
}

// --- Static call-graph analysis backing TestControlRepoAuthoring_ProposerCannotSelfApprove ---

// packageIndex holds every declaration in control_repo_authoring.go's
// production source (never the _test.go files), indexed two ways: by bare
// package-level function name, and by "ReceiverType.MethodName" for every
// method. methodsByReceiver additionally groups methods by receiver type
// name so a walk can be seeded from every method of a given persona type.
type packageIndex struct {
	funcsByName       map[string]*ast.FuncDecl
	methodsByKey      map[string]*ast.FuncDecl
	methodsByReceiver map[string][]*ast.FuncDecl
	all               []*ast.FuncDecl
}

// receiverTypeName returns the identifier name of fl's single receiver
// type, with any leading pointer star removed, or "" if fl names no
// receiver (a package-level function).
func receiverTypeName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	expr := fl.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// buildPackageIndex parses the controlrepoauthoring package's own directory,
// excluding every file ending in "_test.go" — the walker analyses only
// production source, so the token-minting helper living in this very test
// file cannot make the analysis trip over itself.
func buildPackageIndex(t *testing.T) *packageIndex {
	t.Helper()
	fset := token.NewFileSet()
	filter := func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}
	pkgs, err := parser.ParseDir(fset, ".", filter, 0)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	pkg, ok := pkgs["controlrepoauthoring"]
	if !ok {
		names := make([]string, 0, len(pkgs))
		for name := range pkgs {
			names = append(names, name)
		}
		t.Fatalf("expected package %q in %v, found packages: %v", "controlrepoauthoring", ".", names)
	}

	idx := &packageIndex{
		funcsByName:       map[string]*ast.FuncDecl{},
		methodsByKey:      map[string]*ast.FuncDecl{},
		methodsByReceiver: map[string][]*ast.FuncDecl{},
	}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			idx.all = append(idx.all, fn)
			recvType := receiverTypeName(fn.Recv)
			if recvType == "" {
				idx.funcsByName[fn.Name.Name] = fn
				continue
			}
			idx.methodsByKey[recvType+"."+fn.Name.Name] = fn
			idx.methodsByReceiver[recvType] = append(idx.methodsByReceiver[recvType], fn)
		}
	}
	return idx
}

// reachableSelectors walks every method whose receiver type is
// receiverType, and every declaration transitively called from it,
// recording every selector expression syntactically present in each
// visited body. It over-approximates the true call graph in the safe
// direction: it records a selector's bare name for EVERY *ast.SelectorExpr
// encountered, whether or not that selector is actually invoked as a call,
// so it can only report more reachable selectors than truly exist, never
// fewer — a refusal assertion built on this can never produce a false
// pass. selectors holds every bare selector name (e.g. "Approve");
// qualified additionally holds "identifier.Selector" whenever the
// expression's left side is a plain identifier (e.g. "approval.Approve").
// resolved is the count of distinct declarations the walk actually
// visited.
func reachableSelectors(idx *packageIndex, receiverType string) (selectors map[string]bool, qualified map[string]bool, resolved int) {
	selectors = map[string]bool{}
	qualified = map[string]bool{}
	visited := map[*ast.FuncDecl]bool{}

	worklist := append([]*ast.FuncDecl{}, idx.methodsByReceiver[receiverType]...)
	for len(worklist) > 0 {
		fn := worklist[0]
		worklist = worklist[1:]
		if visited[fn] {
			continue
		}
		visited[fn] = true
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				selectors[x.Sel.Name] = true
				if ident, ok := x.X.(*ast.Ident); ok {
					qualified[ident.Name+"."+x.Sel.Name] = true
				}
			case *ast.CallExpr:
				switch callee := x.Fun.(type) {
				case *ast.Ident:
					if pkgFn, ok := idx.funcsByName[callee.Name]; ok {
						worklist = append(worklist, pkgFn)
					}
				case *ast.SelectorExpr:
					if m, ok := idx.methodsByKey[receiverType+"."+callee.Sel.Name]; ok {
						worklist = append(worklist, m)
					}
				}
			}
			return true
		})
	}
	return selectors, qualified, len(visited)
}

// TestControlRepoAuthoring_ProposerCannotSelfApprove is the mechanized form
// of ROADMAP criterion 2 for this example: it parses only this package's own
// directory (the cwd under go test), so it is independently scoped from the
// Phase 5 example's identical-in-shape test, and asserts over the real AST
// that the proposing and deciding call graphs are structurally separate.
func TestControlRepoAuthoring_ProposerCannotSelfApprove(t *testing.T) {
	idx := buildPackageIndex(t)

	proposerSelectors, proposerQualified, proposerResolved := reachableSelectors(idx, "ProposerBackend")
	approverSelectors, approverQualified, approverResolved := reachableSelectors(idx, "ApproverBackend")

	// Liveness — asserted first and separately, because a refusal assertion
	// over an empty set passes for free. This block proves the walker sees
	// real call edges before its refusals mean anything.
	if !proposerQualified["approval.ProposeBody"] {
		t.Errorf("liveness: expected approval.ProposeBody reachable from ProposerBackend, got qualified selectors %v", proposerQualified)
	}
	if !proposerSelectors["ApplyEnvironmentSettings"] {
		t.Errorf("liveness: expected ApplyEnvironmentSettings reachable from ProposerBackend, got selectors %v", proposerSelectors)
	}
	if !approverQualified["approval.Approve"] {
		t.Errorf("liveness: expected approval.Approve reachable from ApproverBackend, got qualified selectors %v", approverQualified)
	}
	if !approverQualified["approval.Reject"] {
		t.Errorf("liveness: expected approval.Reject reachable from ApproverBackend, got qualified selectors %v", approverQualified)
	}
	if proposerResolved < 6 {
		t.Errorf("liveness: expected the walker to resolve at least 6 ProposerBackend methods, resolved %d", proposerResolved)
	}
	if approverResolved < 2 {
		t.Errorf("liveness: expected the walker to resolve at least 2 ApproverBackend methods, resolved %d", approverResolved)
	}

	// Refusal, proposer side: nothing reachable from ProposerBackend may
	// select Approve/Reject/Get on the approval package, IssueToken or Verify
	// on anything, or touch the Documents facet. Apply* selectors are
	// deliberately allowed: an apply only reads an already-approved proposal.
	for _, forbidden := range []string{"approval.Approve", "approval.Reject", "approval.Get"} {
		if proposerQualified[forbidden] {
			t.Errorf("security: ProposerBackend can reach %s — proposing and deciding have landed in one call graph, which docs/approval-pattern.md requires be treated as a security change", forbidden)
		}
	}
	for _, forbidden := range []string{"IssueToken", "Verify"} {
		if proposerSelectors[forbidden] {
			t.Errorf("security: ProposerBackend can reach a selector named %s on some receiver — the proposing persona must never mint or verify a token (docs/approval-pattern.md)", forbidden)
		}
	}
	if proposerSelectors["Documents"] {
		t.Errorf("security: ProposerBackend touches the Documents facet directly — every proposal write must go through the approval package, not raw Documents access (docs/approval-pattern.md)")
	}

	// Refusal, approver side: nothing reachable from ApproverBackend may
	// select Propose or ProposeBody on the approval package, touch the Code,
	// Forge or Documents facets, or select IssueToken.
	for _, forbidden := range []string{"approval.Propose", "approval.ProposeBody"} {
		if approverQualified[forbidden] {
			t.Errorf("security: ApproverBackend can reach %s — deciding and proposing have landed in one call graph (docs/approval-pattern.md)", forbidden)
		}
	}
	for _, forbidden := range []string{"Code", "Forge", "Documents"} {
		if approverSelectors[forbidden] {
			t.Errorf("security: ApproverBackend touches the %s facet directly — it must only decide, never author or materialize (docs/approval-pattern.md)", forbidden)
		}
	}
	if approverSelectors["IssueToken"] {
		t.Errorf("security: ApproverBackend can reach IssueToken — the approving persona must never mint its own token (docs/approval-pattern.md)")
	}

	// Refusal, whole package: IssueToken must appear in no function body
	// anywhere in the package's non-test source — the example's production
	// code cannot mint a token at all.
	for _, fn := range idx.all {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "IssueToken" {
				recv := receiverTypeName(fn.Recv)
				t.Errorf("security: function %s.%s selects IssueToken — no production code in this package may mint a token (docs/approval-pattern.md)", recv, fn.Name.Name)
			}
			return true
		})
	}
}
