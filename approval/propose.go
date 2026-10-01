package approval

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// Propose creates a pending proposal document for node, keyed by the
// node's own id (D-00: doc_id == proposal_id == node.id). It builds the
// node-shaped body and delegates the write to ProposeBody. This file never
// reaches the token facet — proposing has no business verifying anything,
// and a reader should be able to confirm that by opening this one short
// file.
func Propose(ctx context.Context, h *host.Host, node *hostv1.Node, kind Kind) (*Proposal, error) {
	if err := kind.validate(); err != nil {
		return nil, err
	}
	if node == nil || node.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "node with a non-empty id is required")
	}

	nodeBody := map[string]any{"id": node.Id}
	if node.DisplayName != "" {
		nodeBody["display_name"] = node.DisplayName
	}
	if node.Environment != "" {
		nodeBody["environment"] = node.Environment
	}
	if len(node.Facts) > 0 {
		facts := make(map[string]any, len(node.Facts))
		for k, v := range node.Facts {
			facts[k] = unwrapFact(v)
		}
		nodeBody["facts"] = facts
	}

	return ProposeBody(ctx, h, kind, node.Id, map[string]any{keyNode: nodeBody})
}

// ProposeBody creates a pending proposal document under kind.Collection
// keyed by proposalID, carrying exactly the caller's body entries plus a
// status this package sets itself. It is the generic create path for a
// governed action whose subject is not an Inventory node. A body that
// already carries any governance-owned key (status, reason, decided_by,
// decided_at, approved_scope) is refused: those are set by this package when
// a proposal is created or decided and never by a caller, so no caller can
// create a proposal that is born approved or that pre-writes an audit trail
// (a decider, a decision time or a reason) the real approver never wrote.
func ProposeBody(ctx context.Context, h *host.Host, kind Kind, proposalID string, body map[string]any) (*Proposal, error) {
	if err := kind.validate(); err != nil {
		return nil, err
	}
	if proposalID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "proposal id is required")
	}
	for _, k := range governanceKeys {
		if _, present := body[k]; present {
			return nil, status.Errorf(codes.InvalidArgument, "proposal body must not carry the %q key: it is owned by the approval package and never set by a caller", k)
		}
	}

	doc := make(map[string]any, len(body)+1)
	for k, v := range body {
		doc[k] = v
	}
	doc[keyStatus] = StatusPending

	s, err := structpb.NewStruct(doc)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "propose %q: build document body: %v", proposalID, err)
	}

	// The create-only if_version=0 branch IS the no-clobber guarantee
	// (D-01) — do not add a Get-then-check before this write, which would
	// reintroduce the check-then-act window the CAS exists to close.
	resp, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: kind.Collection,
		DocId:      proposalID,
		Body:       &hostv1.Json{Value: s},
		IfVersion:  0,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, ErrAlreadyProposed(proposalID)
		}
		return nil, err
	}

	return &Proposal{ID: proposalID, Status: StatusPending, Version: resp.Version}, nil
}
