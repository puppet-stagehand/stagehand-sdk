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

// approverToken mints a token for approval.ScopeApprove with a five-minute
// lifetime and returns its secret. It stands in for an operator obtaining
// a token out of band: in this test it is one call away from the
// proposing code, and in a real pack it must not be — nothing in
// host.Local would stop it, which is exactly why the propose/decide
// separation is a structural constraint (D-04) written down rather than
// something this package can check at runtime.
func approverToken(t *testing.T, h *host.Host, label string) string {
	t.Helper()
	tok, err := h.Auth.IssueToken(context.Background(), &hostv1.IssueTokenRequest{
		Scope:      approval.ScopeApprove,
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

	proposal, err := approval.Propose(ctx, h, node)
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
	rawDoc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: approval.Collection, DocId: node.Id})
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

	secret := approverToken(t, h, "operator-1")
	approved, err := approval.Approve(ctx, h, approval.ApproveRequest{ProposalID: node.Id, TokenSecret: secret})
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
	if _, err := approval.Propose(ctx, h, first); err != nil {
		t.Fatalf("first Propose: %v", err)
	}

	second := &hostv1.Node{Id: "n1", DisplayName: "second", Environment: "production"}
	_, err := approval.Propose(ctx, h, second)
	if err == nil {
		t.Fatalf("expected second Propose to fail")
	}
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected codes.AlreadyExists, got %v (%v)", status.Code(err), err)
	}

	rawDoc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: approval.Collection, DocId: "n1"})
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
	if _, err := approval.Propose(ctx, h, node); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	rawBefore, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: approval.Collection, DocId: "n2"})
	if err != nil {
		t.Fatalf("Documents.Get before approve: %v", err)
	}
	nodeBefore := rawBefore.Body.Value.AsMap()["node"]

	secret := approverToken(t, h, "auditor")
	approved, err := approval.Approve(ctx, h, approval.ApproveRequest{ProposalID: "n2", TokenSecret: secret})
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

	rawAfter, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: approval.Collection, DocId: "n2"})
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
		_, err := approval.Approve(ctx, h, approval.ApproveRequest{ProposalID: "does-not-exist", TokenSecret: secret})
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
