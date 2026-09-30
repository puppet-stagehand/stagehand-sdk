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
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	hostv1 "github.com/puppet-stagehand/stagehand-sdk/gen/go/stagehand/host/v1"
	"github.com/puppet-stagehand/stagehand-sdk/host"
)

// Kind names one governed action: the Documents collection its proposals
// live in and the approval scope required to decide them. Both fields are
// code-defined by the caller. The caller pinning them as a constant — never
// building either from request input, from a proposal document body, or
// from any value the proposing persona controls — is what preserves the
// property the removed package-level scope constant used to provide (D-02,
// GOV-01): a caller who can create proposals must not be able to name the
// scope required to decide them, or they could pick one they can trivially
// self-issue, reopening the self-approval hole this pattern exists to
// close. Two Kinds that share a Collection but differ in ApproveScope are
// distinct: decide hands h.Auth.Verify exactly the ApproveScope it was
// given and never merges them. A Kind with an empty Collection or an empty
// ApproveScope is refused with ErrKindRequired.
type Kind struct {
	Collection   string
	ApproveScope string
}

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

	// keyApprovedScope records the approval scope decide verified the
	// caller's token against, so a reader of an approved proposal can tell
	// which scope authorized it instead of trusting the bare status string.
	keyApprovedScope = "approved_scope"
)

// governanceKeys are the body keys this package owns. A proposer may not
// supply any of them, because each is either the status or part of the audit
// trail that only Approve and Reject write.
var governanceKeys = []string{keyStatus, keyReason, keyDecidedBy, keyDecidedAt, keyApprovedScope}

// Detail codes attached to this package's errors, used by IsAlreadyDecided
// and by callers inspecting a *status.Status's ErrorDetail directly.
const (
	detailAlreadyProposed = "proposal_already_exists"
	detailAlreadyDecided  = "proposal_already_decided"
	detailReasonRequired  = "reject_reason_required"
	detailKindRequired    = "approval_kind_required"
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

// ErrReasonRequired builds the error Reject returns when its reason
// argument is empty or whitespace-only.
func ErrReasonRequired(proposalID string) error {
	st := status.New(codes.InvalidArgument, "reject requires a non-empty reason for proposal "+proposalID)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailReasonRequired,
		Message: "rejecting proposal " + proposalID + " requires a human-readable reason",
		Fix:     "supply a non-empty Reason describing why the node onboarding was denied",
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}

// ErrKindRequired builds the error every entry point returns when the Kind
// it was handed has an empty field. field names which one ("collection" or
// "approve_scope"). A Kind is refused before any document read and before
// any token verification, so a zero-valued Kind can never reach
// h.Auth.Verify with an empty Scope.
func ErrKindRequired(field string) error {
	msg := "approval kind requires a non-empty " + field
	st := status.New(codes.InvalidArgument, msg)
	withDetails, err := st.WithDetails(&hostv1.ErrorDetail{
		Code:    detailKindRequired,
		Message: msg,
		Fix:     "pass a code-defined approval.Kind with both Collection and ApproveScope set",
	})
	if err != nil {
		return st.Err() // details are best-effort; the status itself must never fail to construct
	}
	return withDetails.Err()
}

// IsKindRequired reports whether err is (or wraps) an ErrKindRequired
// error.
func IsKindRequired(err error) bool {
	return errorDetailCode(err) == detailKindRequired
}

// validate refuses a Kind with an empty Collection or ApproveScope.
func (k Kind) validate() error {
	if k.Collection == "" {
		return ErrKindRequired("collection")
	}
	if k.ApproveScope == "" {
		return ErrKindRequired("approve_scope")
	}
	return nil
}

// IsAlreadyDecided reports whether err is (or wraps) an ErrAlreadyDecided
// error. codes.FailedPrecondition alone is not distinguishable enough to
// branch on — a malformed proposal body can also produce that code — so a
// caller that wants to distinguish "already decided" from any other
// failure must use this rather than comparing status.Code directly.
func IsAlreadyDecided(err error) bool {
	return errorDetailCode(err) == detailAlreadyDecided
}

// errorDetailCode walks a status error's Details and returns the Code of
// the first *hostv1.ErrorDetail it finds, or the empty string when err is
// nil, not a status error, or carries no such detail. It backs
// IsAlreadyDecided.
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

// proposalFromBody builds a Proposal snapshot from a decoded document
// body map, reading each key as a string (defaulting to empty) and
// parsing the stored decision time with time.RFC3339Nano, leaving the
// zero time when the key is absent or unparseable.
func proposalFromBody(proposalID string, m map[string]any, version int64) *Proposal {
	p := &Proposal{ID: proposalID, Version: version}
	if s, ok := m[keyStatus].(string); ok {
		p.Status = s
	}
	if s, ok := m[keyReason].(string); ok {
		p.Reason = s
	}
	if s, ok := m[keyDecidedBy].(string); ok {
		p.DecidedBy = s
	}
	if s, ok := m[keyDecidedAt].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			p.DecidedAt = t
		}
	}
	return p
}

// Get reads a proposal's current state through h.Documents.Get and
// returns that error unchanged when no such document exists (surfacing
// the Documents store's own codes.NotFound). Get requires no token:
// reading a proposal is not a governed action, only deciding one is, and
// the Documents facet is already scoped to the pack. Do not add an
// authorization check here — it would look prudent and buy nothing, since
// nothing about this read crosses the governance boundary Approve/Reject
// guard.
func Get(ctx context.Context, h *host.Host, kind Kind, proposalID string) (*Proposal, error) {
	if err := kind.validate(); err != nil {
		return nil, err
	}
	doc, err := h.Documents.Get(ctx, &hostv1.GetDocumentRequest{Collection: kind.Collection, DocId: proposalID})
	if err != nil {
		return nil, err
	}
	if doc.Body == nil || doc.Body.Value == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "proposal %q has no body", proposalID)
	}
	return proposalFromBody(proposalID, doc.Body.Value.AsMap(), doc.Version), nil
}
