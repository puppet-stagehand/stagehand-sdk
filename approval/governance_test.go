package approval_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// TestApproval_ConcurrentApproveIsExactlyOneWinner races eight distinctly
// labelled approval tokens against one pending proposal, releasing all
// eight from a single closed gate so they arrive at decide's CAS write
// together (approval/decide.go). It asserts exactly one nil error, every
// other error satisfying IsAlreadyDecided (never a raw FailedPrecondition
// a caller could confuse with a malformed proposal), and that the document
// lands at version 2 with decided_by equal to the winning goroutine's
// label — version 2 is the assertion that exactly one write landed, which
// a status check alone would not prove. The CAS Put in decide (via
// host/local/documents.go's IfVersion check) is the sole linearization
// point here: there is no mutex anywhere in approval/ to fall back on.
func TestApproval_ConcurrentApproveIsExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "race-01", DisplayName: "race-01-display", Environment: "staging"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	const n = 8
	labels := make([]string, n)
	secrets := make([]string, n)
	for i := 0; i < n; i++ {
		labels[i] = fmt.Sprintf("racer-%d", i)
		secrets[i] = approverToken(t, h, labels[i])
	}

	errs := make([]error, n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-gate
			_, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: node.Id, TokenSecret: secrets[i]})
			errs[i] = err
		}(i)
	}
	close(gate)
	wg.Wait()

	winner := -1
	nilCount := 0
	for i, err := range errs {
		if err == nil {
			nilCount++
			winner = i
			continue
		}
		if !approval.IsAlreadyDecided(err) {
			st := status.Convert(err)
			t.Fatalf("goroutine %d: expected IsAlreadyDecided, got code %v message %q", i, st.Code(), st.Message())
		}
	}
	if nilCount != 1 {
		t.Fatalf("expected exactly 1 nil error across 8 racing approvals, got %d", nilCount)
	}

	prop, err := approval.Get(ctx, h, testKind, node.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if prop.Version != 2 {
		t.Fatalf("expected version 2 after the race (exactly one write landed), got %d", prop.Version)
	}
	if prop.Status != approval.StatusApproved {
		t.Fatalf("expected status %q, got %q", approval.StatusApproved, prop.Status)
	}
	if prop.DecidedBy != labels[winner] {
		t.Fatalf("expected decided_by %q (the winning goroutine's label), got %q", labels[winner], prop.DecidedBy)
	}
}

// TestApproval_ConcurrentApproveAndRejectResolveToOneOutcome races one
// Approve against one Reject on the same pending proposal through the
// same closed-gate shape. Exactly one of the two must return nil, the
// other must satisfy IsAlreadyDecided, and a subsequent Get must report
// the status the winner's returned Proposal carried — whichever decision
// lands first is the one that stands (D-09's terminality from the other
// side), and the loser is told so rather than allowed to overwrite it.
func TestApproval_ConcurrentApproveAndRejectResolveToOneOutcome(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "race-02", DisplayName: "race-02-display"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	approveSecret := approverToken(t, h, "approver")
	rejectSecret := approverToken(t, h, "rejecter")

	var approveErr, rejectErr error
	var approveProp, rejectProp *approval.Proposal
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-gate
		approveProp, approveErr = approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: node.Id, TokenSecret: approveSecret})
	}()
	go func() {
		defer wg.Done()
		<-gate
		rejectProp, rejectErr = approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: node.Id, TokenSecret: rejectSecret, Reason: "raced rejection"})
	}()
	close(gate)
	wg.Wait()

	nilCount := 0
	var winnerStatus, winnerReason string
	if approveErr == nil {
		nilCount++
		winnerStatus = approveProp.Status
		winnerReason = approveProp.Reason
	} else if !approval.IsAlreadyDecided(approveErr) {
		t.Fatalf("Approve: expected nil or IsAlreadyDecided, got %v", approveErr)
	}
	if rejectErr == nil {
		nilCount++
		winnerStatus = rejectProp.Status
		winnerReason = rejectProp.Reason
	} else if !approval.IsAlreadyDecided(rejectErr) {
		t.Fatalf("Reject: expected nil or IsAlreadyDecided, got %v", rejectErr)
	}
	if nilCount != 1 {
		t.Fatalf("expected exactly 1 nil error between the racing approve and reject, got %d", nilCount)
	}

	prop, err := approval.Get(ctx, h, testKind, node.Id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if prop.Status != winnerStatus {
		t.Fatalf("expected document status %q to equal the winner's returned status, got %q", winnerStatus, prop.Status)
	}
	if winnerStatus == approval.StatusRejected {
		if prop.Reason == "" || prop.Reason != winnerReason {
			t.Fatalf("expected the rejection's reason %q to be recorded, got %q", winnerReason, prop.Reason)
		}
	} else {
		if prop.Reason != "" {
			t.Fatalf("expected no reason recorded when the approval won, got %q", prop.Reason)
		}
	}
}

