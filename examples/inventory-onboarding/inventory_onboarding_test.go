package inventoryonboarding_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	inventoryonboarding "github.com/puppet-stagehand/stagehand-sdk/examples/inventory-onboarding"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
	"github.com/puppet-stagehand/stagehand-sdk/manifest"
)

// scenarioNodeID and scenarioGroupID are deliberately distinct from
// host.Local's built-in Discover/class defaults (db-01/web-01.example.test,
// group "webservers") so that no assertion in this file can pass by
// accidentally matching a default that may change later.
const (
	scenarioNodeID  = "web-07.example.test"
	scenarioGroupID = "production-web"
)

// newHost loads manifest.json from this example's own directory and builds
// one *host.Host seeded with this package's own Discover candidate and
// group-class fixtures — never host.Local's built-in defaults.
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
	return local.New(m.Permissions, m.ID,
		local.WithDiscoverCandidates(
			&hostv1.Node{Id: scenarioNodeID, DisplayName: "web-07", Status: hostv1.Node_DISCOVERED, Environment: "production"},
		),
		local.WithGroupClasses(map[string][]*hostv1.Class{
			scenarioGroupID: {
				{Name: "profile::base"},
				{Name: "role::web_server"},
			},
		}),
	)
}

// approverToken mints an OnboardingKind.ApproveScope-scoped token and returns
// only its secret — standing in for an operator obtaining a token out of
// band. This helper belongs in the test file and nowhere else: it is the
// entire structural claim of this example that no function in
// inventory_onboarding.go can reach it.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      inventoryonboarding.OnboardingKind.ApproveScope,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

func TestInventoryOnboarding_EndToEnd(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)

	// --- Proposer persona: discovers, groups, fact-tags, and proposes. ---
	proposer := inventoryonboarding.NewProposer(h)
	approver := inventoryonboarding.NewApprover(h)

	candidates, err := proposer.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 discover candidate, got %d", len(candidates))
	}
	if candidates[0].Id != scenarioNodeID {
		t.Fatalf("expected candidate id %q, got %q", scenarioNodeID, candidates[0].Id)
	}
	if candidates[0].Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected candidate status DISCOVERED, got %v", candidates[0].Status)
	}

	if err := proposer.AddToGroup(ctx, scenarioNodeID, scenarioGroupID); err != nil {
		t.Fatalf("AddToGroup: %v", err)
	}

	classes, err := proposer.GroupClasses(ctx, scenarioGroupID)
	if err != nil {
		t.Fatalf("GroupClasses: %v", err)
	}
	if len(classes) != 2 || classes[0].Name != "profile::base" || classes[1].Name != "role::web_server" {
		t.Fatalf("expected classes [profile::base role::web_server] in that order, got %+v", classes)
	}

	if _, err := proposer.AttachFacts(ctx, scenarioNodeID, map[string]any{
		"os":        "linux",
		"cpu_count": float64(8),
	}); err != nil {
		t.Fatalf("AttachFacts: %v", err)
	}

	proposal, err := proposer.ProposeOnboarding(ctx, scenarioNodeID)
	if err != nil {
		t.Fatalf("ProposeOnboarding: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("expected proposal status %q, got %q", approval.StatusPending, proposal.Status)
	}

	// --- Operator persona: mints a token out of band and decides. ---
	secret := approverToken(t, h, "operator-1")
	approved, err := approver.Approve(ctx, proposal.ID, secret)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("expected approved status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-1" {
		t.Fatalf("expected DecidedBy %q, got %q", "operator-1", approved.DecidedBy)
	}

	// Onboard is called immediately after Approve's error is checked, with
	// no intervening read of the proposal.
	onboarded, err := proposer.Onboard(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected onboarded node status ONBOARDED, got %v", onboarded.Status)
	}

	found, err := proposer.FindByFact(ctx, "os", "linux")
	if err != nil {
		t.Fatalf("FindByFact: %v", err)
	}
	if len(found) != 1 || found[0].Id != scenarioNodeID {
		t.Fatalf("expected exactly node %q from FindByFact, got %+v", scenarioNodeID, found)
	}
	if found[0].Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected found node status ONBOARDED, got %v", found[0].Status)
	}

	groups, err := h.Inventory.ListNodeGroups(ctx, &hostv1.ListNodeGroupsRequest{NodeId: scenarioNodeID})
	if err != nil {
		t.Fatalf("ListNodeGroups: %v", err)
	}
	if len(groups.Groups) != 1 || groups.Groups[0].Id != scenarioGroupID {
		t.Fatalf("expected node still in group %q after onboarding, got %+v", scenarioGroupID, groups.Groups)
	}
}

// TestInventoryOnboarding_PendingProposalDoesNotOnboard is the runtime half
// of the same property TestInventoryOnboarding_ProposerCannotSelfApprove
// covers structurally: even holding every permission the manifest grants,
// the proposing actor cannot move a node across the governance boundary on
// its own.
func TestInventoryOnboarding_PendingProposalDoesNotOnboard(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)
	proposer := inventoryonboarding.NewProposer(h)

	if _, err := proposer.Discover(ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if err := proposer.AddToGroup(ctx, scenarioNodeID, scenarioGroupID); err != nil {
		t.Fatalf("AddToGroup: %v", err)
	}
	proposal, err := proposer.ProposeOnboarding(ctx, scenarioNodeID)
	if err != nil {
		t.Fatalf("ProposeOnboarding: %v", err)
	}

	if _, err := proposer.Onboard(ctx, proposal.ID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Onboard on a pending proposal: expected codes.FailedPrecondition, got %v (%v)", status.Code(err), err)
	}

	node, err := h.Inventory.GetNode(ctx, &hostv1.GetNodeRequest{Id: scenarioNodeID})
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.Status != hostv1.Node_DISCOVERED {
		t.Fatalf("expected node status DISCOVERED after a refused onboard, got %v", node.Status)
	}
}

