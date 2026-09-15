// Package approval implements a reusable propose/approve/reject governance
// pattern composed entirely from *host.Host's existing Documents and Auth
// fields, and nothing else in its import graph — so a future governed
// action could reuse it unmodified (ONB-04).
//
// This package requires its caller to hold the "tokens:issue" permission,
// because the Auth facet's gate covers Verify as well as IssueToken: a
// host built without that permission refuses every call in this package
// that decides a proposal, before this package ever reads a document.
//
// This package never obtains an approval-scoped token itself — it only
// presents one the caller already holds. A pack whose proposing code path
// can also reach the code that obtains an approval token has defeated the
// self-approval gate in software, not by any check in this package
// failing; the propose and decide call graphs must stay structurally
// separate in the caller, and that separation is the deliverable, not
// something this package's code can enforce at runtime.
//
// Fact values travel through a JSON struct (google.golang.org/protobuf/
// types/known/structpb.Struct), which cannot hold invalid UTF-8. A fact
// that needs to carry raw bytes must be base64-encoded before it is
// proposed and decoded symmetrically on read: host.Local never
// serializes, so it would tolerate the corruption silently, while a real
// backend would reject it outright.
package approval

import (
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
)

// Collection is the D-00 Documents collection this package writes to and
// reads from — the exact literal host/local/inventory.go's OnboardNode
// already binds to (proposalCollection).
const Collection = "inventory-proposals"

// ScopeApprove is the fixed, code-defined approval scope required to
// decide a proposal (D-02). It is never sourced from the proposal
// document's body or from caller-supplied request input: a caller who can
// create proposals must not be able to name the scope required to decide
// them, or they could pick one they can trivially self-issue, reopening
// the self-approval hole this pattern exists to close.
const ScopeApprove = "inventory:approve"

// The complete status vocabulary a proposal document can carry. There is
// no fourth value — no "expired" status exists (D-08).
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// Unexported body key constants. Phase 3's reader (host/local/
// inventory.go's OnboardNode and nodeFromProposal) already binds to these
// exact names, so every write in this package goes through these
// constants rather than repeating literals.
const (
	keyStatus    = "status"
	keyNode      = "node"
	keyReason    = "reason"
	keyDecidedBy = "decided_by"
	keyDecidedAt = "decided_at"
)

// Detail codes attached to this package's errors, used by IsAlreadyDecided
// and by callers inspecting a *status.Status's ErrorDetail directly.
const (
	detailAlreadyProposed = "proposal_already_exists"
	detailAlreadyDecided  = "proposal_already_decided"
	detailReasonRequired  = "reject_reason_required"
)

// Proposal is a read-only snapshot of a proposal document. It carries no
// host reference and no method that writes — every write goes through
// Propose, Approve or Reject.
type Proposal struct {
	ID        string
	Status    string
	Reason    string
	DecidedBy string
	DecidedAt time.Time
	Version   int64
}

// ErrAlreadyProposed builds the error Propose returns when a proposal for
// proposalID already exists (D-01).
func ErrAlreadyProposed(proposalID string) error {
	st := status.New(codes.AlreadyExists, "proposal "+proposalID+" already exists")
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailAlreadyProposed,
		Message: "a proposal for node " + proposalID + " already exists and cannot be overwritten",
		Fix:     "propose a different node id, or wait for the existing proposal to be decided",
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}

// ErrAlreadyDecided builds the error Approve/Reject return when a
// proposal's status is anything other than pending (D-05, D-09).
// currentStatus names what was actually found, so the loser of a race
// learns what the winner did; pass "" when the status could not be
// determined.
func ErrAlreadyDecided(proposalID, currentStatus string) error {
	msg := "proposal " + proposalID + " has already been decided"
	if currentStatus != "" {
		msg += " (status: " + currentStatus + ")"
	}
	st := status.New(codes.FailedPrecondition, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailAlreadyDecided,
		Message: msg,
		Fix:     "propose a new node id to retry — a decided proposal cannot transition back to pending",
	})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// errorDetailCode walks a status error's Details and returns the Code of
// the first *hostv1.ErrorDetail it finds, or the empty string when err is
// nil, not a status error, or carries no such detail. It backs
// IsAlreadyDecided (added in Task 2).
func errorDetailCode(err error) string {
	if err == nil {
		return ""
	}
	st := status.Convert(err)
	for _, d := range st.Details() {
		if ed, ok := d.(*hostv1.ErrorDetail); ok {
			return ed.Code
		}
	}
	return ""
}

// unwrapFact is the write-side partner of local.jsonScalar
// (host/local/inventory.go lines 515-528) — the two are a write/read pair
// across a package boundary and must agree on the single-field "v"
// convention or facts arrive corrupted after onboarding. A nil or
// nil-valued input returns nil. Otherwise unwrapFact reads the fact's
// AsMap() and, when it has exactly one entry whose key is exactly "v",
// returns that entry's value; otherwise it returns the whole map. The key
// check is on the name "v" specifically, not merely on the map having one
// entry, so an incidentally single-key object fact still survives the
// round trip as an object rather than being mistaken for a scalar
// wrapper.
func unwrapFact(j *hostv1.Json) any {
	if j == nil || j.Value == nil {
		return nil
	}
	m := j.Value.AsMap()
	if v, ok := m["v"]; ok && len(m) == 1 {
		return v
	}
	return m
}