// TestApproval_ConcurrentApproveMaterializesOneNode proves the onboarding
// side effect stays exactly-once from both sides of the sequencing
// contract PITFALLS.md's Pitfall 5 describes. The first subtest models
// the pattern as documented: a racer onboards only after its own Approve
// call returned success, so the side effect is causally downstream of the
// CAS write, never of an earlier read. The second subtest is belt and
// braces — every racer onboards regardless of its own result, modelling a
// caller that ignores the sequencing contract — and still must leave
// exactly one node, because OnboardNode's own idempotency
// (host/local/inventory.go's ONBOARDED short-circuit) closes the window
// from the other side. The pattern should not rely on only one of the two
// guarantees.
func TestApproval_ConcurrentApproveMaterializesOneNode(t *testing.T) {
	ctx := context.Background()

	t.Run("only the winner onboards", func(t *testing.T) {
		h := testHost(t, "inventory:rw", "tokens:issue")
		node := &hostv1.Node{Id: "race-onboard-01", DisplayName: "race-onboard-01-display"}
		if _, err := approval.Propose(ctx, h, node, inventoryKind); err != nil {
			t.Fatalf("Propose: %v", err)
		}

		const n = 5
		secrets := make([]string, n)
		for i := 0; i < n; i++ {
			secrets[i] = approverTokenFor(t, h, inventoryKind, fmt.Sprintf("onboard-racer-%d", i))
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				<-gate
				// Sequencing the pattern actually asks for: the side
				// effect happens only after decide's own CAS write has
				// returned success, never off the earlier read.
				if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: inventoryKind, ProposalID: node.Id, TokenSecret: secrets[i]}); err == nil {
					_, _ = h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
				}
			}(i)
		}
		close(gate)
		wg.Wait()

		assertExactlyOneOnboardedNode(t, ctx, h, node.Id)
	})

	t.Run("every racer onboards regardless", func(t *testing.T) {
		h := testHost(t, "inventory:rw", "tokens:issue")
		node := &hostv1.Node{Id: "race-onboard-02", DisplayName: "race-onboard-02-display"}
		if _, err := approval.Propose(ctx, h, node, inventoryKind); err != nil {
			t.Fatalf("Propose: %v", err)
		}

		const n = 5
		secrets := make([]string, n)
		for i := 0; i < n; i++ {
			secrets[i] = approverTokenFor(t, h, inventoryKind, fmt.Sprintf("onboard-racer-b-%d", i))
		}

		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				<-gate
				// Ignoring the sequencing contract on purpose: onboard
				// whether or not this goroutine's own approve won.
				_, _ = approval.Approve(ctx, h, approval.ApproveRequest{Kind: inventoryKind, ProposalID: node.Id, TokenSecret: secrets[i]})
				_, _ = h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
			}(i)
		}
		close(gate)
		wg.Wait()

		assertExactlyOneOnboardedNode(t, ctx, h, node.Id)
	})
}

