package approval

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// ApproveRequest is the input to Approve: the proposal to decide and the
// caller's approval-scoped token secret. There is deliberately no scope
// field here — the required scope is the ScopeApprove package constant
// (D-02), never something a caller can supply.
type ApproveRequest struct {
	ProposalID  string
	TokenSecret string
}

// decide is the single guarded write path shared by Approve and Reject.
// It runs in a fixed order and the order is the security property:
//  1. Refuse an empty proposal id before touching anything.
//  2. Verify the caller's token carries ScopeApprove via h.Auth.Verify,
//     and return its error unchanged. Never compare the returned
//     principal's scopes a second time — the facet already did the
//     comparison, and a host that later namespaces scopes would silently
//     break a second, redundant check here.
//  3. Only now read the proposal with h.Documents.Get, returning its
//     error unchanged so an absent proposal surfaces as the store's own
//     codes.NotFound. Verification precedes the read deliberately: a
//     caller who cannot decide anything must not be able to use this
//     function to learn which proposal ids exist (T-04-04).
//  4. Guard the body, check it is exactly StatusPending — anything else,
//     including absent, non-string, approved or rejected, is refused via
//     ErrAlreadyDecided (D-05, D-09). This is what makes rejection
//     terminal: decide already refuses anything whose status is not
//     pending, so there is no rejected -> pending edge to remove because
//     there is none to write. There is likewise no expiry: a pending
//     proposal waits indefinitely (D-08) because nothing in this SDK has
//     a scheduling mechanism, and inventing one for a package whose job
//     is to compose two existing facets would be new, unproven machinery.
//  5. Copy every entry of the body read back so the node object and any
//     additive field a later version writes survive untouched, then set
//     the new status, the deciding principal's Label, and the current
//     time as an RFC3339Nano UTC string (a time.Time cannot be put
//     directly into a structpb.Struct).
//  6. Write with h.Documents.Put using IfVersion set to the version Get
//     returned. This single CAS write is the linearization point for the
//     whole pattern — there is no local mutex anywhere in this package:
//     whoever's Put succeeds made the decision, and everyone else lost.
//     A returned codes.Aborted is translated into ErrAlreadyDecided,
//     naming what the winner actually recorded via one best-effort Get.
func decide(ctx context.Context, h *host.Host, proposalID, tokenSecret, newStatus, reason string) (*Proposal, error) {
	if proposalID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "proposal id is required")
	}

	principal, err := h.Auth.Verify(ctx, &hostv1.VerifyTokenRequest{Secret: tokenSecret, Scope: ScopeApprove})
	if err != nil {
		return nil, err
	}

	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: Collection, DocId: proposalID})
	if err != nil {
		return nil, err
	}
	if doc.Body == nil || doc.Body.Value == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "proposal %q has no body", proposalID)
	}
	m := doc.Body.Value.AsMap()

	currentStatus, _ := m[keyStatus].(string)
	if currentStatus != StatusPending {
		return nil, ErrAlreadyDecided(proposalID, currentStatus)
	}

	next := make(map[string]any, len(m)+3)
	for k, v := range m {
		next[k] = v
	}
	next[keyStatus] = newStatus
	next[keyDecidedBy] = principal.Label
	decidedAt := time.Now().UTC()
	next[keyDecidedAt] = decidedAt.Format(time.RFC3339Nano)
	if reason != "" {
		next[keyReason] = reason
	}

	s, err := structpb.NewStruct(next)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decide %q: build document body: %v", proposalID, err)
	}

	resp, err := h.Documents.Put(ctx, &hostv1.PutDocumentRequest{
		Collection: Collection,
		DocId:      proposalID,
		Body:       &hostv1.Json{Value: s},
		IfVersion:  doc.Version,
	})
	if err != nil {
		if status.Code(err) == codes.Aborted {
			// A raw version-mismatch conflict tells the caller nothing
			// about governance. Best-effort read of what the winner
			// actually recorded, so the error can name it; fall back to
			// an empty status if that read fails.
			winnerStatus := ""
			if winnerDoc, gerr := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: Collection, DocId: proposalID}); gerr == nil && winnerDoc.Body != nil && winnerDoc.Body.Value != nil {
				winnerStatus, _ = winnerDoc.Body.Value.AsMap()[keyStatus].(string)
			}
			return nil, ErrAlreadyDecided(proposalID, winnerStatus)
		}
		return nil, err
	}

	return &Proposal{
		ID:        proposalID,
		Status:    newStatus,
		Reason:    reason,
		DecidedBy: principal.Label,
		DecidedAt: decidedAt,
		Version:   resp.Version,
	}, nil
}

// Approve is a thin call into decide with the approved status and an
// empty reason. There is no exported way to set a status that skips
// verification or the CAS write.
func Approve(ctx context.Context, h *host.Host, req ApproveRequest) (*Proposal, error) {
	return decide(ctx, h, req.ProposalID, req.TokenSecret, StatusApproved, "")
}

// RejectRequest is the input to Reject: the proposal to decide, the
// caller's approval-scoped token secret, and the required human-readable
// reason (D-06).
type RejectRequest struct {
	ProposalID  string
	TokenSecret string
	Reason      string
}

// Reject trims req.Reason and refuses with ErrReasonRequired when what
// remains is empty, before it presents the token — an argument that can
// never succeed should not consume a verification, and the check reveals
// nothing about stored state. It then delegates to decide with the
// rejected status and the trimmed reason. Every rejection carries an
// audit trail for why a node was denied; that is the whole reason the
// field is required rather than optional.
func Reject(ctx context.Context, h *host.Host, req RejectRequest) (*Proposal, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return nil, ErrReasonRequired(req.ProposalID)
	}
	return decide(ctx, h, req.ProposalID, req.TokenSecret, StatusRejected, reason)
}
