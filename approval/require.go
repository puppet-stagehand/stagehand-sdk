package approval

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// detailNotApproved is the ErrorDetail code ErrNotApproved attaches, read back
// by IsNotApproved.
const detailNotApproved = "proposal_not_approved"

// ErrNotApproved builds the refusal RequireApproved returns when a proposal
// document does not carry the evidence of an approval under approveScope.
// reason says which of the facts was missing, so a caller reading the message
// learns whether the proposal is simply undecided or carries a forged or
// incomplete record. The status is FailedPrecondition: the caller can proceed
// once a proposal exists and has really been approved.
func ErrNotApproved(proposalID, approveScope, reason string) error {
	msg := "proposal " + proposalID + " is not approved: " + reason
	st := status.New(codes.FailedPrecondition, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailNotApproved,
		Message: msg,
		Fix:     "propose it again and have an operator holding the " + approveScope + " scope approve it through approval.Approve",
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}

// IsNotApproved reports whether err is (or wraps) an ErrNotApproved error.
// codes.FailedPrecondition alone is not distinguishable enough to branch on,
// because a malformed body also produces it, so a caller that wants to tell
// "not approved" apart from any other failure must use this.
func IsNotApproved(err error) bool {
	return errorDetailCode(err) == detailNotApproved
}

// RequireApproved is the single read-only definition of what an approved
// proposal is. It returns nil only when doc's body records all three facts
// approval.Approve writes: a status equal to StatusApproved, an
// approved_scope equal to kind.ApproveScope, and a non-empty decided_by. A
// status string alone proves nothing: the host's Documents guard refuses a
// pack's forged transition (FND-03), but RequireApproved stays the single
// read-side definition and still demands the provenance, so a record that
// reaches the store by any other path is judged the same way. The scope and
// the decider are the provenance only Approve records.
//
// It is read-only by construction. Its only inputs are a document and a
// code-defined Kind: it holds no *host.Host, writes nothing, verifies no token
// and calls nothing in decide.go or propose.go. That is what lets an apply path
// call it without being able to decide the proposal it applies (the
// propose/decide/apply separation in docs/approval-pattern.md). Never call it
// from a propose or decide path, and never build kind from a request field or a
// proposal body.
//
// It reads the same unexported key constants Approve writes, so the writer and
// this reader cannot drift apart.
//
// An empty Kind is refused with ErrKindRequired before the document is read. A
// nil document or one with no body is refused with FailedPrecondition. Every
// other refusal satisfies IsNotApproved.
func RequireApproved(doc *hostv1.Document, kind Kind) error {
	if err := kind.validate(); err != nil {
		return err
	}
	if doc == nil || doc.Body == nil || doc.Body.Value == nil {
		return status.Errorf(codes.FailedPrecondition, "proposal %q has no body", doc.GetDocId())
	}
	id := doc.GetDocId()
	body := doc.Body.Value.AsMap()

	if st, _ := body[keyStatus].(string); st != StatusApproved {
		return ErrNotApproved(id, kind.ApproveScope, "its status is not "+StatusApproved)
	}
	scope, _ := body[keyApprovedScope].(string)
	by, _ := body[keyDecidedBy].(string)
	if scope != kind.ApproveScope || by == "" {
		return ErrNotApproved(id, kind.ApproveScope,
			"it carries no record of a decision under the "+kind.ApproveScope+" scope")
	}
	return nil
}