// assertExactlyOneOnboardedNode lists nodes and fails unless exactly one
// record for nodeID exists, at hostv1.Node_ONBOARDED.
func assertExactlyOneOnboardedNode(t *testing.T, ctx context.Context, h *host.Host, nodeID string) {
	t.Helper()
	lst, err := h.Inventory.ListNodes(ctx, &hostv1.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	count := 0
	for _, n := range lst.Nodes {
		if n.Id == nodeID {
			count++
			if n.Status != hostv1.Node_ONBOARDED {
				t.Fatalf("expected node %q status ONBOARDED, got %v", nodeID, n.Status)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 node record for %q, got %d", nodeID, count)
	}
}

// TestApprovalForgery_DirectPutIsRefused pins the FND-03 guard: a pack that
// holds only the always-granted Documents facet cannot decide an Inventory
// proposal by writing a body straight into inventory-proposals. This test used
// to pin the opposite (the accepted INT-4 residual: the store could not tell a
// Put from approval.Approve); host.Local's Documents facet now registers the
// approval Kinds and refuses any status transition that does not carry the
// approver token decide passes in gRPC metadata.
//
// The residual is closed by FND-03. The test pins four things: a bare status
// flip is refused and leaves the stored proposal pending at the same version, a
// fully forged body (status, approved_scope and decided_by) is refused the same
// way, OnboardNode still refuses the pending proposal, and approval.Approve
// with a real inventory:approve token still decides it so the node onboards.
func TestApprovalForgery_DirectPutIsRefused(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "bypass-01", DisplayName: "bypass-01-display"}
	if _, err := approval.Propose(ctx, h, node, inventoryKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: inventoryKind.Collection, DocId: node.Id})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}

	// putBody writes body over the proposal with no token anywhere in the
	// call, the way a pack holding only Documents would.
	putBody := func(body map[string]any, ifVersion int64) error {
		t.Helper()
		s, err := structpb.NewStruct(body)
		if err != nil {
			t.Fatalf("structpb.NewStruct: %v", err)
		}
		_, err = h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
			Collection: inventoryKind.Collection,
			DocId:      node.Id,
			Body:       &hostv1.Json{Value: s},
			IfVersion:  ifVersion,
		})
		return err
	}
	assertStillPending := func(step string) {
		t.Helper()
		after, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: inventoryKind.Collection, DocId: node.Id})
		if err != nil {
			t.Fatalf("%s: Documents.Get: %v", step, err)
		}
		if after.Version != doc.Version {
			t.Fatalf("%s: stored proposal moved from version %d to %d", step, doc.Version, after.Version)
		}
		if got := after.Body.Value.AsMap()["status"]; got != approval.StatusPending {
			t.Fatalf("%s: stored proposal status is %v, want pending", step, got)
		}
	}

	// Half one: a bare status flip is refused.
	body := doc.Body.Value.AsMap()
	body["status"] = approval.StatusApproved
	if err := putBody(body, doc.Version); err == nil {
		t.Fatalf("a direct Put of status approved was accepted; FND-03 requires the approver token")
	} else if !local.IsApprovalTransitionRefused(err) {
		t.Fatalf("expected IsApprovalTransitionRefused for the status-only flip, got %v", err)
	}
	assertStillPending("status-only flip")

	// Half two: a body that forges the provenance as well is refused too.
	body["approved_scope"] = inventoryKind.ApproveScope
	body["decided_by"] = "forger"
	if err := putBody(body, doc.Version); err == nil {
		t.Fatalf("a fully forged approved record was accepted; FND-03 requires the approver token")
	} else if !local.IsApprovalTransitionRefused(err) {
		t.Fatalf("expected IsApprovalTransitionRefused for the fully forged record, got %v", err)
	}
	assertStillPending("fully forged record")

	if _, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id}); err == nil {
		t.Fatalf("OnboardNode onboarded a proposal that is still pending")
	} else if !approval.IsNotApproved(err) {
		t.Fatalf("expected IsNotApproved for the still-pending proposal, got %v", err)
	}

	// The legitimate path still works: approval.Approve carries the token in
	// metadata and the node onboards.
	tok := approverTokenFor(t, h, inventoryKind, "ops-alice")
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: inventoryKind, ProposalID: node.Id, TokenSecret: tok}); err != nil {
		t.Fatalf("approval.Approve with a real inventory:approve token: %v", err)
	}
	onboarded, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
	if err != nil {
		t.Fatalf("OnboardNode after approval.Approve: %v", err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected node status ONBOARDED after a real approval, got %v", onboarded.Status)
	}
}

