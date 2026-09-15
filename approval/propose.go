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
// node's own id (D-00: doc_id == proposal_id == node.id). This file never
// reaches the token facet — proposing has no business verifying anything,
// and a reader should be able to confirm that by opening this one short
// file.
func Propose(ctx context.Context, h *host.Host, node *hostv1.Node) (*Proposal, error) {
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

	body := map[string]any{
		keyStatus: StatusPending,
		keyNode:   nodeBody,
	}
	s, err := structpb.NewStruct(body)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "propose %q: build document body: %v", node.Id, err)
	}

	// The create-only if_version=0 branch IS the no-clobber guarantee
	// (D-01) — do not add a Get-then-check before this write, which would
	// reintroduce the check-then-act window the CAS exists to close.
	resp, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: Collection,
		DocId:      node.Id,
		Body:       &hostv1.Json{Value: s},
		IfVersion:  0,
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, ErrAlreadyProposed(node.Id)
		}
		return nil, err
	}

	return &Proposal{ID: node.Id, Status: StatusPending, Version: resp.Version}, nil
}
