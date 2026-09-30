package approval_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
	"github.com/puppet-stagehand/stagehand-sdk/host/local"
)

// testHost builds a host.Local instance scoped to the given permissions,
// using the built-in Discover fixture set. Tests that need a specific
// Discover candidate set call local.New directly with
// local.WithDiscoverCandidates instead.
func testHost(t *testing.T, perms ...string) *host.Host {
	t.Helper()
	return local.New(perms, "opentofu")
}

// testKind is the Kind almost every test in this package drives. Neither
// of its literals is one of Inventory's, so a reintroduced hardcoded
// default anywhere in approval/ would fail this suite rather than pass by
// coincidence (GOV-01).
var testKind = approval.Kind{Collection: "test-proposals", ApproveScope: "test:approve"}

// inventoryKind is the one Kind that matches what host/local's OnboardNode
// reads (its "inventory-proposals" collection). Only the tests that compose
// approval with Inventory.OnboardNode use it; nothing in approval/ itself
// depends on it.
var inventoryKind = approval.Kind{Collection: "inventory-proposals", ApproveScope: "inventory:approve"}

// approverToken mints a token for testKind.ApproveScope with a five-minute
// lifetime and returns its secret. It stands in for an operator obtaining
// a token out of band: in this test it is one call away from the
// proposing code, and in a real pack it must not be — nothing in
// host.Local would stop it, which is exactly why the propose/decide
// separation is a structural constraint (D-04) written down rather than
// something this package can check at runtime.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	return approverTokenFor(t, h, testKind, label)
}

// approverTokenFor mints a token carrying kind.ApproveScope.
func approverTokenFor(t *testing.T, h *host.Host, kind approval.Kind, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      kind.ApproveScope,
		Label:      label,
		TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("approverToken: IssueToken: %v", err)
	}
	return tok.Secret
}

// scalarFact builds a *hostv1.Json wrapping v in the single-field "v"
// convention this package's unwrapFact / host/local's jsonScalar both
// rely on.
func scalarFact(t *testing.T, v any) *hostv1.Json {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"v": v})
	if err != nil {
		t.Fatalf("scalarFact: structpb.NewStruct: %v", err)
	}
	return &hostv1.Json{Value: s}
}

// objectFact builds a *hostv1.Json wrapping m directly as a plain object,
// distinct from scalarFact's single-field "v" wrapper.
func objectFact(t *testing.T, m map[string]any) *hostv1.Json {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("objectFact: structpb.NewStruct: %v", err)
	}
	return &hostv1.Json{Value: s}
}