// TestApprovalScopeSourcedFromKindOnly parses this package's real
// production source and asserts, over the AST, that the only place an
// approval scope is handed to Auth.Verify is decide's VerifyTokenRequest,
// and that its Scope field is exactly the selector kind.ApproveScope — never
// a string literal, never a value read from a proposal body or a request.
// T-09-01: this is the mechanized form of "the caller pins the scope".
func TestApprovalScopeSourcedFromKindOnly(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse approval/: %v", err)
	}
	pkg, ok := pkgs["approval"]
	if !ok {
		t.Fatalf("package approval not found among %v", pkgs)
	}

	found := 0
	for name, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "VerifyTokenRequest" {
				return true
			}
			found++
			if !strings.HasSuffix(name, "decide.go") {
				t.Errorf("VerifyTokenRequest constructed in %s; only decide.go may present a token", name)
			}
			scopeSeen := false
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Scope" {
					continue
				}
				scopeSeen = true
				val, ok := kv.Value.(*ast.SelectorExpr)
				if !ok {
					t.Errorf("security: VerifyTokenRequest.Scope in %s is %T, want the selector kind.ApproveScope", name, kv.Value)
					continue
				}
				x, isIdent := val.X.(*ast.Ident)
				if !isIdent || x.Name != "kind" || val.Sel.Name != "ApproveScope" {
					t.Errorf("security: VerifyTokenRequest.Scope in %s is not exactly kind.ApproveScope", name)
				}
			}
			if !scopeSeen {
				t.Errorf("security: VerifyTokenRequest in %s sets no Scope field", name)
			}
			return true
		})
	}
	// Liveness: a walk that saw nothing proves nothing.
	if found != 1 {
		t.Fatalf("expected exactly one VerifyTokenRequest literal in approval/ production source, found %d", found)
	}
}

// TestKindRequired proves every entry point refuses a Kind with an empty
// Collection or ApproveScope with InvalidArgument / IsKindRequired. The
// host deliberately lacks tokens:issue: if Approve or Reject reached
// h.Auth.Verify they would return PermissionDenied instead, so seeing
// InvalidArgument proves the refusal precedes token verification.
func TestKindRequired(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw")
	node := &hostv1.Node{Id: "kind-required"}

	kinds := map[string]approval.Kind{
		"empty collection":    {Collection: "", ApproveScope: "test:approve"},
		"empty approve scope": {Collection: "test-proposals", ApproveScope: ""},
		"both empty":          {},
	}
	for name, kind := range kinds {
		kind := kind
		t.Run(name, func(t *testing.T) {
			calls := map[string]func() error{
				"Propose": func() error { _, err := approval.Propose(ctx, h, node, kind); return err },
				"ProposeBody": func() error {
					_, err := approval.ProposeBody(ctx, h, kind, "kind-required", map[string]any{"k": "v"})
					return err
				},
				"Get": func() error { _, err := approval.Get(ctx, h, kind, "kind-required"); return err },
				"Approve": func() error {
					_, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: kind, ProposalID: "kind-required", TokenSecret: "never-issued"})
					return err
				},
				"Reject": func() error {
					_, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: kind, ProposalID: "kind-required", TokenSecret: "never-issued", Reason: "no"})
					return err
				},
			}
			for entry, call := range calls {
				err := call()
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("%s: expected InvalidArgument, got %v", entry, err)
				}
				if !approval.IsKindRequired(err) {
					t.Errorf("%s: expected IsKindRequired to report true, got %v", entry, err)
				}
			}
		})
	}

	if approval.IsKindRequired(nil) || approval.IsKindRequired(approval.ErrAlreadyDecided("x", "")) {
		t.Error("IsKindRequired reported true for an unrelated error")
	}
}

// TestKindsSharingACollectionDoNotMerge proves two Kinds with the same
// Collection but different ApproveScope stay distinct: a token minted for
// one Kind's scope cannot decide a proposal under the other.
func TestKindsSharingACollectionDoNotMerge(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")
	kindA := approval.Kind{Collection: "shared-proposals", ApproveScope: "a:approve"}
	kindB := approval.Kind{Collection: "shared-proposals", ApproveScope: "b:approve"}

	if _, err := approval.ProposeBody(ctx, h, kindA, "shared-1", map[string]any{"what": "thing"}); err != nil {
		t.Fatalf("ProposeBody: %v", err)
	}
	bSecret := approverTokenFor(t, h, kindB, "b-holder")
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: kindA, ProposalID: "shared-1", TokenSecret: bSecret}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a token for %q must not decide a %q proposal: got %v", kindB.ApproveScope, kindA.ApproveScope, err)
	}
	aSecret := approverTokenFor(t, h, kindA, "a-holder")
	prop, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: kindA, ProposalID: "shared-1", TokenSecret: aSecret})
	if err != nil {
		t.Fatalf("Approve with the right scope: %v", err)
	}
	if prop.Status != approval.StatusApproved {
		t.Fatalf("expected approved, got %q", prop.Status)
	}
}

