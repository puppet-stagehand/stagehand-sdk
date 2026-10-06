package local

// This file is the pack-facing Documents facet's approval guard (FND-03).
//
// Documents is a generic JSON store: left alone it would let any pack that
// holds it write status "approved" into a proposal, which is exactly the
// self-approval the approval package exists to prevent. host.Local therefore
// keeps one registry of approval Kinds and, for a Put or Delete that targets a
// registered Kind's collection, enforces two rules the real console enforces:
//
//   - A status transition (pending to approved or rejected) must carry the
//     approver token approval.decide holds, in gRPC metadata under
//     approval.TokenMetadataKey, and the token must verify against the Kind's
//     ApproveScope through the same Auth path decide used.
//   - A decided proposal is immutable: nothing may Put over it, and a Delete of
//     it needs a valid approver token for that Kind.
//
// Creating a pending proposal (approval.Propose, approval.ProposeBody, the Code
// facet's ProposeImport) stays an ordinary Put.
//
// The in-process *Locked helpers the facets use are deliberately unguarded:
// they model the console writing on its own behalf. SeedDocument (seed.go) is
// the sanctioned operator/test entry to that same path.
//
// The approver token is a secret. No refusal, log line or error text in this
// file names a metadata value; refusals name only the collection, the document
// id and the approve scope (D-06, T-13-08).

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/puppet-stagehand/stagehand-sdk/approval"
	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// Detail codes attached to the two guard refusals, read back by
// IsApprovalTransitionRefused and IsApprovalProposalDecided.
const (
	detailCodeApprovalTransitionRequiresToken = "approval_transition_requires_token"
	detailCodeApprovalProposalDecided         = "approval_proposal_decided"
)

// approvalKinds is the one registry of approval Kinds host.Local guards. Each
// is the code-defined value the facet that reads the proposals already pins.
// Phase 17 registers deploy-proposals here with one line.
var approvalKinds = []approval.Kind{overwriteApprovalKind, inventoryApprovalKind}

// registeredKind returns the registered Kind whose collection is exactly
// collection.
func registeredKind(collection string) (approval.Kind, bool) {
	for _, k := range approvalKinds {
		if k.Collection == collection {
			return k, true
		}
	}
	return approval.Kind{}, false
}