func TestApproval_ProposeApproveOnboardEndToEnd(t *testing.T) {
	ctx := context.Background()
	start := time.Now()

	h := local.New([]string{"inventory:rw", "tokens:issue"}, "opentofu", local.WithDiscoverCandidates(
		&hostv1.Node{Id: "web-01.example.test", DisplayName: "web-01", Status: hostv1.Node_DISCOVERED, Environment: "production"},
	))

	discovered, err := h.Inventory.Discover(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(discovered.Candidates) != 1 {
		t.Fatalf("expected 1 discover candidate, got %d", len(discovered.Candidates))
	}
	candidate := discovered.Candidates[0]

	node := &hostv1.Node{
		Id:          candidate.Id,
		DisplayName: candidate.DisplayName,
		Environment: candidate.Environment,
		Facts: map[string]*hostv1.Json{
			"os":  scalarFact(t, "linux"),
			"cpu": objectFact(t, map[string]any{"cores": float64(8)}),
		},
	}

	proposal, err := approval.Propose(ctx, h, node, inventoryKind)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal.Status != approval.StatusPending {
		t.Fatalf("expected status %q, got %q", approval.StatusPending, proposal.Status)
	}
	if proposal.Version != 1 {
		t.Fatalf("expected version 1, got %d", proposal.Version)
	}

	// Read the raw document back to assert the stored shape: top-level
	// status, a node object carrying id/display_name/environment/facts,
	// and no status key nested inside the node object.
	rawDoc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: inventoryKind.Collection, DocId: node.Id})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	rawBody := rawDoc.Body.Value.AsMap()
	if rawBody["status"] != approval.StatusPending {
		t.Fatalf("expected stored status %q, got %v", approval.StatusPending, rawBody["status"])
	}
	rawNode, ok := rawBody["node"].(map[string]any)
	if !ok {
		t.Fatalf("expected stored node object, got %T", rawBody["node"])
	}
	if rawNode["id"] != node.Id || rawNode["display_name"] != node.DisplayName || rawNode["environment"] != node.Environment {
		t.Fatalf("stored node object mismatch: %+v", rawNode)
	}
	if _, hasFacts := rawNode["facts"]; !hasFacts {
		t.Fatalf("expected stored node object to carry facts, got %+v", rawNode)
	}
	if _, hasStatus := rawNode["status"]; hasStatus {
		t.Fatalf("expected no status key nested inside the node object, got %+v", rawNode)
	}

	secret := approverTokenFor(t, h, inventoryKind, "operator-1")
	approved, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: inventoryKind, ProposalID: node.Id, TokenSecret: secret})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != approval.StatusApproved {
		t.Fatalf("expected status %q, got %q", approval.StatusApproved, approved.Status)
	}
	if approved.DecidedBy != "operator-1" {
		t.Fatalf("expected DecidedBy %q, got %q", "operator-1", approved.DecidedBy)
	}
	if approved.DecidedAt.Before(start) {
		t.Fatalf("expected DecidedAt no earlier than test start, got %v (start %v)", approved.DecidedAt, start)
	}

	onboarded, err := h.Inventory.OnboardNode(ctx, &hostv1.OnboardNodeRequest{ProposalId: node.Id})
	if err != nil {
		t.Fatalf("OnboardNode: %v", err)
	}
	if onboarded.Status != hostv1.Node_ONBOARDED {
		t.Fatalf("expected node status ONBOARDED, got %v", onboarded.Status)
	}
	if onboarded.DisplayName != node.DisplayName || onboarded.Environment != node.Environment {
		t.Fatalf("onboarded node mismatch: %+v", onboarded)
	}
	if len(onboarded.Facts) != 2 {
		t.Fatalf("expected 2 facts on onboarded node, got %d", len(onboarded.Facts))
	}

	queried, err := h.Inventory.QueryNodes(ctx, &hostv1.QueryNodesRequest{
		Field: "os",
		Op:    hostv1.QueryNodesRequest_EQ,
		Value: scalarFact(t, "linux"),
	})
	if err != nil {
		t.Fatalf("QueryNodes: %v", err)
	}
	if len(queried.Nodes) != 1 || queried.Nodes[0].Id != node.Id {
		t.Fatalf("expected exactly node %q from QueryNodes, got %+v", node.Id, queried.Nodes)
	}
}

func TestApproval_SecondProposeDoesNotClobber(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	first := &hostv1.Node{Id: "n1", DisplayName: "first", Environment: "staging"}
	if _, err := approval.Propose(ctx, h, first, testKind); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	second := &hostv1.Node{Id: "n1", DisplayName: "second", Environment: "production"}
	_, err := approval.Propose(ctx, h, second, testKind)
	if err == nil {
		t.Fatalf("expected second Propose to fail")
	}
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected codes.AlreadyExists, got %v (%v)", status.Code(err), err)
	}

	rawDoc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "n1"})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	if rawDoc.Version != 1 {
		t.Fatalf("expected version 1 after a rejected second propose, got %d", rawDoc.Version)
	}
	rawNode := rawDoc.Body.Value.AsMap()["node"].(map[string]any)
	if rawNode["display_name"] != "first" || rawNode["environment"] != "staging" {
		t.Fatalf("expected the first proposal's content to survive, got %+v", rawNode)
	}
}