// TestProposeBodyRefusesCallerStatus proves a body carrying the status key
// is refused and no document is created (T-09-03).
func TestProposeBodyRefusesCallerStatus(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	_, err := approval.ProposeBody(ctx, h, testKind, "born-approved", map[string]any{"status": approval.StatusApproved, "k": "v"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if _, gerr := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "born-approved"}); status.Code(gerr) != codes.NotFound {
		t.Fatalf("a refused ProposeBody must create nothing; Documents.Get returned %v", gerr)
	}
}

// TestProposeBodyRefusesGovernanceKeys proves a proposer cannot pre-seed any
// key the approval package owns (WR-05): a pre-written reason, decider, decision
// time or approved scope would otherwise surface as the approver's audit trail.
func TestProposeBodyRefusesGovernanceKeys(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	for _, key := range []string{"status", "reason", "decided_by", "decided_at", "approved_scope"} {
		id := "seeded-" + key
		_, err := approval.ProposeBody(ctx, h, testKind, id, map[string]any{key: "spoofed", "k": "v"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("key %q: expected InvalidArgument, got %v", key, err)
		}
		if _, gerr := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: id}); status.Code(gerr) != codes.NotFound {
			t.Fatalf("key %q: a refused ProposeBody must create nothing; Documents.Get returned %v", key, gerr)
		}
	}
}

// TestApproveDropsAnInheritedReason proves Approve does not carry a reason
// already present in the stored body into the approved record (WR-05). Such a
// body can only arise from a direct Documents.Put, since ProposeBody refuses it.
func TestApproveDropsAnInheritedReason(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")
	if _, err := approval.ProposeBody(ctx, h, testKind, "r1", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("ProposeBody: %v", err)
	}
	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "r1"})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	body := doc.Body.Value.AsMap()
	body["reason"] = "approved by security team"
	s, err := structpb.NewStruct(body)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: testKind.Collection, DocId: "r1", Body: &hostv1.Json{Value: s}, IfVersion: doc.Version,
	}); err != nil {
		t.Fatalf("Documents.Put: %v", err)
	}

	prop, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "r1", TokenSecret: approverToken(t, h, "auditor")})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if prop.Reason != "" {
		t.Fatalf("Approve returned reason %q, want empty", prop.Reason)
	}
	got, err := approval.Get(ctx, h, testKind, "r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Reason != "" {
		t.Fatalf("stored approved record carries reason %q, want none", got.Reason)
	}
}

// TestProposeBodyCreateOnly proves ProposeBody writes a pending document
// carrying exactly the caller's entries plus status, and that a second
// proposal for the same id is refused without touching the first.
func TestProposeBodyCreateOnly(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	prop, err := approval.ProposeBody(ctx, h, testKind, "body-1", map[string]any{"target": "prod", "n": 3.0})
	if err != nil {
		t.Fatalf("ProposeBody: %v", err)
	}
	if prop.Status != approval.StatusPending {
		t.Fatalf("expected pending, got %q", prop.Status)
	}
	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "body-1"})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	got := doc.Body.Value.AsMap()
	want := map[string]any{"status": approval.StatusPending, "target": "prod", "n": 3.0}
	if len(got) != len(want) {
		t.Fatalf("expected exactly keys %v, got %v", want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: want %v, got %v", k, v, got[k])
		}
	}

	_, err = approval.ProposeBody(ctx, h, testKind, "body-1", map[string]any{"target": "other"})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists on a second proposal, got %v", err)
	}
	after, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "body-1"})
	if err != nil {
		t.Fatalf("Documents.Get after loser: %v", err)
	}
	if after.Version != doc.Version || after.Body.Value.AsMap()["target"] != "prod" {
		t.Fatalf("the losing proposal modified the stored document: %v", after.Body.Value.AsMap())
	}
}