// guardRefusal builds a PERMISSION_DENIED status carrying one ErrorDetail.
// Details are best-effort; the status itself must never fail to construct.
func guardRefusal(code, msg, fix string) error {
	st := status.New(codes.PermissionDenied, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{Code: code, Message: msg, Fix: fix})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// ErrApprovalTransitionRequiresToken builds the refusal a Documents Put or
// Delete returns when it would move a proposal's status, or write governance
// fields, without an approver token that verifies against approveScope.
func ErrApprovalTransitionRequiresToken(collection, docID, approveScope string) error {
	msg := "a write to " + collection + "/" + docID + " that changes its approval status needs an approver token for scope " + approveScope
	return guardRefusal(detailCodeApprovalTransitionRequiresToken, msg,
		"decide proposals only through approval.Approve or approval.Reject with a token carrying the "+approveScope+
			" scope; a direct Documents write cannot set status, reason, decided_by, decided_at or approved_scope")
}

// ErrApprovalProposalDecided builds the refusal a Documents Put or Delete
// returns when it targets a proposal that has already been approved or
// rejected.
func ErrApprovalProposalDecided(collection, docID string) error {
	msg := "proposal " + collection + "/" + docID + " has been decided and is immutable"
	return guardRefusal(detailCodeApprovalProposalDecided, msg,
		"a decided proposal in "+collection+" is immutable; propose a new one with approval.ProposeBody or approval.Propose")
}

// IsApprovalTransitionRefused reports whether err is (or wraps) an
// ErrApprovalTransitionRequiresToken error. It compares the ErrorDetail code
// rather than codes.PermissionDenied alone, which an undeclared facet also
// produces.
func IsApprovalTransitionRefused(err error) bool {
	return overwriteDetailCode(err) == detailCodeApprovalTransitionRequiresToken
}

// IsApprovalProposalDecided reports whether err is (or wraps) an
// ErrApprovalProposalDecided error.
func IsApprovalProposalDecided(err error) bool {
	return overwriteDetailCode(err) == detailCodeApprovalProposalDecided
}

// transitionPrincipal returns the verified principal behind the approver token
// in ctx's incoming metadata, or nil when there is none. It fails closed: nil
// when the server was built without an auth facet, when the metadata does not
// carry exactly one token value, or when Verify refuses the token for
// kind.ApproveScope (unknown, expired or wrong scope).
//
// It runs before the caller takes s.mu, so docs.mu and auth.mu are never held
// together.
func (s *documentsServer) transitionPrincipal(ctx context.Context, kind approval.Kind) *hostv1.Principal {
	if s.auth == nil {
		return nil
	}
	toks := approval.IncomingApproverTokens(ctx)
	if len(toks) != 1 {
		return nil
	}
	p, err := s.auth.Verify(ctx, &hostv1.VerifyTokenRequest{Secret: toks[0], Scope: kind.ApproveScope})
	if err != nil {
		return nil
	}
	return p
}

// bodyStatus returns the string status of body and whether it carried one.
func bodyStatus(body *hostv1.Json) (string, bool) {
	if body == nil || body.Value == nil {
		return "", false
	}
	v, ok := body.Value.Fields["status"]
	if !ok {
		return "", false
	}
	sv, ok := v.GetKind().(*structpb.Value_StringValue)
	if !ok {
		return "", false
	}
	return sv.StringValue, true
}

// isGovernanceKey reports whether key is one of the approval package's
// governance-owned body keys.
func isGovernanceKey(key string) bool {
	for _, k := range approval.GovernanceKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// payloadEqual reports whether a and b carry the same non-governance entries,
// compared key by key in both directions with proto.Equal.
func payloadEqual(a, b *structpb.Struct) bool {
	var af, bf map[string]*structpb.Value
	if a != nil {
		af = a.Fields
	}
	if b != nil {
		bf = b.Fields
	}
	for k, av := range af {
		if isGovernanceKey(k) {
			continue
		}
		bv, ok := bf[k]
		if !ok || !proto.Equal(av, bv) {
			return false
		}
	}
	for k := range bf {
		if isGovernanceKey(k) {
			continue
		}
		if _, ok := af[k]; !ok {
			return false
		}
	}
	return true
}

// checkProposalPut applies the transition and immutability rules to a Put on a
// registered Kind's collection. existing/exists describe the stored document;
// p is the verified approver principal or nil. The caller has already passed
// the CAS preconditions, so a racing decide loser never reaches this function
// and still gets the store's Aborted. Rules, in order:
//
//	(a) a stored proposal whose status is not exactly pending is immutable;
//	(b) a pending body with no governance key but status is a create or an edit
//	    of a pending proposal and is allowed;
//	(c) an approved or rejected body is a transition: allowed only over a
//	    stored pending proposal, with a verified principal, approved_scope equal
//	    to the Kind's scope, decided_by equal to the principal's label and every
//	    non-governance entry unchanged;
//	(d) anything else, including a nil or empty body or a missing or non-string
//	    status, is refused.
func checkProposalPut(kind approval.Kind, collection, docID string, existing *hostv1.Document, exists bool, body *hostv1.Json, p *hostv1.Principal) error {
	if exists {
		if st, ok := bodyStatus(existing.Body); !ok || st != approval.StatusPending {
			return ErrApprovalProposalDecided(collection, docID)
		}
	}

	newStatus, ok := bodyStatus(body)
	if !ok {
		return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
	}

	switch newStatus {
	case approval.StatusPending:
		for k := range body.Value.Fields {
			if k != "status" && isGovernanceKey(k) {
				return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
			}
		}
		return nil
	case approval.StatusApproved, approval.StatusRejected:
		if !exists || p == nil {
			return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
		}
		if scope, _ := stringField(body.Value, "approved_scope"); scope != kind.ApproveScope {
			return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
		}
		if by, _ := stringField(body.Value, "decided_by"); by != p.Label {
			return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
		}
		var existingBody *structpb.Struct
		if existing.Body != nil {
			existingBody = existing.Body.Value
		}
		if !payloadEqual(existingBody, body.Value) {
			return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
		}
		return nil
	default:
		return ErrApprovalTransitionRequiresToken(collection, docID, kind.ApproveScope)
	}
}

// stringField reads key from st as a string.
func stringField(st *structpb.Struct, key string) (string, bool) {
	if st == nil {
		return "", false
	}
	v, ok := st.Fields[key]
	if !ok {
		return "", false
	}
	sv, ok := v.GetKind().(*structpb.Value_StringValue)
	if !ok {
		return "", false
	}
	return sv.StringValue, true
}

// checkProposalDelete applies the immutability rule to a Delete on a registered
// Kind's collection: a pending proposal may be deleted, a decided one only by a
// caller holding a valid approver token for the Kind.
func checkProposalDelete(kind approval.Kind, collection, docID string, existing *hostv1.Document, p *hostv1.Principal) error {
	if st, ok := bodyStatus(existing.Body); ok && st == approval.StatusPending {
		return nil
	}
	if p != nil {
		return nil
	}
	return ErrApprovalProposalDecided(collection, docID)
}