func TestApproval_ApproveWritesAuditTrail(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")
	start := time.Now()

	node := &hostv1.Node{Id: "n2", DisplayName: "n2-display", Environment: "staging"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	rawBefore, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "n2"})
	if err != nil {
		t.Fatalf("Documents.Get before approve: %v", err)
	}
	nodeBefore := rawBefore.Body.Value.AsMap()["node"]

	secret := approverToken(t, h, "auditor")
	approved, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n2", TokenSecret: secret})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.DecidedBy != "auditor" {
		t.Fatalf("expected DecidedBy %q, got %q", "auditor", approved.DecidedBy)
	}
	if approved.DecidedAt.Before(start) {
		t.Fatalf("expected DecidedAt no earlier than test start, got %v", approved.DecidedAt)
	}
	if approved.Version != 2 {
		t.Fatalf("expected version 2 after approval, got %d", approved.Version)
	}

	rawAfter, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "n2"})
	if err != nil {
		t.Fatalf("Documents.Get after approve: %v", err)
	}
	afterBody := rawAfter.Body.Value.AsMap()
	if afterBody["decided_by"] != "auditor" {
		t.Fatalf("expected stored decided_by %q, got %v", "auditor", afterBody["decided_by"])
	}
	if _, ok := afterBody["decided_at"].(string); !ok {
		t.Fatalf("expected stored decided_at to be a string, got %T", afterBody["decided_at"])
	}
	nodeAfter := afterBody["node"]
	if fmtEqual(nodeBefore, nodeAfter) == false {
		t.Fatalf("expected node object unchanged by approval: before %+v after %+v", nodeBefore, nodeAfter)
	}

	t.Run("unknown proposal id", func(t *testing.T) {
		secret := approverToken(t, h, "auditor-2")
		_, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "does-not-exist", TokenSecret: secret})
		if err == nil {
			t.Fatalf("expected an error for an unknown proposal id")
		}
		if status.Code(err) != codes.NotFound {
			t.Fatalf("expected codes.NotFound, got %v (%v)", status.Code(err), err)
		}
	})
}

// fmtEqual is a small deep-equality helper for map[string]any values built
// from structpb.Struct.AsMap(), avoiding a reflect.DeepEqual import for
// this one comparison.
func fmtEqual(a, b any) bool {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok != bok {
		return false
	}
	if !aok {
		return a == b
	}
	if len(am) != len(bm) {
		return false
	}
	for k, av := range am {
		bv, ok := bm[k]
		if !ok {
			return false
		}
		if !fmtEqual(av, bv) {
			return false
		}
	}
	return true
}

func TestApproval_DecideRequiresTheApprovalScope(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "n-scope", DisplayName: "n-scope-display", Environment: "staging"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// A token that is perfectly valid for something else must still be
	// refused here: the facet compares scopes for equality and carries
	// exactly one per token.
	wrongScope, err := h.Auth.IssueToken(ctx, &hostv1.IssueTokenRequest{Scope: "some:other-scope", Label: "wrong-scope-holder", TtlSeconds: 300})
	if err != nil {
		t.Fatalf("IssueToken(wrong scope): %v", err)
	}

	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-scope", TokenSecret: wrongScope.Secret}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Approve with wrong-scope token: expected codes.PermissionDenied, got %v (%v)", status.Code(err), err)
	}
	if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-scope", TokenSecret: wrongScope.Secret, Reason: "no"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Reject with wrong-scope token: expected codes.PermissionDenied, got %v (%v)", status.Code(err), err)
	}

	// A secret no token was ever issued for is Unauthenticated, not
	// PermissionDenied.
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-scope", TokenSecret: "never-issued"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Approve with unissued secret: expected codes.Unauthenticated, got %v (%v)", status.Code(err), err)
	}
	if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-scope", TokenSecret: "never-issued", Reason: "no"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Reject with unissued secret: expected codes.Unauthenticated, got %v (%v)", status.Code(err), err)
	}

	// The proposal must still be pending after every refusal above.
	prop, err := approval.Get(ctx, h, testKind, "n-scope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if prop.Status != approval.StatusPending {
		t.Fatalf("expected proposal to still be pending after refused decisions, got %q", prop.Status)
	}
}

