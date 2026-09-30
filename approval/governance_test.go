package approval_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
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

// TestApproval_DocumentsHasNoFieldLevelACL exists to pass, not to catch a
// regression: it pins a known, accepted property of the Documents facet
// contract (PITFALLS.md's Pitfall 6) rather than leaving it to be
// rediscovered later as a defect. The Documents facet is a generic
// namespaced JSON document store with no concept of collections, field
// names or state machines — Put will happily flip a proposal's status to
// approved for any caller holding *host.Host, with no token presented at
// all. The approval/ package's integrity guarantee lives entirely in
// being the only code path that is supposed to write this collection past
// creation; that is a discipline enforced by convention, not by anything
// the facet itself checks, and this test names the boundary a real
// console host would have to defend differently.
func TestApproval_DocumentsHasNoFieldLevelACL(t *testing.T) {
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
	body := doc.Body.Value.AsMap()
	body["status"] = approval.StatusApproved

	s, err := structpb.NewStruct(body)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}

	// No token presented anywhere in this call — Documents.Put does not
	// require one, and this is exactly the bypass the test's name says it
	// pins.
	if _, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: inventoryKind.Collection,
		DocId:      node.Id,
		Body:       &hostv1.Json{Value: s},
		IfVersion:  doc.Version,
	}); err != nil {
		t.Fatalf("Documents.Put (direct bypass, no token): %v", err)
	}

	onboarded, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
	if err != nil {
		t.Fatalf("OnboardNode after direct bypass: %v", err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected node status ONBOARDED after the unguarded write, got %v", onboarded.Status)
	}
}
