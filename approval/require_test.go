package approval_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// handWritten builds a proposal document the way anything holding the host
// could: straight into a *hostv1.Document, with no approval.Approve involved.
func handWritten(t *testing.T, id string, body map[string]any) *hostv1.Document {
	t.Helper()
	s, err := structpb.NewStruct(body)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return &hostv1.Document{DocId: id, Body: &hostv1.Json{Value: s}, Version: 1}
}

// proposeAndFetch proposes a node under kind, optionally approves it under
// approveKind (the Kind whose scope the token carries), and returns the stored
// document.
func proposeAndFetch(t *testing.T, h *host.Host, kind approval.Kind, approveKind *approval.Kind, id string) *hostv1.Document {
	t.Helper()
	ctx := context.Background()
	if _, err := approval.Propose(ctx, h, &hostv1.Node{Id: id, DisplayName: id}, kind); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if approveKind != nil {
		secret := approverTokenFor(t, h, *approveKind, "ops")
		// Approve under approveKind but against kind's collection: the approval
		// package cannot refuse a scope that travels in the caller's Kind, which
		// is the cross-scope hole RequireApproved exists to close.
		if _, err := approval.Approve(ctx, h, approval.ApproveRequest{
			Kind:        approval.Kind{Collection: kind.Collection, ApproveScope: approveKind.ApproveScope},
			ProposalID:  id,
			TokenSecret: secret,
		}); err != nil {
			t.Fatalf("Approve: %v", err)
		}
	}
	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kind.Collection, DocId: id})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	return doc
}

func wantNotApproved(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal, got nil")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
	if !approval.IsNotApproved(err) {
		t.Fatalf("expected IsNotApproved, got %v", err)
	}
}

func TestRequireApproved_PendingIsRefused(t *testing.T) {
	h := testHost(t, "tokens:issue")
	doc := proposeAndFetch(t, h, testKind, nil, "n-pending")
	wantNotApproved(t, approval.RequireApproved(doc, testKind))
}

func TestRequireApproved_RejectedIsRefused(t *testing.T) {
	h := testHost(t, "tokens:issue")
	proposeAndFetch(t, h, testKind, nil, "n-rejected")
	secret := approverToken(t, h, "ops")
	if _, err := approval.Reject(context.Background(), h, approval.RejectRequest{
		Kind: testKind, ProposalID: "n-rejected", TokenSecret: secret, Reason: "no",
	}); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	doc, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "n-rejected"})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	wantNotApproved(t, approval.RequireApproved(doc, testKind))
}

func TestRequireApproved_ApprovedThroughApprovePasses(t *testing.T) {
	h := testHost(t, "tokens:issue")
	doc := proposeAndFetch(t, h, testKind, &testKind, "n-ok")
	if err := approval.RequireApproved(doc, testKind); err != nil {
		t.Fatalf("expected nil for a proposal approved via approval.Approve, got %v", err)
	}
}

func TestRequireApproved_StatusOnlyApprovedIsRefused(t *testing.T) {
	doc := handWritten(t, "forged", map[string]any{"status": "approved", "node": map[string]any{"id": "forged"}})
	err := approval.RequireApproved(doc, testKind)
	wantNotApproved(t, err)
	if !strings.Contains(err.Error(), testKind.ApproveScope) {
		t.Fatalf("refusal must name the required scope %q, got %q", testKind.ApproveScope, err.Error())
	}
}

func TestRequireApproved_ApprovedUnderDifferentKindScopeIsRefused(t *testing.T) {
	h := testHost(t, "tokens:issue")
	// Approved for real, but under inventory:approve; checked against testKind.
	doc := proposeAndFetch(t, h, testKind, &inventoryKind, "n-cross")
	err := approval.RequireApproved(doc, testKind)
	wantNotApproved(t, err)
	if !strings.Contains(err.Error(), testKind.ApproveScope) {
		t.Fatalf("refusal must name the required scope %q, got %q", testKind.ApproveScope, err.Error())
	}
	// And it passes for the Kind whose scope really decided it.
	if err := approval.RequireApproved(doc, approval.Kind{Collection: testKind.Collection, ApproveScope: inventoryKind.ApproveScope}); err != nil {
		t.Fatalf("expected nil under the scope that decided it, got %v", err)
	}
}

func TestRequireApproved_EmptyDecidedByIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		by   any
	}{{"empty", ""}, {"absent", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"status": "approved", "approved_scope": testKind.ApproveScope}
			if tc.by != nil {
				body["decided_by"] = tc.by
			}
			wantNotApproved(t, approval.RequireApproved(handWritten(t, "p", body), testKind))
		})
	}
}

func TestRequireApproved_NonStringStatusIsRefused(t *testing.T) {
	for _, v := range []any{true, float64(1), nil} {
		doc := handWritten(t, "p", map[string]any{"status": v, "approved_scope": testKind.ApproveScope, "decided_by": "ops"})
		wantNotApproved(t, approval.RequireApproved(doc, testKind))
	}
	wantNotApproved(t, approval.RequireApproved(handWritten(t, "p", map[string]any{"approved_scope": testKind.ApproveScope, "decided_by": "ops"}), testKind))
}

func TestRequireApproved_EmptyKindIsRefusedBeforeReadingDocument(t *testing.T) {
	good := handWritten(t, "p", map[string]any{"status": "approved", "approved_scope": "x", "decided_by": "ops"})
	for _, k := range []approval.Kind{{}, {Collection: "c"}, {ApproveScope: "s"}} {
		err := approval.RequireApproved(good, k)
		if err == nil || !approval.IsKindRequired(err) {
			t.Fatalf("Kind %+v: expected IsKindRequired, got %v", k, err)
		}
	}
	// Checked before the document is read: a nil document with an empty Kind
	// still reports the Kind problem.
	if err := approval.RequireApproved(nil, approval.Kind{}); !approval.IsKindRequired(err) {
		t.Fatalf("nil doc with empty Kind: expected IsKindRequired, got %v", err)
	}
}

func TestRequireApproved_NilDocumentAndNilBody(t *testing.T) {
	for name, doc := range map[string]*hostv1.Document{
		"nil document": nil,
		"nil body":     {DocId: "p"},
		"nil value":    {DocId: "p", Body: &hostv1.Json{}},
	} {
		err := approval.RequireApproved(doc, testKind)
		if err == nil || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s: expected FailedPrecondition, got %v", name, err)
		}
	}
}

func TestRequireApproved_IsReadOnlyAndIdempotent(t *testing.T) {
	h := testHost(t, "tokens:issue")
	doc := proposeAndFetch(t, h, testKind, &testKind, "n-idem")
	before := doc.Version
	for i := 0; i < 2; i++ {
		if err := approval.RequireApproved(doc, testKind); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	after, err := h.Documents.Get(context.Background(), &hostv1.GetDocumentRequest{Collection: testKind.Collection, DocId: "n-idem"})
	if err != nil {
		t.Fatalf("Documents.Get: %v", err)
	}
	if doc.Version != before || after.Version != before {
		t.Fatalf("RequireApproved changed the document version: before %d, doc %d, stored %d", before, doc.Version, after.Version)
	}
}