func TestApproval_DecideVerifiesBeforeReadingTheProposal(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	// A bad secret against a proposal id that was never proposed must
	// surface the token error, not codes.NotFound — an unauthorized
	// caller learns nothing about which proposal ids exist.
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "never-proposed", TokenSecret: "never-issued"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Approve: expected codes.Unauthenticated (not NotFound), got %v (%v)", status.Code(err), err)
	}
	if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "never-proposed", TokenSecret: "never-issued", Reason: "no"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Reject: expected codes.Unauthenticated (not NotFound), got %v (%v)", status.Code(err), err)
	}

	// A host built without the tokens:issue permission is refused by the
	// Auth facet's own gate before any document is read.
	hNoTokens := testHost(t, "inventory:rw")
	if _, err := approval.Approve(ctx, hNoTokens, approval.ApproveRequest{Kind: testKind, ProposalID: "never-proposed", TokenSecret: "anything"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Approve on host without tokens:issue: expected codes.PermissionDenied, got %v (%v)", status.Code(err), err)
	}
	if _, err := approval.Reject(ctx, hNoTokens, approval.RejectRequest{Kind: testKind, ProposalID: "never-proposed", TokenSecret: "anything", Reason: "no"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Reject on host without tokens:issue: expected codes.PermissionDenied, got %v (%v)", status.Code(err), err)
	}
}

func TestApproval_RejectRequiresAReason(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "n-reason", DisplayName: "n-reason-display"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	secret := approverToken(t, h, "operator")

	for _, reason := range []string{"", "   ", "\t\n"} {
		_, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-reason", TokenSecret: secret, Reason: reason})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Reject with reason %q: expected codes.InvalidArgument, got %v (%v)", reason, status.Code(err), err)
		}
	}

	prop, err := approval.Get(ctx, h, testKind, "n-reason")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if prop.Status != approval.StatusPending {
		t.Fatalf("expected proposal to still be pending after every empty-reason reject, got %q", prop.Status)
	}
	if prop.Version != 1 {
		t.Fatalf("expected version 1 (no write happened), got %d", prop.Version)
	}
}

func TestApproval_RejectIsTerminal(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "n-terminal", DisplayName: "n-terminal-display"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	secret := approverToken(t, h, "operator")

	rejected, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-terminal", TokenSecret: secret, Reason: "not ready"})
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != approval.StatusRejected || rejected.Reason != "not ready" {
		t.Fatalf("unexpected rejected proposal: %+v", rejected)
	}

	// Rejecting an already-rejected proposal is refused, and the stored
	// reason/decision time are still the first rejection's.
	_, err = approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-terminal", TokenSecret: secret, Reason: "second try"})
	if !approval.IsAlreadyDecided(err) {
		t.Fatalf("Reject on rejected proposal: expected IsAlreadyDecided, got %v", err)
	}

	// Approving a rejected proposal is refused too — no rejected ->
	// approved edge exists.
	_, err = approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-terminal", TokenSecret: secret})
	if !approval.IsAlreadyDecided(err) {
		t.Fatalf("Approve on rejected proposal: expected IsAlreadyDecided, got %v", err)
	}

	after, err := approval.Get(ctx, h, testKind, "n-terminal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != approval.StatusRejected || after.Reason != "not ready" {
		t.Fatalf("expected the first rejection's content to survive both refused calls, got %+v", after)
	}
}