// TestInventoryOnboarding_RejectedProposalDoesNotOnboard proves rejection is
// terminal end-to-end through this example's own persona types: a rejected
// proposal never onboards, a second decide attempt is refused, and an
// empty-reason reject is refused before it touches anything.
func TestInventoryOnboarding_RejectedProposalDoesNotOnboard(t *testing.T) {
	ctx := context.Background()
	h := newHost(t)
	proposer := inventoryonboarding.NewProposer(h)
	approver := inventoryonboarding.NewApprover(h)

	if _, err := proposer.Discover(ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	proposal, err := proposer.ProposeOnboarding(ctx, scenarioNodeID)
	if err != nil {
		t.Fatalf("ProposeOnboarding: %v", err)
	}

	secret := approverToken(t, h, "operator-2")
	const reason = "node is not in the change window"
	rejected, err := approver.Reject(ctx, proposal.ID, secret, reason)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != approval.StatusRejected {
		t.Fatalf("expected rejected status %q, got %q", approval.StatusRejected, rejected.Status)
	}
	if rejected.Reason != reason {
		t.Fatalf("expected rejected reason %q, got %q", reason, rejected.Reason)
	}

	if _, err := proposer.Onboard(ctx, proposal.ID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Onboard on a rejected proposal: expected codes.FailedPrecondition, got %v (%v)", status.Code(err), err)
	}

	// Rejection is terminal: a second decide attempt on the same proposal
	// fails and approval.IsAlreadyDecided reports true for that error.
	if _, err := approver.Approve(ctx, proposal.ID, secret); !approval.IsAlreadyDecided(err) {
		t.Fatalf("second Approve on a rejected proposal: expected IsAlreadyDecided, got %v", err)
	}

	if _, err := approver.Reject(ctx, proposal.ID, secret, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Reject with an empty reason: expected codes.InvalidArgument, got %v (%v)", status.Code(err), err)
	}
}

// --- Static call-graph analysis backing TestInventoryOnboarding_ProposerCannotSelfApprove ---

// packageIndex holds every declaration in inventory_onboarding.go's
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

// buildPackageIndex parses the inventoryonboarding package's own directory,
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
	pkg, ok := pkgs["inventoryonboarding"]
	if !ok {
		names := make([]string, 0, len(pkgs))
		for name := range pkgs {
			names = append(names, name)
		}
		t.Fatalf("expected package %q in %v, found packages: %v", "inventoryonboarding", ".", names)
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

// TestInventoryOnboarding_ProposerCannotSelfApprove is the mechanized form
// of ROADMAP criterion 2: it parses this package's own production source
// and asserts, over the real AST, that the proposing and deciding call
// graphs are structurally separate.
func TestInventoryOnboarding_ProposerCannotSelfApprove(t *testing.T) {
	idx := buildPackageIndex(t)

	proposerSelectors, proposerQualified, proposerResolved := reachableSelectors(idx, "ProposerBackend")
	approverSelectors, approverQualified, approverResolved := reachableSelectors(idx, "ApproverBackend")

	// Liveness — asserted first and separately, because a refusal
	// assertion over an empty set passes for free. This block proves the
	// walker sees real call edges before its refusals mean anything.
	if !proposerQualified["approval.Propose"] {
		t.Errorf("liveness: expected approval.Propose reachable from ProposerBackend, got qualified selectors %v", proposerQualified)
	}
	if !proposerSelectors["OnboardNode"] {
		t.Errorf("liveness: expected OnboardNode reachable from ProposerBackend, got selectors %v", proposerSelectors)
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
	// select Approve/Reject/Get on the approval package, IssueToken or
	// Verify on anything, or touch the Documents facet.
	for _, forbidden := range []string{"approval.Approve", "approval.Reject", "approval.Get"} {
		if proposerQualified[forbidden] {
			t.Errorf("security: ProposerBackend can reach %s — proposing and deciding have landed in one call graph, which docs/approval-pattern.md requires be treated as a security change", forbidden)
		}
	}
	for _, forbidden := range []string{"IssueToken", "Verify"} {
		if proposerSelectors[forbidden] {
			t.Errorf("security: ProposerBackend can reach a selector named %s on some receiver — the proposing persona must never mint or verify a token", forbidden)
		}
	}
	if proposerSelectors["Documents"] {
		t.Errorf("security: ProposerBackend touches the Documents facet directly — every proposal write must go through the approval package, not raw Documents access")
	}

	// Refusal, approver side: nothing reachable from ApproverBackend may
	// select Propose on the approval package, touch the Inventory or
	// Documents facets, or select IssueToken.
	if approverQualified["approval.Propose"] {
		t.Errorf("security: ApproverBackend can reach approval.Propose — deciding and proposing have landed in one call graph")
	}
	if approverSelectors["Inventory"] {
		t.Errorf("security: ApproverBackend touches the Inventory facet directly — it must only decide, never materialize")
	}
	if approverSelectors["Documents"] {
		t.Errorf("security: ApproverBackend touches the Documents facet directly")
	}
	if approverSelectors["IssueToken"] {
		t.Errorf("security: ApproverBackend can reach IssueToken — the approving persona must never mint its own token")
	}

	// Refusal, whole package: IssueToken must appear in no function body
	// anywhere in the package's non-test source — the example's
	// production code cannot mint a token at all.
	for _, fn := range idx.all {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "IssueToken" {
				recv := receiverTypeName(fn.Recv)
				t.Errorf("security: function %s%s selects IssueToken — no production code in this package may mint a token", recv, fn.Name.Name)
			}
			return true
		})
	}
}