func TestApproval_ApproveOnADecidedProposalIsRefused(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	node := &hostv1.Node{Id: "n-decided", DisplayName: "n-decided-display"}
	if _, err := approval.Propose(ctx, h, node, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	secret := approverToken(t, h, "operator")

	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-decided", TokenSecret: secret}); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	_, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-decided", TokenSecret: secret})
	if !approval.IsAlreadyDecided(err) {
		t.Fatalf("second Approve on an approved proposal: expected IsAlreadyDecided, got %v", err)
	}

	_, err = approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-decided", TokenSecret: secret, Reason: "too late"})
	if !approval.IsAlreadyDecided(err) {
		t.Fatalf("Reject on an approved proposal: expected IsAlreadyDecided, got %v", err)
	}

	after, err := approval.Get(ctx, h, testKind, "n-decided")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != approval.StatusApproved {
		t.Fatalf("expected proposal to still be approved after both refusals, got %q", after.Status)
	}
}

func TestApproval_GetReadsTheAuditTrail(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	pending := &hostv1.Node{Id: "n-pending", DisplayName: "n-pending-display"}
	if _, err := approval.Propose(ctx, h, pending, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	prop, err := approval.Get(ctx, h, testKind, "n-pending")
	if err != nil {
		t.Fatalf("Get(pending): %v", err)
	}
	if prop.Status != approval.StatusPending {
		t.Fatalf("expected status pending, got %q", prop.Status)
	}
	if prop.Reason != "" || prop.DecidedBy != "" || !prop.DecidedAt.IsZero() {
		t.Fatalf("expected empty decision fields on a pending proposal, got %+v", prop)
	}

	approvedNode := &hostv1.Node{Id: "n-approved", DisplayName: "n-approved-display"}
	if _, err := approval.Propose(ctx, h, approvedNode, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	secret := approverToken(t, h, "auditor")
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "n-approved", TokenSecret: secret}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approvedProp, err := approval.Get(ctx, h, testKind, "n-approved")
	if err != nil {
		t.Fatalf("Get(approved): %v", err)
	}
	if approvedProp.Status != approval.StatusApproved || approvedProp.DecidedBy != "auditor" || approvedProp.DecidedAt.IsZero() {
		t.Fatalf("expected a fully populated audit trail on an approved proposal, got %+v", approvedProp)
	}

	rejectedNode := &hostv1.Node{Id: "n-rejected", DisplayName: "n-rejected-display"}
	if _, err := approval.Propose(ctx, h, rejectedNode, testKind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "n-rejected", TokenSecret: secret, Reason: "not needed"}); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	rejectedProp, err := approval.Get(ctx, h, testKind, "n-rejected")
	if err != nil {
		t.Fatalf("Get(rejected): %v", err)
	}
	if rejectedProp.Status != approval.StatusRejected || rejectedProp.Reason != "not needed" || rejectedProp.DecidedBy != "auditor" || rejectedProp.DecidedAt.IsZero() {
		t.Fatalf("expected a fully populated audit trail on a rejected proposal, got %+v", rejectedProp)
	}
}

func TestApproval_ProposalNotFound(t *testing.T) {
	ctx := context.Background()
	h := testHost(t, "inventory:rw", "tokens:issue")

	if _, err := approval.Get(ctx, h, testKind, "does-not-exist"); status.Code(err) != codes.NotFound {
		t.Fatalf("Get: expected codes.NotFound, got %v (%v)", status.Code(err), err)
	}

	secret := approverToken(t, h, "operator")
	if _, err := approval.Approve(ctx, h, approval.ApproveRequest{Kind: testKind, ProposalID: "does-not-exist", TokenSecret: secret}); status.Code(err) != codes.NotFound {
		t.Fatalf("Approve: expected codes.NotFound, got %v (%v)", status.Code(err), err)
	}
	if _, err := approval.Reject(ctx, h, approval.RejectRequest{Kind: testKind, ProposalID: "does-not-exist", TokenSecret: secret, Reason: "no"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Reject: expected codes.NotFound, got %v (%v)", status.Code(err), err)
	}
}
